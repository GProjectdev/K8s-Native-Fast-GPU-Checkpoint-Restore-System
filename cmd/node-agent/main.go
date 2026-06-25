// Command node-agent is the GPU C/R Node Agent. It is deployed as a DaemonSet:
// one replica per node. On startup it installs the selective CUDA-interception
// library onto the node, then watches GPUCheckpoint CRs and performs the actual
// checkpoint for any CR whose target Pod runs on this node.
package main

import (
	"flag"
	"os"

	"k8s.io/klog/v2"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"k8s.io/apimachinery/pkg/runtime"

	gpucrv1alpha1 "github.com/GProjectdev/K8s-Native-Fast-GPU-Checkpoint-Restore-System/api/v1alpha1"
	"github.com/GProjectdev/K8s-Native-Fast-GPU-Checkpoint-Restore-System/internal/agent"
)

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = gpucrv1alpha1.AddToScheme(scheme)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	var (
		metricsAddr   string
		probeAddr     string
		kubeletURL    string
		kubeletCA     string
		kubeletInsec  bool
		distDir       string
		hostLibDir    string
		hostRunDir    string
		dryRun        bool
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe endpoint")
	flag.StringVar(&kubeletURL, "kubelet-url", envOr("KUBELET_URL", "https://127.0.0.1:10250"), "kubelet secure endpoint")
	flag.StringVar(&kubeletCA, "kubelet-ca", envOr("KUBELET_CA", ""), "kubelet CA bundle path (empty + insecure to skip verify)")
	flag.BoolVar(&kubeletInsec, "kubelet-insecure", envOr("KUBELET_INSECURE", "true") == "true", "skip kubelet TLS verification")
	flag.StringVar(&distDir, "interceptor-dist-dir", envOr("INTERCEPTOR_DIST_DIR", "/opt/gpu-cr-dist"), "dir holding prebuilt interceptor artifacts in the agent image")
	flag.StringVar(&hostLibDir, "host-lib-dir", envOr("HOST_LIB_DIR", "/host/gpu-cr/lib"), "host library dir mounted into the agent (-> /var/lib/gpu-cr/lib on the node)")
	flag.StringVar(&hostRunDir, "host-run-dir", envOr("HOST_RUN_DIR", "/host/gpu-cr/run"), "host run dir for GCR control channels")
	flag.BoolVar(&dryRun, "dry-run", envOr("DRY_RUN", "false") == "true", "skip privileged host ops (dev clusters without GPUs)")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		klog.Fatal("NODE_NAME env var is required (set via Downward API spec.nodeName)")
	}

	// (1) Install the selective interception library onto the node.
	im := agent.NewInterceptorManager(distDir, hostLibDir, hostRunDir)
	if err := im.Install(); err != nil {
		klog.Fatalf("install interceptor library: %v", err)
	}

	// (2) Kubelet checkpoint API client (bearer token from the SA).
	token, _ := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	kc, err := agent.NewKubeletClient(kubeletURL, string(token), kubeletCA, kubeletInsec)
	if err != nil {
		klog.Fatalf("kubelet client: %v", err)
	}

	// (3) Manager.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		klog.Fatalf("create manager: %v", err)
	}

	cp := agent.NewCheckpointer(im, kc, dryRun)
	// Allow pointing at host-mounted binaries / a specific CRI runtime.
	// crictl itself also reads CONTAINER_RUNTIME_ENDPOINT from the env.
	if v := os.Getenv("CUDA_CHECKPOINT_BIN"); v != "" {
		cp.CudaCheckpointBin = v
	}
	if v := os.Getenv("CRICTL_BIN"); v != "" {
		cp.CrictlBin = v
	}
	if os.Getenv("GCR_INTERCEPTION") == "false" {
		cp.GCRInterception = false
	}
	r := &agent.Reconciler{
		Client:       mgr.GetClient(),
		NodeName:     nodeName,
		Checkpointer: cp,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		klog.Fatalf("setup reconciler: %v", err)
	}

	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)

	klog.Infof("GPU C/R Node Agent starting on node %s (dryRun=%t)", nodeName, dryRun)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		klog.Fatalf("manager exited: %v", err)
	}
}
