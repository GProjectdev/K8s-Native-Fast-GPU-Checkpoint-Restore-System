# Setup & Usage — Verified End-to-End (CRI-O)

This is the **battle-tested** guide: from fresh VMs to a working GPU
checkpoint, with every gotcha we actually hit folded in. Target stack:

| Component | Version |
|-----------|---------|
| OS | Ubuntu 22.04 LTS |
| Kubernetes | v1.33 (kubeadm) |
| Container runtime | **CRI-O v1.33** (socket `/run/crio/crio.sock`) |
| CNI | Cilium |
| NVIDIA driver | **550** (CUDA 12.4) — ships nothing extra; see 2.1/2.2 |
| CRIU | from `ppa:criu/ppa` |
| NVIDIA Container Toolkit | ≥ 1.19 |

> 🇰🇷 한국어: [`SETUP.ko.md`](SETUP.ko.md)

Topology: **1 master (no GPU) + N GPU workers**. The base cluster is built with
[GProjectdev/Kubernetes_Installer_with_CRIO](https://github.com/GProjectdev/Kubernetes_Installer_with_CRIO)
(`k8s-masternode-setup.sh`, `k8s-workernode-setup.sh`). Everything below is what
you do **after** that installer.

---

## 1. Enable the container-checkpoint feature gate (all nodes that checkpoint)

```bash
echo 'KUBELET_EXTRA_ARGS="--feature-gates=ContainerCheckpoint=true"' | sudo tee /etc/default/kubelet
sudo systemctl daemon-reload && sudo systemctl restart kubelet
```

Verify (replace with a real GPU worker node name; `405`/method-not-allowed = OK):

```bash
kubectl get --raw /api/v1/nodes/<gpu-worker>/proxy/checkpoint/ ; echo
```

---

## 2. GPU worker preparation (run on EACH GPU worker, as root)

### 2.1 NVIDIA driver 550 — install GCC 12 FIRST (critical)

The DKMS kernel module must be built with the **same GCC the kernel was built
with**. Ubuntu 22.04's 6.8 cloud kernel (e.g. GCP) is built with **GCC 12**, but
the default `gcc` is 11 → DKMS fails with
`unrecognized command-line option '-ftrivial-auto-var-init=zero'`.

```bash
sudo apt-get update
sudo apt-get install -y build-essential dkms gcc-12 linux-headers-$(uname -r)
sudo update-alternatives --install /usr/bin/gcc gcc /usr/bin/gcc-12 60
sudo update-alternatives --install /usr/bin/gcc gcc /usr/bin/gcc-11 50
sudo update-alternatives --set gcc /usr/bin/gcc-12

wget https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2204/x86_64/cuda-keyring_1.1-1_all.deb
sudo dpkg -i cuda-keyring_1.1-1_all.deb && sudo apt-get update
sudo apt-get install -y nvidia-driver-550
sudo reboot
```

After reboot:

```bash
sudo dkms status     # nvidia/550.x, <kernel>: installed  (NOT "added")
nvidia-smi           # GPU + driver 550.x
```

If `dkms status` shows only `added` or `nvidia-smi` says "couldn't communicate",
the module didn't build — fix gcc (above) then `sudo dkms install nvidia/<ver> -k $(uname -r) --force`.

### 2.2 cuda-checkpoint binary (not on PATH from apt)

```bash
git clone https://github.com/NVIDIA/cuda-checkpoint.git
sudo install -m 0755 cuda-checkpoint/bin/x86_64_Linux/cuda-checkpoint /usr/bin/cuda-checkpoint
cuda-checkpoint --help
```

### 2.3 NVIDIA Container Toolkit + CRIU + one CRI-O drop-in

```bash
# Toolkit
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
  | sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
  | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
  | sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list
sudo apt-get update && sudo apt-get install -y nvidia-container-toolkit

# CRIU + runc
sudo add-apt-repository -y ppa:criu/ppa && sudo apt-get update
sudo apt-get install -y criu runc
sudo criu check     # "Looks good."
```

Now a **single CRI-O drop-in** that does three things at once:
- makes the **nvidia runtime the default** (so the NVIDIA device plugin can see
  GPUs and advertise `nvidia.com/gpu`),
- defines a **runc** runtime,
- gives both a `monitor_path` (without it crio 1.33 fails to start:
  `failed to translate monitor fields for runtime nvidia: "conmon" not found`).

CRIU checkpoint support is auto-enabled when `criu` is present — do **not** add the
removed `enable_criu_support` key (it makes crio fail).

```bash
sudo tee /etc/crio/crio.conf.d/99-gpu-cr.conf >/dev/null <<'CONF'
[crio.runtime]
default_runtime = "nvidia"

[crio.runtime.runtimes.nvidia]
runtime_path = "/usr/bin/nvidia-container-runtime"
runtime_type = "oci"
monitor_path = "/usr/libexec/crio/conmon"

[crio.runtime.runtimes.runc]
runtime_path = "/usr/sbin/runc"
runtime_type = "oci"
runtime_root = "/run/runc"
monitor_path = "/usr/libexec/crio/conmon"
CONF
# remove any broken nvidia drop-in from `nvidia-ctk runtime configure` if present
sudo rm -f /etc/crio/crio.conf.d/99-nvidia.toml
sudo systemctl restart crio
sudo systemctl is-active crio        # active
```

### 2.4 Storage — put container storage + pull staging on the big disk

Cloud VMs often boot a tiny (~10 GB) root with a large **unmounted** data disk,
so CRI-O image storage fills root → `DiskPressure` evictions and image pulls fail
with `no space left on device` in `/var/tmp`.

```bash
lsblk ; df -h /                          # find the unmounted big disk (e.g. /dev/sdb)
sudo systemctl stop kubelet crio
sudo umount -R /var/lib/containers/storage/overlay 2>/dev/null || true
sudo blkid /dev/sdb                      # empty (no output) before mkfs!
sudo mkfs.ext4 -F /dev/sdb
sudo mv /var/lib/containers /var/lib/containers.bak 2>/dev/null || true
sudo mkdir -p /var/lib/containers
echo '/dev/sdb /var/lib/containers ext4 defaults,nofail 0 2' | sudo tee -a /etc/fstab
sudo mount /var/lib/containers
sudo rm -rf /var/lib/containers.bak      # frees root; this is what clears DiskPressure

# pull staging (/var/tmp) also lives on root → redirect it onto the big disk
sudo mkdir -p /var/lib/containers/vartmp
grep -q '/var/lib/containers/vartmp /var/tmp ' /etc/fstab || \
  echo '/var/lib/containers/vartmp /var/tmp none bind 0 0' | sudo tee -a /etc/fstab
sudo mount /var/tmp
sudo systemctl start crio kubelet
df -h / /var/lib/containers /var/tmp     # containers + tmp now ~big disk
```

> **Also relocate the checkpoint OUTPUT dirs to the big disk** — the interceptor
> writes the GPU data-buffer copy to `/var/lib/gcr-checkpoint` and CRIU writes the
> container tar to `/var/lib/kubelet/checkpoints`; both default to the small root
> and will refill it (DiskPressure) on large workloads:
>
> ```bash
> sudo mkdir -p /var/lib/containers/gcr-checkpoint /var/lib/containers/kubelet-checkpoints
> sudo mkdir -p /var/lib/gcr-checkpoint /var/lib/kubelet/checkpoints
> echo '/var/lib/containers/gcr-checkpoint /var/lib/gcr-checkpoint none bind 0 0' | sudo tee -a /etc/fstab
> echo '/var/lib/containers/kubelet-checkpoints /var/lib/kubelet/checkpoints none bind 0 0' | sudo tee -a /etc/fstab
> sudo mount /var/lib/gcr-checkpoint ; sudo mount /var/lib/kubelet/checkpoints
> ```

### 2.5 GPU C/R runtime directories

```bash
sudo mkdir -p /var/lib/gpu-cr/lib /var/lib/gpu-cr/run /var/lib/gcr-checkpoint
```

(The Node Agent installs `libgcr-interceptor.so` into `/var/lib/gpu-cr/lib`; the
control channel lives under `/var/lib/gpu-cr/run`; checkpoints land in
`/var/lib/gcr-checkpoint`.)

---

## 3. Master: device plugin, labels, deploy the system

```bash
# NVIDIA device plugin + GPU node label (DaemonSet nodeSelector uses it)
kubectl apply -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.15.0/deployments/static/nvidia-device-plugin.yml
kubectl label node <gpu-worker> nvidia.com/gpu.present=true --overwrite

# wait, then confirm the GPU is advertised
kubectl -n kube-system rollout restart ds/nvidia-device-plugin-daemonset
kubectl get node <gpu-worker> -o jsonpath='{.status.capacity.nvidia\.com/gpu}{"\n"}'   # -> 1
```

---

## 4. Build & publish the agent image (build host w/ Go 1.22+, gcc/make, buildah)

```bash
cd K8s-Native-Fast-GPU-Checkpoint-Restore-System
buildah bud -f Dockerfile -t docker.io/<you>/gpu-cr-node-agent:v1.2 .
buildah login docker.io
buildah push docker.io/<you>/gpu-cr-node-agent:v1.2 docker://docker.io/<you>/gpu-cr-node-agent:v1.2
```

> Rebuild a NEW tag whenever the interceptor/agent code changes — the agent
> installs its bundled `libgcr-interceptor.so` onto the node at startup, so a
> stale image means a stale interceptor.

---

## 5. Deploy the system (master)

```bash
kubectl apply -f config/crd/gpu-cr.io_gpucheckpoints.yaml
kubectl apply -f deploy/rbac.yaml
kubectl label ns gpu-cr-system pod-security.kubernetes.io/enforce=privileged --overwrite

# Edit deploy/daemonset-crio.yaml: set image to docker.io/<you>/gpu-cr-node-agent:v1.2
kubectl apply -f deploy/daemonset-crio.yaml
kubectl -n gpu-cr-system get po -o wide        # 1/1 Running per GPU node
kubectl -n gpu-cr-system logs ds/gpu-cr-node-agent --tail=20   # "Node Agent starting"
```

---

## 6. Run a GPU workload + request a checkpoint

Use [`deploy/sample-pod.yaml`](../deploy/sample-pod.yaml) — it ships a
driver-550-compatible PyTorch workload **and the control-channel wiring** the
interceptor needs (`GCR_POD_UID`, `GCR_CONTROL_DIR`, and the
`/var/lib/gpu-cr/run` mount). Do **not** use a Pod manifest that lacks these — the
interceptor cannot ACK and step 1 times out.

```bash
kubectl apply -f deploy/sample-pod.yaml
kubectl get po vllm-gcr-pod -o wide
kubectl logs vllm-gcr-pod | grep -E 'GPU tensor allocated|\[gcr\]'
#   GPU tensor allocated ...
#   [gcr] interceptor loaded (pid=...): watching /var/lib/gpu-cr/run/<uid>/control
```

Then the checkpoint CR ([`deploy/sample-gpucheckpoint.yaml`](../deploy/sample-gpucheckpoint.yaml));
`container` must match (`cuda-app`), `period: "000000"` = one-shot:

```bash
kubectl apply -f deploy/sample-gpucheckpoint.yaml
kubectl get gpucheckpoints -w        # Checkpointing -> Completed
kubectl -n gpu-cr-system logs -l app.kubernetes.io/name=gpu-cr-node-agent --tail=80 -f
```

Expected pipeline (agent + pod logs interleaved):
- agent `checkpoint start ...` → pod `[gcr] checkpoint signal received` → `intercepted-info dumped` → `ACK sent`
- agent `step 1/5 done` → `step 2/5 ... cuda-checkpoint` → `step 3/5 ... kubelet produced` → `step 4/5 stored`

---

## 7. Verify the result

```bash
kubectl describe gpucheckpoint ckpt-vllm-001 | tail
ls -lh /var/lib/gcr-checkpoint/                    # checkpoint-*.tar (on the worker)
cat /var/lib/gpu-cr/run/*/intercepted-info         # intercepted GPU buffers
```

---

## 8. Troubleshooting (everything we hit)

| Symptom | Cause / fix |
|---------|-------------|
| `nvidia-smi` "couldn't communicate" / `dkms status` = `added` | DKMS didn't build. GCC mismatch — install `gcc-12`, set default, `dkms install ... --force` (2.1). |
| `unrecognized option '-ftrivial-auto-var-init=zero'` | default gcc older than the kernel's GCC → install/select gcc-12 (2.1). |
| `cuda-checkpoint: command not found` | install prebuilt binary from `NVIDIA/cuda-checkpoint` (2.2). |
| crio won't start: `failed to translate monitor fields for runtime nvidia: "conmon" not found` | nvidia runtime drop-in missing `monitor_path` → use the 99-gpu-cr.conf in 2.3. |
| crio won't start after editing `crio.conf.d` | unknown key (e.g. `enable_criu_support`) or a runtime with no definition → `journalctl -xeu crio | tail`; use 2.3 drop-in. |
| `0/ N nodes ... Insufficient nvidia.com/gpu` | device plugin can't see GPU → nvidia runtime must be the CRI-O default (2.3); restart device plugin (3). |
| agent Pod: `hostPath type check failed: ...containerd.sock is not a socket` | node is CRI-O → use `deploy/daemonset-crio.yaml` (CRI socket dir `/run/crio`). |
| agent CrashLoop: `listen :8081: address already in use` | hostNetwork port clash → metrics/health on :9290/:9291 (daemonset-crio already does). |
| crictl in agent tries 3 default endpoints / `connection refused` | `CONTAINER_RUNTIME_ENDPOINT` not set or socket file (not dir) mounted → daemonset-crio mounts `/run/crio` dir + sets endpoint. |
| Pod `Evicted: DiskPressure` / pull `no space left on device` in `/var/tmp` | root disk full → mount data disk at `/var/lib/containers` + bind `/var/tmp` (2.4). |
| vllm engine init fails: "driver too old / pytorch.org" | image's CUDA newer than driver 550 → use a CUDA ≤12.4 image (sample-pod uses pytorch cu121). |
| `data-buffer checkpoint did not ack: timeout ... /run/<uid>/control` | Pod missing control-channel wiring, or stale agent image. Use `deploy/sample-pod.yaml` (UID + `/var/lib/gpu-cr/run` mount) **and** an agent image built from current code (2.x interceptor). |
| `resolve container pid: no running container <name>` | CR `podRef.container` doesn't match the Pod's container name. |
