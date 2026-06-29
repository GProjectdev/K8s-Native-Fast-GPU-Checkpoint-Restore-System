# Setup & Usage — VM 생성 이후 따라만 하면 동작 (검증 완료)

이 문서는 **GPU VM을 새로 만든 직후**부터, GCR 방식의 GPU Checkpoint 시스템이
Kubernetes(CRI-O)에서 실제로 동작하는 상태까지를 그대로 따라가는 가이드입니다.
실제 클러스터(K8s v1.33 + CRI-O + Cilium + NVIDIA A100, GCP)에서 end-to-end로 검증했습니다.

## 0. 무엇이 되는가 (검증된 동작)

`GPUCheckpoint` CR을 만들면, 해당 Pod가 뜬 노드의 **Node Agent**가 다음을 수행합니다.

1. **데이터(GCR 데이터 엔진)** — Pod에 LD_PRELOAD된 인터셉터가 `cudaMalloc`을 **VMM으로
   백킹**(직접 소유)하고, 체크포인트 시 데이터 버퍼를 **host로 복사 + 물리메모리만 해제
   (가상주소 보존)**. 프레임워크 무관(PyTorch expandable_segments 불필요).
2. **제어** — `cuda-checkpoint`(호스트 헬퍼 경유)로 control state 처리.
3. **CPU+데이터** — kubelet checkpoint API → CRI-O → **CRIU(CPU만)** 가 프로세스(+host에
   올라온 GPU 데이터)를 덤프 → tar 생성.
4. **비파괴적 resume** — cuda-checkpoint resume + 같은 VA로 remap + H2D → 소스 계속 실행.

> 전제: **NVIDIA 드라이버 570+** (550/560은 cuda-checkpoint가 /dev/nvidia* fd를 안 닫아
> CRIU 실패). 단일 **300GB 부팅 디스크**면 디스크 곡예가 전부 불필요합니다.

---

## 1. VM 생성

- GCP A100 인스턴스(예: `a2-highgpu-1g`), Ubuntu 22.04 LTS
- **부팅 디스크 300GB**, 추가 디스크 연결 안 함
- 마스터 1 + GPU 워커 N

## 2. Kubernetes 기본 (외부 Installer)

마스터/워커 K8s(CRI-O)는 아래를 그대로 따릅니다.
`https://github.com/GProjectdev/Kubernetes_Installer_with_CRIO`
마스터 셋업 → `kubeadm init` → CNI → 각 워커 `kubeadm join`. `kubectl get nodes` Ready 확인.

## 3. GPU 워커 준비 — 스크립트 한 방 (각 GPU 워커, root)

드라이버 설치 때문에 **재부팅 1회**가 필요합니다. 스크립트가 알아서 멈췄다 재실행하면 됩니다.

```bash
git clone https://github.com/GProjectdev/K8s-Native-Fast-GPU-Checkpoint-Restore-System.git
cd K8s-Native-Fast-GPU-Checkpoint-Restore-System
sudo bash quickstart/scripts/gpu-worker-setup.sh   # 드라이버 570 설치 후 종료
sudo reboot
sudo bash quickstart/scripts/gpu-worker-setup.sh   # 나머지 전부 완료
```

스크립트가 하는 일: gcc-12, **드라이버 570**, cuda-checkpoint 바이너리, NVIDIA Container
Toolkit, CRIU+runc, **crun 위임 보정**(nvidia-container-runtime), CRI-O drop-in(nvidia 기본 +
monitor_path), kubelet `ContainerCheckpoint` 게이트, GPU C/R 디렉터리, **cuda-checkpoint 호스트
헬퍼 서비스**, 그리고 **CRIU cuda_plugin 비활성화**(CRIU가 GPU를 안 건드리게 = GCR).

## 4. 마스터: 배포

```bash
cd K8s-Native-Fast-GPU-Checkpoint-Restore-System
bash quickstart/scripts/master-deploy.sh
```

device plugin 설치 → GPU 노드 라벨 → CRD + RBAC + `gpu-cr-system` ns + Node Agent DaemonSet.

**에이전트 동작 모드 설정(전체 GCR):**

```bash
kubectl -n gpu-cr-system set env ds/gpu-cr-node-agent GCR_INTERCEPTION=true CUDA_CHECKPOINT_SKIP=false
kubectl -n gpu-cr-system rollout restart ds/gpu-cr-node-agent
```

## 5. 에이전트 이미지 빌드 (build host: Go 1.22+, gcc/make, buildah)

이미지 하나에 **에이전트 + 인터셉터(.so)** 가 함께 빌드됩니다. 같은 태그로 재빌드 시
DaemonSet은 `imagePullPolicy: Always` 라서 새 코드를 받습니다.

```bash
buildah bud -f Dockerfile -t jeongseungjun/gpu-cr-node-agent:v1.0 .
buildah push jeongseungjun/gpu-cr-node-agent:v1.0 docker://docker.io/jeongseungjun/gpu-cr-node-agent:v1.0
kubectl -n gpu-cr-system rollout restart ds/gpu-cr-node-agent
kubectl -n gpu-cr-system rollout status ds/gpu-cr-node-agent
```

