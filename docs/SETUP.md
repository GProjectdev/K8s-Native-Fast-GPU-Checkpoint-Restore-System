# Setup & Usage — From VM creation to a working system (verified)

Follow this top-to-bottom right after creating a fresh GPU VM to reach a working
GCR-style GPU checkpoint system on Kubernetes (CRI-O). Verified end-to-end on
K8s v1.33 + CRI-O + Cilium + NVIDIA A100 (GCP).

## 0. What works (verified)

Creating a `GPUCheckpoint` CR makes the Node Agent on the Pod's node:
1. **Data (GCR data engine)** — the LD_PRELOAD interceptor backs `cudaMalloc` with
   the CUDA **VMM** API (owns the memory), and on checkpoint copies buffers to the
   host and frees ONLY the physical memory while preserving the virtual address.
   Framework-agnostic (no PyTorch `expandable_segments` needed).
2. **Control** — `cuda-checkpoint` (via a host helper) handles control state.
3. **CPU + data** — kubelet checkpoint API → CRI-O → **CRIU (CPU only)** dumps the
   process incl. the host-resident GPU data → a tar.
4. **Non-destructive resume** — cuda-checkpoint resume + remap to the same VA + H2D.

> Requires **NVIDIA driver 570+** (550/560 leave `/dev/nvidia*` fds open so CRIU
> fails). A single **300GB boot disk** removes all the disk juggling.

## 1. VM
GCP A100 (e.g. `a2-highgpu-1g`), Ubuntu 22.04, **300GB boot disk, no extra disks**, 1 master + N GPU workers.

## 2. Base Kubernetes
Use `https://github.com/GProjectdev/Kubernetes_Installer_with_CRIO` (master init + worker join). `kubectl get nodes` Ready.

## 3. GPU worker prep (each worker, root) — one script, one reboot
```bash
git clone https://github.com/GProjectdev/K8s-Native-Fast-GPU-Checkpoint-Restore-System.git
cd K8s-Native-Fast-GPU-Checkpoint-Restore-System
sudo bash quickstart/scripts/gpu-worker-setup.sh   # installs driver 570, asks to reboot
sudo reboot
sudo bash quickstart/scripts/gpu-worker-setup.sh   # finishes everything
```
Sets up: gcc-12, **driver 570**, cuda-checkpoint, NVIDIA Container Toolkit, CRIU+runc,
**crun delegation fix**, CRI-O drop-in, kubelet `ContainerCheckpoint` gate, GPU C/R
dirs, the **cuda-checkpoint host helper service**, and **disables the CRIU cuda_plugin**
(so CRIU does CPU only = GCR).

## 4. Master: deploy
```bash
bash quickstart/scripts/master-deploy.sh
kubectl -n gpu-cr-system set env ds/gpu-cr-node-agent GCR_INTERCEPTION=true CUDA_CHECKPOINT_SKIP=false
kubectl -n gpu-cr-system rollout restart ds/gpu-cr-node-agent
```

## 5. Build the agent image (build host: Go 1.22+, gcc/make, buildah)
```bash
buildah bud -f Dockerfile -t jeongseungjun/gpu-cr-node-agent:v1.0 .
buildah push jeongseungjun/gpu-cr-node-agent:v1.0 docker://docker.io/jeongseungjun/gpu-cr-node-agent:v1.0
kubectl -n gpu-cr-system rollout restart ds/gpu-cr-node-agent
```

## 6. Run a GPU pod + request a checkpoint
```bash
kubectl apply -f deploy/sample-pod-l1.yaml                 # GCR_VMM_ALLOC=1 + interceptor
kubectl get pod cuda-l1-pod -o wide -w
kubectl logs cuda-l1-pod | grep '\[gcr\]'                  # [gcr][vmm-alloc] req=4294967296 ...
kubectl apply -f deploy/sample-gpucheckpoint-l1.yaml       # apply right after a "checksum OK"
kubectl get gpucheckpoints.gpu-cr.io -w                    # Checkpointing -> Completed
```

## 7. Verify
```bash
kubectl logs cuda-l1-pod | grep '\[gcr\]\[engine\]'
#  freeze: N segs, ~4GB copied to host, physical released (VA kept); 0 failed
#  remap:  N segs restored to same VA + H2D; 0 failed
kubectl logs cuda-l1-pod | grep checksum | tail           # checksum still OK after remap
ls -lh /var/lib/gcr-checkpoint/                            # checkpoint-*.tar
```
Pass = GPUCheckpoint `Completed` + `freeze/remap … 0 failed` + tar + checksum preserved.

## 8. How it works
5-step pipeline (`internal/agent/checkpoint.go`): (1) interceptor freeze (D2H + free
physical, keep VA) → (2) cuda-checkpoint control → (3) kubelet/CRI-O/CRIU dumps CPU +
host data (GPU untouched; cuda_plugin off, driver 570 releases fds) → (4) store →
(5) resume + remap to same VA. **CRIU never checkpoints the GPU**; the GPU state is
moved to host memory first by the interceptor (data) + cuda-checkpoint (control).

## 9. Troubleshooting
See `docs/SETUP.ko.md` §9 for the full table (driver 570 requirement, crun delegation,
libcuda RTLD resolution, no cuGetProcAddress hooking, GOSUMDB=off, etc.).
