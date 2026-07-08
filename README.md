# K8s-Native GPU Checkpoint/Restore System

Kubernetes-native GPU **container** checkpointing: a `GPUCheckpoint` Custom
Resource plus a per-node **GPU C/R Node Agent** (DaemonSet, no separate
controller — each agent watches the CR directly and acts only on CRs targeting
its own node).

> 🇰🇷 한국어 셋업: [`docs/SETUP.ko.md`](docs/SETUP.ko.md)

## Branches

- **`main` (this branch) — CRIUgpu.** The GPU+CPU checkpoint is done by the
  **kubelet Checkpoint API → CRI-O → CRIU + the NVIDIA `cuda_plugin`**. The Node
  Agent only orchestrates: resolve the target → call the kubelet checkpoint API →
  store the produced tar. No in-Pod interceptor and no host cuda-checkpoint helper.
- **`v1.0` — GCR data engine.** The GCR-paper approach: an in-Pod VMM interceptor
  (owns `cudaMalloc`, freeze/remap) + a host `cuda-checkpoint` helper + CRIU
  (CPU-only, `cuda_plugin` disabled). Preserved on the `v1.0` branch.

## How it works (main / CRIUgpu)

1. User applies a `GPUCheckpoint` CR (`workloadRef`, `storage`, `schedule`).
2. The Node Agent on the target node calls the **kubelet Checkpoint API**. CRI-O
   drives **CRIU + the NVIDIA cuda_plugin**, which checkpoint the container's CPU
   process *and* GPU state into a tar.
3. The agent copies that tar to `.spec.storage.path`.

Requires **NVIDIA driver 570+** (so `cuda-checkpoint` releases the `/dev/nvidia*`
fds and CRIU can dump) and the **CRIU cuda_plugin installed/enabled**.

## GPUCheckpoint CR

```yaml
apiVersion: gpu-cr.io/v1alpha1
kind: GPUCheckpoint
metadata: { name: ckpt-sample-001, namespace: default }
spec:
  workloadRef:
    kind: Pod                 # Pod (default) | Deployment | StatefulSet (reserved)
    namespace: default
    name: cuda-sample-pod
    container: cuda-app       # optional; first container if empty
    nodeInfo: ""              # optional; resolved from the Pod if empty
  storage:
    type: hostPath            # hostPath | nfs | s3
    path: /var/lib/gcr-checkpoint
  schedule: ""                # Go duration ("5m","1h"); empty = one-shot
# status: phase, observedNode, lastCheckpointTime, checkpointCount,
#         lastCheckpointPath, conditions  (defined on the CRD, set by the agent)
```

`workloadRef` generalizes the earlier `podRef` (adds `kind`); `schedule` replaces
the earlier `period` (Go duration instead of HHMMSS).

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
quickstart/scripts/  gpu-worker-setup.sh, master-deploy.sh
docs/                SETUP guides
```

## Roadmap

- **Restore from tar into a new container** (CRIU restore via CRI-O). *(next)*
- **Multi-workload `workloadRef`** (Deployment / StatefulSet → per-replica). *(next)*
- **Periodic `schedule`** hardening and storage backends (nfs / s3).

## Acknowledgements

Developed at the Distributed Cloud and Network Research Laboratory (DCN Lab).
GPU C/R builds on NVIDIA `cuda-checkpoint` + CRIU (`cuda_plugin`) and the GCR
paper *GPU Checkpoint/Restore Made Fast and Lightweight* (FAST '26).
