package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/klog/v2"

	gpucrv1alpha1 "github.com/GProjectdev/K8s-Native-Fast-GPU-Checkpoint-Restore-System/api/v1alpha1"
)

// Checkpointer executes the end-to-end GCR checkpoint pipeline on the local node.
//
// Order of operations follows the GCR paper + DCN Progress Report:
//
//  1. Selective interception-based data-buffer checkpoint
//     (signal the in-Pod GCR hook -> copy GPU data buffers to host, then
//     release/unmap the physical GPU memory while keeping the virtual page table)
//  2. Driver-integrated control-state checkpoint via cuda-checkpoint
//     (suspend CUDA in the process; remaining device state is evicted to host)
//  3. Container checkpoint via the kubelet checkpoint API
//     (CRIU snapshots the CPU-side process incl. the host-resident GPU buffers)
//  4. Store the produced archive to the backend defined in the CR .spec.storage
//  5. Resume the workload (control state -> running) so periodic checkpointing
//     does not terminate the job.
type Checkpointer struct {
	Interceptor *InterceptorManager
	Kubelet     *KubeletClient

	// CudaCheckpointBin is the cuda-checkpoint binary path (NVIDIA driver >= 550).
	CudaCheckpointBin string
	// CrictlBin resolves the container's host PID and container id.
	CrictlBin string
	// DryRun skips the privileged host operations (for dev clusters without GPUs);
	// the control flow, status updates and storage layout still run end-to-end.
	DryRun bool
	// GCRInterception gates step 1 (selective data-buffer checkpoint via the
	// in-Pod GCR hook). Set false when the upstream GCR hook (libcuda.so) is not
	// wired up; the pipeline then relies on cuda-checkpoint + CRIU only.
	GCRInterception bool
	// Nsenter runs cuda-checkpoint inside the host's namespaces (via `nsenter -t
	// 1 -a`) so it executes exactly as on the host — correct driver libs, /dev,
	// /proc, ld cache — avoiding container lib/namespace mismatches.
	Nsenter bool
	// CudaCheckpointHostBin is the cuda-checkpoint path as seen on the HOST
	// (used when Nsenter is true).
	CudaCheckpointHostBin string
	// SkipCudaCheckpoint skips the agent's manual cuda-checkpoint step entirely
	// and lets CRI-O + CRIU (with the NVIDIA CUDA plugin) checkpoint the GPU as
	// part of the container checkpoint (the CRIUgpu approach). Use this when the
	// agent cannot run cuda-checkpoint cross-namespace.
	SkipCudaCheckpoint bool
	// CudaHelperDir, when set, makes the agent delegate cuda-checkpoint to a
	// HOST-side helper (gpu-cr-cuda-helper.service) by writing a request file
	// here instead of exec'ing cuda-checkpoint in-container (which stack-smashes
	// due to a glibc ABI mismatch). The helper also resolves the real GPU PID
	// in the container's subtree, so we pass the container init PID.
	CudaHelperDir string

	run commandRunner
}

// commandRunner is injectable so the pipeline can be unit-tested.
type commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func defaultRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// NewCheckpointer wires the pipeline with sane defaults.
func NewCheckpointer(im *InterceptorManager, kc *KubeletClient, dryRun bool) *Checkpointer {
	return &Checkpointer{
		Interceptor:       im,
		Kubelet:           kc,
		CudaCheckpointBin:     "cuda-checkpoint",
		CudaCheckpointHostBin: "/usr/bin/cuda-checkpoint",
		CrictlBin:             "crictl",
		DryRun:                dryRun,
		GCRInterception:       true,
		run:               defaultRunner,
	}
}

// Target describes a resolved checkpoint target on this node.
type Target struct {
	Namespace   string
	Pod         string
	PodUID      string
	Container   string
	ContainerID string
	HostPID     int
	Incremental bool
	Storage     gpucrv1alpha1.StorageSpec
}

// Result reports the produced artifact.
type Result struct {
	ArchivePath string
	TakenAt     time.Time
}

