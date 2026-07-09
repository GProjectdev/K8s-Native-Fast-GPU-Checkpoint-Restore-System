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

// nfsMountBase is where the agent mounts NFS backends (type: nfs). Each distinct
// server:export gets its own subdir. The mount lives in the agent's mount
// namespace and is (re)created on demand.
const nfsMountBase = "/var/lib/gpu-cr/nfs"

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
	// DryRun skips the privileged kubelet checkpoint (dev clusters without GPUs);
	// the control flow, status updates and storage layout still run end-to-end.
	DryRun bool
}

// NewCheckpointer wires the pipeline with sane defaults.
func NewCheckpointer(im *InterceptorManager, kc *KubeletClient, dryRun bool) *Checkpointer {
	return &Checkpointer{Interceptor: im, Kubelet: kc, GCRInterception: true, DryRun: dryRun}
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

	// (3) Store to the backend declared in the CR.
	phStore := time.Now()
	stored, err := c.store(t, produced)
	if err != nil {
		if c.GCRInterception {
			_ = c.Interceptor.Signal(t.PodUID, GCRSignalRestore)
		}
		return nil, fmt.Errorf("store checkpoint: %w", err)
	}
	storeDur = time.Since(phStore)
	klog.V(2).Infof("step 3/4 done: stored checkpoint at %s (store %s)", stored, storeDur)

	// (4) Remap the data buffers back to the device so the source keeps running.
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
	total := time.Since(start)
	klog.Infof("step 4/4 done: checkpoint stored at %s; source resumed (remapped); took %s",
		stored, total)
	// Machine-parseable phase breakdown (seconds). freeze = Selective Interception
	// data checkpoint; kubelet = CRIUgpu (cuda_plugin GPU dump + CRIU CPU dump +
	// CRI-O tar); store = copy tar to backend; remap = interceptor restore.
	klog.Infof("PHASE_TIMES pod=%s/%s gcr=%t freeze_s=%.3f kubelet_s=%.3f store_s=%.3f remap_s=%.3f total_s=%.3f",
		t.Namespace, t.Pod, c.GCRInterception,
		freezeDur.Seconds(), kubeletDur.Seconds(), storeDur.Seconds(), remapDur.Seconds(), total.Seconds())
	return &Result{ArchivePath: stored, TakenAt: start}, nil
}

// store copies the produced archive into the CR-defined backend.
func (c *Checkpointer) store(t Target, produced []string) (string, error) {
	switch t.Storage.Type {
	case gpucrv1alpha1.StorageNFS:
		return c.storeNFS(t, produced)
	case gpucrv1alpha1.StorageHostPath, "":
		// hostPath must be a real mounted volume in the agent, not the container's
		// ephemeral overlay (otherwise the tar is silently lost on Pod restart).
		if !c.DryRun {
			if err := ensureRealMount(t.Storage.Path); err != nil {
				return "", err
			}
		}
		return c.storeToDir(t, produced, t.Storage.Path)
	case gpucrv1alpha1.StorageS3:
		return "", fmt.Errorf("storage type s3 not yet implemented (endpoint=%s, path=%s)",
			t.Storage.Endpoint, t.Storage.Path)
	default:
		return "", fmt.Errorf("unknown storage type %q", t.Storage.Type)
	}
}

// storeNFS mounts the CR's NFS backend (endpoint:path) in the agent's mount
// namespace on demand and writes the checkpoint tar there. This honours the CR's
// storage.endpoint/path directly — no DaemonSet volume needs to be pre-declared.
func (c *Checkpointer) storeNFS(t Target, produced []string) (string, error) {
	server := strings.TrimSpace(t.Storage.Endpoint)
	remote := strings.TrimSpace(t.Storage.Path)
	if server == "" || remote == "" {
		return "", fmt.Errorf("nfs storage requires spec.storage.endpoint (server) and spec.storage.path (export); got endpoint=%q path=%q", server, remote)
	}
	mnt := filepath.Join(nfsMountBase, sanitizeMount(server+"_"+remote))
	if !c.DryRun {
		if err := ensureNFSMounted(server, remote, mnt); err != nil {
			return "", fmt.Errorf("mount nfs %s:%s at %s: %w", server, remote, mnt, err)
		}
	}
	return c.storeToDir(t, produced, mnt)
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
	return dst, nil
}

// ensureNFSMounted mounts server:remote at mnt if it is not already a mountpoint.
// Requires nfs-common (mount.nfs) in the image and a privileged container.
func ensureNFSMounted(server, remote, mnt string) error {
	if isMountpoint(mnt) {
		return nil
	}
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return err
	}
	src := fmt.Sprintf("%s:%s", server, remote)
	if out, err := exec.Command("mount", "-t", "nfs", "-o", "nfsvers=4,nolock", src, mnt).CombinedOutput(); err != nil {
		// fall back to version auto-negotiation (NFSv3, etc.)
		if out2, err2 := exec.Command("mount", "-t", "nfs", src, mnt).CombinedOutput(); err2 != nil {
			return fmt.Errorf("mount.nfs failed: %v: %s; fallback: %v: %s",
				err, strings.TrimSpace(string(out)), err2, strings.TrimSpace(string(out2)))
		}
	}
	if !isMountpoint(mnt) {
		return fmt.Errorf("mount reported success but %s is not a mountpoint", mnt)
	}
	klog.Infof("mounted NFS %s at %s", src, mnt)
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
		return fmt.Errorf("storage path %q is not a mounted volume — it maps to the agent's ephemeral container filesystem (overlay %q). Declare a volume for this path in the DaemonSet, or use spec.storage.type: nfs with endpoint+path", p, best)
	}
	return nil
}

// unescapeMount decodes the octal escapes /proc/mounts uses for spaces/tabs.
func unescapeMount(s string) string {
	r := strings.NewReplacer("\040", " ", "\011", "	", "\012", "
", "\134", "\")
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