## 6. GPU Pod 실행 + 체크포인트

샘플 `deploy/sample-pod-l1.yaml` 은 PyTorch에 **`GCR_VMM_ALLOC=1`**(우리 VMM allocator) +
인터셉터 LD_PRELOAD가 걸려 있고, 4GB 텐서를 잡고 체크섬을 주기적으로 출력합니다.

```bash
kubectl apply -f deploy/sample-pod-l1.yaml
kubectl get pod cuda-l1-pod -o wide -w        # Running
kubectl logs cuda-l1-pod | grep '\[gcr\]'      # [gcr][vmm-alloc] req=4294967296 ... 확인
```

체크포인트 요청(= `GPUCheckpoint` CR 생성). **`checksum ... OK` 직후**에 적용하세요.

```bash
kubectl apply -f deploy/sample-gpucheckpoint-l1.yaml
kubectl get gpucheckpoints.gpu-cr.io -w        # Checkpointing -> Completed
```

## 7. 검증

```bash
# 인터셉터: 데이터 엔진 동작
kubectl logs cuda-l1-pod | grep '\[gcr\]\[engine\]'
#  [gcr][engine] freeze: 2 segs, ~4GB copied to host, physical released (VA kept); 0 failed
#  [gcr][engine] remap: 2 segs restored to same VA + H2D; 0 failed

# 데이터 정합성: remap 이후의 체크섬이 그대로면 통과
kubectl logs cuda-l1-pod | grep checksum | tail

# 워커: 아티팩트
ls -lh /var/lib/gcr-checkpoint/                # checkpoint-*.tar
```

**통과 기준**: GPUCheckpoint `Completed` + `freeze/remap … 0 failed` + tar 생성 +
복원 후 체크섬 유지.

## 8. 동작 원리 (GCR 파이프라인)

`internal/agent/checkpoint.go` 5단계:

1. **freeze (인터셉터)** — VMM으로 소유한 각 세그먼트를 D2H로 host 버퍼에 복사 →
   `cuMemUnmap`+`cuMemRelease`(물리만 해제, VA 예약 유지).
2. **control (cuda-checkpoint)** — 호스트 헬퍼가 GPU 프로세스의 control state를 suspend.
3. **container checkpoint (kubelet→CRI-O→CRIU)** — CRIU가 **CPU 프로세스 + host 데이터**를
   덤프(GPU는 안 건드림 — cuda_plugin off, 드라이버 570이 fd까지 release).
4. **store** — CR `.spec.storage.path`로 tar 저장.
5. **resume (비파괴적)** — cuda-checkpoint resume → 인터셉터가 `cuMemCreate`+`cuMemMap`을
   **같은 VA**에 + H2D로 데이터 복귀.

핵심: **CRIU는 GPU를 절대 체크포인트하지 않습니다.** GPU는 인터셉터(데이터) + cuda-checkpoint
(control)가 host 메모리로 내리고, CRIU는 그 host 메모리를 포함한 CPU 상태만 덤프합니다.

## 9. 트러블슈팅 (실제로 겪은 것들)

| 증상 | 원인 / 조치 |
|---|---|
| DKMS 드라이버 빌드 실패 | gcc major 불일치 → 스크립트가 gcc-12 강제 |
| `nvidia-smi` 안 됨 | 드라이버 설치 후 재부팅 → 스크립트 재실행 |
| DaemonSet DESIRED=0 / 워커 `cri-o://Unknown` / Cilium 1개만 Ready | nvidia-container-runtime가 crun 미위임 → 컨테이너 실패 → `node.cilium.io/agent-not-ready` 테인트. `runtimes=["crun","runc"]` + crun PATH 보장 후 `restart crio kubelet`(스크립트가 처리) |
| 전 Pod `CreateContainerError` + `no runtime binary found [["crun"]]` | `nvidia-ctk config --set`이 값을 깨뜨림 → config.toml을 `runtimes = ["crun", "runc"]`로 직접 수정(스크립트가 처리) |
| step3 CRIU `chr 195` / `-52` | 드라이버 550/560 한계(fd 미해제). **드라이버 570+** 필요 |
| `[gcr][vmm-alloc] unresolved syms` | libcuda를 RTLD_LOCAL로 로드 → `dlopen("libcuda.so.1")` 핸들로 dlsym(인터셉터가 처리) |
| torch가 CUDA 초기화에서 Exit 1 | `cuGetProcAddress` 후킹 금지(핫패스). 현재 인터셉터는 cudaMalloc/cuMem*만 후킹 |
| `[gcr][engine] remap fail` | cuda-checkpoint↔VA 상호작용. 드라이버 570 + 본 구성에서 0 failed 확인됨 |
| 빌드 `sum.golang.org` 스트림 에러 | Dockerfile에 `GOSUMDB=off` 반영됨 |

자세한 설계: `docs/LEVEL1-design.ko.md`.