// Checkpoint runs the full pipeline and returns the stored archive path.
func (c *Checkpointer) Checkpoint(ctx context.Context, t Target) (*Result, error) {
	start := time.Now()
	klog.Infof("checkpoint start: pod=%s/%s container=%s pid=%d incremental=%t",
		t.Namespace, t.Pod, t.Container, t.HostPID, t.Incremental)

	// (1) Selective data-buffer checkpoint via the GCR hook (optional).
	if c.GCRInterception {
		if err := c.Interceptor.Signal(t.PodUID, GCRSignalCkpt); err != nil {
			return nil, fmt.Errorf("signal GCR data-buffer checkpoint: %w", err)
		}
		if !c.DryRun {
			if err := c.Interceptor.WaitForAck(t.PodUID, 90*time.Second); err != nil {
				return nil, fmt.Errorf("data-buffer checkpoint did not ack: %w", err)
			}
		}
		klog.V(2).Info("step 1/5 done: GPU data buffers checkpointed, physical memory released")
	} else {
		klog.V(2).Info("step 1/5 skipped: GCR interception disabled; using cuda-checkpoint + CRIU only")
	}

	// (2) Control-state checkpoint: suspend CUDA via cuda-checkpoint.
	if err := c.cudaToggle(ctx, t.HostPID); err != nil {
		return nil, fmt.Errorf("cuda-checkpoint suspend: %w", err)
	}
	if c.SkipCudaCheckpoint {
		klog.V(2).Info("step 2/5 skipped: cuda-checkpoint delegated to CRI-O/CRIU (CRIUgpu)")
	} else {
		klog.V(2).Info("step 2/5 done: control state suspended (cuda-checkpoint)")
	}

	// (3) Container checkpoint through the kubelet API (CRIU).
	var produced []string
	if c.DryRun {
		produced = []string{c.simulatedArchive(t)}
	} else {
		var err error
		produced, err = c.Kubelet.Checkpoint(ctx, t.Namespace, t.Pod, t.Container)
		if err != nil {
			// Best-effort resume before bailing out so we don't leave the job suspended.
			_ = c.cudaToggle(ctx, t.HostPID)
			return nil, fmt.Errorf("kubelet container checkpoint: %w", err)
		}
	}
	klog.V(2).Infof("step 3/5 done: kubelet produced %v", produced)

	// (4) Store to the backend declared in the CR.
	stored, err := c.store(t, produced)
	if err != nil {
		_ = c.cudaToggle(ctx, t.HostPID)
		return nil, fmt.Errorf("store checkpoint: %w", err)
	}
	klog.V(2).Infof("step 4/5 done: stored checkpoint at %s", stored)

	// (5) Checkpoint-only. The source is intentionally LEFT in the checkpointed
	// (frozen) state: CUDA is suspended (cuda-checkpoint), the GPU data buffers are
	// resident on the host, and their physical GPU memory has been released while
	// the virtual addresses are preserved. RESTORE — cuda-checkpoint resume + GCR
	// remap (recreate physical, map to the same VA, copy host->device) — is a
	// SEPARATE, user-triggered operation (and tar->container restore needs CRI-O
	// support). The interceptor exposes GCRSignalRestore for that manual trigger.
	klog.Infof("step 5/5 done: checkpoint stored at %s; source left frozen (restore is triggered separately); took %s",
		stored, time.Since(start))

	return &Result{ArchivePath: stored, TakenAt: start}, nil
}

