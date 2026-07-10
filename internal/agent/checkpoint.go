package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/klog/v2"

	gpucrv1alpha1 "github.com/GProjectdev/K8s-Native-Fast-GPU-Checkpoint-Restore-System/api/v1alpha1"
)

// mountBase is where the agent mounts file-based storage backends (nfs, efs,
// cifs, cephfs, ...) declared via `type: mount` (or the `type: nfs` alias). Each
// distinct source gets its own subdir; the mount lives in the agent's mount
// namespace and is (re)created on demand. Block storage (EBS, ...) is not a
// simple mount — use CSI/PVC for that.
const mountBase = "/var/lib/gpu-cr/mnt"

// Checkpointer runs the GCR checkpoint pipeline on the local node, using GCR's
// control/data separation but with the CONTROL STATE handled by CRIUgpu:
//
//  1. Selective Interception data checkpoint (in-Pod interceptor): copy the GPU
//     data buffers to host memory and free the physical GPU memory while keeping
//     the virtual addresses. The device is then left with only GPU control state.
//  2. CRIUgpu container checkpoint via the kubelet API: CRI-O + CRIU + the NVIDIA
//     cuda_plugin checkpoint the remaining GPU control state AND dump the CPU
//     process (including the host-resident data buffers) into a tar. (This
//     replaces the earlier host cuda-checkpoint helper.)
//  3. Store the tar to the CR-defined backend.
//  4. Remap the data buffers back to the device (non-destructive resume) via the
//     interceptor.
type Checkpointer struct {
	Interceptor *InterceptorManager
	Kubelet     *KubeletClient
	// GCRInterception gates the Selective Interception data steps (1 and 4). When
	// false, CRIUgpu alone handles the whole GPU (no control/data separation).
	GCRInterception bool
	// DataDir is the node dir where the in-Pod interceptor writes the data blob
	// (<DataDir>/<podUID>/data.blob). Mounted into the agent so it can persist it.
	DataDir string
	// PersistBlob copies that data blob next to the tar so the stored checkpoint is
	// COMPLETE and restorable (tar = CPU + GPU control state; blob = GPU data).
	PersistBlob bool
	// DryRun skips the privileged kubelet checkpoint (dev clusters without GPUs);
	// the control flow, status updates and storage layout still run end-to-end.
	DryRun bool
}

// NewCheckpointer wires the pipeline with sane defaults.
func NewCheckpointer(im *InterceptorManager, kc *KubeletClient, dryRun bool) *Checkpointer {
	return &Checkpointer{Interceptor: im, Kubelet: kc, GCRInterception: true, DataDir: "/var/lib/gcr-data", PersistBlob: true, DryRun: dryRun}
}

// Target describes a resolved checkpoint target on this node.
type Target struct {
	Namespace string
	Pod       string
	PodUID    string // keys the interceptor control channel
	Container string
	Storage   gpucrv1alpha1.StorageSpec
}

// Result reports the produced artifact.
type Result struct {
	ArchivePath string
	TakenAt     time.Time
}

