# Level 1 설계 — GCR 데이터 엔진 (VMM 인터셉션)

## 목표

논문 GCR의 control/data 분리를 실제로 구현한다. 데이터 버퍼는 **인터셉션이 main memory로
복사 + 물리메모리만 해제(가상주소 보존)**, 제어 상태는 cuda-checkpoint, CPU+메모리는 CRIU.

핵심 불변식:
- **CRIU는 GPU를 안 본다** (Level 0에서 검증됨).
- 데이터는 **프로세스 host 메모리**에 있어 CRIU가 자동 캡처(option i).
- 가상주소(VA)는 절대 해제하지 않는다 → 복원 시 같은 주소로 remap → 포인터 일관성 유지.

## 왜 VMM인가 (전제)

`cudaFree`로 물리메모리를 비우면 VA가 사라져 복원 시 주소가 바뀐다(GCR이 푸는 address
consistency 문제). VA를 보존한 채 물리만 해제하려면 **CUDA VMM API**가 필요하다:

- `cuMemAddressReserve` / `cuMemAddressFree` — 가상주소 예약/해제
- `cuMemCreate` / `cuMemRelease` — 물리 핸들 생성/해제
- `cuMemMap` / `cuMemUnmap` — 물리↔가상 매핑/해제
- `cuMemSetAccess` — 접근권한

**PyTorch는 `PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True` 를 켜면 위 VMM API로 세그먼트를
할당한다.** 따라서 우리는 스톡 PyTorch allocator를 수정하지 않고, 이 VMM 호출만 후킹하면 된다.
(후킹은 텐서 단위가 아니라 **세그먼트 단위** — 더 적고 큰 단위라 오히려 단순.)

## 인터셉터 레지스트리 (preload.c 확장)

VMM 호출을 후킹해 세그먼트 매핑을 추적한다:

```
struct seg {
  CUdeviceptr va;     // cuMemMap 대상 가상주소
  size_t      size;
  CUmemGenericAllocationHandle handle;  // cuMemCreate 핸들 (체크포인트 시 release)
  CUmemAllocationProp prop;             // 복원 시 동일 속성으로 재생성
  void       *host_buf; // 체크포인트 시 D2H 복사본 (process 메모리)
  int         live;
};
```

후킹 대상: `cuMemCreate`(핸들·prop 기록), `cuMemMap`(va↔handle 매핑 기록),
`cuMemUnmap`/`cuMemRelease`/`cuMemAddressFree`(레지스트리 정리).

## 체크포인트 시퀀스

에이전트 step1 신호("checkpoint") → 인터셉터가 각 live 세그먼트에 대해:

1. `host_buf = malloc(size)` (프로세스 메모리 → CRIU가 캡처)
2. `cuMemcpyDtoH(host_buf, va, size)` (가능하면 Async + stream으로 고대역폭)
3. `cuMemUnmap(va, size)` → `cuMemRelease(handle)`  // 물리만 해제, **VA 예약은 유지**
4. 레지스트리에 {va, size, prop, host_buf} 보존

완료 후 ACK. 이 시점에 device에는 데이터가 없으므로 cuda-checkpoint(step2)는 **control state만**
체크포인트(데이터 중복 복사 없음 = GCR의 효율).

## 복원/재개 시퀀스 (remap)

체크포인트 흐름은 `--leave-running`이라 워크로드가 계속 돌아야 하므로, 체크포인트 직후에도
**반드시 remap이 필요**하다(데이터를 device로 되돌려야 앱이 정상 동작). 따라서 remap은 두 경우
모두에서 쓰인다: (a) 체크포인트 후 resume, (b) tar로부터 restore.

에이전트가 step5에서 cuda-checkpoint resume(control) 후 인터셉터에 "restore" 신호 →
각 세그먼트에 대해:

1. `cuMemCreate(&new_handle, size, prop, 0)`  // 동일 prop으로 물리 재생성
2. `cuMemMap(va, size, 0, new_handle, 0)` + `cuMemSetAccess`  // **같은 VA**에 매핑
3. `cuMemcpyHtoD(va, host_buf, size)`  // 데이터 복귀
4. `free(host_buf)`; 레지스트리 갱신(handle=new_handle, live=1)

VA를 한 번도 해제하지 않았으므로 같은 주소 remap이 성립 → 앱의 포인터가 그대로 유효.

## 검증 전략 (restore-from-tar 없이 가능)

체크포인트+resume 한 번 안에 copy→unmap→remap이 모두 일어나므로, **워크로드가 체크포인트 후에도
데이터 정합성을 유지하는지**로 데이터 엔진을 검증할 수 있다:

1. Pod에 `PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True` + 인터셉터 LD_PRELOAD, `GCR_INTERCEPTION=true`
2. 워크로드: 알려진 값으로 텐서 채우고 주기적으로 **체크섬 출력** (예: `x.sum()`)
3. 체크포인트 1회 트리거
4. 기대: 인터셉터 로그에 `copied N segs, M bytes` / `unmapped` / `remapped`, 그리고
   체크포인트 후 워크로드의 **체크섬이 동일**(데이터가 host 왕복 후에도 정확)
5. CRIU tar 크기 ≈ 데이터 크기(host_buf가 캡처됨)

이게 통과하면 GCR 데이터 경로(인터셉션→main memory→remap)가 검증된 것.

## 바꿀 파일

- `interceptor/preload.c` — VMM 후킹 + 레지스트리 + checkpoint/restore 핸들러 (가장 큰 작업)
- `deploy/sample-pod-*.yaml` — `PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True` 추가
- `internal/agent/checkpoint.go` — step5에서 "restore" 신호로 remap 트리거 (이미 Signal 구조 있음)
- 에이전트 env: `GCR_INTERCEPTION=true` 로 복귀 (Level 0에선 false였음)

## 위험 / 미검증 지점 (정직하게)

1. **remap 타이밍**: CRIU 복원/resume 직후 앱 스레드가 데이터에 접근하기 전에 remap이 끝나야 함.
   완화: cuda-checkpoint suspend 동안 CUDA API가 lock되어 앱이 블록됨 / CRIU가 프로세스를 잠깐
   freeze. 이 창에서 remap을 끝낸다. 그래도 경쟁 가능성은 실측 필요.
2. **cuMemCreate prop 복제**: 원본 prop(device, type)을 정확히 기록·재현해야 함.
3. **cuda-checkpoint의 page table 체크포인트와 우리 VA 관리의 상호작용** — 누가 VA를 보존하는지
   (cuda-checkpoint page table vs 우리 reservation) 충돌 가능 → 실측으로 확인.
4. **expandable_segments가 실제 VMM인지** 드라이버에서 확인(후킹 hit 로그로).
5. single-process, UVM/IPC 미지원(NCCL 등 멀티프로세스는 후속).

## 단계 분해 (구현 순서)

1. preload.c에 VMM 후킹 + 레지스트리만 추가 → 로그로 "세그먼트가 잡히는지" 확인 (no copy yet)
2. checkpoint 핸들러: D2H 복사만(unmap 없이) → "copied bytes" 확인 (안전)
3. unmap/release 추가 → device 메모리가 실제로 빠지는지(nvidia-smi) 확인
4. restore 핸들러: cuMemCreate+Map+H2D → 체크섬 정합성 확인
5. 에이전트 step5 remap 신호 연동 → end-to-end