// cudaToggle flips the CUDA state (running <-> suspended) for the process.
func (c *Checkpointer) cudaToggle(ctx context.Context, pid int) error {
	if c.DryRun || pid <= 0 || c.SkipCudaCheckpoint {
		return nil
	}
	// Preferred path: a HOST helper runs cuda-checkpoint natively (in-container
	// execution stack-smashes). It targets the GPU PID in the container subtree.
	if c.CudaHelperDir != "" {
		return c.cudaHelperToggle(ctx, pid)
	}
	name := c.CudaCheckpointBin
	args := []string{"--toggle", "--pid", fmt.Sprintf("%d", pid)}
	if c.Nsenter {
		// Run cuda-checkpoint in the host namespaces with a CLEAN environment
		// (like sudo): inheriting the agent's LD_LIBRARY_PATH etc. crashes it.
		//   nsenter -t 1 -a -- /usr/bin/env -i PATH=... /usr/bin/cuda-checkpoint ...
		name = "nsenter"
		pre := []string{"-t", "1", "-a", "--",
			"/usr/bin/env", "-i",
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			c.CudaCheckpointHostBin}
		args = append(pre, args...)
	}
	out, err := c.run(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("%s: %v (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// cudaHelperToggle delegates the cuda-checkpoint toggle to the host-side helper.
// containerPID is the container's init PID; the helper resolves the actual
// GPU-using PID(s) in its subtree and toggles them on the host.
func (c *Checkpointer) cudaHelperToggle(ctx context.Context, containerPID int) error {
	if err := os.MkdirAll(c.CudaHelperDir, 0o755); err != nil {
		return fmt.Errorf("cuda helper dir: %w", err)
	}
	id := fmt.Sprintf("%d-%d", containerPID, time.Now().UnixNano())
	reqPath := filepath.Join(c.CudaHelperDir, id+".req")
	resPath := filepath.Join(c.CudaHelperDir, id+".res")
	if err := os.WriteFile(reqPath, []byte(fmt.Sprintf("toggle %d\n", containerPID)), 0o644); err != nil {
		return fmt.Errorf("write cuda helper request: %w", err)
	}
	defer os.Remove(resPath)
	deadline := time.Now().Add(120 * time.Second)
	for {
		if b, err := os.ReadFile(resPath); err == nil {
			parts := strings.SplitN(string(b), "\n", 2)
			rc := strings.TrimSpace(parts[0])
			detail := ""
			if len(parts) > 1 {
				detail = strings.TrimSpace(parts[1])
			}
			if rc != "0" {
				return fmt.Errorf("cuda helper rc=%s: %s", rc, detail)
			}
			klog.V(2).Infof("cuda helper ok (container pid %d): %s", containerPID, detail)
			return nil
		}
		if time.Now().After(deadline) {
			_ = os.Remove(reqPath)
			return fmt.Errorf("cuda helper timeout (pid %d); is gpu-cr-cuda-helper.service running on the node?", containerPID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// store moves/copies the produced archive(s) into the CR-defined backend path
// and returns the canonical stored path.
func (c *Checkpointer) store(t Target, produced []string) (string, error) {
	switch t.Storage.Type {
	case gpucrv1alpha1.StorageHostPath, gpucrv1alpha1.StorageNFS, "":
		// hostPath and NFS both present as a mounted directory to the agent.
		if err := os.MkdirAll(t.Storage.Path, 0o755); err != nil {
			return "", err
		}
		name := fmt.Sprintf("checkpoint-%s_%s-%s-%d.tar",
			t.Pod, t.Namespace, t.Container, time.Now().Unix())
		dst := filepath.Join(t.Storage.Path, name)
		if c.DryRun {
			if err := os.WriteFile(dst, []byte("simulated-gcr-checkpoint"), 0o644); err != nil {
				return "", err
			}
			return dst, nil
		}
		if err := copyFile(produced[0], dst, 0o644); err != nil {
			return "", err
		}
		return dst, nil
	case gpucrv1alpha1.StorageS3:
		// S3 upload is intentionally left as an integration point; the archive
		// path + endpoint are surfaced so an uploader sidecar/job can pick it up.
		return "", fmt.Errorf("storage type s3 not yet implemented (endpoint=%s, path=%s)",
			t.Storage.Endpoint, t.Storage.Path)
	default:
		return "", fmt.Errorf("unknown storage type %q", t.Storage.Type)
	}
}

func (c *Checkpointer) simulatedArchive(t Target) string {
	return filepath.Join("/var/lib/kubelet/checkpoints",
		fmt.Sprintf("checkpoint-%s_%s-%s.tar", t.Pod, t.Namespace, t.Container))
}

// ResolvePID resolves the host PID and container id for a Pod's container via
// crictl. Returns (containerID, pid, error).
func (c *Checkpointer) ResolvePID(ctx context.Context, namespace, pod, container string) (string, int, error) {
	if c.DryRun {
		return "dryrun-container", 0, nil
	}
	// Find the container id by pod+container label.
	psOut, err := c.run(ctx, c.CrictlBin, "ps", "--label",
		fmt.Sprintf("io.kubernetes.pod.name=%s", pod), "--label",
		fmt.Sprintf("io.kubernetes.pod.namespace=%s", namespace), "--label",
		fmt.Sprintf("io.kubernetes.container.name=%s", container),
		"-q", "--no-trunc")
	if err != nil {
		return "", 0, fmt.Errorf("crictl ps: %v (%s)", err, strings.TrimSpace(string(psOut)))
	}
	cid := strings.TrimSpace(strings.SplitN(string(psOut), "\n", 2)[0])
	if cid == "" {
		return "", 0, fmt.Errorf("no running container %s in pod %s/%s", container, namespace, pod)
	}
	// Inspect to read the host PID.
	inspectOut, err := c.run(ctx, c.CrictlBin, "inspect", cid)
	if err != nil {
		return "", 0, fmt.Errorf("crictl inspect: %v", err)
	}
	var inspect struct {
		Info struct {
			Pid int `json:"pid"`
		} `json:"info"`
	}
	if err := json.Unmarshal(inspectOut, &inspect); err != nil {
		return "", 0, fmt.Errorf("parse crictl inspect: %w", err)
	}
	return cid, inspect.Info.Pid, nil
}
