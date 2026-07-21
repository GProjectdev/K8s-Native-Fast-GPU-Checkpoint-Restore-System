# K8s-Native GPU Checkpoint/Restore System

Kubernetes-native GPU **컨테이너** 체크포인트: `GPUCheckpoint` CR + 노드별 **GPU C/R
Node Agent**(DaemonSet, 별도 컨트롤러 없이 CR 직접 watch, 자기 노드 대상 CR만 처리).
멀티 replica(Deployment/StatefulSet/Job)는 별도 중앙 컨트롤러인 **WorkloadCheckpoint
오케스트레이터**([`orchestrator/`](orchestrator/))가 Pod마다 `GPUCheckpoint` 자식을
만들어 처리합니다.

> 셋업 가이드: [`docs/SETUP.ko.md`](docs/SETUP.ko.md)

## 브랜치

- **`main`(현재) — GCR 인터셉션(데이터) + CRIUgpu(제어).** GCR의 control/data 분리 유지:
  인-Pod 인터셉터가 Selective Interception **데이터** 체크포인트(`cudaMalloc`을 VMM으로 소유,
  freeze/remap), GPU **control state**는 kubelet 체크포인트 경로의 **CRIU + `cuda_plugin`**
  (CRIUgpu)이 처리 — 예전의 호스트 `cuda-checkpoint` 헬퍼를 대체.
- **`v1.0` — GCR 인터셉션(데이터) + 호스트 cuda-checkpoint 헬퍼(제어).** control을 호스트
  헬퍼 + plain CRIU(`cuda_plugin` 비활성화)로. `v1.0` 브랜치에 보존.

## 동작 (main)

GCR control/data 분리, **control state를 CRIUgpu가 담당**:

1. **데이터(Selective Interception)** — 인-Pod 인터셉터(LD_PRELOAD, `cudaMalloc`을 VMM으로 소유)
   가 GPU 데이터 버퍼를 host 메모리로 복사 + 물리메모리 해제(VA 보존) → device엔 control state만.
   빼낸 버퍼는 **외부 blob**(`data.blob`)에 쓰고, 덤프 전에 `munmap`하여 CRIU가 tar에 넣지 않음.
2. **제어+CPU(CRIUgpu)** — 에이전트가 kubelet 체크포인트 호출 → CRI-O + CRIU + `cuda_plugin`이
   남은 GPU control state + CPU 프로세스를 **tar**로 (대용량 GPU 데이터는 tar에 없음).
3. **remap** — 인터셉터가 데이터를 device로 복귀(비파괴적 resume, critical path 밖).
4. **저장** — CRIUgpu **`.tar`** + **`.blob`** 둘 다 저장. 완전한 체크포인트 = **tar + blob**
   (복원에 둘 다 필요). [`docs/DATA-ENGINE.md`](docs/DATA-ENGINE.md) 참고.

전제: **드라이버 570+**, **CRIU `cuda_plugin` 설치/활성화**.

## GPUCheckpoint CR

```yaml
spec:
  workloadRef:                # (기존 podRef 확장) kind 추가
    kind: Pod                 # Pod 전용. Deployment/StatefulSet/Job은 WorkloadCheckpoint(orchestrator/)
    namespace: default
    name: cuda-sample-pod
    container: cuda-app       # 생략 시 첫 컨테이너
    nodeInfo: ""              # 생략 시 Pod에서 해석
  storage: { type: hostPath, path: /var/lib/gcr-checkpoint }  # hostPath|mount|nfs|pvc|s3
  schedule: ""                # 빈 값=one-shot; Go duration("5m") 또는 cron("0 */2 * * *")
```

`workloadRef`는 `podRef`를 확장(`kind`), `schedule`은 `period`를 대체하며 **Go duration 또는 cron**을
받습니다. Node Agent는 `kind: Pod`만 처리하고, 멀티 replica는 orchestrator가 Pod별 자식으로 fan-out.
`status`(phase/observedNode/checkpointCount/lastCheckpointPath/conditions)는 CRD에 정의됨.

## 빠른 시작

[`docs/SETUP.ko.md`](docs/SETUP.ko.md) 를 따르세요. 요약:

```bash
sudo bash quickstart/scripts/gpu-worker-setup.sh   # 각 워커(root), 재부팅 후 재실행
bash quickstart/scripts/master-deploy.sh           # 마스터
buildah bud -f Dockerfile -t docker.io/<you>/gpu-cr-node-agent:latest . && \
 buildah push docker.io/<you>/gpu-cr-node-agent:latest docker://docker.io/<you>/gpu-cr-node-agent:latest
kubectl -n gpu-cr-system rollout restart ds/gpu-cr-node-agent
kubectl apply -f deploy/sample-pod.yaml
kubectl apply -f deploy/sample-gpucheckpoint.yaml
kubectl get gpucheckpoints.gpu-cr.io -w
```

## 로드맵
- tar+blob → 새 컨테이너 복원(CRI-O) *(다음)*.
- 멀티 워크로드(Deployment/StatefulSet/Job → replica별): **완료** (WorkloadCheckpoint orchestrator).
- cron `schedule`: **완료**(duration/cron), 분산 작업용 barrier 스냅샷은 스캐폴딩됨.
- pvc/s3 등 추가 스토리지 mover.

## 트러블슈팅

| 증상 | 원인 / 조치 |
|---|---|
| `.tar`만 있고 `.blob` 없음 | 데이터 엔진 꺼짐. `GCR_INTERCEPTION=false`면 baseline로 동작 → 미설정/`true`로. Pod가 `READY ... gpu_alloc` 찍힌 뒤 체크포인트했는지, 로그에 `[gcr] interceptor loaded ... [VMM hooks active]` 있는지 확인. |
| agent 로그 `no data blob at ...` | freeze 미실행 — 체크포인트 시 Pod가 READY 아님, 또는 그 노드에 `libgcr-interceptor.so` 마운트 안 됨(LD_PRELOAD 실패). |
| CRIU `-52` (Connected TCP socket) | 살아있는 TCP 소켓(HF keep-alive 등). `quickstart`가 `/etc/criu/default.conf`에 `tcp-close` 등을 씀. |
| CRIU `chr 195` / cuda_plugin 오류 | 드라이버 **570+** 및 CRIU `cuda_plugin` 설치/활성화. |
| checkpoint API `404`/`DeadlineExceeded` | kubelet `--feature-gates=ContainerCheckpoint=true` + `--runtime-request-timeout=30m`, agent `--kubelet-timeout=30m`. |
| 복원 시 `could not load libcriu.so.2` | crun/CRIU 불일치 — CRIU 4.2엔 **crun 1.26**(1.20 아님). |
| 오프라인 모델 tokenizer/`config.json` 오류 | 로컬 모델 dir에 가중치뿐 아니라 `config.json`·tokenizer 파일·샤드 `*.index.json`까지 다 있어야 함. Pod에 `sentencepiece tiktoken protobuf` 설치. |

## 감사의 글
DCN Lab. NVIDIA `cuda-checkpoint` + CRIU(`cuda_plugin`) 및 GCR 논문(FAST '26) 기반.
