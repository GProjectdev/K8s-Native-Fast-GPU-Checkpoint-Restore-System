# Quickstart — 새 GCP VM에서 GPU C/R 시스템까지 (클린 셋업)

기존 VM을 모두 지우고 **단일 부팅 디스크 300GB(추가 스토리지 없음)** 로 새로 만든 뒤,
이 문서를 위에서 아래로 그대로 따라가면 GCR 논문 기반 GPU Checkpoint/Restore 시스템이
Kubernetes(CRI-O) 위에서 배포·테스트되는 상태까지 도달합니다.

> 단일 300GB 부팅 디스크를 쓰므로, 기존에 겪던 **별도 데이터 디스크 마운트 / bind 마운트 /
> TMPDIR 재배치 / DiskPressure** 문제는 전부 사라집니다. 모든 경로가 부팅 디스크 위에 있습니다.

---

## 0. 사전 조건 (VM 생성)

- GCP A100 인스턴스 (예: `a2-highgpu-1g`), Ubuntu 22.04 LTS  *(논문 환경: A100-40GB ×2, NVLink, PCIe 4.0 / CUDA 12.6)*
- **부팅 디스크 300GB**, 추가 디스크 **연결하지 않음**
- 마스터 1대 + GPU 워커 N대 (워커에 GPU 부착)
- 방화벽: 노드 간 통신 + (테스트 PC에서) kube-apiserver 6443 허용

---

## 1. Kubernetes 기본 구성 — 외부 Installer 사용

마스터/워커의 K8s(CRI-O) 기본 설치는 이 저장소가 아니라 아래 Installer를 그대로 따릅니다.

```
https://github.com/GProjectdev/Kubernetes_Installer_with_CRIO
```

- **마스터 노드**: 저장소의 마스터 셋업 스크립트 실행 → `kubeadm init` → CNI 적용 →
  `kubeadm join` 명령 확보
- **각 워커 노드**: 워커 셋업 스크립트 실행 → 위 `kubeadm join` 으로 클러스터 합류

여기까지 끝나면 `kubectl get nodes` 가 Ready 로 보여야 합니다. (GPU/CRIU/체크포인트
관련 추가 작업은 이 Installer에는 없으므로 **2단계에서 본 저장소 스크립트로 보강**합니다.)

---

## 2. GPU 워커 보강 — `gpu-worker-setup.sh` (각 GPU 워커에서)

Installer가 끝난 **각 GPU 워커**에서 root로 실행합니다. NVIDIA 드라이버 설치 때문에
**재부팅이 1번 필요**합니다. 스크립트가 알아서 “드라이버 설치 → 재부팅 요청 → 재실행 시 나머지 완료”
흐름으로 동작합니다.

```bash
# 저장소를 워커에 받아오기 (또는 scp 로 quickstart/ 만 복사해도 됨)
git clone https://github.com/GProjectdev/K8s-Native-Fast-GPU-Checkpoint-Restore-System.git
cd K8s-Native-Fast-GPU-Checkpoint-Restore-System

sudo bash quickstart/scripts/gpu-worker-setup.sh   # 1차: 드라이버 설치 후 종료
sudo reboot
# 재부팅 후 다시 접속해서
sudo bash quickstart/scripts/gpu-worker-setup.sh   # 2차: 나머지 전부 완료
```

이 스크립트가 하는 일:

1. **gcc-12** + 커널 헤더 (GCP 6.x 커널과 컴파일러 major를 맞춰 NVIDIA DKMS 빌드 실패 방지)
2. **NVIDIA 드라이버 560 / CUDA 12.6** (GCR 논문 §6 환경과 동일; cuda-checkpoint는 ≥550 필요) → *여기서 재부팅 요청*
3. **cuda-checkpoint** 바이너리 설치 (`/usr/bin/cuda-checkpoint`) + 호스트에서 동작 확인
4. **NVIDIA Container Toolkit** (CRI-O가 `nvidia.com/gpu` 노출)
5. **CRIU + runc** 설치 + CRIU **CUDA 플러그인 유무 점검**(로그 출력)
6. **CRI-O drop-in** — `nvidia`를 기본 런타임으로, `monitor_path`(conmon) 지정 →
   디바이스 플러그인 동작 + crio 정상 기동
