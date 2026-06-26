# Quickstart — fresh GCP VM to a working GPU C/R system (clean setup)

Delete the old VMs, recreate with a **single 300GB boot disk (no extra storage)**,
then follow this top-to-bottom to reach a deployed & testable GCR-based GPU
Checkpoint/Restore system on Kubernetes (CRI-O).

> With one 300GB boot disk, all the previous pain (separate data-disk mounts,
> bind mounts, TMPDIR relocation, DiskPressure) disappears — every path lives on
> the boot disk.

## 0. Prereqs (VM creation)
- GCP A100 instance (e.g. `a2-highgpu-1g`), Ubuntu 22.04 LTS
- **300GB boot disk**, **no** additional disks
- 1 master + N GPU workers; firewall allows node-to-node + apiserver 6443

## 1. Base Kubernetes — external installer
Install the master/worker K8s (CRI-O) base with:
`https://github.com/GProjectdev/Kubernetes_Installer_with_CRIO`
Run the master setup → `kubeadm init` → CNI → `kubeadm join` on each worker.
`kubectl get nodes` should be Ready. GPU/CRIU/checkpoint extras are added in step 2.

## 2. GPU worker prep — `gpu-worker-setup.sh` (on each GPU worker, as root)
The NVIDIA driver install needs **one reboot**; the script handles the
install → reboot → re-run flow.
```bash
git clone https://github.com/GProjectdev/K8s-Native-Fast-GPU-Checkpoint-Restore-System.git
cd K8s-Native-Fast-GPU-Checkpoint-Restore-System
sudo bash quickstart/scripts/gpu-worker-setup.sh   # installs driver, asks to reboot
sudo reboot
sudo bash quickstart/scripts/gpu-worker-setup.sh   # finishes everything
```
It sets up: gcc-12 + headers, NVIDIA driver 550, cuda-checkpoint binary, NVIDIA
Container Toolkit, CRIU+runc (+CRIU CUDA-plugin probe), CRI-O drop-in (nvidia
default + monitor_path), kubelet `ContainerCheckpoint` gate, and the GPU C/R dirs.

## 3. Deploy — `master-deploy.sh` (on master)
```bash
bash quickstart/scripts/master-deploy.sh
```
Installs NVIDIA device plugin → labels GPU nodes → applies CRD + RBAC +
`gpu-cr-system` ns + the CRI-O DaemonSet (image
`docker.io/jeongseungjun/gpu-cr-node-agent:v1.0`, `imagePullPolicy: Always`).

## 4. Test — deploy a GPU pod, then request a checkpoint
```bash
kubectl apply -f deploy/sample-pod-pytorch.yaml
kubectl get pod cuda-gcr-pod -o wide -w
kubectl apply -f deploy/sample-gpucheckpoint-pytorch.yaml
kubectl get gpucheckpoints -w
kubectl -n gpu-cr-system logs -l app.kubernetes.io/name=gpu-cr-node-agent -f
```
5-step pipeline (`internal/agent/checkpoint.go`): (1) selective-interception data
buffer checkpoint → (2) cuda-checkpoint control-state suspend → (3) kubelet
checkpoint API (CRI-O/CRIU) → (4) store to `.spec.storage.path` → (5) resume.
Success → `checkpoint-*.tar` in `/var/lib/gcr-checkpoint/`, CR `Completed`.

## 5. GPU-checkpoint mechanism note (current frontier)
Two paths for steps 2-3: **(A)** cuda-checkpoint via `CUDA_CHECKPOINT_NSENTER=true`
(host namespaces) then CRIU; **(B)** delegate GPU dump to CRIU (CRIUgpu) with
`CUDA_CHECKPOINT_SKIP=true`, which needs the CRIU CUDA plugin. The worker script
prints whether that plugin is present.

See `docs/SETUP.md` for detailed verification logs. Troubleshooting table in
`README.ko.md`.
