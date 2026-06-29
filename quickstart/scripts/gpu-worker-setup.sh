#!/usr/bin/env bash
# =============================================================================
# gpu-worker-setup.sh — GPU worker prep for the GCR-based GPU C/R system (CRI-O)
#
# Run this AS ROOT on every GPU **worker** node, AFTER you have completed the
# base Kubernetes setup from:
#   https://github.com/GProjectdev/Kubernetes_Installer_with_CRIO
# (i.e. k8s-workernode-setup.sh has run and the node has `kubeadm join`ed).
#
# Assumptions (clean GCP VM, single ~300GB boot disk, NO extra data disk):
#   * Ubuntu 22.04, GCP A100 instance, kernel 6.x
#   * CRI-O already installed & running (by the installer above)
#   * Everything lives on the boot disk -> NO disk relocation / bind mounts.
#
# The NVIDIA driver install needs a REBOOT. The script detects this: it installs
# the driver, then EXITS asking you to reboot and re-run. The second run finishes
# the GPU tooling (cuda-checkpoint, container toolkit, CRIU, CRI-O drop-in,
# kubelet feature gate, GPU C/R dirs).
#
#   sudo bash gpu-worker-setup.sh        # 1st run  -> installs driver, asks reboot
#   sudo reboot
#   sudo bash gpu-worker-setup.sh        # 2nd run  -> finishes everything
# =============================================================================
set -Eeuo pipefail
log(){ echo -e "\n\033[1;32m[gpu-worker-setup]\033[0m $*"; }
warn(){ echo -e "\033[1;33m[gpu-worker-setup]\033[0m $*"; }
die(){ echo -e "\033[1;31m[gpu-worker-setup] ERROR:\033[0m $*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "run as root (sudo)."

# -----------------------------------------------------------------------------
# 1) GCC 12 — the GCP 6.x kernel is built with gcc-12; the NVIDIA DKMS module
#    must compile with the same major to avoid '-ftrivial-auto-var-init=zero'
#    / version-mismatch build failures.
# -----------------------------------------------------------------------------
log "1/8  toolchain + headers (gcc-12, dkms, kernel headers)"
export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get install -y build-essential dkms gcc-12 g++-12 "linux-headers-$(uname -r)" \
                   curl wget git gnupg ca-certificates software-properties-common
update-alternatives --install /usr/bin/gcc gcc /usr/bin/gcc-12 60 --slave /usr/bin/g++ g++ /usr/bin/g++-12
update-alternatives --set gcc /usr/bin/gcc-12
log "gcc -> $(gcc -dumpversion)"

# -----------------------------------------------------------------------------
# 2) NVIDIA driver 570+ (CUDA C/R of PyTorch via CRIU needs >=570 — NVML/device-fd handling; 550/560 leave /dev/nvidia* fds open and CRIU fails)
#    If the driver is not yet usable, install it and STOP for a reboot.
# -----------------------------------------------------------------------------
if ! command -v nvidia-smi >/dev/null 2>&1 || ! nvidia-smi >/dev/null 2>&1; then
  log "2/8  installing NVIDIA driver 570 (reboot required afterwards)"
  if [ ! -f /usr/share/keyrings/cuda-archive-keyring.gpg ] && \
     ! dpkg -l | grep -q cuda-keyring; then
    wget -qO /tmp/cuda-keyring.deb \
      https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2204/x86_64/cuda-keyring_1.1-1_all.deb
    dpkg -i /tmp/cuda-keyring.deb
    apt-get update -y
  fi
  apt-get install -y cuda-drivers-570 || apt-get install -y nvidia-driver-570
  warn "=============================================================="
  warn " NVIDIA driver installed. REBOOT now, then RE-RUN this script:"
  warn "   sudo reboot"
  warn "   sudo bash $0"
  warn "=============================================================="
  exit 0
fi
log "2/8  NVIDIA driver OK"
nvidia-smi -L || die "nvidia-smi failed after reboot — check driver install."

# -----------------------------------------------------------------------------
# 3) cuda-checkpoint binary (NVIDIA control-state checkpoint utility)
# -----------------------------------------------------------------------------
log "3/8  cuda-checkpoint binary"
if ! command -v cuda-checkpoint >/dev/null 2>&1; then
  rm -rf /tmp/cuda-checkpoint
  git clone --depth 1 https://github.com/NVIDIA/cuda-checkpoint.git /tmp/cuda-checkpoint
  BIN=$(find /tmp/cuda-checkpoint -name cuda-checkpoint -type f -path '*x86_64*' | head -1)
  [ -n "$BIN" ] || BIN=$(find /tmp/cuda-checkpoint -name cuda-checkpoint -type f | head -1)
  install -m 0755 "$BIN" /usr/bin/cuda-checkpoint
fi
# Smoke test ON THE HOST (it must work here — the agent will nsenter into host ns).
cuda-checkpoint --help >/dev/null 2>&1 && log "cuda-checkpoint installed: $(command -v cuda-checkpoint)" \
  || warn "cuda-checkpoint --help returned nonzero (ok if usage still printed)."

# -----------------------------------------------------------------------------
# 4) NVIDIA Container Toolkit (so CRI-O can expose nvidia.com/gpu to pods)
# -----------------------------------------------------------------------------
log "4/8  NVIDIA Container Toolkit"
if ! command -v nvidia-ctk >/dev/null 2>&1; then
  curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
    | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
  curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
    | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
    > /etc/apt/sources.list.d/nvidia-container-toolkit.list
  apt-get update -y
  apt-get install -y nvidia-container-toolkit
fi
nvidia-ctk runtime configure --runtime=crio --set-as-default 2>/dev/null || true

# -----------------------------------------------------------------------------
# 5) CRIU + runc (container checkpoint engine for the kubelet checkpoint API)
# -----------------------------------------------------------------------------
log "5/8  CRIU + runc"
add-apt-repository -y ppa:criu/ppa 2>/dev/null || true
apt-get update -y
apt-get install -y criu runc || apt-get install -y criu
criu check || warn "criu check reported issues (often fine for container C/R)."
criu --version || true
# CRIU CUDA plugin probe — needed for CRIU to dump a *GPU* container itself.
echo "  -- CRIU CUDA plugin probe --"
ls -l /usr/lib/criu/ /usr/libexec/criu/ 2>/dev/null || true
find / -name '*cuda*plugin*' 2>/dev/null | grep -i criu || \
  warn "No CRIU CUDA plugin found. GPU-container CRIU dump may need NVIDIA's CRIU cuda plugin; cuda-checkpoint (step2) handles the GPU state in the meantime."

# -----------------------------------------------------------------------------
# 6) CRI-O drop-in: nvidia default runtime (for the device plugin) + runc,
#    BOTH with monitor_path so conmon is found. Single file, removes older ones.
# -----------------------------------------------------------------------------
log "6/8  CRI-O runtime drop-in (nvidia default + monitor_path)"
RUNC_BIN="$(command -v runc || echo /usr/sbin/runc)"
CONMON="/usr/libexec/crio/conmon"; [ -x "$CONMON" ] || CONMON="$(command -v conmon || echo /usr/bin/conmon)"
mkdir -p /etc/crio/crio.conf.d
rm -f /etc/crio/crio.conf.d/99-nvidia.toml 2>/dev/null || true
cat > /etc/crio/crio.conf.d/99-gpu-cr.conf <<CONF
[crio.runtime]
default_runtime = "nvidia"

[crio.runtime.runtimes.nvidia]
runtime_path = "/usr/bin/nvidia-container-runtime"
runtime_type = "oci"
monitor_path = "${CONMON}"

[crio.runtime.runtimes.runc]
runtime_path = "${RUNC_BIN}"
runtime_type = "oci"
runtime_root = "/run/runc"
monitor_path = "${CONMON}"
CONF
# CRITICAL: this CRI-O uses crun as its low-level runtime (/run/crun), but
# nvidia-container-runtime's default delegate list is ["docker-runc","runc"] and
# does NOT include crun -> every container fails ("container does not exist"),
# which breaks Cilium on the node, which taints it node.cilium.io/agent-not-ready
# and blocks all DaemonSets. Make nvidia-container-runtime delegate to crun.
# Ensure the crun binary CRI-O uses is resolvable by nvidia-container-runtime,
# then write a PROPER TOML list. Do NOT use `nvidia-ctk config --set` here: it
# stores the value as a single string -> candidate "[[\"crun\"]]" -> not found.
CRUN="$(command -v crun || ls /usr/libexec/crio/crun /usr/sbin/crun 2>/dev/null | head -1)"
[ -n "$CRUN" ] && [ ! -e /usr/bin/crun ] && ln -sf "$CRUN" /usr/bin/crun
if [ -f /etc/nvidia-container-runtime/config.toml ]; then
  sed -i 's|^[[:space:]]*runtimes[[:space:]]*=.*|runtimes = ["crun", "runc"]|' /etc/nvidia-container-runtime/config.toml
fi
echo "  crun -> ${CRUN:-(none)}; $(grep -n runtimes /etc/nvidia-container-runtime/config.toml 2>/dev/null)"
systemctl daemon-reload
systemctl restart crio
sleep 2
systemctl is-active --quiet crio && log "CRI-O active" || die "crio failed to start — run: journalctl -u crio -n 50"

# -----------------------------------------------------------------------------
# 7) kubelet ContainerCheckpoint feature gate (enables the checkpoint API)
# -----------------------------------------------------------------------------
log "7/8  kubelet ContainerCheckpoint feature gate"
mkdir -p /etc/default
if ! grep -q 'ContainerCheckpoint' /etc/default/kubelet 2>/dev/null; then
  if grep -q 'KUBELET_EXTRA_ARGS' /etc/default/kubelet 2>/dev/null; then
    sed -i 's#KUBELET_EXTRA_ARGS="#KUBELET_EXTRA_ARGS="--feature-gates=ContainerCheckpoint=true #' /etc/default/kubelet
  else
    echo 'KUBELET_EXTRA_ARGS="--feature-gates=ContainerCheckpoint=true"' >> /etc/default/kubelet
  fi
fi
systemctl daemon-reload
systemctl restart kubelet
sleep 2
systemctl is-active --quiet kubelet && log "kubelet active" || warn "kubelet not active yet — check: journalctl -u kubelet -n 50"

# -----------------------------------------------------------------------------
# 8) GPU C/R runtime dirs on the boot disk (no relocation needed @300GB).
#    These match the hostPath mounts in deploy/daemonset-crio.yaml + sample pod.
# -----------------------------------------------------------------------------
log "8/9  GPU C/R host directories"
mkdir -p /var/lib/gpu-cr/lib /var/lib/gpu-cr/run \
         /var/lib/gcr-checkpoint /var/lib/kubelet/checkpoints /var/lib/gpu-cr/cuda-req
chmod 0755 /var/lib/gpu-cr /var/lib/gpu-cr/lib /var/lib/gpu-cr/run /var/lib/gcr-checkpoint /var/lib/gpu-cr/cuda-req

# -----------------------------------------------------------------------------
# 9) cuda-checkpoint HOST helper service. The node agent runs in a container
#    where cuda-checkpoint stack-smashes (glibc ABI mismatch). The agent writes
#    requests to /var/lib/gpu-cr/cuda-req and this host service executes
#    cuda-checkpoint natively against the real GPU PID in the container subtree.
# -----------------------------------------------------------------------------
log "9/9  cuda-checkpoint host helper service"
HELPER_SRC="$(dirname "$(readlink -f "$0")")/gpu-cr-cuda-helper.sh"
if [ -f "$HELPER_SRC" ]; then
  install -m 0755 "$HELPER_SRC" /usr/local/bin/gpu-cr-cuda-helper.sh
  cat > /etc/systemd/system/gpu-cr-cuda-helper.service <<UNIT
[Unit]
Description=GPU C/R cuda-checkpoint host helper
After=multi-user.target

[Service]
ExecStart=/usr/local/bin/gpu-cr-cuda-helper.sh
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable --now gpu-cr-cuda-helper.service
  systemctl is-active --quiet gpu-cr-cuda-helper.service \
    && log "cuda helper service active" \
    || warn "cuda helper not active — check: journalctl -u gpu-cr-cuda-helper -n 30"
else
  warn "gpu-cr-cuda-helper.sh not found next to this script; copy it to /usr/local/bin and create the service manually."
fi

log "DONE. Worker is ready. Summary:"
echo "  driver         : $(nvidia-smi --query-gpu=driver_version --format=csv,noheader | head -1)"
echo "  cuda-checkpoint: $(command -v cuda-checkpoint)"
echo "  criu           : $(criu --version 2>/dev/null | head -1)"
echo "  crio           : $(systemctl is-active crio)"
echo "  kubelet gate   : ContainerCheckpoint=true"
echo "  cuda helper    : $(systemctl is-active gpu-cr-cuda-helper 2>/dev/null)"
echo "  dirs           : /var/lib/{gpu-cr,gcr-checkpoint,kubelet/checkpoints}"
echo
echo "Next: from the MASTER, label this node so the agent schedules on it:"
echo "  kubectl label node <this-node> nvidia.com/gpu.present=true --overwrite"