// Checkpoint runs the pipeline and returns the stored archive path.
func (c *Checkpointer) Checkpoint(ctx context.Context, t Target) (*Result, error) {
	start := time.Now()
	klog.Infof("checkpoint start: pod=%s/%s container=%s (GCR interception + CRIUgpu)",
		t.Namespace, t.Pod, t.Container)

	// Per-phase timers so the benchmark can attribute the cost of the Selective
	// Interception data checkpoint (freeze) vs. the CRIUgpu container checkpoint
	// (kubelet) vs. store vs. remap. Emitted as a single parseable PHASE_TIMES line.
	var freezeDur, kubeletDur, storeDur, remapDur time.Duration

	// (1) Selective Interception data checkpoint: interceptor copies GPU data
	// buffers to host memory and frees the physical GPU memory (keeps the VA).
	if c.GCRInterception {
		ph := time.Now()
		if err := c.Interceptor.Signal(t.PodUID, GCRSignalCkpt); err != nil {
			return nil, fmt.Errorf("signal data-buffer checkpoint: %w", err)
		}
		if !c.DryRun {
			if err := c.Interceptor.WaitForAck(t.PodUID, 120*time.Second); err != nil {
				return nil, fmt.Errorf("data-buffer checkpoint did not ack: %w", err)
			}
		}
		freezeDur = time.Since(ph)
		klog.V(2).Infof("step 1/4 done: GPU data buffers checkpointed to host, physical memory freed (freeze %s)", freezeDur)
	}

	// (2) CRIUgpu container checkpoint via the kubelet API. cuda_plugin
	// checkpoints the remaining GPU control state; CRIU dumps CPU + host data.
	var produced []string
	{
		ph := time.Now()
		if c.DryRun {
			produced = []string{c.simulatedArchive(t)}
		} else {
			var err error
			produced, err = c.Kubelet.Checkpoint(ctx, t.Namespace, t.Pod, t.Container)
			if err != nil {
				// Best-effort remap so we don't leave the source with freed data.
				if c.GCRInterception {
					_ = c.Interceptor.Signal(t.PodUID, GCRSignalRestore)
				}
				return nil, fmt.Errorf("kubelet CRIUgpu checkpoint: %w", err)
			}
		}
		kubeletDur = time.Since(ph)
	}
	klog.V(2).Infof("step 2/4 done: CRIUgpu produced %v (kubelet %s)", produced, kubeletDur)

	// (3) Remap FIRST — resume the workload as soon as the GPU control state is
	// checkpointed, so the durable store below runs OUTSIDE the workload's downtime
	// window (GCR: minimize downtime; the slow NFS copy no longer blocks the source).
	if c.GCRInterception {
		ph := time.Now()
		if err := c.Interceptor.Signal(t.PodUID, GCRSignalRestore); err != nil {
			klog.Errorf("signal data remap failed: %v", err)
		} else if !c.DryRun {
			if err := c.Interceptor.WaitForAck(t.PodUID, 120*time.Second); err != nil {
				klog.Errorf("data remap did not ack: %v", err)
			}
		}
		remapDur = time.Since(ph)
	}
	downtime := freezeDur + kubeletDur + remapDur
	klog.V(2).Infof("step 3/4 done: source resumed (remap %s); workload downtime %s", remapDur, downtime)

	// (4) Store the tar (+ data blob) to the CR backend. The workload is already
	// running, so this copy is off the critical path (not counted in downtime).
	phStore := time.Now()
	stored, err := c.store(t, produced)
	if err != nil {
		return nil, fmt.Errorf("store checkpoint (workload already resumed): %w", err)
	}
	storeDur = time.Since(phStore)
	total := time.Since(start)
	klog.Infof("step 4/4 done: checkpoint stored at %s; workload downtime %s, store %s, total %s",
		stored, downtime, storeDur, total)
	// freeze = Selective Interception data checkpoint; kubelet = CRIUgpu; remap =
	// interceptor restore; downtime = freeze+kubelet+remap (workload paused); store =
	// durable copy AFTER resume (off critical path); total = whole call.
	klog.Infof("PHASE_TIMES pod=%s/%s gcr=%t freeze_s=%.3f kubelet_s=%.3f store_s=%.3f remap_s=%.3f downtime_s=%.3f total_s=%.3f",
		t.Namespace, t.Pod, c.GCRInterception,
		freezeDur.Seconds(), kubeletDur.Seconds(), storeDur.Seconds(), remapDur.Seconds(), downtime.Seconds(), total.Seconds())
	return &Result{ArchivePath: stored, TakenAt: start}, nil
}

