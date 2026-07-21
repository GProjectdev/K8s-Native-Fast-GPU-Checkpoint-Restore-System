# Setup & Usage — GCR interception + CRIUgpu (from VM creation to a working system)

This branch (main) keeps GCR's **control/data separation**: an in-Pod
**interceptor** (LD_PRELOAD) does the Selective Interception **data** checkpoint
(offloads GPU buffers to an external `data.blob`, frees physical memory), and the
GPU **control state + CPU** are checkpointed by **CRIUgpu** (kubelet Checkpoint
API → CRI-O → CRIU + the NVIDIA cuda_plugin). The `v1.0` branch uses a host
`cuda-checkpoint` helper instead of CRIUgpu for the control state.

## 0. Overview
Creating a `GPUCheckpoint` CR makes the Node Agent: (1) **freeze** — the
interceptor offloads GPU data to `data.blob` and releases physical memory;
(2) **CRIUgpu** — call the kubelet Checkpoint API; CRI-O/CRIU + cuda_plugin
checkpoint the remaining GPU control state + CPU process into a **tar**;
(3) **remap** the data back (resume); (4) **store** both `.tar` **and** `.blob`.
A complete checkpoint = **tar + blob** — see `docs/DATA-ENGINE.md`.

> Requires **NVIDIA driver 570+**, the **CRIU cuda_plugin installed/enabled**, and
> a single 300GB boot disk.

## 1–5. Bring-up
Same scripts as before: base K8s via `Kubernetes_Installer_with_CRIO`, then on
each GPU worker `sudo bash quickstart/scripts/gpu-worker-setup.sh` (installs
driver 570, cuda-checkpoint, toolkit, **CRIU + cuda_plugin enabled**, crun fix,
CRI-O drop-in, kubelet ContainerCheckpoint gate), then on master
`bash quickstart/scripts/master-deploy.sh`. Build the agent image with
`buildah bud -f Dockerfile -t <img> .` and roll out the DaemonSet.

## 6. Run + checkpoint
```bash
kubectl apply -f deploy/sample-pod.yaml
kubectl apply -f deploy/sample-gpucheckpoint.yaml
kubectl get gpucheckpoints.gpu-cr.io -w        # Checkpointing -> Completed
ls -lh /var/lib/gcr-checkpoint/                 # checkpoint-*.tar AND checkpoint-*.blob
```
> Requires `GCR_INTERCEPTION` unset/`true` on the DaemonSet (its default). With
> `GCR_INTERCEPTION=false` the agent runs baseline CRIUgpu and writes no `.blob`.
> For Deployment/StatefulSet/Job, use the WorkloadCheckpoint orchestrator
> (`orchestrator/`).

## 7. GPUCheckpoint CR
`.spec.workloadRef {kind: Pod, namespace, name, container, nodeInfo}` (the agent
resolves `kind: Pod` only — multi-replica goes through WorkloadCheckpoint),
`.spec.storage {type: hostPath|mount|nfs|pvc|s3, path/source/endpoint/...}`,
`.spec.schedule` (empty = one-shot; Go duration **or** cron). `.status` (phase,
observedNode, checkpointCount, lastCheckpointPath, conditions) is on the CRD and
updated by the agent.

See `docs/SETUP.ko.md` for the detailed Korean guide and troubleshooting.
