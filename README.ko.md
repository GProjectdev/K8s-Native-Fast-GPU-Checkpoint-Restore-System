# K8s-Native Fast GPU Checkpoint/Restore System

**GCR**(*GPU Checkpoint/Restore Made Fast and Lightweight*, FAST '26) 방식을
쿠버네티스 네이티브로 구현한 프로젝트입니다. Custom Resource와 노드별 에이전트로
구성되어, GPU Pod를 **워크로드 수정 없이** 투명하게, 그리고 주기적으로
체크포인트할 수 있게 합니다. GCR의 **제어/데이터 분리 하이브리드 C/R**을
쿠버네티스 환경으로 가져온 것이 핵심입니다.

> 상태: **Phase 1** — `GPUCheckpoint` CR + `GPU C/R Node Agent` (체크포인트 경로).
> **별도의 GPU C/R Controller는 없습니다.** `GPUCheckpoint` CR에 필요한 정보
> (`podRef.nodeInfo`, `storage`, `period`)를 모두 담아두면, 각 Node Agent가 CR을
> **직접 watch**하여 자신의 노드를 대상으로 하는 CR을 수행합니다. 복원 경로(custom
> container runtime)는 다음 단계입니다. ([로드맵](#로드맵) 참고)

본 작업의 기반:

- 논문: *GPU Checkpoint/Restore Made Fast and Lightweight* (Zeng 외, 칭화대학교, FAST '26)
- 업스트림 코드: <https://github.com/thustorage/GCR>
- DCN Lab Progress Report (2026-06-16), "Design Checkpoint/Restore System in Kubernetes"

> 영어 문서는 [`README.md`](README.md)를 참고하세요.

---

## 왜 필요한가

시스템 수준 GPU C/R은 탄력적 서버리스 확장, 빠른 작업 전환, 내결함성 학습을
가능하게 합니다. GCR은 다음을 통해 **낮은 C/R 지연**과 **거의 0에 가까운 정상 실행
오버헤드**를 동시에 달성합니다.

- **제어/데이터 분리** — `LD_PRELOAD`로 GPU *메모리* API
  (`cuMemCreate/Map/Unmap/Release`)만 선택적으로 가로채고(오버헤드 < 1%), 제어
  상태는 효율적인 드라이버 통합 경로(`cuda-checkpoint`)를 사용합니다.
- **가상/물리 메모리 분리** — GPU 페이지 테이블(가상 주소)은 보존한 채 물리
  메모리만 해제하고, 복원 시 재매핑하여 주소 일관성 오버헤드를 제거합니다.
- **섀도우 실행 + 더티 템플릿** — 변경된 버퍼만 저장하는 증분 체크포인팅.

이 프로젝트는 위 메커니즘을 쿠버네티스 기본 요소에 결합합니다.

---

## 아키텍처

```
                       Kubernetes Cluster
  Control Plane
  ┌───────────────────────────────────────────────────────────┐
  │   GPUCheckpoint CR  (podRef.nodeInfo, storage, period)        │
  └───────────────────────────────────────────────────────────┘
                          ▲
                          │ (1) Watch  — 별도 컨트롤러 없음
  Worker Node             │
  ┌───────────────────────────────────────────────────────────┐
  │  GPU Pod                              GPU C/R Node Agent      │
  │   ├─ GPU APP                          (DaemonSet, 본 저장소)  │
  │   └─ GPU Selective Interceptor  ◄──(2) 시그널 / 체크포인트     │
  │        (libgcr-interceptor.so)                                │
  └───────────────────────────────────────────────────────────┘
                                   │ (3) Checkpoint.tar 푸시
                                   ▼
                          Shared Storage (hostPath / NFS / S3)
```

**GPU C/R Controller는 없습니다.** Node Agent가 **노드당 1개**(DaemonSet)로
동작하며 `GPUCheckpoint` CR을 직접 watch하고, `podRef.nodeInfo`가 자신의 노드와
일치하는 CR만 수행합니다. 따라서 모든 무거운 작업은 로컬에서 이뤄지고, 컨트롤
플레인은 선언적 CR 하나로 유지됩니다.

### 체크포인트 파이프라인 (`GPUCheckpoint` 단위)

1. **선택적 데이터버퍼 체크포인트** — 에이전트가 Pod 내부의 GCR hook에 시그널을
   보냄 (`internal/agent/interceptor.go`, GCR 시그널 `1=ckpt`). hook은 GPU 데이터
   버퍼를 호스트 메모리로 복사하고, 가상 페이지 테이블은 유지한 채 물리 GPU
   메모리를 해제/언맵합니다.
2. **제어 상태 체크포인트** — `cuda-checkpoint --toggle --pid <pid>`로 프로세스의
   CUDA를 suspend (드라이버 통합 경로).
3. **컨테이너 체크포인트** — 에이전트가 **kubelet 체크포인트 API**
   (`POST /checkpoint/{ns}/{pod}/{container}`)를 호출, CRIU가 호스트로 옮겨진 GPU
   버퍼를 포함해 CPU 측 프로세스를 스냅샷합니다.
4. **저장** — 생성된 아카이브를 CR의 `.spec.storage`에 정의된 백엔드에 기록.
5. **재개** — 워크로드를 다시 실행 상태로 되돌려, 주기적 체크포인팅이 작업을
   종료시키지 않도록 합니다.

---

## `GPUCheckpoint` 커스텀 리소스

```yaml
apiVersion: gpu-cr.io/v1alpha1
kind: GPUCheckpoint
metadata:
  name: ckpt-vllm-001
  namespace: default
spec:
  podRef:                       # 어떤 Pod(어느 노드)를 체크포인트할지
    nodeInfo: gpu-node-1        # Pod가 있는 노드; 이 노드의 에이전트가 CR을 수행
    namespace: default
    name: vllm-gcr-pod
    container: vllm             # 선택값; 생략 시 첫 번째 컨테이너 사용
  storage:                      # 아카이브를 저장할 파일시스템 / 경로
    type: hostPath              # hostPath | nfs | s3
    path: /var/lib/gcr-checkpoint
  period: "000500"             # HHMMSS 주기; "000000"/생략 = 1회
  incremental: true            # 첫 체크포인트 이후 변경분만 저장
```

| 필드 | 의미 |
|------|------|
| `podRef` | 대상 Pod: `nodeInfo`(Pod가 있는 노드), `namespace`, `name`, `container`. `nodeInfo`가 자신의 노드와 같을 때만 에이전트가 동작하며, 비어 있으면 Pod에서 노드를 해석. |
| `storage` | `Checkpoint.tar`를 기록할 백엔드 타입과 경로. |
| `period` | 고정폭 `HHMMSS` 체크포인트 주기. `"000030"`=30초, `"000500"`=5분, `"010000"`=1시간. `"000000"` 또는 빈 값 = 1회. |
| `incremental` | 첫 체크포인트 이후 GCR 섀도우 실행 기반 증분 체크포인팅 활성화. |

CRD: [`config/crd/gpu-cr.io_gpucheckpoints.yaml`](config/crd/gpu-cr.io_gpucheckpoints.yaml)

---

## GPU C/R Node Agent

다음을 수행하는 DaemonSet(`cmd/node-agent`)입니다.

- **시작 시 인터셉터 라이브러리 설치** — 호스트 라이브러리 디렉토리
  (`/var/lib/gpu-cr/lib`)를 만들고 `libgcr-interceptor.so`(제공 시 GCR hook
  `libcuda.so`도)를 복사해, 노드의 GPU Pod가 이를 `LD_PRELOAD`할 수 있게 합니다.
- **`GPUCheckpoint` CR을 watch**하고, 자신의 노드로 필터링한 뒤,
  controller-runtime의 requeue 메커니즘으로 `.spec.period` 스케줄을 처리합니다.
- 위의 **5단계 체크포인트 파이프라인을 실행**하고 `.status`를 갱신합니다
  (`phase`, `checkpointCount`, `lastCheckpointTime`, `lastCheckpointPath`,
  conditions).

주요 소스 파일:

| 파일 | 역할 |
|------|------|
| `cmd/node-agent/main.go` | 매니저 부트스트랩, 라이브러리 설치, 플래그/환경변수 |
| `internal/agent/reconciler.go` | 노드 필터 reconcile + period 스케줄링 |
| `internal/agent/checkpoint.go` | 5단계 체크포인트 파이프라인 + crictl PID 해석 |
| `internal/agent/interceptor.go` | 라이브러리 설치 + GCR 시그널 채널 |
| `internal/agent/kubelet.go` | kubelet 체크포인트 API 클라이언트 |
| `internal/agent/period.go` | `HHMMSS` 주기 파싱 |

---

## 선택적 CUDA 인터셉션 (`LD_PRELOAD`)

`interceptor/preload.c`는 GPU Pod가 로드하는 shim입니다. `dlopen`을 후킹하여, CUDA
런타임이 `libcuda.so.1`을 로드할 때 대신 GCR의 hook 드라이버
(`$GCR_HOME/libcuda.so`)를 로드하게 합니다. 이 hook은 GPU 메모리 관리 API만
선택적으로 가로챕니다(`libcublas`에서 오는 호출은 그대로 실제 드라이버로 전달).
`thustorage/GCR`의 `GCR/preload.c`를 그대로 따른 구현입니다.

Pod 연결 예시 ([`deploy/sample-pod.yaml`](deploy/sample-pod.yaml) 참고):

```yaml
env:
  - name: LD_PRELOAD
    value: /opt/gpu-cr/libgcr-interceptor.so
  - name: GCR_HOME
    value: /opt/gpu-cr
volumeMounts:
  - name: gpu-cr-lib
    mountPath: /opt/gpu-cr
    readOnly: true
volumes:
  - name: gpu-cr-lib
    hostPath:
      path: /var/lib/gpu-cr/lib   # Node Agent가 설치
      type: Directory
```

> 실제 `cuMem*` 인터셉션을 수행하는 **GCR hook 드라이버**(`libcuda.so`)는 업스트림
> `thustorage/GCR`(`GCR/build.sh`)에서 빌드하여 shim 옆에 둡니다. 본 저장소는 shim과
> 에이전트 오케스트레이션을 제공하며, 업스트림 hook 빌드는 노드 사전 준비 사항입니다.

---

## 저장소 구조

```
.
├── api/v1alpha1/                  # GPUCheckpoint 타입 + deepcopy + scheme
├── cmd/node-agent/                # 에이전트 엔트리포인트
├── internal/agent/                # reconciler, 파이프라인, kubelet, interceptor, period
├── interceptor/                   # LD_PRELOAD shim (preload.c) + Makefile
├── config/crd/                    # CustomResourceDefinition
├── deploy/                        # rbac, daemonset, 샘플 Pod, 샘플 CR
├── Dockerfile                     # 에이전트 + shim 이미지 빌드 (Buildah/Containerfile 호환)
└── README.md / README.ko.md
```

---

## 사전 준비 / 서버 설정

이 시스템을 실제로 실행·테스트하려면 GPU 노드, 컨테이너 런타임, 쿠버네티스
클러스터를 미리 준비해야 합니다. 아래는 전체 목록이며, 순수 제어 흐름만 점검할
때는 GPU/CRIU 부분을 건너뛰고 에이전트를 `--dry-run=true`로 실행하면 됩니다.

### 1. 하드웨어

| 항목 | 요구사항 |
|------|----------|
| GPU | 드라이버 ≥ 550이 지원하는 NVIDIA GPU (논문/Progress Report는 A100-40GB 사용). |
| 호스트 RAM | 체크포인트 백엔드가 **CPU 메모리**이므로, 스냅샷할 GPU 메모리 이상 확보 (예: A100-40GB 워크로드면 ≥ 40 GB) + 여유분. |
| 디스크 | `Checkpoint.tar` 저장 경로의 여유 공간 (LLM은 체크포인트당 수십 GB). |

### 2. GPU 노드 OS 패키지

```bash
# NVIDIA 드라이버 >= 550 — 제어 상태 C/R에 쓰는 cuda-checkpoint 바이너리 포함
nvidia-smi                       # 드라이버 >= 550.x
which cuda-checkpoint            # 존재해야 함 (보통 /usr/bin/cuda-checkpoint)
cuda-checkpoint --help

# CRIU >= 3.17 — kubelet/CRI 컨테이너 체크포인트가 CPU 측 상태 저장에 사용
sudo apt-get install -y criu     # 또는 소스 빌드
criu --version
sudo criu check                  # "Looks good." 가 떠야 함

# NVIDIA Container Toolkit — 컨테이너에 GPU 노출
#   https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/
nvidia-ctk --version
```

> 실제 선택적 `cuMem*` 인터셉션을 수행하는 **GCR hook 드라이버**(`libcuda.so`)는
> 본 저장소에 포함되지 않습니다. 업스트림
> [`thustorage/GCR`](https://github.com/thustorage/GCR)(`GCR/build.sh`)에서 빌드한
> 뒤, 각 GPU 노드의 `/var/lib/gpu-cr/lib/`에 (에이전트가 설치하는
> `libgcr-interceptor.so` 옆에) `libcuda.so`를 배치하세요. 없으면 shim이 실제
> 드라이버로 폴백하여 GPU 측 C/R이 동작하지 않습니다.

### 3. 체크포인트를 지원하는 컨테이너 런타임

**containerd ≥ 1.7** 또는 CRIU 지원으로 빌드된 **CRI-O ≥ 1.25**를 사용하세요.

```bash
# containerd: 에이전트가 사용할 CRI 소켓 확인
ls -l /run/containerd/containerd.sock
# CRI-O 사용 시: /run/crio/crio.sock  (DaemonSet의 hostPath를 맞게 수정)
```

### 4. 쿠버네티스 클러스터 구성

```text
# 쿠버네티스 v1.30+ 권장.
# kubelet 과 kube-apiserver 양쪽에서 ContainerCheckpoint feature gate 활성화
# (1.30에서 beta; 이전 버전은 명시적으로 켜야 함):
#   --feature-gates=ContainerCheckpoint=true
```

- **NVIDIA device plugin**(가능하면 GPU Feature Discovery도) — Pod가
  `nvidia.com/gpu`를 요청할 수 있고, 노드에 DaemonSet이 셀렉트하는
  `nvidia.com/gpu.present=true` 라벨이 붙도록:
  ```bash
  kubectl get nodes -L nvidia.com/gpu.present
  kubectl describe node <gpu