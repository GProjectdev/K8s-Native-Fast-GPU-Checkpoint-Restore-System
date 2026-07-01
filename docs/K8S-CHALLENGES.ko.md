# GCR을 Kubernetes에 구현할 때의 한계와 해결

## 핵심 문제 (한 줄)

**GCR은 베어메탈에서 한 프로세스를 직접 제어하는 라이브러리**다(직접
`cuda-checkpoint` → `criu dump`를 순서대로 호출). 반면 **Kubernetes는 프로세스를
Pod / CRI / 런타임 뒤로 추상화**해 그 제어권을 가져간다. 따라서 이 프로젝트의 본질은:

> **K8s가 가져간 "체크포인트 제어권"을 되찾고(Node Agent 오케스트레이션 · 호스트 헬퍼 ·
> 호스트 컨트롤 채널), 플랫폼을 GCR 방향으로 조종(CRIUgpu 비활성화 · 드라이버 570 ·
> crun 위임)하여 GCR 파이프라인을 K8s 위에 재현하는 것.**

## 요약 대응표 (한계 → 해결)

| # | K8s 한계 / 문제 | 구현한 해결책 |
|---|---|---|
| 1 | 체크포인트 실행 순서를 직접 제어 불가 (kubelet→CRI-O→런타임→CRIU만 가능) | Node Agent가 kubelet API를 **감싸 오케스트레이션**(전에 freeze, 후에 remap 신호) |
| 2 | 런타임이 CRIUgpu(cuda_plugin) 자동 로드 → GCR과 충돌·`-52` 실패 | **cuda_plugin 비활성화** → CRIU는 CPU만, GPU는 인터셉터+cuda-checkpoint |
| 3 | 컨테이너 내 에이전트가 호스트 cuda-checkpoint 실행 시 stack smashing | **호스트 systemd 헬퍼**로 위임(컨트롤 파일로 요청) |
| 4 | 컨테이너 init PID ≠ GPU 프로세스 PID (멀티프로세스) | 헬퍼가 **하위 트리에서 GPU PID 탐지**(nvidia-smi / `--get-state` 프로브) |
| 5 | 데이터 경로가 앱·에이전트·런타임 컨테이너로 분산 | **호스트 공유 컨트롤 채널 + hostPath**, 데이터는 앱 host 메모리에 두어 CRIU가 캡처 |
| 6 | 워크로드 무수정 인터셉션 + LD_PRELOAD 우회(dlopen RTLD_LOCAL) | 에이전트가 노드에 `.so` 설치 + `LD_PRELOAD`, **libcuda 핸들 직접 dlopen**으로 심볼 해석 |
| 7 | kubelet 체크포인트 API 제약(feature gate, `--leave-running`, tar만) | 게이트 활성화 · 비파괴적 resume · tar→컨테이너 복원은 **남은 과제(CRI-O)** |
| 8 | controller-runtime 재조정 핫루프 → 워크로드 크래시 | `GenerationChangedPredicate`(spec 변경 시에만 reconcile) |
| 9 | 스택 전체 결합(드라이버 fd·crun 위임·device plugin 스케줄) | 드라이버 570 · crun 위임 보정 · 라벨/plugin 처리 자동화(스크립트) |

---

## 상세

### 1. 체크포인트 실행 순서를 직접 제어할 수 없음
- **한계**: 베어메탈 GCR은 "데이터 freeze → cuda-checkpoint → criu dump → remap"을 직접
  순서대로 실행한다. K8s에선 체크포인트가 **kubelet → CRI-O → 런타임 → CRIU**로만 일어나,
  그 사이에 GCR 단계를 끼워 넣을 수 없다.
- **해결**: **Node Agent가 kubelet 체크포인트 API를 감싸 오케스트레이션**한다. API 호출 전에
  인터셉터에 freeze 신호를, 후에 remap 신호를 보내고 cuda-checkpoint는 별도로 실행한다.
  런타임이 감추는 순서를 에이전트가 다시 조립한다. (`internal/agent/checkpoint.go` 5단계)

### 2. 런타임이 CRIUgpu(cuda_plugin)를 자동으로 붙임 → GCR과 충돌
- **한계**: CRI-O 체크포인트가 CRIU를 돌릴 때 NVIDIA **cuda_plugin이 자동 로드**되어 CRIU가
  **GPU까지 직접 체크포인트(CRIUgpu)** 하려 한다. "GPU는 인터셉터+cuda-checkpoint, CPU만
  CRIU"인 GCR과 충돌하며, 실제로 `dump.log`에서 `Checkpointing CUDA devices … -52`로 실패했다.
- **해결**: **cuda_plugin 비활성화**(`.so` 이름 변경) → CRIU는 **CPU 상태만** 덤프. GPU는
  이미 인터셉터(데이터)+cuda-checkpoint(control)가 host 메모리로 내려 둔 상태.

### 3. 에이전트는 컨테이너 안, cuda-checkpoint는 호스트 바이너리
- **한계**: 에이전트의 자연스러운 형태는 Pod(DaemonSet)인데, **컨테이너 프로세스가 호스트
  cuda-checkpoint를 실행하면 glibc ABI 불일치로 `stack smashing detected`**. nsenter로도 깨짐.
