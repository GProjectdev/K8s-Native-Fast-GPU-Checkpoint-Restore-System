# WorkloadCheckpoint Orchestrator

워크로드(Deployment · StatefulSet · Job · Pod) 하나를 지정하면, 그 워크로드의
**GPU Pod 각각에 대해 `GPUCheckpoint` 자식을 자동 생성**하고 상태를 집계하는
상위 컨트롤러입니다.

## 왜 필요한가

기존 `GPUCheckpoint`는 **Pod 1개 = 노드 1개 = Node Agent 1개**를 전제로 합니다.
하지만 Deployment/StatefulSet은 여러 Pod(replica)를 여러 노드에서 굴리므로,
"Deployment를 체크포인트" 하려면 누군가 **Pod 목록으로 풀어서 Pod마다 요청을
나눠줘야**(fan-out) 합니다. 이 orchestrator가 그 역할을 합니다.

## 설계 — Node Agent를 건드리지 않음

- 이 컴포넌트는 **별도 디렉터리(`orchestrator/`) + 별도 바이너리 + 별도 Deployment**입니다.
- Node Agent(DaemonSet) 코드는 **일절 변경되지 않습니다.** Agent는 지금처럼
  `GPUCheckpoint(kind=Pod)`만 watch 합니다.
- orchestrator는 실제 체크포인트 작업을 하지 않고, **`GPUCheckpoint` 오브젝트를
  생성만** 합니다. 나머지(freeze → CRIUgpu → store)는 각 노드의 Agent가 수행.

```
WorkloadCheckpoint (사용자 생성, workloadRef: Deploy/STS/Job/Pod)
      │  orchestrator 컨트롤러: workloadRef → 대상 Pod 목록 resolve
      ▼
GPUCheckpoint × N  (Pod당 1개, ownerRef=WorkloadCheckpoint, kind=Pod)
      │  각 노드의 Node Agent가 자기 Pod 것만 집어 처리 (단일 작성자)
      ▼
freeze → CRIUgpu(dump) → remap(resume) → store(tar+blob)
      │  자식 status → orchestrator가 집계
      ▼
WorkloadCheckpoint.status (total / completed / failed / targets[])
```

핵심 불변식: **GPUCheckpoint는 항상 Pod 1개**. workloadRef의 다중성은 전부
orchestrator가 fan-out으로 흡수하므로 자식 status 작성자가 유일하고, ownerRef로
GC/추적이 자동입니다.

## 빌드 & 배포

레포 루트에서 (shared `api/v1alpha1` import 때문에 컨텍스트가 루트여야 함):

```bash
# 0) CRD 등록 (기존 GPUCheckpoint CRD는 그대로 두고 하나만 추가)
kubectl apply -f orchestrator/config/crd/gpu-cr.io_workloadcheckpoints.yaml

# 1) 이미지 빌드/푸시
docker build -f orchestrator/Dockerfile -t <registry>/gpu-cr-orchestrator:latest .
docker push <registry>/gpu-cr-orchestrator:latest

# 2) orchestrator deploy 의 image: 를 위 태그로 바꾼 뒤
kubectl apply -f orchestrator/deploy/orchestrator.yaml
kubectl -n gpu-cr-system rollout status deploy/gpu-cr-orchestrator
```

> 코드는 `controller-gen` 없이도 컴파일되도록 deepcopy를 직접 넣어 두었지만,
> 타입을 수정하면 `make generate`(controller-gen)로 재생성하는 것을 권장합니다.

## 사용법

### Deployment의 모든 GPU replica 체크포인트
```bash
kubectl apply -f orchestrator/deploy/sample-workloadcheckpoint.yaml
```
```yaml
apiVersion: gpu-cr.io/v1alpha1
kind: WorkloadCheckpoint
metadata: { name: infer-deploy-ckpt, namespace: default }
spec:
  workloadRef: { kind: Deployment, namespace: default, name: gpu-infer, container: cuda-app }
  action: Checkpoint
  storage: { type: nfs, endpoint: "10.178.0.14", source: "10.178.0.14:/mnt/nfs", path: /mnt/nfs, subPath: gcr }
```
`subPath`(또는 hostPath면 `path`)에는 replica 충돌 방지를 위해 orchestrator가
**Pod 이름을 자동으로 덧붙입니다** → `gcr/<pod-name>/`.

### 진행 상황 보기
```bash
kubectl get wckpt                      # Kind/Action/Phase/Total/Done/Failed
kubectl describe wckpt infer-deploy-ckpt
kubectl get gpuckpt -l gpu-cr.io/workload-checkpoint=infer-deploy-ckpt   # 자식들
```
`status.targets[]` 에 Pod별 (node / childName / phase / path / message)가 집계됩니다.

### 다른 워크로드 종류
- `kind: StatefulSet` / `kind: Job` / `kind: Pod` 모두 동일하게 동작.
- `podSelector`로 일부 replica만 대상으로 좁힐 수 있음(워크로드 selector와 AND).
- `requireGPU: true`(기본)면 `nvidia.com/gpu`를 요청한 Pod만 대상.
- `maxConcurrent: N`으로 동시에 진행하는 자식 수를 제한(0 = 무제한).

### 정리
`WorkloadCheckpoint`를 지우면 ownerRef로 자식 `GPUCheckpoint`가 함께 GC 됩니다.
```bash
kubectl delete wckpt infer-deploy-ckpt
```

## 현재 범위 / 주의

- **Action=Checkpoint**만 완전 동작합니다. `Action=Restore`는 스키마·컨트롤러
  분기까지 스캐폴딩돼 있으나, **Node Agent의 restore 데이터플레인이 아직 없어**
  현재는 `Failed`로 명시적 거부합니다(추후 tar+blob 복원 경로 연결 예정).
- **Deployment restore 비권장**: replica가 fungible이라 복원 자리가 없습니다.
  restore가 의미 있는 대상은 Pod · Job · StatefulSet(안정적 identity)입니다.
- **분산 학습(Job)**: `coordination: Barrier`를 지정하면 컨트롤러에 barrier 훅이
  호출됩니다. 현재는 no-op 스텁이며, 일관된 스냅샷(예: collective 경계에서 전
  replica 동시 freeze)을 위한 데이터플레인 연동은 TODO입니다. 독립 추론 replica는
  `None`(기본)으로 충분합니다.

## 파일

| 경로 | 내용 |
|---|---|
| `api/v1alpha1/workloadcheckpoint_types.go` | CRD 타입(spec/status) |
| `api/v1alpha1/zz_generated.deepcopy.go` | deepcopy(수기 작성) |
| `controllers/workloadcheckpoint_controller.go` | resolve → fan-out → 집계 |
| `cmd/main.go` | orchestrator 매니저 바이너리 |
| `config/crd/gpu-cr.io_workloadcheckpoints.yaml` | CRD 매니페스트 |
| `deploy/orchestrator.yaml` | SA + RBAC + Deployment |
| `deploy/sample-workloadcheckpoint.yaml` | 예시 CR |
| `Dockerfile` | orchestrator 이미지(루트에서 빌드) |
