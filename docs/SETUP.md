# Setup Guide — Master & Worker Nodes

Step-by-step preparation to run and test the **K8s-Native Fast GPU
Checkpoint/Restore System**. Follow the sections in order. Commands target
**Ubuntu 22.04 LTS**, **Kubernetes v1.30 (kubeadm)**, and **containerd**.

> 🇰🇷 한국어: [`SETUP.ko.md`](SETUP.ko.md)

## Topology

```
            ┌──────────────────────┐
            │   Master Node        │   control plane (no GPU needed)
            │   - kube-apiserver    │   ContainerCheckpoint feature gate
            │   - controller/sched  │   device-plugin + this system deployed here
            └──────────┬───────────┘
                       │ join
        ┌──────────────┼──────────────┐
        ▼                              ▼
┌──────────────────┐          ┌──────────────────┐
│  Worker Node 1   │   ...    │  Worker Node N   │   GPU nodes
│  NVIDIA driver550 │          │  NVIDIA driver550 │   CRIU + cuda-checkpoint
│  containerd+nvidia│          │  containerd+nvidia│   GCR hook driver staged
│  GPU C/R Node Agent (DaemonSet runs here)        │
└──────────────────┘          └──────────────────┘
```

## Version matrix (tested target)

| Component | Version |
|-----------|---------|
| OS | Ubuntu 22.04 LTS |
| Kubernetes | v1.30.x (kubeadm/kubelet/kubectl) |
| Container runtime | containerd ≥ 1.7 |
| NVIDIA driver | ≥ 550 (ships `cuda-checkpoint`) |
| CRIU | ≥ 3.17 |
| NVIDIA Container Toolkit | ≥ 1.14 |

---

# Part A — Common base (run on EVERY node: master + workers)

Run everything in Part A on **all** nodes.

### A-1. Host prerequisites

```bash
# Become root
sudo -i

# Set a unique hostname per node (example)
# hostnamectl set-hostname master       # on master
# hostnamectl set-hostname worker-1     # on each worker

# Disable swap (required by kubelet)
swapoff -a
sed -i '/ swap / s/^/#/' /etc/fstab
```

### A-2. Kernel modules & sysctl

```bash
cat <<EOF | tee /etc/modules-load.d/k8s.conf
overlay
br_netfilter
EOF
modprobe overlay
modprobe br_netfilter

cat <<EOF | tee /etc/sysctl.d/k8s.conf
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF
sysctl --system
```

### A-3. Install containerd

```bash
apt-get update
apt-get install -y ca-certificates curl gnupg

install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
  | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $VERSION_CODENAME) stable" \
  | tee /etc/apt/sources.list.d/docker.list
apt-get update
apt-get install -y containerd.io

# Generate default config and enable systemd cgroups (required for kubeadm)
mkdir -p /etc/containerd
containerd config default | tee /etc/containerd/config.toml >/dev/null
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
systemctl restart containerd
systemctl enable containerd
```

### A-4. Install kubeadm, kubelet, kubectl (v1.30)

```bash
curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.30/deb/Release.key \
  | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] \
https://pkgs.k8s.io/core:/stable:/v1.30/deb/ /" \
  | tee /etc/apt/sources.list.d/kubernetes.list
apt-get update
apt-get install -y kubelet kubeadm kubectl
apt-mark hold kubelet kubeadm kubectl
```

### A-5. Enable the `ContainerCheckpoint` feature gate on the kubelet

This is required on every node where checkpoints are taken (all workers; harmless
on master).

```bash
mkdir -p /etc/systemd/system/kubelet.service.d
cat <<EOF | tee /etc/systemd/system/kubelet.service.d/20-checkpoint.conf
[Service]
Environment="KUBELET_EXTRA_ARGS=--feature-gates=ContainerCheckpoint=true"
EOF
systemctl daemon-reload
# kubelet restarts automatically once the node is initialized/joined.
```

---

# Part B — Master Node only

Run Part B **only on the master**.

### B-1. Initialize the control plane (with the feature gate on the apiserver)

