# 설치 가이드 — Master & Worker 노드

**K8s-Native Fast GPU Checkpoint/Restore System**을 실행·테스트하기 위한 단계별
준비 가이드입니다. 순서대로 따라 하세요. 명령어는 **Ubuntu 22.04 LTS**,
**Kubernetes v1.30 (kubeadm)**, **containerd** 기준입니다.

> 🇺🇸 English: [`SETUP.md`](SETUP.md)

## 구성도

```
            ┌──────────────────────┐
            │   Master Node        │   컨트롤 플레인 (GPU 불필요)
            │   - kube-apiserver    │   ContainerCheckpoint feature gate
            │   - controller/sched  │   device-plugin + 본 시스템 배포
            └──────────┬───────────┘
                       │ join
        ┌──────────────┼──────────────┐
        ▼                              ▼
┌──────────────────┐          ┌──────────────────┐
│  Worker Node 1   │   ...    │  Worker Node N   │   GPU 노드
│  NVIDIA 드라이버550│          │  NVIDIA 드라이버550│   CRIU + cuda-checkpoint
│  containerd+nvidia│          │  containerd+nvidia│   GCR hook 드라이버 배치
│  GPU C/R Node Agent (DaemonSet가 여기서 동작)     │
└──────────────────┘          └──────────────────┘
```

## 버전 매트릭스 (목표 기준)

| 구성요소 | 버전 |
|----------|------|
| OS | Ubuntu 22.04 LTS |
| Kubernetes | v1.30.x (kubeadm/kubelet/kubectl) |
| 컨테이너 런타임 | containerd ≥ 1.7 |
| NVIDIA 드라이버 | ≥ 550 (`cuda-checkpoint` 포함) |
| CRIU | ≥ 3.17 |
| NVIDIA Container Toolkit | ≥ 1.14 |

---

# Part A — 공통 베이스 (모든 노드: master + worker)

Part A는 **모든** 노드에서 실행합니다.

### A-1. 호스트 사전 설정

```bash
# root 진입
sudo -i

# 노드마다 고유 호스트네임 설정 (예시)
# hostnamectl set-hostname master       # master에서
# hostnamectl set-hostname worker-1     # 각 worker에서

# 스왑 비활성화 (kubelet 필수)
swapoff -a
sed -i '/ swap / s/^/#/' /etc/fstab
```

### A-2. 커널 모듈 & sysctl

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

### A-3. containerd 설치

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

# 기본 설정 생성 + systemd cgroup 활성화 (kubeadm 필수)
mkdir -p /etc/containerd
containerd config default | tee /etc/containerd/config.toml >/dev/null
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
systemctl restart containerd
systemctl enable containerd
```

### A-4. kubeadm, kubelet, kubectl 설치 (v1.30)

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

### A-5. kubelet에 `ContainerCheckpoint` feature gate 활성화

체크포인트를 수행하는 모든 노드(모든 worker)에 필요합니다(master에 있어도 무해).

```bash
mkdir -p /etc/systemd/system/kubelet.service.d
cat <<EOF | tee /etc/systemd/system/kubelet.service.d/20-checkpoint.conf
[Service]
Environment="KUBELET_EXTRA_ARGS=--feature-gates=ContainerCheckpoint=true"
EOF
systemctl daemon-reload
# 노드 초기화/조인 후 kubelet이 자동 재시작됩니다.
```

---

# Part B — Master 노드 전용

Part B는 **master에서만** 실행합니다.

### B-1. 컨트롤 플레인 초기화 (apiserver에 feature gate 포함)

```bash
# CNI에 맞는 pod 네트워크 CIDR 선택 (아래는 Calico 기본값)
kubeadm init \
  --pod-network-cidr=192.168.0.0/16 \
  --feature-gates=ContainerCheckpoint=true \
  --apiserver-extra-args feature-gates=ContainerCheckpoint=true
