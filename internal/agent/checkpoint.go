package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"k8s.io/klog/v2"

	gpucrv1alpha1 "github.com/GProjectdev/K8s-Native-Fast-GPU-Checkpoint-Restore-System/api/v1alpha1"
)

// Checkpointer performs the container checkpoint on the local node using the
// CRIUgpu path: the kubelet checkpoint API drives CRI-O + CRIU, and the NVIDIA
// CRIU cuda_plugin checkpoints the GPU as part of the container checkpoint.
// The agent only orchestrates: resolve target -> kubelet checkpoint -> store.
type Checkpointer struct {
	Kubelet *KubeletClient
	// DryRun skips the privileged kubelet checkpoint (dev clusters without GPUs);
	// the control flow, status updates and storage layout still run end-to-end.
	DryRun bool
}

// NewCheckpointer wires the checkpointer.
func NewCheckpointer(kc *KubeletClient, dryRun bool) *Checkpointer {
	return &Checkpointer{Kubelet: kc, DryRun: dryRun}
}

// Target describes a resolved checkpoint target on this node.
type Target struct {
	Namespace string
	Pod       string
	Container string
	Storage   gpucrv1alpha1.StorageSpec
}

// Result reports the produced artifact.
type Result struct {
	ArchivePath string
	TakenAt     time.Time
}

// Checkpoint takes the container checkpoint (CRIUgpu) and stores the archive.
func (c *Checkpointer) Checkpoint(ctx context.Context, t Target) (*Result, error) {
	start := time.Now()
	klog.Infof("checkpoint start: pod=%s/%s container=%s (CRIUgpu)", t.Namespace, t.Pod, t.Container)

	// (1) Container checkpoint via the kubelet API. CRI-O + CRIU + the NVIDIA
	// cuda_plugin checkpoint both the CPU process and the GPU state.
	var produced []string
	if c.DryRun {
		produced = []string{c.simulatedArchive(t)}
	} else {
		var err error
		produced, err = c.Kubelet.Checkpoint(ctx, t.Namespace, t.Pod, t.Container)
		if err != nil {
			return nil, fmt.Errorf("kubelet container checkpoint: %w", err)
		}
	}
	klog.V(2).Infof("step 1/2 done: kubelet produced %v", produced)

	// (2) Store to the backend declared in the CR.
	stored, err := c.store(t, produced)
	if err != nil {
		return nil, fmt.Errorf("store checkpoint: %w", err)
	}
	klog.Infof("step 2/2 done: checkpoint stored at %s; took %s", stored, time.Since(start))
	return &Result{ArchivePath: stored, TakenAt: start}, nil
}

// store copies the produced archive into the CR-defined backend and returns the
// canonical stored path.
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

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Chmod(mode)
}
