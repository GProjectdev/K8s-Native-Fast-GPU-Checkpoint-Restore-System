package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"k8s.io/klog/v2"
)

// GCR control signals. These mirror the `signal_controls.signal` field used by
// the GCR hook library (see thustorage/GCR GCR/common.h):
//
//	0: idle, 1: ckpt, 2: restore, 10: init
const (
	GCRSignalIdle    = 0
	GCRSignalCkpt    = 1
	GCRSignalRestore = 2
	GCRSignalInit    = 10
)

// InterceptorManager installs the selective CUDA-interception library onto the
// node and signals the in-Pod GCR hook to perform a checkpoint.
//
// Layout on the host (exposed to GPU Pods via hostPath):
//
//	<HostLibDir>/                 (e.g. /var/lib/gpu-cr/lib  ->  Pod /opt/gpu-cr)
//	  libgcr-interceptor.so       LD_PRELOAD dlopen shim (preload.c)
//	  libcuda.so                  GCR hook driver (selective mem-API interception)
//	<HostRunDir>/<key>/control    one-byte control channel polled by the hook
type InterceptorManager struct {
	// DistDir is where the agent image ships the prebuilt interceptor artifacts.
	DistDir string
	// HostLibDir is the host directory (hostPath-mounted into the agent) that
	// the artifacts are copied into, and that GPU Pods mount read-only.
	HostLibDir string
	// HostRunDir holds per-workload control channels.
	HostRunDir string
}

// NewInterceptorManager builds a manager from the standard mount points.
func NewInterceptorManager(distDir, hostLibDir, hostRunDir string) *InterceptorManager {
	return &InterceptorManager{DistDir: distDir, HostLibDir: hostLibDir, HostRunDir: hostRunDir}
}

// Install creates the host library directory and copies the interceptor
// artifacts into it. It is invoked once when the Node Agent starts so that any
// GPU Pod scheduled to this node can LD_PRELOAD the library.
func (m *InterceptorManager) Install() error {
	if err := os.MkdirAll(m.HostLibDir, 0o755); err != nil {
		return fmt.Errorf("create host lib dir %s: %w", m.HostLibDir, err)
	}
	if err := os.MkdirAll(m.HostRunDir, 0o755); err != nil {
		return fmt.Errorf("create host run dir %s: %w", m.HostRunDir, err)
	}

	entries, err := os.ReadDir(m.DistDir)
	if err != nil {
		// The dist dir is optional in dev/dry-run mode; warn and continue.
		klog.Warningf("interceptor dist dir %s not readable (%v); skipping library install", m.DistDir, err)
		return nil
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		src := filepath.Join(m.DistDir, e.Name())
		dst := filepath.Join(m.HostLibDir, e.Name())
		if err := copyFile(src, dst, 0o755); err != nil {
			return fmt.Errorf("install %s: %w", e.Name(), err)
		}
		klog.Infof("installed interceptor artifact %s -> %s", src, dst)
	}
	return nil
}

// Signal writes a control signal for the GCR hook running inside the workload.
// key uniquely identifies the workload (we use the Pod UID). The in-Pod hook
// polls <HostRunDir>/<key>/control and acts on the value, exactly like GCR's
// shared-memory signal_controls channel but bridged through a host file so the
// agent (a different container/process) can trigger it.
func (m *InterceptorManager) Signal(key string, signal int) error {
	dir := filepath.Join(m.HostRunDir, key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create control dir %s: %w", dir, err)
	}
	ctrl := filepath.Join(dir, "control")
	if err := os.WriteFile(ctrl, []byte(fmt.Sprintf("%d", signal)), 0o644); err != nil {
		return fmt.Errorf("write control %s: %w", ctrl, err)
	}
	klog.V(2).Infof("GCR signal=%d written to %s", signal, ctrl)
	return nil
}

// WaitForAck blocks until the hook resets the control channel back to idle (0),
// signalling that the requested phase finished, or until timeout elapses.
func (m *InterceptorManager) WaitForAck(key string, timeout time.Duration) error {
	ctrl := filepath.Join(m.HostRunDir, key, "control")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(ctrl)
		if err == nil && (len(b) == 0 || string(b) == fmt.Sprintf("%d", GCRSignalIdle)) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for GCR ack on %s", ctrl)
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