// store copies the produced archive into the CR-defined backend. Backends are
// dispatched by spec.storage.type; adding a new one is a single case here.
func (c *Checkpointer) store(t Target, produced []string) (string, error) {
	switch t.Storage.Type {
	case gpucrv1alpha1.StorageMount:
		// Generic file-mount backend: anything mount(8) understands
		// (nfs, nfs4, efs, cifs, cephfs, glusterfs, ...) via fsType/source/options.
		return c.storeMount(t, produced, t.Storage.FsType, t.Storage.Source,
			t.Storage.Options, firstNonEmpty(t.Storage.SubPath, t.Storage.Path))
	case gpucrv1alpha1.StorageNFS:
		// Convenience alias for a file mount: source = endpoint:path, fsType = nfs.
		server := strings.TrimSpace(t.Storage.Endpoint)
		export := strings.TrimSpace(t.Storage.Path)
		if server == "" || export == "" {
			return "", fmt.Errorf("nfs storage requires storage.endpoint (server) and storage.path (export)")
		}
		return c.storeMount(t, produced, "nfs", server+":"+export,
			firstNonEmpty(t.Storage.Options, "nfsvers=4,nolock"), t.Storage.SubPath)
	case gpucrv1alpha1.StorageHostPath, "":
		// hostPath must be a real mounted volume in the agent, not the container's
		// ephemeral overlay (otherwise the tar is silently lost on Pod restart).
		if !c.DryRun {
			if err := ensureRealMount(t.Storage.Path); err != nil {
				return "", err
			}
		}
		return c.storeToDir(t, produced, t.Storage.Path)
	case gpucrv1alpha1.StoragePVC:
		return "", fmt.Errorf("storage type pvc (CSI-backed: EBS, EFS, ...) needs the checkpoint mover, which is not yet enabled; " +
			"for block storage use a PVC referenced as a DaemonSet volume, or use type: mount for file backends")
	case gpucrv1alpha1.StorageS3:
		return "", fmt.Errorf("storage type s3 not yet implemented (endpoint=%s, path=%s)",
			t.Storage.Endpoint, t.Storage.Path)
	default:
		return "", fmt.Errorf("unknown storage type %q", t.Storage.Type)
	}
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// storeMount mounts a file-based backend (fsType/source/options) in the agent's
// mount namespace on demand and writes the checkpoint tar under it (+subdir).
// Covers any filesystem mount(8) supports: nfs, efs, cifs, cephfs, ...
func (c *Checkpointer) storeMount(t Target, produced []string, fsType, source, options, subdir string) (string, error) {
	fsType = strings.TrimSpace(fsType)
	source = strings.TrimSpace(source)
	if fsType == "" || source == "" {
		return "", fmt.Errorf("mount storage requires storage.fsType and storage.source (got fsType=%q source=%q)", fsType, source)
	}
	mnt := filepath.Join(mountBase, sanitizeMount(fsType+"_"+source))
	if !c.DryRun {
		if err := ensureMounted(fsType, source, options, mnt); err != nil {
			return "", fmt.Errorf("mount %s %s at %s: %w", fsType, source, mnt, err)
		}
	}
	dir := mnt
	if sp := strings.Trim(strings.TrimSpace(subdir), "/"); sp != "" {
		dir = filepath.Join(mnt, sp)
	}
	return c.storeToDir(t, produced, dir)
}

// storeToDir writes (or, in dry-run, simulates) the checkpoint tar into dir.
func (c *Checkpointer) storeToDir(t Target, produced []string, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("checkpoint-%s_%s-%s-%d.tar",
		t.Pod, t.Namespace, t.Container, time.Now().Unix())
	dst := filepath.Join(dir, name)
	if c.DryRun {
		if err := os.WriteFile(dst, []byte("simulated-checkpoint"), 0o644); err != nil {
			return "", err
		}
		return dst, nil
	}
	if len(produced) == 0 {
		return "", fmt.Errorf("kubelet produced no checkpoint archive")
	}
	if err := copyFile(produced[0], dst, 0o644); err != nil {
		return "", err
	}
	// Selective Interception keeps the GPU data in an external blob (out of the tar).
	// Persist it next to the tar so the checkpoint is COMPLETE and restorable.
	if c.GCRInterception && c.PersistBlob {
		blobSrc := filepath.Join(c.DataDir, t.PodUID, "data.blob")
		if fi, err := os.Stat(blobSrc); err == nil && fi.Size() > 0 {
			blobDst := strings.TrimSuffix(dst, ".tar") + ".blob"
			if err := copyFile(blobSrc, blobDst, 0o644); err != nil {
				return "", fmt.Errorf("persist data blob %s: %w", blobSrc, err)
			}
			klog.Infof("stored data blob %s (%d bytes) next to %s — checkpoint is complete", blobDst, fi.Size(), dst)
		} else {
			klog.Warningf("GCR interception on but no data blob at %s; checkpoint holds control+CPU only and may not restore standalone", blobSrc)
		}
	}
	// Free the node-local kubelet checkpoint now that it lives in the backend, so
	// large tars don't pile up on the boot disk and trigger disk-pressure eviction.
	if produced[0] != dst {
		if err := os.Remove(produced[0]); err != nil {
			klog.V(2).Infof("could not remove local checkpoint %s: %v", produced[0], err)
		}
	}
	return dst, nil
}

// ensureMounted mounts source at mnt with fsType/options if it is not already a
// mountpoint. Requires the matching mount helper in the image (nfs-common for
// nfs/efs, cifs-utils for cifs, ...) and a privileged container.
func ensureMounted(fsType, source, options, mnt string) error {
	if isMountpoint(mnt) {
		return nil
	}
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return err
	}
	args := []string{"-t", fsType}
	if strings.TrimSpace(options) != "" {
		args = append(args, "-o", options)
	}
	args = append(args, source, mnt)
	if out, err := exec.Command("mount", args...).CombinedOutput(); err != nil {
		// for nfs, retry once with auto-negotiated version/options
		if fsType == "nfs" || fsType == "nfs4" {
			if out2, err2 := exec.Command("mount", "-t", fsType, source, mnt).CombinedOutput(); err2 != nil {
				return fmt.Errorf("mount failed: %v: %s; fallback: %v: %s",
					err, strings.TrimSpace(string(out)), err2, strings.TrimSpace(string(out2)))
			}
		} else {
			return fmt.Errorf("mount -t %s %s %s failed: %v: %s",
				fsType, source, mnt, err, strings.TrimSpace(string(out)))
		}
	}
	if !isMountpoint(mnt) {
		return fmt.Errorf("mount reported success but %s is not a mountpoint", mnt)
	}
	klog.Infof("mounted %s %s at %s", fsType, source, mnt)
	return nil
}