```bash
# Choose a pod network CIDR matching your CNI (Calico default below).
kubeadm init \
  --pod-network-cidr=192.168.0.0/16 \
  --feature-gates=ContainerCheckpoint=true \
  --apiserver-extra-args feature-gates=ContainerCheckpoint=true
```

> If your kubeadm version rejects `--apiserver-extra-args`, use a config file
> instead (see [B-1b](#b-1b-alternative-kubeadm-config-file)).

### B-2. Configure kubectl for your user

```bash
# As the regular (non-root) user:
mkdir -p $HOME/.kube
sudo cp -i /etc/kubernetes/admin.conf $HOME/.kube/config
sudo chown $(id -u):$(id -g) $HOME/.kube/config
kubectl get nodes        # master should appear (NotReady until CNI is installed)
```

### B-3. Install a CNI (Calico example)

```bash
kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.28.0/manifests/calico.yaml
kubectl get pods -n kube-system -w     # wait until calico + coredns are Running
```

### B-4. Save the join command (for workers)

```bash
kubeadm token create --print-join-command
# Copy the printed `kubeadm join ...` line — you will run it on each worker (Part C-7).
```

### B-5. Install the NVIDIA device plugin (cluster-wide)

```bash
kubectl apply -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.15.0/deployments/static/nvidia-device-plugin.yml
# Optional but recommended: GPU Feature Discovery to auto-label nodes
#   https://github.com/NVIDIA/gpu-feature-discovery
```

If your GPU nodes don't get the `nvidia.com/gpu.present=true` label automatically,
label them manually (the DaemonSet's nodeSelector uses it):

```bash
kubectl label node <worker-name> nvidia.com/gpu.present=true
```

### B-6. Build & push the GPU C/R Node Agent image (Buildah)

The DaemonSet runs a container image, so you must build and publish it before
deploying. Run this on a **build host** that has Go 1.22+, gcc/make, and Buildah
(this can be the master or any workstation with repo access).

```bash
# Install build tools (Ubuntu)
apt-get install -y golang-go gcc make buildah

# From the repo root: build the agent + interceptor shim into one image
buildah bud -f Dockerfile -t ghcr.io/gprojectdev/gpu-cr-node-agent:latest .

# Push to a registry your nodes can pull from
buildah login ghcr.io                         # username + PAT/token
buildah push ghcr.io/gprojectdev/gpu-cr-node-agent:latest \
  docker://ghcr.io/gprojectdev/gpu-cr-node-agent:latest
```

> Image name must match `image:` in `deploy/daemonset.yaml`. If you use a
> different registry/name, edit that field accordingly.

**Air-gapped / no registry?** Export an OCI archive and import it into containerd
on each GPU worker instead of pushing:

```bash
# On the build host
buildah push ghcr.io/gprojectdev/gpu-cr-node-agent:latest \
  oci-archive:/tmp/gpu-cr-node-agent.tar:ghcr.io/gprojectdev/gpu-cr-node-agent:latest
scp /tmp/gpu-cr-node-agent.tar <worker>:/tmp/

# On each GPU worker — import into the k8s.io namespace containerd uses
ctr -n k8s.io images import /tmp/gpu-cr-node-agent.tar
# Then set imagePullPolicy: IfNotPresent (already the default in the DaemonSet).
```

### B-7. Deploy this system (after workers have joined & image is published)

```bash
# From the repo root, on the master:
kubectl apply -f config/crd/gpu-cr.io_gpucheckpoints.yaml
kubectl apply -f deploy/rbac.yaml
kubectl label ns gpu-cr-system pod-security.kubernetes.io/enforce=privileged --overwrite
kubectl apply -f deploy/daemonset.yaml
kubectl -n gpu-cr-system get pods -o wide     # one node-agent per GPU node
```

### B-1b. (Alternative) kubeadm config file

If extra-args flags don't work on your kubeadm, use this instead of B-1:

```yaml
# kubeadm-config.yaml
apiVersion: kubeadm.k8s.io/v1beta3
kind: ClusterConfiguration
kubernetesVersion: v1.30.0
networking:
  podSubnet: 192.168.0.0/16
apiServer:
  extraArgs:
    feature-gates: ContainerCheckpoint=true
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
featureGates:
  ContainerCheckpoint: true
```

```bash
kubeadm init --config kubeadm-config.yaml
```

---

# Part C — Worker Node only (GPU nodes)

Run Part C on **each GPU worker**. (Part A must already be done on this node.)

### C-1. Install the NVIDIA driver (≥ 550)

> **Critical — DKMS / GCC match.** The driver's kernel module is compiled with
> the system's default `gcc`, which **must match the GCC the running kernel was
> built with**. On Ubuntu 22.04 with a 6.8 HWE/cloud kernel (e.g. GCP), the kernel
> is built with **GCC 12** while the default `gcc` is 11, so the DKMS build fails
> with `unrecognized command-line option '-ftrivial-auto-var-init=zero'`. Install
> and select GCC 12 **before** installing the driver.

```bash
# 0) Make the build toolchain match the kernel's compiler
sudo apt-get update
sudo apt-get install -y build-essential dkms gcc-12 linux-headers-$(uname -r)
sudo update-alternatives --install /usr/bin/gcc gcc /usr/bin/gcc-12 60
sudo update-alternatives --install /usr/bin/gcc gcc /usr/bin/gcc-11 50
sudo update-alternatives --set gcc /usr/bin/gcc-12
cat /proc/version          # note the GCC version the kernel was built with
gcc --version              # must match (e.g. 12.x)

# 1) Add the CUDA/NVIDIA repo for Ubuntu 22.04
wget https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2204/x86_64/cuda-keyring_1.1-1_all.deb
sudo dpkg -i cuda-keyring_1.1-1_all.deb
sudo apt-get update

# 2) Install a driver branch >= 550. For A100 / datacenter GPUs on recent
#    kernels, the open-module flavor (nvidia-driver-550-open) is recommended.
sudo apt-get install -y nvidia-driver-550
sudo reboot
```

After reboot, verify the module built, loaded, and the GPU is visible:

```bash
sudo dkms status           # nvidia/550.x, <kernel>: installed   (NOT just "added")
sudo modprobe nvidia
nvidia-smi                 # shows the GPU + driver 550.x
```

> If `dkms status` shows `added` (not `installed`), or `nvidia-smi` reports
> "couldn't communicate with the NVIDIA driver", the kernel module did not build.
> This is almost always the GCC mismatch above — see Troubleshooting. After
> fixing gcc: `sudo dkms install nvidia/<ver> -k $(uname -r) --force`.

### C-1b. Install the `cuda-checkpoint` binary

`cuda-checkpoint` is **not** placed on `PATH` by the apt driver packages. Install
the prebuilt binary from NVIDIA (requires a working driver):

```bash
git clone https://github.com/NVIDIA/cuda-checkpoint.git
ls cuda-checkpoint/bin/                                    # find the x86_64 build dir
sudo install -m 0755 cuda-checkpoint/bin/x86_64_Linux/cuda-checkpoint /usr/bin/cuda-checkpoint
cuda-checkpoint --help
```

### C-2. Install the NVIDIA Container Toolkit

```bash
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
  | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
  | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
  | tee /etc/apt/sources.list.d/nvidia-container-toolkit.list
apt-get update
apt-get install -y nvidia-container-toolkit

# Wire the NVIDIA runtime into containerd and make it the default
nvidia-ctk runtime configure --runtime=containerd --set-as-default
systemctl restart containerd
```

### C-3. Install CRIU (≥ 3.17)

```bash
apt-get install -y criu
criu --version
criu check                       # expect "Looks good."
```

> If the distro's CRIU is older than 3.17, build from source:
> <https://criu.org/Installation>

### C-4. Stage the interceptor library & runtime dirs

```bash
mkdir -p /var/lib/gpu-cr/lib /var/lib/gpu-cr/run /var/lib/gcr-checkpoint
# The Node Agent installs libgcr-interceptor.so here on startup.
```

### C-5. Build & stage the GCR hook driver (`libcuda.so`)

The selective `cuMem*` interception driver is built from upstream:

```bash
git clone https://github.com/thustorage/GCR.git
cd GCR/GCR
bash build.sh                    # produces libcuda.so (see upstream README)
cp libcuda.so /var/lib/gpu-cr/lib/libcuda.so
```

> Without this file, the interceptor shim falls back to the real driver and no
> GPU-side checkpoint happens. (You can still test orchestration in dry-run.)

### C-6. Confirm the kubelet feature gate (from Part A-5)

```bash
cat /etc/systemd/system/kubelet.service.d/20-checkpoint.conf   # must contain ContainerCheckpoint=true
```

### C-7. Join the cluster

```bash
# Paste the join command printed by the master in B-4:
kubeadm join <MASTER_IP>:6443 --token <token> \
  --discovery-token-ca-cert-hash sha256:<hash>

# After join, ensure the kubelet picked up the feature gate:
systemctl restart kubelet
```

### C-8. Verify on the master

```bash
kubectl get nodes -o wide                          # worker Ready
kubectl describe node <worker> | grep nvidia.com/gpu   # nvidia.com/gpu: 1
```

---

# Part D — End-to-end smoke test

From the master, once everything is up:

```bash
# 1. Run a GPU workload wired for GCR interception
kubectl apply -f deploy/sample-pod.yaml

# 2. Set podRef.nodeInfo in deploy/sample-gpucheckpoint.yaml to the worker name,
#    then request checkpoints
kubectl apply -f deploy/sample-gpucheckpoint.yaml

# 3. Watch the CR status update (Phase -> Completed, Count increments per period)
kubectl get gpucheckpoints -w

# 4. Inspect the produced archive on the worker
ssh <worker> 'ls -lh /var/lib/gcr-checkpoint/'
```

### Pre-flight checklist (run on a worker)

```bash
nvidia-smi -L
cuda-checkpoint --help
criu check
ls /run/containerd/containerd.sock
ls /var/lib/gpu-cr/lib/libcuda.so
kubectl version            # (from master) server >= v1.30
```

### Dry-run (no GPU available)

Set `--dry-run=true` in `deploy/daemonset.yaml` (already an arg) to validate the
reconcile loop, CR status updates, and storage layout without driver/CRIU.

---

# Appendix — CRI-O clusters (e.g. the GProjectdev installer)

Parts A–C above assume **containerd**. If your cluster runs **CRI-O** (socket
`/var/run/crio/crio.sock`, as the
[Kubernetes_Installer_with_CRIO](https://github.com/GProjectdev/Kubernetes_Installer_with_CRIO)
sets up), apply these differences.

### CRI-O-1. Point the agent at the CRI-O socket

In `deploy/daemonset.yaml` change **all three** socket references and the env to
CRI-O, then re-apply:

```bash
sed -i 's#/run/containerd/containerd.sock#/var/run/crio/crio.sock#g' deploy/daemonset.yaml
kubectl apply -f deploy/daemonset.yaml
kubectl -n gpu-cr-system set env ds/gpu-cr-node-agent \
  CONTAINER_RUNTIME_ENDPOINT=unix:///var/run/crio/crio.sock
```

> Symptom if skipped: agent Pod stuck with
> `hostPath type check failed: /run/containerd/containerd.sock is not a socket file`.

### CRI-O-2. CRIU + runc runtime (checkpoint support)

CRI-O enables checkpoint/restore automatically when CRIU is present — you do
**not** need `enable_criu_support` (it is not a valid key on recent CRI-O and
makes crio fail to start). Install CRIU + runc and define a `runc` runtime:

```bash
sudo add-apt-repository -y ppa:criu/ppa && sudo apt-get update
sudo apt-get install -y criu runc
sudo criu check                         # "Looks good."
which runc                              # usually /usr/sbin/runc

sudo tee /etc/crio/crio.conf.d/99-runc.conf >/dev/null <<'EOF'
[crio.runtime]
default_runtime = "runc"

[crio.runtime.runtimes.runc]
runtime_path = "/usr/sbin/runc"
runtime_type = "oci"
runtime_root = "/run/runc"
EOF
sudo systemctl restart crio
sudo systemctl status crio --no-pager   # active (running)
```

### CRI-O-3. NVIDIA GPUs on CRI-O — use CDI, not a custom runtime

`nvidia-ctk runtime configure --runtime=crio` writes a `nvidia` runtime drop-in
that **breaks crio** on 1.33 (`failed to translate monitor fields for runtime
nvidia: "conmon" not found in $PATH`). Prefer **CDI**:

```bash
# remove the broken nvidia runtime drop-in if you ran the command above
sudo rm -f /etc/crio/crio.conf.d/99-nvidia.toml
sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml
nvidia-ctk cdi list
sudo systemctl restart crio
```

> If you must keep the custom runtime, add
> `monitor_path = "/usr/libexec/crio/conmon"` to the `[crio.runtime.runtimes.nvidia]`
> block instead of removing it.

### CRI-O-4. Node storage / DiskPressure

Cloud images often boot a small (~10 GB) root disk while a large data disk sits
**unmounted**, so CRI-O image storage (`/var/lib/containers`) fills the root and
the kubelet evicts Pods with `DiskPressure`. Mount the data disk for container
storage:

```bash
lsblk ; df -h /                          # is the big disk (e.g. /dev/sdb) unmounted?
sudo systemctl stop kubelet crio
sudo umount -R /var/lib/containers/storage/overlay 2>/dev/null || true
sudo mkfs.ext4 -F /dev/sdb               # ONLY if the disk is empty (blkid shows nothing)
sudo mv /var/lib/containers /var/lib/containers.bak
sudo mkdir -p /var/lib/containers
echo '/dev/sdb /var/lib/containers ext4 defaults,nofail 0 2' | sudo tee -a /etc/fstab
sudo mount /var/lib/containers
sudo systemctl start crio kubelet
sudo rm -rf /var/lib/containers.bak      # reclaim the root space (this is what clears DiskPressure)
df -h / /var/lib/containers
```

---

# Troubleshooting

| Symptom | Likely cause / fix |
|---------|--------------------|
| `kubelet checkpoint returned 404/feature` | `ContainerCheckpoint` gate not enabled on kubelet **and** apiserver. Re-check A-5 / B-1. |
| `criu check` fails | CRIU too old or missing kernel options; build ≥ 3.17 from source. |
| Pod can't see GPU | NVIDIA driver/toolkit not installed, or the container runtime (containerd/CRI-O) not configured (C-1/C-2). |
| node-agent not scheduled | Node missing `nvidia.com/gpu.present=true` label (B-5) or PodSecurity blocking privileged (B-6 label). |
| `nvidia-smi` "couldn't communicate" / `dkms status` = `added` | DKMS module did not build — usually a GCC mismatch. Install `gcc-12`, `update-alternatives --set gcc /usr/bin/gcc-12`, then `dkms install nvidia/<ver> -k $(uname -r) --force` (C-1). |
| `unrecognized command-line option '-ftrivial-auto-var-init=zero'` | Default `gcc` older than the kernel's build GCC. Install & select `gcc-12` before building the driver (C-1). |
| `cuda-checkpoint: command not found` | Binary not on PATH; install the prebuilt binary from github.com/NVIDIA/cuda-checkpoint (C-1b). Requires a working driver. |
| No GPU-side checkpoint | GCR hook `libcuda.so` not staged in `/var/lib/gpu-cr/lib/` (C-5). |
| agent Pod: `hostPath type check failed: ...containerd.sock is not a socket` | Node runs CRI-O, not containerd. Point the daemonset socket + `CONTAINER_RUNTIME_ENDPOINT` at `/var/run/crio/crio.sock` (Appendix CRI-O-1). |
| agent Pod `Evicted: node had condition [DiskPressure]` | Container storage on a small root disk. Mount the data disk at `/var/lib/containers` and remove the old `.bak` (Appendix CRI-O-4). |
| crio won't start: `failed to translate monitor fields for runtime nvidia: "conmon" not found` | Broken `nvidia` runtime drop-in from `nvidia-ctk runtime configure --runtime=crio`. Remove it and use CDI, or add `monitor_path` (Appendix CRI-O-3). |
| crio won't start after editing `crio.conf.d` | A drop-in references a runtime with no definition, or an unknown key like `enable_criu_support`. Check `journalctl -xeu crio | tail`; use the minimal runc drop-in (Appendix CRI-O-2). |
