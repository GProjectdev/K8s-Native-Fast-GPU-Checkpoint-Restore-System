# Setup & Usage — CRIUgpu 방식 (VM 생성 이후 따라만 하면 동작)

이 브랜치(main)는 GPU 체크포인트를 **CRIUgpu**로 수행합니다. Node Agent는 오케스트레이션만
하고, 실제 GPU+CPU 체크포인트는 **kubelet Checkpoint API → CRI-O → CRIU + NVIDIA cuda_plugin**
이 담당합니다. (인터셉터/LD_PRELOAD·호스트 cuda-checkpoint 헬퍼는 사용하지 않습니다 — 그건
`v1.0` 브랜치의 GCR 데이터 엔진 방식입니다.)

## 0. 동작 개요

`GPUCheckpoint` CR을 만들면, 대상 노드의 Node Agent가:
1. **kubelet Checkpoint API** 호출 → CRI-O/CRIU + **cuda_plugin** 이 컨테이너(CPU + GPU)를 tar로 생성
2. 그 tar를 CR `.spec.storage.path`로 저장

> 전제: **NVIDIA 드라이버 570+** (cuda-checkpoint가 `/dev/nvidia*` fd까지 release해야 CRIU 성공),
> **CRIU cuda_plugin 설치/활성화**, 단일 300GB 부팅 디스크.

## 1. VM
GCP A100(예: `a2-highgpu-1g`), Ubuntu 22.04, **부팅 디스크 300GB**, 추가 디스크 없음. 마스터 1 + GPU 워커 N.

## 2. Kubernetes 기본
`https://github.com/GProjectdev/Kubernetes_Installer_with_CRIO` 로 마스터 init + 워커 join. `kubectl get nodes` Ready.

## 3. GPU 워커 준비 (각 워커, root) — 스크립트 한 방, 재부팅 1회
```bash
git clone https://github.com/GProjectdev/K8s-Native-Fast-GPU-Checkpoint-Restore-System.git
cd K8s-Native-Fast-GPU-Checkpoint-Restore-System
sudo bash quickstart/scripts/gpu-worker-setup.sh   # 드라이버 570 설치 후 종료
sudo reboot
sudo bash quickstart/scripts/gpu-worker-setup.sh   # 나머지 완료
```
설치: gcc-12, **드라이버 570**, cuda-checkpoint, NVIDIA Container Toolkit, **CRIU + cuda_plugin(활성화)**,
crun 위임 보정, CRI-O drop-in, kubelet `ContainerCheckpoint` 게이트, 체크포인트 디렉터리.

> 스크립트가 `cuda_plugin.so`가 있는지 확인합니다. `cuda_plugin MISSING` 이 뜨면 NVIDIA의 CRIU
> cuda 플러그인(또는 GPU 지원 CRIU 빌드)을 설치해야 CRIUgpu가 동작합니다.

## 4. 마스터: 배포
```bash
bash quickstart/scripts/master-deploy.sh   # device plugin + 라벨 + CRD/RBAC/DaemonSet
```

## 5. 에이전트 이미지 빌드 (build host: Go 1.22+, buildah)
```bash
buildah bud -f Dockerfile -t docker.io/<you>/gpu-cr-node-agent:latest .
buildah push docker.io/<you>/gpu-cr-node-agent:latest docker://docker.io/<you>/gpu-cr-node-agent:latest
# deploy/daemonset-crio.yaml 의 image: 를 본인 태그로 맞춘 뒤
kubectl -n gpu-cr-system rollout restart ds/gpu-cr-node-agent
```

## 6. 실행 + 체크포인트
```bash
kubectl apply -f deploy/sample-pod.yaml                # 평범한 GPU Pod (인터셉터 없음)
kubectl get pod cuda-sample-pod -o wide -w             # Running
kubectl apply -f deploy/sample-gpucheckpoint.yaml      # GPUCheckpoint CR
kubectl get gpucheckpoints.gpu-cr.io -w                # Checkpointing -> Completed
```

## 7. 검증
```bash
kubectl -n gpu-cr-system logs -l app.kubernetes.io/name=gpu-cr-node-agent | tail
ls -lh /var/lib/gcr-checkpoint/                        # checkpoint-*.tar
```
통과 = GPUCheckpoint `Completed` + tar 생성. (CRIUgpu가 실패하면 워커의
`/var/lib/containers/storage/overlay-containers/<id>/userdata/dump.log` 확인.)

## 8. GPUCheckpoint CR 스키마

```yaml
apiVersion: gpu-cr.io/v1alpha1
kind: GPUCheckpoint
metadata: { name: ckpt-sample-001, namespace: default }
spec:
  workloadRef:                # (기존 podRef 확장) 대상 워크로드
    kind: Pod                 # Pod (기본) | Deployment | StatefulSet(예약)
    namespace: default
    name: cuda-sample-pod
    container: cuda-app       # 생략 시 첫 컨테이너
    nodeInfo: ""              # 생략 시 Pod의 spec.nodeName 으로 해석
  storage:
    type: hostPath            # hostPath | nfs | s3
    path: /var/lib/gcr-checkpoint
  schedule: ""                # (기존 period 대체) 빈 값=one-shot, "5m"/"1h"=주기 (Go duration)
status:                       # CRD에 정의됨 (agent가 갱신)
  phase: Completed            # Pending|Checkpointing|Completed|Failed
  observedNode: ...
  lastCheckpointTime: ...
  checkpointCount: 1
  lastCheckpointPath: /var/lib/gcr-checkpoint/checkpoint-...tar
  conditions: [...]
```

## 9. 트러블슈팅
| 증상 | 조치 |
|---|---|
| CRIU 실패 `chr 195` / `-52` | 드라이버 **570+** 필요 |
| dump.log `cuda_plugin` 관련 실패 | CRIU cuda_plugin 미설치/버전 → 설치 확인 (`cuda_plugin.so`) |
| DaemonSet DESIRED=0 / Cilium not ready | nvidia-container-runtime crun 미위임 → 스크립트가 보정 |
| checkpoint API 404 | kubelet `ContainerCheckpoint` feature gate |