// isMountpoint reports whether path is an exact mountpoint in /proc/mounts.
func isMountpoint(path string) bool {
	p := filepath.Clean(path)
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && filepath.Clean(unescapeMount(fields[1])) == p {
			return true
		}
	}
	return false
}

// ensureRealMount fails if path resolves only to the container's overlay root,
// i.e. it is not backed by an explicitly mounted volume. This turns the old
// silent "write to ephemeral fs" behaviour into a clear error.
func ensureRealMount(path string) error {
	p := filepath.Clean(path)
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil // cannot verify; do not block
	}
	defer f.Close()
	best, bestFs := "", ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		mp := filepath.Clean(unescapeMount(fields[1]))
		if mp == "/" || mp == p || strings.HasPrefix(p+"/", mp+"/") {
			if len(mp) >= len(best) {
				best, bestFs = mp, fields[2]
			}
		}
	}
	if best == "/" && (bestFs == "overlay" || bestFs == "overlayfs") {
		return fmt.Errorf("storage path %q is not a mounted volume — it maps to the agent's ephemeral container filesystem (overlay %q). Declare a volume for this path in the DaemonSet, or use spec.storage.type: mount/nfs", p, best)
	}
	return nil
}

// unescapeMount decodes the octal escapes /proc/mounts uses for spaces/tabs.
func unescapeMount(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

// sanitizeMount turns a server:export into a filesystem-safe subdir name.
func sanitizeMount(s string) string {
	r := strings.NewReplacer("/", "_", ":", "_", " ", "_")
	return strings.Trim(r.Replace(s), "_")
}

func (c *Checkpointer) simulatedArchive(t Target) string {
	return filepath.Join("/var/lib/kubelet/checkpoints",
		fmt.Sprintf("checkpoint-%s_%s-%s.tar", t.Pod, t.Namespace, t.Container))
}
