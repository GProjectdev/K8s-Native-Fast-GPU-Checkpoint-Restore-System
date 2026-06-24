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

### B-6. Deploy this system (after workers have joined)

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

```bash
# Add the CUDA/NVIDIA repo for Ubuntu 22.04
wget https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2204/x86_64/cuda-keyring_1.1-1_all.deb
dpkg -i cuda-keyring_1.1-1_all.deb
apt-get update

# Install a driver branch >= 550 (e.g. 550 or 560)
apt-get install -y nvidia-driver-550
reboot
```

After reboot, verify the driver **and** the `cuda-checkpoint` binary:

```bash
nvidia-smi                       # driver 550.x+
which cuda-checkpoint            # /usr/bin/cuda-checkpoint
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

# Troubleshooting

| Symptom | Likely cause / fix |
|---------|--------------------|
| `kubelet checkpoint returned 404/feature` | `ContainerCheckpoint` gate not enabled on kubelet **and** apiserver. Re-check A-5 / B-1. |
| `criu check` fails | CRIU too old or missing kernel options; build ≥ 3.17 from source. |
| Pod can't see GPU | NVIDIA driver/toolkit not installed, or containerd not configured (C-1/C-2). |
| node-agent not scheduled | Node missing `nvidia.com/gpu.present=true` label (B-5) or PodSecurity blocking privileged (B-6 label). |
| `cuda-checkpoint: command not found` | Driver < 550; install a 550+ branch (C-1). |
| No GPU-side checkpoint | GCR hook `libcuda.so` not staged in `/var/lib/gpu-cr/lib/` (C-5). |
