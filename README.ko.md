# K8s-Native GPU Checkpoint/Restore System

Kubernetes-native GPU **컨테이너** 체크포인트: `GPUCheckpoint` CR + 노드별 **GPU C/R
Node Agent**(DaemonSet, 별도 컨트롤러 없이 CR 직접 watch, 자기 노드 대상 CR만 처리).

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
2. **제어+CPU(CRIUgpu)** — 에이전트가 kubelet 체크포인트 호출 → CRI-O + CRIU + `cuda_plugin`이
   남은 GPU control state + CPU 프로세스(host 데이터 포함)를 tar로.
3. **저장** → `.spec.storage.path`.
4. **remap** — 인터셉터가 데이터를 device로 복귀(비파괴적 resume).

전제: **드라이버 570+**, **CRIU `cuda_plugin` 설치/활성화**.

## GPUCheckpoint CR

```yaml
spec:
  workloadRef:                # (기존 podRef 확장) kind 추가
    kind: Pod                 # Pod(기본) | Deployment | StatefulSet(예약)
    namespace: default
    name: cuda-sample-pod
    container: cuda-app       # 생략 시 첫 컨테이너
    nodeInfo: ""              # 생략 시 Pod에서 해석
  storage: { type: hostPath, path: /var/lib/gcr-checkpoint }
  schedule: ""                # (기존 period 대체) Go duration; 빈 값=one-shot
```

`workloadRef`는 `podRef`를 확장(`kind` 추가), `schedule`은 `period`(HHMMSS)를 Go duration으로 대체.
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
- tar → 새 컨테이너 복원(CRI-O). / `workloadRef` 멀티 워크로드(Deployment/StatefulSet). / 주기 `schedule`·스토리지 백엔드(nfs/s3).

## 감사의 글
DCN Lab. NVIDIA `cuda-checkpoint` + CRIU(`cuda_plugin`) 및 GCR 논문(FAST '26) 기반.
