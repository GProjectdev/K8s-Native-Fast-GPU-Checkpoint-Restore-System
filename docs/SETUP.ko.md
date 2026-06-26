# 설치 & 사용 — 검증된 엔드투엔드 (CRI-O)

새 VM에서 시작해 실제로 GPU 체크포인트가 동작할 때까지의 **검증된** 가이드입니다.
그동안 실제로 겪은 모든 함정을 반영했습니다. 대상 스택:

| 구성요소 | 버전 |
|----------|------|
| OS | Ubuntu 22.04 LTS |
| Kubernetes | v1.33 (kubeadm) |
| 컨테이너 런타임 | **CRI-O v1.33** (소켓 `/run/crio/crio.sock`) |
| CNI | Cilium |
| NVIDIA 드라이버 | **550** (CUDA 12.4) — 2.1/2.2 참고 |
| CRIU | `ppa:criu/ppa` |
| NVIDIA Container Toolkit | ≥ 1.19 |

> 🇺🇸 English: [`SETUP.md`](SETUP.md)

구성: **마스터 1대(GPU 불필요) + GPU 워커 N대**. 기본 클러스터는
[GProjectdev/Kubernetes_Installer_with_CRIO](https://github.com/GProjectdev/Kubernetes_Installer_with_CRIO)
(`k8s-masternode-setup.sh`, `k8s-workernode-setup.sh`)로 구성합니다. 아래는 그
설치 **이후**에 하는 작업입니다.

---

## 1. 컨테이너 체크포인트 feature gate 활성화 (체크포인트하는 모든 노드)

```bash
echo 'KUBELET_EXTRA_ARGS="--feature-gates=ContainerCheckpoint=true"' | sudo tee /etc/default/kubelet
sudo systemctl daemon-reload && sudo systemctl restart kubelet
```

확인 (실제 GPU 워커 이름으로; `405`/method-not-allowed = 정상):

```bash
kubectl get --raw /api/v1/nodes/<gpu-worker>/proxy/checkpoint/ ; echo
```

---

## 2. GPU 워커 준비 (각 GPU 워커에서 root로)

### 2.1 NVIDIA 드라이버 550 — GCC 12를 먼저 설치 (필수)

DKMS 커널 모듈은 **커널을 빌드한 GCC와 같은 버전**으로 컴파일돼야 합니다.
Ubuntu 22.04의 6.8 클라우드 커널(예: GCP)은 **GCC 12**로 빌드됐는데 기본 `gcc`가
11이라 DKMS가 `unrecognized command-line option '-ftrivial-auto-var-init=zero'`로
실패합니다.

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

재부팅 후:

```bash
sudo dkms status     # nvidia/550.x, <kernel>: installed  ("added" 아님)
nvidia-smi           # GPU + 드라이버 550.x
```

`added`만 뜨거나 "couldn't communicate"면 모듈 미빌드 → gcc 고친 뒤
`sudo dkms install nvidia/<ver> -k $(uname -r) --force`.

### 2.2 cuda-checkpoint 바이너리 (apt로는 PATH에 없음)

```bash
git clone https://github.com/NVIDIA/cuda-checkpoint.git
sudo install -m 0755 cuda-checkpoint/bin/x86_64_Linux/cuda-checkpoint /usr/bin/cuda-checkpoint
cuda-checkpoint --help
```

### 2.3 NVIDIA Container Toolkit + CRIU + CRI-O 드롭인 하나

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

**CRI-O 드롭인 하나**로 세 가지를 동시에 처리합니다:
- **nvidia 런타임을 기본 런타임으로** (그래야 NVIDIA device plugin이 GPU를 인식해
  `nvidia.com/gpu`를 광고),
- **runc** 런타임 정의,
- 둘 다 `monitor_path` 지정 (없으면 crio 1.33이
  `failed to translate monitor fields for runtime nvidia: "conmon" not found`로 죽음).

CRIU 체크포인트는 `criu`만 있으면 자동 활성화됩니다 — 제거된 `enable_criu_support`
키는 넣지 마세요(crio가 죽습니다).

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
sudo rm -f /etc/crio/crio.conf.d/99-nvidia.toml
sudo systemctl restart crio
sudo systemctl is-active crio        # active
```

### 2.4 스토리지 — 컨테이너 저장소 + pull 임시물을 큰 디스크로

클라우드 VM은 작은(~10GB) 루트 + 큰 **미마운트** 데이터 디스크인 경우가 많아,
CRI-O 이미지 저장소가 루트를 채워 `DiskPressure`로 evict되고 pull이
`/var/tmp ... no space left on device`로 실패합니다.

```bash
lsblk ; df -h /                          # 미마운트 큰 디스크 확인 (예: /dev/sdb)
sudo systemctl stop kubelet crio
sudo umount -R /var/lib/containers/storage/overlay 2>/dev/null || true
sudo blkid /dev/sdb                      # mkfs 전 비어있는지 (출력 없음)
sudo mkfs.ext4 -F /dev/sdb
sudo mv /var/lib/containers /var/lib/containers.bak 2>/dev/null || true
sudo mkdir -p /var/lib/containers
echo '/dev/sdb /var/lib/containers ext4 defaults,nofail 0 2' | sudo tee -a /etc/fstab
sudo mount /var/lib/containers
sudo rm -rf /var/lib/containers.bak      # 루트 공간 회수 (DiskPressure 해소 핵심)

# pull 임시물(/var/tmp)도 루트에 있음 → 큰 디스크로 bind
sudo mkdir -p /var/lib/containers/vartmp
grep -q '/var/lib/containers/vartmp /var/tmp ' /etc/fstab || \
  echo '/var/lib/containers/vartmp /var/tmp none bind 0 0' | sudo tee -a /etc/fstab
sudo mount /var/tmp
sudo systemctl start crio kubelet
df -h / /var/lib/containers /var/tmp
```

> **체크포인트 출력 디렉토리도 큰 디스크로 옮기세요** — 인터셉터는 GPU 데이터버퍼
> 복사본을 `/var/lib/gcr-checkpoint`에, CRIU는 컨테이너 tar를
> `/var/lib/kubelet/checkpoints`에 씁니다. 둘 다 기본은 작은 루트라 큰 워크로드에서
> 루트를 다시 채워 DiskPressure를 일으킵니다:
>
> ```bash
> sudo mkdir -p /var/lib/containers/gcr-checkpoint /var/lib/containers/kubelet-checkpoints
> sudo mkdir -p /var/lib/gcr-checkpoint /var/lib/kubelet/checkpoints
> echo '/var/lib/containers/gcr-checkpoint /var/lib/gcr-checkpoint none bind 0 0' | sudo tee -a /etc/fstab
> echo '/var/lib/containers/kubelet-checkpoints /var/lib/kubelet/checkpoints none bind 0 0' | sudo tee -a /etc/fstab
> sudo mount /var/lib/gcr-checkpoint ; sudo mount /var/lib/kubelet/checkpoints
> ```

### 2.5 GPU C/R 런타임 디렉토리

```bash
sudo mkdir -p /var/lib/gpu-cr/lib /var/lib/gpu-cr/run /var/lib/gcr-checkpoint
```

---

## 3. 마스터: device plugin, 라벨, 시스템 배포

```bash
kubectl apply -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.15.0/deployments/static/nvidia-device-plugin.yml
kubectl label node <gpu-worker> nvidia.com/gpu.present=true --overwrite
kubectl -n kube-system rollout restart ds/nvidia-device-plugin-daemonset
kubectl get node <gpu-worker> -o jsonpath='{.status.capacity.nvidia\.com/gpu}{"\n"}'   # -> 1
```

---

## 4. 에이전트 이미지 빌드·게시 (Go 1.22+, gcc/make, buildah 있는 빌드 호스트)

```bash
cd K8s-Native-Fast-GPU-Checkpoint-Restore-System
buildah bud -f Dockerfile -t docker.io/<you>/gpu-cr-node-agent:v1.2 .
buildah login docker.io
buildah push docker.io/<you>/gpu-cr-node-agent:v1.2 docker://docker.io/<you>/gpu-cr-node-agent:v1.2
```

> 인터셉터/에이전트 코드가 바뀌면 **새 태그로 재빌드**하세요. 에이전트는 시작 시
> 자신에 포함된 `libgcr-interceptor.so`를 노드에 설치하므로, 옛 이미지 = 옛 인터셉터.

---

## 5. 시스템 배포 (마스터)

```bash
kubectl apply -f config/crd/gpu-cr.io_gpucheckpoints.yaml
kubectl apply -f deploy/rbac.yaml
kubectl label ns gpu-cr-system pod-security.kubernetes.io/enforce=privileged --overwrite

# deploy/daemonset-crio.yaml 의 image를 docker.io/<you>/gpu-cr-node-agent:v1.2 로 수정
kubectl apply -f deploy/daemonset-crio.yaml
kubectl -n gpu-cr-system get po -o wide        # GPU 노드마다 1/1 Running
kubectl -n gpu-cr-system logs ds/gpu-cr-node-agent --tail=20
```

---

## 6. GPU 워크로드 실행 + 체크포인트 요청

[`deploy/sample-pod.yaml`](../deploy/sample-pod.yaml)을 사용하세요 — 드라이버 550
호환 PyTorch 워크로드 **와 인터셉터가 필요로 하는 제어채널 연결**(`GCR_POD_UID`,
`GCR_CONTROL_DIR`, `/var/lib/gpu-cr/run` 마운트)이 포함돼 있습니다. 이것이 없는 Pod
매니페스트(예: 옛 `sample.yaml`)를 쓰면 인터셉터가 ACK하지 못해 step 1에서
타임아웃됩니다.

```bash
kubectl apply -f deploy/sample-pod.yaml
kubectl get po vllm-gcr-pod -o wide
kubectl logs vllm-gcr-pod | grep -E 'GPU tensor allocated|\[gcr\]'
#   GPU tensor allocated ...
#   [gcr] interceptor loaded (pid=...): watching /var/lib/gpu-cr/run/<uid>/control
```

체크포인트 CR([`deploy/sample-gpucheckpoint.yaml`](../deploy/sample-gpucheckpoint.yaml));
`container`는 Pod 컨테이너명(`cuda-app`)과 일치, `period: "000000"` = 1회:

```bash
kubectl apply -f deploy/sample-gpucheckpoint.yaml
kubectl get gpucheckpoints -w        # Checkpointing -> Completed
kubectl -n gpu-cr-system logs -l app.kubernetes.io/name=gpu-cr-node-agent --tail=80 -f
```

기대 파이프라인(에이전트+Pod 로그):
- agent `checkpoint start` → pod `[gcr] checkpoint signal received` → `intercepted-info dumped` → `ACK sent`
- agent `step 1/5 done` → `step 2/5 cuda-checkpoint` → `step 3/5 kubelet produced` → `step 4/5 stored`

---

## 7. 결과 검증

```bash
kubectl describe gpucheckpoint ckpt-vllm-001 | tail
ls -lh /var/lib/gcr-checkpoint/                    # checkpoint-*.tar (워커)
cat /var/lib/gpu-cr/run/*/intercepted-info         # 가로챈 GPU 버퍼
```

---

## 8. 트러블슈팅 (겪은 것 전부)

| 증상 | 원인 / 해결 |
|------|-------------|
| `nvidia-smi` "couldn't communicate" / `dkms status` = `added` | DKMS 미빌드. GCC 불일치 — gcc-12 설치/기본설정 후 `dkms install ... --force` (2.1). |
| `unrecognized option '-ftrivial-auto-var-init=zero'` | 기본 gcc가 커널 빌드 GCC보다 낮음 → gcc-12 (2.1). |
| `cuda-checkpoint: command not found` | `NVIDIA/cuda-checkpoint` prebuilt 설치 (2.2). |
| crio 시작 실패 `... runtime nvidia: "conmon" not found` | nvidia 런타임에 `monitor_path` 누락 → 2.3의 99-gpu-cr.conf 사용. |
| `crio.conf.d` 편집 후 crio 시작 실패 | 모르는 키(`enable_criu_support`)나 정의 없는 런타임 → `journalctl -xeu crio | tail`; 2.3 드롭인. |
| `0/N nodes ... Insufficient nvidia.com/gpu` | device plugin이 GPU 못 봄 → nvidia를 CRI-O 기본 런타임으로 (2.3) + device plugin 재시작 (3). |
| agent Pod `hostPath ...containerd.sock is not a socket` | 노드가 CRI-O → `deploy/daemonset-crio.yaml`(소켓 디렉토리 `/run/crio`). |
| agent CrashLoop `listen :8081: address already in use` | hostNetwork 포트 충돌 → metrics/health :9290/:9291 (daemonset-crio 기본). |
| crictl이 기본 엔드포인트 3개 시도 / `connection refused` | `CONTAINER_RUNTIME_ENDPOINT` 미설정 또는 소켓 "파일" 마운트 → daemonset-crio는 `/run/crio` 디렉토리 마운트 + 엔드포인트 설정. |
| Pod `Evicted: DiskPressure` / pull `no space left on device` (`/var/tmp`) | 루트 디스크 가득 → 데이터 디스크를 `/var/lib/containers`에 마운트 + `/var/tmp` bind (2.4). |
| vllm 엔진 init 실패 "driver too old / pytorch.org" | 이미지 CUDA가 드라이버 550보다 최신 → CUDA ≤12.4 이미지 사용(sample-pod는 pytorch cu121). |
| `data-buffer checkpoint did not ack: timeout ... /run/<uid>/control` | Pod에 제어채널 연결 없음 또는 옛 에이전트 이미지 → `deploy/sample-pod.yaml`(UID+`/var/lib/gpu-cr/run` 마운트) + 현재 코드로 빌드한 에이전트 이미지 사용. |
| `resolve container pid: no running container <name>` | CR `podRef.container`가 Pod 컨테이너명과 불일치. |