7. **kubelet `ContainerCheckpoint` feature gate** 활성화 (체크포인트 API on)
8. **GPU C/R 디렉터리** 생성: `/var/lib/gpu-cr/{lib,run}`, `/var/lib/gcr-checkpoint`,
   `/var/lib/kubelet/checkpoints` (전부 부팅 디스크)

완료 후 드라이버/cuda-checkpoint/criu/crio/feature-gate 요약이 출력됩니다.

---

## 3. 시스템 배포 — `master-deploy.sh` (마스터에서)

모든 GPU 워커에서 2단계가 끝난 뒤, 마스터에서 실행합니다.

```bash
cd K8s-Native-Fast-GPU-Checkpoint-Restore-System
bash quickstart/scripts/master-deploy.sh
```

순서: NVIDIA device plugin 설치 → GPU 노드 라벨(`nvidia.com/gpu.present=true`) →
CRD + RBAC + `gpu-cr-system` 네임스페이스 → **Node Agent DaemonSet(CRI-O 변형)** 적용.

에이전트 이미지는 Docker Hub `docker.io/jeongseungjun/gpu-cr-node-agent:v1.0`
(`imagePullPolicy: Always`) 를 받습니다. 직접 빌드·푸시하려면:

```bash
# (선택) 같은 태그로 재빌드 — Always 라서 노드가 새 코드를 받음
buildah bud -f Dockerfile -t jeongseungjun/gpu-cr-node-agent:v1.0 .
buildah push jeongseungjun/gpu-cr-node-agent:v1.0 \
        docker://docker.io/jeongseungjun/gpu-cr-node-agent:v1.0
kubectl -n gpu-cr-system rollout restart ds/gpu-cr-node-agent
```

확인:

```bash
kubectl get nodes -L nvidia.com/gpu.present
kubectl -n gpu-cr-system get pods -o wide
kubectl -n gpu-cr-system logs -l app.kubernetes.io/name=gpu-cr-node-agent --tail=40
```

---

## 4. 테스트 — GPU Pod 배포 후 체크포인트 요청

GPU 워커에 **GCR 논문과 동일한 vLLM 0.9.1 Pod**(드라이버 560 / CUDA 12.6)를 띄웁니다.
작은 대표 모델(OPT-125M)을 서빙해 빠르게 로드되며 실제 GPU 데이터 버퍼를 잡습니다.
인터셉터를 LD_PRELOAD 합니다. (Llama/Qwen 등으로 `--model`만 바꾸면 됩니다.)

```bash
kubectl apply -f deploy/sample-pod-vllm.yaml
kubectl get pod vllm-gcr-pod -o wide -w     # Running 확인
kubectl logs vllm-gcr-pod | tail            # 모델 로드 + "[gcr] interceptor loaded" 확인
```

> 드라이버를 560으로 못 올리는 환경이면, PyTorch(CUDA 12.1) 대체 Pod를 쓰세요:
> `kubectl apply -f deploy/sample-pod-pytorch.yaml` (이때는 드라이버 550로 충분).

체크포인트 요청 = **GPUCheckpoint CR** 생성. 워커의 Node Agent가 자신의 노드 Pod만
감지해 파이프라인을 수행합니다.

```bash
kubectl apply -f deploy/sample-gpucheckpoint-vllm.yaml      # PyTorch면 -pytorch.yaml
kubectl get gpucheckpoints -w               # Phase: Checkpointing -> Completed
kubectl -n gpu-cr-system logs -l app.kubernetes.io/name=gpu-cr-node-agent --tail=80 -f
```

파이프라인 5단계 (코드 `internal/agent/checkpoint.go`):

1. **Selective Interception 데이터버퍼 체크포인트** — 에이전트가 Pod 내 인터셉터에
   신호 → 인터셉터가 추적 중인 GPU 버퍼 목록(intercepted-info) 덤프 + ACK
2. **제어상태 체크포인트 (cuda-checkpoint)** — 컨테이너 PID에 대해 CUDA suspend
3. **컨테이너 체크포인트 (kubelet checkpoint API → CRI-O/CRIU)** — CPU측 프로세스 스냅샷
4. **저장** — CR `.spec.storage.path`(`/var/lib/gcr-checkpoint`)로 아카이브 복사
5. **재개** — cuda-checkpoint resume (주기적 체크포인트가 잡을 죽이지 않도록)

성공 시 `/var/lib/gcr-checkpoint/` 에 `checkpoint-*.tar` 가 생기고 CR이 `Completed` 가 됩니다.

