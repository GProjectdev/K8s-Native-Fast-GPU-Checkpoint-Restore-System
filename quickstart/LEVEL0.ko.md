# Level 0 검증 — K8s에서 GPU C/R 배관이 한 번 통과하는지

목표: **인터셉터 없이**, GCR의 CPU/제어 경로만으로 체크포인트가 end-to-end로 통과하는지
확인한다. 이게 통과하면 그 위에 GCR 데이터 엔진(VMM, Level 1/2)을 얹는다.

구성(config B, GCR-정렬): **에이전트가 cuda-checkpoint로 GPU를 분리 → cuda_plugin은 꺼서
CRIU는 CPU+메모리만 덤프**. CRIU는 GPU를 건드리지 않는다.

> 전제: 드라이버 **570+** (550/560은 cuda-checkpoint가 /dev/nvidia* fd를 안 닫아 CRIU 실패).

## 1. 워커 준비 (각 GPU 워커)

```bash
# 드라이버 570 (이미 했으면 생략)
sudo apt-get update && sudo apt-get install -y cuda-drivers-570 || sudo apt-get install -y nvidia-driver-570
sudo reboot
# 재부팅 후
nvidia-smi                                   # Driver Version: 570.x

# cuda-checkpoint 최신본
cd /tmp && rm -rf cuda-checkpoint && git clone --depth 1 https://github.com/NVIDIA/cuda-checkpoint.git
sudo install -m 0755 cuda-checkpoint/bin/x86_64_Linux/cuda-checkpoint /usr/bin/cuda-checkpoint

# cuda_plugin 비활성화 (CRIU가 GPU를 만지지 않게 = GCR)
sudo find / -name 'cuda_plugin*' 2>/dev/null
sudo mv /usr/lib/criu/cuda_plugin.so /usr/lib/criu/cuda_plugin.so.disabled 2>/dev/null || true

# 호스트 헬퍼 최신본 + 가동
cd ~/K8s-Native-Fast-GPU-Checkpoint-Restore-System && git pull
sudo install -m 0755 quickstart/scripts/gpu-cr-cuda-helper.sh /usr/local/bin/gpu-cr-cuda-helper.sh
sudo systemctl restart gpu-cr-cuda-helper
systemctl is-active gpu-cr-cuda-helper
```

### 워커에서 fd 분리 사전 검증 (가장 중요)

```bash
# 아무 GPU pod 하나 떠 있는 상태에서
GPUPID=$(nvidia-smi --query-compute-apps=pid --format=csv,noheader,nounits | head -1)
sudo /usr/bin/cuda-checkpoint --toggle --pid $GPUPID
ls -l /proc/$GPUPID/fd | grep -i nvidia      # ← 570에서 '비어 있어야' config B 성립
sudo /usr/bin/cuda-checkpoint --toggle --pid $GPUPID
```
fd가 비면 → cuda-checkpoint가 GPU를 완전히 떼었다는 뜻 → plain CRIU로 덤프 가능.

## 2. 에이전트 구성 (마스터)

```bash
# 인터셉터 OFF(Level 0), cuda-checkpoint는 에이전트가 직접(helper) 수행(SKIP=false 기본)
kubectl -n gpu-cr-system set env ds/gpu-cr-node-agent GCR_INTERCEPTION=false CUDA_CHECKPOINT_SKIP=false
kubectl -n gpu-cr-system rollout restart ds/gpu-cr-node-agent
kubectl -n gpu-cr-system rollout status ds/gpu-cr-node-agent
```

## 3. 테스트 (마스터)

```bash
kubectl delete pod cuda-plain-pod --force --grace-period=0 --ignore-not-found
kubectl apply -f deploy/sample-pod-plain.yaml
kubectl get pod cuda-plain-pod -o wide -w          # RESTARTS 0, Running 안정
kubectl logs cuda-plain-pod | grep "GPU tensor allocated"

kubectl apply -f deploy/sample-gpucheckpoint-plain.yaml
kubectl get gpucheckpoints.gpu-cr.io -w            # Completed 기대
kubectl -n gpu-cr-system logs -l app.kubernetes.io/name=gpu-cr-node-agent -f
```

## 4. 결과 확인 (워커)

```bash
journalctl -u gpu-cr-cuda-helper -n 20 --no-pager  # toggle 처리 로그
ls -lh /var/lib/gcr-checkpoint/                     # checkpoint-*.tar 생성
```

## 성공 기준 (Level 0 통과)

- GPUCheckpoint Phase = **Completed**
- `/var/lib/gcr-checkpoint/`에 **checkpoint tar** 생성
- Pod는 계속 Running(--leave-running) — step5 resume까지 정상

여기까지 되면 K8s C/R 배관(에이전트→cuda-checkpoint→kubelet→CRIU(CPU)→저장→resume)이
검증된 것. 다음 단계로 **GCR 데이터 엔진(VMM 인터셉터: D2H 복사 + cuMemUnmap/가상주소 보존 +
복원 remap)** 을 이 위에 구현한다.

## 안 되면 보는 곳

| 증상 | 확인 |
|---|---|
| step3 CRIU 실패(`chr 195`/`-52`) | 드라이버 570인지, fd 분리 사전검증이 비는지, cuda_plugin이 정말 비활성인지 |
| `no GPU process under container pid` | 헬퍼가 GPU PID를 못 찾음 → pod가 GPU 잡고 Running인지, 헬퍼 재시작 |
| Pod가 RESTARTS 증가 | 인터셉터가 아직 붙어있지 않은지(plain pod 사용), describe로 종료사유 |
