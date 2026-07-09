# GPU Checkpoint Benchmark — inference services & timing

Measures **checkpoint time** across different **single-process inference
frameworks** and **model sizes** on a single A100 40GB worker.

## Hard constraint: single-process only
CRIUgpu uses the NVIDIA CRIU `cuda_plugin`, which supports **single-process CUDA
only** (no CUDA IPC / NCCL / multi-process). So multi-process servers are ruled
out: **vLLM (V1 uses 2 processes even on 1 GPU), TGI, TorchServe (multi-worker),
Triton (multi-instance)**. We test single-process inference:

| Framework | Service | Example models (A100 40GB) |
|---|---|---|
| PyTorch + Transformers | in-process load + generate | gpt2, gpt2-large, facebook/opt-1.3b, facebook/opt-6.7b |
| TensorFlow / Keras | in-process predict | ResNet50, EfficientNetB7 |
| JAX / Flax | in-process (add a `infer-jax.tmpl.yaml`) | FlaxGPT2 |

The dominant factor in checkpoint time is the **GPU/host memory footprint**, so
sweeping model size (esp. in PyTorch) gives the timing curve.

## Run (from the master)
```bash
bash benchmark/run.sh                 # writes bench-results.csv
# tune: OUT=res.csv TIMEOUT=1200 bash benchmark/run.sh
```
The driver sets the agent to **pure CRIUgpu** (`GCR_INTERCEPTION=false`) so the
test is framework-agnostic (no in-Pod interceptor). For each config it: deploys
the pod → waits for `READY` (model loaded + warmed up) → takes a one-shot
`GPUCheckpoint` → records the checkpoint time.

## Metrics (CSV columns)
- `ready_s` — pod load + warmup wall time
- `checkpoint_took` — the agent's internal checkpoint duration (`... took Xs`)
- `wall_s` — apply-CR → `Completed` wall time (includes reconcile latency)
- `phase`, `path` — result + produced tar path
- **tar size**: on the worker, `ls -lh /var/lib/gcr-checkpoint/` (proxy for the
  CPU+GPU footprint captured).

## Prereqs
Driver 570+, CRIU `cuda_plugin` installed/enabled, the agent DaemonSet running,
outbound network on the worker (models download from Hugging Face / Keras).

## GCR variant (optional)
To compare the GCR data engine vs pure CRIUgpu **for PyTorch**, deploy
`deploy/sample-pod.yaml` (interceptor + `GCR_VMM_ALLOC=1`) with
`GCR_INTERCEPTION=true` and checkpoint it — same timing method.
