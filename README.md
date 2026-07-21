# K8s-Native GPU Checkpoint/Restore System

Kubernetes-native GPU **container** checkpointing: a `GPUCheckpoint` Custom
Resource plus a per-node **GPU C/R Node Agent** (DaemonSet, no separate
controller — each agent watches the CR directly and acts only on CRs targeting
its own node). An optional **WorkloadCheckpoint** orchestrator (a separate,
central controller under [`orchestrator/`](orchestrator/)) fans a workload
(Deployment / StatefulSet / Job / Pod) out to one per-Pod `GPUCheckpoint` per
replica — see [`orchestrator/README.md`](orchestrator/README.md).

> 🇰🇷 한국어 셋업: [`docs/SETUP.ko.md`](docs/SETUP.ko.md)

## Branches

- **`main` (this branch) — GCR interception (data) + CRIUgpu (control).** Keeps
  GCR's control/data separation: the in-Pod interceptor does the Selective
  Interception **data** checkpoint (owns `cudaMalloc` via the CUDA VMM API;
  freeze/remap), and the GPU **control state** is checkpointed by **CRIU + the
  NVIDIA `cuda_plugin`** via the kubelet Checkpoint API (CRIUgpu) — replacing the
  earlier host `cuda-checkpoint` helper.
- **`v1.0` — GCR interception (data) + host cuda-checkpoint helper (control).**
  Control state via a host `cuda-checkpoint` helper + plain CRIU (`cuda_plugin`
  disabled). Preserved on the `v1.0` branch.

## How it works (main)

GCR control/data separation, with the **control state handled by CRIUgpu**:

1. **Data (Selective Interception)** — the in-Pod interceptor (LD_PRELOAD; owns
   `cudaMalloc` via the CUDA VMM API) copies the GPU data buffers to host memory
   and frees the physical GPU memory while keeping the virtual addresses. The
   device is left with only GPU control state.
   The offloaded buffers go to an **external blob** (`data.blob`), which is
   `munmap`ped before the dump so CRIU does **not** serialize it into the tar.
2. **Control + CPU (CRIUgpu)** — the agent calls the kubelet Checkpoint API;
   CRI-O + CRIU + the NVIDIA `cuda_plugin` checkpoint the remaining GPU control
   state and the CPU process into a **tar** (the bulky GPU data is not in it).
3. **Remap** — the interceptor maps the data buffers back to the device
   (non-destructive resume), off the checkpoint's critical path.
4. **Store** — the agent writes both artifacts to storage: the CRIUgpu **`.tar`**
   plus the **`.blob`**. A complete checkpoint = **tar + blob** (both needed to
   restore). See [`docs/DATA-ENGINE.md`](docs/DATA-ENGINE.md).

Requires **NVIDIA driver 570+** and the **CRIU `cuda_plugin` installed/enabled**.

## GPUCheckpoint CR

```yaml
apiVersion: gpu-cr.io/v1alpha1
kind: GPUCheckpoint
metadata: { name: ckpt-sample-001, namespace: default }
spec:
  workloadRef:
    kind: Pod                 # Pod only (per-Pod primitive). For Deployment/
                              # StatefulSet/Job use WorkloadCheckpoint (orchestrator/).
    namespace: default
    name: cuda-sample-pod
    container: cuda-app       # optional; first container if empty
    nodeInfo: ""              # optional; resolved from the Pod if empty
  storage:
    type: hostPath            # hostPath | mount | nfs | pvc | s3
    path: /var/lib/gcr-checkpoint
  schedule: ""                # empty = one-shot; Go duration ("5m","1h") OR
                              # cron ("0 */2 * * *", "@hourly")
# status: phase, observedNode, lastCheckpointTime, checkpointCount,
#         lastCheckpointPath, conditions  (defined on the CRD, set by the agent)
```

`workloadRef` generalizes the earlier `podRef` (adds `kind`); `schedule` replaces
the earlier `period` and accepts a Go duration **or** a cron expression. The
Node Agent resolves only `kind: Pod`; multi-replica workloads are handled by the
WorkloadCheckpoint orchestrator, which creates one per-Pod `GPUCheckpoint` each.

## Quick start