- **해결**: **호스트 systemd 헬퍼(`gpu-cr-cuda-helper`)**. 컨테이너 에이전트가 호스트 공유
  파일(`/var/lib/gpu-cr/cuda-req`)로 요청하면 헬퍼가 cuda-checkpoint를 호스트 네이티브로 실행.
  "오케스트레이션=컨테이너 / 호스트 전용 동작=헬퍼"로 분리.

### 4. PID·네임스페이스 불일치 (컨테이너 init ≠ GPU 프로세스)
- **한계**: kubelet/crictl이 주는 건 **컨테이너 init PID**인데, 실제 CUDA 프로세스는 자식일
  수 있다(vLLM 등). cuda-checkpoint는 GPU를 쥔 PID를 대상으로 해야 한다.
- **해결**: 헬퍼가 **컨테이너 하위 트리에서 GPU PID를 탐지**한다. 실행 중엔 `nvidia-smi`
  compute-apps, suspend 후엔(nvidia-smi에서 사라지므로) `cuda-checkpoint --get-state` 프로브로
  하위 프로세스를 스캔해 CUDA 프로세스를 찾는다.

### 5. 데이터 경로가 여러 컨테이너로 분산
- **한계**: GCR은 한 프로세스 안에서 끝나지만, K8s에선 **인터셉터(앱 컨테이너) / 에이전트
  (별도 컨테이너) / CRIU(런타임)** 가 분리돼 있어 freeze·remap 시점과 데이터 위치를 조율해야 한다.
- **해결**: **호스트 공유 컨트롤 채널**(`/var/lib/gpu-cr/run/<uid>/control`) + hostPath 공유
  디렉터리. 에이전트가 freeze/remap 신호를 쓰고 ACK를 대기. 인터셉터가 복사한 데이터는 **앱
  프로세스의 host 메모리**에 두어 CRIU가 CPU 덤프 시 자동으로 함께 캡처하게 한다.

### 6. 워크로드 수정 없이 인터셉터 주입 + LD_PRELOAD 우회
- **한계**: 임의의 GPU Pod에 이미지 변경 없이 선택적 인터셉션을 넣어야 한다. 게다가 PyTorch가
  `dlopen("libcuda.so.1", RTLD_LOCAL)` + 핸들 dlsym으로 드라이버 함수를 찾아 **LD_PRELOAD를 우회**.
- **해결**: **Node Agent가 노드에 `libgcr-interceptor.so`를 설치**(hostPath) → Pod는
  `LD_PRELOAD` env + read-only 마운트로 로드. 우회는 인터셉터가 **libcuda 핸들을 직접 dlopen**해
  드라이버 심볼을 해석하는 것으로 해결. (초기화를 깨뜨리는 `cuGetProcAddress` 후킹은 제거)

### 7. kubelet 체크포인트 API의 제약
- **한계**: 체크포인트 API는 **feature gate**로 잠겨 있고, `--leave-running` 강제, **tar만
  생성**(새 컨테이너로의 복원 기능 없음).
- **해결**: 각 노드에 게이트 활성화, tar를 아티팩트로 사용, **비파괴적 resume(remap)** 로 소스를
  유지. **tar→새 컨테이너 복원은 여전히 열린 한계**로, CRI-O/런타임 수정이 필요한 남은 과제.

### 8. controller-runtime 재조정 시맨틱
- **한계**: status 갱신마다 자기 자신 watch가 재트리거되어 **약 0.1초 핫루프** → cuda-checkpoint를
  반복 토글하며 초기화 중인 워크로드가 크래시했다.
- **해결**: `GenerationChangedPredicate` — **spec 변경 시에만** reconcile. 요청당 1회만 실행.

### 9. 스택 전체가 노출되며 생긴 환경 결합
- **한계**: K8s가 드라이버·런타임·CNI를 엮어 연쇄 장애가 발생: (a) 드라이버 **<570은 `/dev/nvidia*`
  fd 미해제 → CRIU 실패**, (b) nvidia-container-runtime의 **crun 미위임 → 전 컨테이너 실패 → Cilium이
  노드에 `agent-not-ready` 테인트 → DaemonSet 스케줄 불가**, (c) device plugin 라벨/스케줄 이슈.
- **해결**: **드라이버 570 강제**, **crun 위임 보정**(`runtimes=["crun","runc"]` + PATH 보장),
  라벨/plugin 처리 — 워커/마스터 스크립트로 자동화.

---

## 남은 한계 (열린 과제)

- **tar → 새 컨테이너 복원** — kubelet 체크포인트 API는 tar만 만든다. 복원 재사용은 CRI-O/런타임
  수정이 필요(제어상태→데이터 순서). *(다음, 사용자 트리거)*
- **저장 백엔드가 디스크** — 논문은 CPU 메모리(RAM) 캐시. tmpfs 백엔드 + 이중복사 제거로 지연 단축 여지.
- **동기 pageable 복사** — pinned + `cudaMemcpyAsync`(고대역폭)로 개선 여지.
- **전체 복사(비증분)** — shadow execution / dirty templates로 증분화 시 크기·시간 축소.
- **single-process, UVM/IPC/NCCL 미지원** — cuda-checkpoint 제약과 동일.
