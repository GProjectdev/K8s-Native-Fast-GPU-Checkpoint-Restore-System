package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/klog/v2"

	gpucrv1alpha1 "github.com/GProjectdev/K8s-Native-Fast-GPU-Checkpoint-Restore-System/api/v1alpha1"
)

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
	case gpucrv1alpha1.StorageHostPath, gpucrv1alpha1.StorageNFS, "":
		if err := os.MkdirAll(t.Storage.Path, 0o755); err != nil {
			return "", err
		}
		name := fmt.Sprintf("checkpoint-%s_%s-%s-%d.tar",
			t.Pod, t.Namespace, t.Container, time.Now().Unix())
		dst := filepath.Join(t.Storage.Path, name)
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
	case gpucrv1alpha1.StorageS3:
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