Follow [`docs/SETUP.ko.md`](docs/SETUP.ko.md) (KO) / [`docs/SETUP.md`](docs/SETUP.md)
(EN) from a fresh GPU VM. Condensed:

```bash
# each GPU worker (root): driver 570 + toolkit + CRIU(+cuda_plugin) + CRI-O drop-in + gate
sudo bash quickstart/scripts/gpu-worker-setup.sh   # reboot, then re-run
# master: device plugin + label + CRD/RBAC/DaemonSet
bash quickstart/scripts/master-deploy.sh
# build + roll out the agent image
buildah bud -f Dockerfile -t docker.io/<you>/gpu-cr-node-agent:latest . \
 && buildah push docker.io/<you>/gpu-cr-node-agent:latest docker://docker.io/<you>/gpu-cr-node-agent:latest
kubectl -n gpu-cr-system rollout restart ds/gpu-cr-node-agent
# run + checkpoint
kubectl apply -f deploy/sample-pod.yaml
kubectl apply -f deploy/sample-gpucheckpoint.yaml
kubectl get gpucheckpoints.gpu-cr.io -w            # Completed
```

## Repository layout

```
api/v1alpha1/        GPUCheckpoint CRD types (WorkloadRef, StorageSpec, Status)
cmd/node-agent/      agent entrypoint
internal/agent/      reconciler, kubelet checkpoint client, checkpointer, schedule
config/crd/          CRD manifest
deploy/              DaemonSet (CRI-O), RBAC, sample Pod + GPUCheckpoint
orchestrator/        WorkloadCheckpoint CRD + fan-out controller (separate binary)
quickstart/scripts/  gpu-worker-setup.sh, master-deploy.sh
docs/                SETUP guides, DATA-ENGINE.md
```

## Roadmap

- **Restore from tar + blob into a new container** (CRIU restore via CRI-O). *(next)*
- **Multi-workload orchestration** (Deployment / StatefulSet / Job → per-replica):
  **done** via the WorkloadCheckpoint orchestrator (`orchestrator/`).
- **Cron `schedule`**: **done** (Go duration or cron); coordinated (barrier)
  snapshots for distributed jobs are scaffolded.
- Storage backends beyond hostPath/mount/nfs (pvc/s3 movers).

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Only `.tar`, no `.blob` | Data engine off. The Node Agent runs baseline when `GCR_INTERCEPTION=false`; leave it unset/`true`. Also confirm the Pod printed `READY ... gpu_alloc` **before** checkpointing and its logs show `[gcr] interceptor loaded ... [VMM hooks active]`. |
| `no data blob at /var/lib/gcr-data/<uid>/...` (agent log) | Interceptor didn't freeze — Pod not READY at checkpoint time, or `LD_PRELOAD`/`/opt/gpu-cr/libgcr-interceptor.so` not mounted on that node. |
| CRIU dump `-52` ("Connected TCP socket") | Live TCP sockets (e.g. HF keep-alive). `quickstart` writes `/etc/criu/default.conf` with `tcp-close`/`ext-unix-sk`/`file-locks`. |
| CRIU `chr 195` / cuda_plugin errors | NVIDIA driver **570+** and CRIU `cuda_plugin` installed/enabled. |
| checkpoint API `404` / `DeadlineExceeded` | kubelet needs `--feature-gates=ContainerCheckpoint=true` and `--runtime-request-timeout=30m`; agent `--kubelet-timeout=30m` (all set by `quickstart`). |
| Restore: `could not load libcriu.so.2` | crun/CRIU mismatch — use **crun 1.26** (not 1.20) with CRIU 4.2. |
| Offline model: tokenizer/`config.json` error | The local model dir needs **all** files (`config.json`, `tokenizer.json`/`vocab.json`+`merges.txt`, `tokenizer_config.json`, weights, sharded `*.index.json`), not just weights; install `sentencepiece tiktoken protobuf` in the Pod. |

## Acknowledgements

Developed at the Distributed Cloud and Network Research Laboratory (DCN Lab).
GPU C/R builds on NVIDIA `cuda-checkpoint` + CRIU (`cuda_plugin`) and the GCR
paper *GPU Checkpoint/Restore Made Fast and Lightweight* (FAST '26).
