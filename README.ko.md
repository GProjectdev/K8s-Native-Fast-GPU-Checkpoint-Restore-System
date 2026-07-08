# K8s-Native GPU Checkpoint/Restore System

Kubernetes-native GPU **컨테이너** 체크포인트: `GPUCheckpoint` CR + 노드별 **GPU C/R
Node Agent**(DaemonSet, 별도 컨트롤러 없이 CR 직접 watch, 자기 노드 대상 CR만 처리).

> 셋업 가이드: [`docs/SETUP.ko.md`](docs/SETUP.ko.md)

## 브랜치

- **`main`(현재) — CRIUgpu.** GPU+CPU 체크포인트를 **kubelet Checkpoint API → CRI-O →
  CRIU + NVIDIA `cuda_plugin`** 이 수행. Node Agent는 오케스트레이션만(대상 해석 → kubelet
  체크포인트 호출 → tar 저장). 인터셉터/호스트 헬퍼 없음.
- **`v1.0` — GCR 데이터 엔진.** 논문 방식(인-Pod VMM 인터셉터 + 호스트 cuda-checkpoint 헬퍼
  + CRIU CPU-only, `cuda_plugin` 비활성화). `v1.0` 브랜치에 보존.

## 동작 (main / CRIUgpu)

1. `GPUCheckpoint` CR 적용(`workloadRef`, `storage`, `schedule`).
2. 대상 노드의 Node Agent가 **kubelet Checkpoint API** 호출 → CRI-O가 **CRIU + cuda_plugin**
   으로 컨테이너의 CPU 프로세스 **와** GPU 상태를 tar로 생성.
3. 그 tar를 `.spec.storage.path`로 복사.

전제: **드라이버 570+**, **CRIU cuda_plugin 설치/활성화**.

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