```bash
ls -lh /var/lib/gcr-checkpoint/            # 워커에서
kubectl get gpucheckpoint ckpt-cuda-001 -o yaml | yq '.status'
```

---

## 5. GPU 체크포인트 메커니즘에 관한 주의 (현재 전선)

GPU 컨테이너의 **2~3단계**는 환경에 따라 두 갈래입니다.

- **A. cuda-checkpoint(2단계) + CRIU(3단계):** 에이전트가 `cuda-checkpoint --toggle`
  로 GPU 상태를 먼저 호스트로 내린 뒤, CRIU는 GPU가 빠진 CPU 프로세스만 덤프.
  에이전트는 `CUDA_CHECKPOINT_NSENTER=true`로 **호스트 네임스페이스에서** cuda-checkpoint를
  실행합니다(컨테이너 안에서 직접 실행하면 깨지는 사례가 있어 nsenter 사용).
- **B. CRIU에 위임(CRIUgpu):** CRIU가 GPU까지 직접 덤프. 이 경우 **CRIU CUDA 플러그인**이
  필요하며, 에이전트는 `CUDA_CHECKPOINT_SKIP=true`(2단계 생략)로 둡니다.

`gpu-worker-setup.sh`의 5단계가 CRIU CUDA 플러그인 유무를 출력합니다.
플러그인이 없고 cuda-checkpoint도 컨테이너 경로에서 불안정하면, A 경로의 nsenter 방식
(현재 daemonset-crio.yaml 기본값)으로 진행하세요. 두 옵션 토글:

```bash
# B 경로(CRIU 위임)로 전환해 보려면:
kubectl -n gpu-cr-system set env ds/gpu-cr-node-agent CUDA_CHECKPOINT_SKIP=true
# 1단계 인터셉션을 잠시 분리해 순수 CRIU 경로만 검증하려면:
kubectl -n gpu-cr-system set env ds/gpu-cr-node-agent GCR_INTERCEPTION=false
kubectl -n gpu-cr-system rollout restart ds/gpu-cr-node-agent
```

---

## 부록 — 문제 해결 빠른 표

| 증상 | 원인 / 조치 |
|---|---|
| DKMS 드라이버 빌드 실패 | gcc major 불일치 → 스크립트가 gcc-12 강제 설정 (1단계) |
| `nvidia-smi` 안 됨 | 드라이버 설치 후 **재부팅** 필요 → 스크립트 재실행 |
| `cuda-checkpoint: command not found` | 3단계가 NVIDIA repo에서 설치 |
| Pod `Insufficient nvidia.com/gpu` | device plugin 미설치/라벨 누락 → `master-deploy.sh` |
| crio 기동 실패(conmon) | nvidia 런타임 drop-in에 `monitor_path` 필요 (6단계가 설정) |
| `libcuda.so.1 not found` (에이전트) | 호스트 `/usr/lib/x86_64-linux-gnu` 마운트 + `LD_LIBRARY_PATH` (daemonset-crio.yaml에 반영됨) |
| checkpoint API 404/disabled | kubelet `ContainerCheckpoint` 게이트 (7단계) |
| `kubectl get nodes` NotReady | Installer의 CNI 적용 확인 |
| DaemonSet DESIRED=0 (device plugin/node-agent Pod 안 뜸) | 워커에서 nvidia-container-runtime가 crun 미위임 → 컨테이너 실패 → Cilium not ready → `node.cilium.io/agent-not-ready` 테인트. `nvidia-ctk config --in-place --set nvidia-container-runtime.runtimes='["crun","runc"]'` 후 `systemctl restart crio kubelet` |
| 워커 `cri-o://Unknown` / Cilium 1개만 Ready | 위와 동일 원인(crun 위임 누락). 수정 후 워커 Cilium Ready·테인트 해제 확인 |
| 워커 전 Pod `CreateContainerError` + `no runtime binary found from candidate list: [["crun"]]` | `nvidia-ctk config --set`이 runtimes를 문자열로 깨뜨림. config.toml을 직접 `runtimes = ["crun", "runc"]`로 수정하고, crun이 PATH에 없으면 `ln -sf $(ls /usr/libexec/crio/crun) /usr/bin/crun` 후 `systemctl restart crio kubelet` |

자세한 검증 로그/배경은 `docs/SETUP.ko.md` 참고.
