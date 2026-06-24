# K8s-Native Fast GPU Checkpoint/Restore System

A Kubernetes-native implementation of the **GCR** (*GPU Checkpoint/Restore Made
Fast and Lightweight*, FAST '26) approach, packaged as a Custom Resource plus a
per-node agent. This repository brings GCR's **control/data-separated hybrid
C/R** into a Kubernetes cluster so that GPU Pods can be checkpointed
transparently, on a schedule, without modifying the workload.

> Status: **Phase 1** — `GPUCheckpoint` CR + `GPU C/R Node Agent` (checkpoint
> path). There is **no separate GPU C/R Controller**: the `GPUCheckpoint` CR
> carries everything required (`podRef.nodeInfo`, `storage`, `period`), and each
> Node Agent **watches the CR directly** and acts on the ones targeting its own
> node. The restore path (custom container runtime) is planned next
> (see [Roadmap](#roadmap)).

> 🇰🇷 한국어 문서: [`README.ko.md`](README.ko.md)

This work is based on:

- Paper: *GPU Checkpoint/Restore Made Fast and Lightweight* (Zeng et al., Tsinghua University, FAST '26)
- Upstream artifact: <https://github.com/thustorage/GCR>
- DCN Lab Progress Report (2026-06-16), "Design Checkpoint/Restore System in Kubernetes"

---

## Why

System-level GPU C/R enables elastic serverless scaling, fast task switching, and
fault-tolerant training. GCR achieves low C/R latency **and** near-zero
normal-execution overhead by:

- **Control/data separation** — only the GPU *memory* APIs
  (`cuMemCreate/Map/Unmap/Release`) are intercepted via `LD_PRELOAD` (< 1%
  overhead), while control state uses the efficient driver-integrated path
  (`cuda-checkpoint`).
- **Virtual/physical memory decoupling** — the GPU page table (virtual
  addresses) is preserved while physical memory is released and re-mapped on
  restore, removing address-consistency overhead.
- **Shadow execution + dirty templates** — incremental checkpointing that stores
  only modified buffers.

This project wires those mechanisms into Kubernetes primitives.

---

## Architecture

```
                       Kubernetes Cluster
  Control Plane
  ┌───────────────────────────────────────────────────────────┐
  │   GPUCheckpoint CR  (podRef.nodeInfo, storage, period)        │
  └───────────────────────────────────────────────────────────┘
                          ▲
                          │ (1) Watch  — no separate controller
  Worker Node             │
  ┌───────────────────────────────────────────────────────────┐
  │  GPU Pod                              GPU C/R Node Agent      │
  │   ├─ GPU APP                          (DaemonSet, this repo)  │
  │   └─ GPU Selective Interceptor  ◄──(2) signal / checkpoint    │
  │        (libgcr-interceptor.so)                                │
  └───────────────────────────────────────────────────────────┘
                                   │ (3) push Checkpoint.tar
                                   ▼
                          Shared Storage (hostPath / NFS / S3)
```

There is **no GPU C/R Controller**. The Node Agent runs **one replica per node**
(DaemonSet), watches `GPUCheckpoint` CRs directly, and acts only on the ones
whose `podRef.nodeInfo` matches its own node — so all heavy operations are local
and the control plane stays a single declarative CR.

### Checkpoint pipeline (per `GPUCheckpoint`)

1. **Selective data-buffer checkpoint** — the agent signals the in-Pod GCR hook
   (`internal/agent/interceptor.go`, GCR signal `1=ckpt`). The hook copies GPU
   data buffers to host memory and releases/unmaps physical GPU memory while
   keeping the virtual page table.
2. **Control-state checkpoint** — `cuda-checkpoint --toggle --pid <pid>` suspends
   CUDA in the process (driver-integrated path).
3. **Container checkpoint** — the agent calls the **kubelet checkpoint API**
   (`POST /checkpoint/{ns}/{pod}/{container}`), which drives CRIU to snapshot the
   CPU-side process, including the host-resident GPU buffers.
4. **Store** — the produced archive is written to the backend declared in
   `.spec.storage`.
5. **Resume** — the workload is resumed so periodic checkpointing keeps the job
   alive.

---

## The `GPUCheckpoint` Custom Resource

```yaml
apiVersion: gpu-cr.io/v1alpha1
kind: GPUCheckpoint
metadata:
  name: ckpt-vllm-001
  namespace: default
spec:
  podRef:                       # which Pod (and where) to checkpoint
    nodeInfo: gpu-node-1        # node the Pod runs on; the agent on this node acts on the CR
    namespace: default
    name: vllm-gcr-pod
    container: vllm             # optional; defaults to the first container
  storage:                      # which filesystem / path to store the artifact
    type: hostPath              # hostPath | nfs | s3
    path: /var/lib/gcr-checkpoint
  period: "000500"             # HHMMSS interval; "000000"/omit = one-shot
  incremental: true            # dirty-only after the first checkpoint
```

| Field | Meaning |
|-------|---------|
| `podRef` | Target Pod: `nodeInfo` (node the Pod runs on), `namespace`, `name`, `container`. The agent only acts when `nodeInfo` matches the node it runs on; if `nodeInfo` is empty it falls back to resolving the node from the Pod. |
| `storage` | Backend type and path where `Checkpoint.tar` is written. |
| `period` | Fixed-width `HHMMSS` checkpoint interval. `"000030"`=30 s, `"000500"`=5 min, `"010000"`=hourly. `"000000"` or empty = one-shot. |
| `incremental` | Enable GCR shadow-execution incremental checkpointing after the first checkpoint. |

CRD: [`config/crd/gpu-cr.io_gpucheckpoints.yaml`](config/crd/gpu-cr.io_gpucheckpoints.yaml)

---

## The GPU C/R Node Agent

A DaemonSet (`cmd/node-agent`) that:

- **Installs the interceptor library on startup** — creates the host library
  directory (`/var/lib/gpu-cr/lib`) and copies in `libgcr-interceptor.so` (and,
  when provided, the GCR hook `libcuda.so`) so any GPU Pod on the node can
  `LD_PRELOAD` it.
- **Watches `GPUCheckpoint` CRs**, filters to its own node, and honours
  `.spec.period` scheduling via the controller-runtime requeue mechanism.
- **Executes the 5-step checkpoint pipeline** above and updates
  `.status` (`phase`, `checkpointCount`, `lastCheckpointTime`,
  `lastCheckpointPath`, conditions).

Key source files:

| File | Responsibility |
|------|----------------|
| `cmd/node-agent/main.go` | Manager bootstrap, library install, flags/env |
| `internal/agent/reconciler.go` | Node-filtered reconcile + period scheduling |
| `internal/agent/checkpoint.go` | 5-step checkpoint pipeline + crictl PID resolution |
| `internal/agent/interceptor.go` | Library install + GCR signal channel |
| `internal/agent/kubelet.go` | Kubelet checkpoint API client |
| `internal/agent/period.go` | `HHMMSS` period parsing |

---

## Selective CUDA interception (`LD_PRELOAD`)

`interceptor/preload.c` is the shim a GPU Pod loads. It hooks `dlopen` so that
when the CUDA runtime loads `libcuda.so.1`, GCR's hook driver
(`$GCR_HOME/libcuda.so`) is loaded instead — which selectively intercepts only
the GPU memory-management APIs (calls from `libcublas` are passed straight to the
real driver). This mirrors `thustorage/GCR` `GCR/preload.c`.

Pod wiring (see [`deploy/sample-pod.yaml`](deploy/sample-pod.yaml)):

```yaml
env:
  - name: LD_PRELOAD
    value: /opt/gpu-cr/libgcr-interceptor.so
  - name: GCR_HOME
    value: /opt/gpu-cr
volumeMounts:
  - name: gpu-cr-lib
    mountPath: /opt/gpu-cr
    readOnly: true
volumes:
  - name: gpu-cr-lib
    hostPath:
      path: /var/lib/gpu-cr/lib   # installed by the Node Agent
      type: Directory
```

> The **GCR hook driver** (`libcuda.so`) that performs the actual `cuMem*`
> interception is built from upstream `thustorage/GCR` (`GCR/build.sh`) and
> dropped next to the shim. The shim and the agent orchestration are provided
> here; building the upstream hook is a node prerequisite.

---

## Repository layout

```
.
├── api/v1alpha1/                  # GPUCheckpoint types + deepcopy + scheme
├── cmd/node-agent/                # agent entrypoint
├── internal/agent/                # reconciler, pipeline, kubelet, interceptor, period
├── interceptor/                   # LD_PRELOAD shim (preload.c) + Makefile
├── config/crd/                    # CustomResourceDefinition
├── deploy/                        # rbac, daemonset, sample Pod, sample CR
├── Dockerfile                     # builds agent + shim image (Buildah/Containerfile compatible)
└── README.md / README.ko.md
```

---

## Prerequisites & Server Setup

> 📖 **Step-by-step install guide (Master + Worker, copy-paste commands):**
> [`docs/SETUP.md`](docs/SETUP.md) · 한국어 [`docs/SETUP.ko.md`](docs/SETUP.ko.md)

To actually run and test this system you need to prepare the GPU nodes, the
container runtime, and the Kubernetes cluster up front. The list below is the
full set; for a pure control-flow smoke test you can skip the GPU/CRIU pieces and
run the agent with `--dry-run=true`.

### 1. Hardware

| Item | Requirement |
|------|-------------|
| GPU | NVIDIA GPU supported by driver ≥ 550 (paper/Progress Report used A100-40GB). |
| Host RAM | The checkpoint backend is **CPU memory**, so size RAM ≥ the GPU memory you intend to snapshot (e.g. ≥ 40 GB for an A100-40GB workload), plus headroom. |
| Disk | Free space on the storage path for `Checkpoint.tar` (tens of GB per checkpoint for LLMs). |

### 2. GPU node OS packages

```bash
# NVIDIA driver >= 550 — ships the cuda-checkpoint binary used for control-state C/R
nvidia-smi                       # driver >= 550.x
which cuda-checkpoint            # must exist (usually /usr/bin/cuda-checkpoint)
cuda-checkpoint --help

# CRIU >= 3.17 — kubelet/CRI container checkpoint uses it for CPU-side state
sudo apt-get install -y criu     # or build from source
criu --version
sudo criu check                  # should report "Looks good."

# NVIDIA Container Toolkit — exposes GPUs to containers
#   https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/
nvidia-ctk --version
```

> The **GCR hook driver** (`libcuda.so`) that performs the actual selective
> `cuMem*` interception is **not** part of this repo. Build it from upstream
> [`thustorage/GCR