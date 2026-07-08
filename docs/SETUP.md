# Setup & Usage — CRIUgpu (from VM creation to a working system)

This branch (main) checkpoints the GPU via **CRIUgpu**: the Node Agent only
orchestrates, and the actual GPU+CPU checkpoint is done by the **kubelet
Checkpoint API → CRI-O → CRIU + the NVIDIA cuda_plugin**. There is no in-Pod
interceptor and no host cuda-checkpoint helper (that is the GCR data-engine
approach preserved on the `v1.0` branch).

## 0. Overview
Creating a `GPUCheckpoint` CR makes the Node Agent: (1) call the kubelet
Checkpoint API — CRI-O/CRIU + cuda_plugin checkpoint the container (CPU + GPU)
into a tar; (2) store the tar to `.spec.storage.path`.

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
ls -lh /var/lib/gcr-checkpoint/                 # checkpoint-*.tar
```

## 7. GPUCheckpoint CR
`.spec.workloadRef {kind(Pod|Deployment|StatefulSet), namespace, name, container,
nodeInfo}`, `.spec.storage {type, path, endpoint}`, `.spec.schedule` (Go duration;
empty = one-shot). `.status` (phase, observedNode, checkpointCount,
lastCheckpointPath, conditions) is defined on the CRD and updated by the agent.

See `docs/SETUP.ko.md` for the detailed Korean guide and troubleshooting.