```

> kubeadm 버전이 `--apiserver-extra-args`를 거부하면 설정 파일 방식을 쓰세요
> ([B-1b](#b-1b-대안-kubeadm-설정-파일)).

### B-2. 사용자용 kubectl 설정

```bash
# 일반(비root) 사용자로:
mkdir -p $HOME/.kube
sudo cp -i /etc/kubernetes/admin.conf $HOME/.kube/config
sudo chown $(id -u):$(id -g) $HOME/.kube/config
kubectl get nodes        # master 표시 (CNI 설치 전까지 NotReady)
```

### B-3. CNI 설치 (Calico 예시)

```bash
kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.28.0/manifests/calico.yaml
kubectl get pods -n kube-system -w     # calico + coredns Running 까지 대기
```

### B-4. 조인 명령 저장 (worker용)

```bash
kubeadm token create --print-join-command
# 출력된 `kubeadm join ...` 줄을 복사 — 각 worker에서 실행 (Part C-7)
```

### B-5. NVIDIA device plugin 설치 (클러스터 전역)

```bash
kubectl apply -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.15.0/deployments/static/nvidia-device-plugin.yml
# 권장: GPU Feature Discovery로 노드 자동 라벨링
#   https://github.com/NVIDIA/gpu-feature-discovery
```

GPU 노드에 `nvidia.com/gpu.present=true` 라벨이 자동으로 안 붙으면 수동 라벨링
(DaemonSet의 nodeSelector가 사용):

```bash
kubectl label node <worker-name> nvidia.com/gpu.present=true
```

### B-6. GPU C/R Node Agent 이미지 빌드 & 푸시 (Buildah)

DaemonSet은 컨테이너 이미지를 실행하므로, 배포 전에 이미지를 빌드·게시해야
합니다. Go 1.22+, gcc/make, Buildah가 있는 **빌드 호스트**(master 또는 저장소에
접근 가능한 워크스테이션)에서 실행하세요.

```bash
# 빌드 도구 설치 (Ubuntu)
apt-get install -y golang-go gcc make buildah

# 저장소 루트에서: 에이전트 + 인터셉터 shim을 한 이미지로 빌드
buildah bud -f Dockerfile -t ghcr.io/gprojectdev/gpu-cr-node-agent:latest .

# 노드가 pull 가능한 레지스트리로 푸시
buildah login ghcr.io                         # 사용자명 + PAT/토큰
buildah push ghcr.io/gprojectdev/gpu-cr-node-agent:latest \
  docker://ghcr.io/gprojectdev/gpu-cr-node-agent:latest
```

> 이미지 이름은 `deploy/daemonset.yaml`의 `image:`와 일치해야 합니다. 다른
> 레지스트리/이름을 쓰면 해당 필드를 수정하세요.

**에어갭 / 레지스트리 없음?** 푸시 대신 OCI 아카이브로 내보내 각 GPU worker의
containerd에 import 하세요:

```bash
# 빌드 호스트에서
buildah push ghcr.io/gprojectdev/gpu-cr-node-agent:latest \
  oci-archive:/tmp/gpu-cr-node-agent.tar:ghcr.io/gprojectdev/gpu-cr-node-agent:latest
scp /tmp/gpu-cr-node-agent.tar <worker>:/tmp/

# 각 GPU worker에서 — containerd가 쓰는 k8s.io 네임스페이스로 import
ctr -n k8s.io images import /tmp/gpu-cr-node-agent.tar
# 이후 imagePullPolicy: IfNotPresent (DaemonSet 기본값) 사용.
```

### B-7. 본 시스템 배포 (worker 조인 + 이미지 게시 후)

```bash
# 저장소 루트에서, master에서:
kubectl apply -f config/crd/gpu-cr.io_gpucheckpoints.yaml
kubectl apply -f deploy/rbac.yaml
kubectl label ns gpu-cr-system pod-security.kubernetes.io/enforce=privileged --overwrite
kubectl apply -f deploy/daemonset.yaml
kubectl -n gpu-cr-system get pods -o wide     # GPU 노드마다 node-agent 1개
```

### B-1b. (대안) kubeadm 설정 파일

extra-args 플래그가 안 되면 B-1 대신:

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

# Part C — Worker 노드 전용 (GPU 노드)

Part C는 **각 GPU worker**에서 실행합니다. (이 노드에 Part A가 이미 완료돼 있어야 함)

### C-1. NVIDIA 드라이버 설치 (≥ 550)

```bash
# Ubuntu 22.04용 CUDA/NVIDIA 저장소 추가
wget https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2204/x86_64/cuda-keyring_1.1-1_all.deb
dpkg -i cuda-keyring_1.1-1_all.deb
apt-get update

# 550 이상 드라이버 브랜치 설치 (예: 550 또는 560)
apt-get install -y nvidia-driver-550
reboot
```

재부팅 후 드라이버 **와** `cuda-checkpoint` 바이너리 확인:

```bash
nvidia-smi                       # 드라이버 550.x+
which cuda-checkpoint            # /usr/bin/cuda-checkpoint
cuda-checkpoint --help
```

### C-2. NVIDIA Container Toolkit 설치

```bash
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
  | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
  | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
  | tee /etc/apt/sources.list.d/nvidia-container-toolkit.list
apt-get update
apt-get install -y nvidia-container-toolkit

# containerd에 NVIDIA 런타임 연동 + 기본 런타임으로 설정
nvidia-ctk runtime configure --runtime=containerd --set-as-default
systemctl restart containerd
```

### C-3. CRIU 설치 (≥ 3.17)

```bash
apt-get install -y criu
criu --version
criu check                       # "Looks good." 기대
```

> 배포판 CRIU가 3.17보다 낮으면 소스 빌드: <https://criu.org/Installation>

### C-4. 인터셉터 라이브러리 / 런타임 디렉토리 준비

```bash
mkdir -p /var/lib/gpu-cr/lib /var/lib/gpu-cr/run /var/lib/gcr-checkpoint
# Node Agent가 시작 시 libgcr-interceptor.so를 여기 설치합니다.
```

### C-5. GCR hook 드라이버(`libcuda.so`) 빌드 & 배치

선택적 `cuMem*` 인터셉션 드라이버는 업스트림에서 빌드:

```bash
git clone https://github.com/thustorage/GCR.git
cd GCR/GCR
bash build.sh                    # libcuda.so 생성 (업스트림 README 참고)
cp libcuda.so /var/lib/gpu-cr/lib/libcuda.so
```

> 이 파일이 없으면 인터셉터 shim이 실제 드라이버로 폴백하여 GPU 측 체크포인트가
> 동작하지 않습니다. (dry-run으로 오케스트레이션은 테스트 가능)

### C-6. kubelet feature gate 확인 (A-5에서 설정)

```bash
cat /etc/systemd/system/kubelet.service.d/20-checkpoint.conf   # ContainerCheckpoint=true 포함돼야 함
```

### C-7. 클러스터 조인

```bash
# B-4에서 master가 출력한 join 명령 붙여넣기:
kubeadm join <MASTER_IP>:6443 --token <token> \
  --discovery-token-ca-cert-hash sha256:<hash>

# 조인 후 kubelet이 feature gate를 반영하도록:
systemctl restart kubelet
```

### C-8. master에서 확인

```bash
kubectl get nodes -o wide                          # worker Ready
kubectl describe node <worker> | grep nvidia.com/gpu   # nvidia.com/gpu: 1
```

---

# Part D — 엔드투엔드 스모크 테스트

모두 올라온 뒤 master에서:

```bash
# 1. GCR 인터셉션이 연결된 GPU 워크로드 실행
kubectl apply -f deploy/sample-pod.yaml

# 2. deploy/sample-gpucheckpoint.yaml의 podRef.