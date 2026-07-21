# Checkpoint benchmark — our system vs. baseline

Measures GPU-checkpoint cost of **our system** (GCR Selective-Interception DATA
freeze/remap + CRIUgpu control state) against a **pure-CRIUgpu baseline**, across
several inference frameworks and model sizes.

## Constraint
CRIUgpu's `cuda_plugin` supports **single-process CUDA only** (no NCCL / CUDA IPC /
multi-process). So the workloads here are single-process servers. vLLM V1, TGI,
Triton and multi-worker TorchServe are multi-process and are intentionally excluded.

## Modes (`MODES`)
| mode | agent | pod | what it measures |
|------|-------|-----|------------------|
| `gcr` | `GCR_INTERCEPTION=true` | carries the LD_PRELOAD interceptor (`GCR_VMM_ALLOC=1`, control/data mounts) | **our system**: data buffers frozen to host memory by the interceptor, control state by CRIUgpu |
| `baseline` | `GCR_INTERCEPTION=false` | plain pod, no interceptor | pure CRIUgpu (whole GPU state via cuda_plugin) |

Same models run under both modes → rows in the CSV are directly comparable.

## Models (`CONFIGS`, edit in `run.sh`)
- PyTorch (HF Transformers, fp16): `gpt2`, `gpt2-large`, `facebook/opt-1.3b`, `facebook/opt-6.7b` — sweeps GPU footprint
- TensorFlow (Keras Applications): `ResNet50`, `EfficientNetB7`

## Run (on the master)
```bash
bash benchmark/run.sh                                  # gcr then baseline, all models
MODES=gcr bash benchmark/run.sh                        # only our system
MODES="gcr baseline" TIMEOUT=1200 OUT=res.csv bash benchmark/run.sh
```
The script flips the node-agent's `GCR_INTERCEPTION` per mode automatically and
restores nothing on exit — set it back yourself if needed.

## Output — `bench-results.csv`
`mode, framework, model, pod, ready_s, checkpoint_took, wall_s, phase, path`
- `checkpoint_took` — from the node-agent log (`took Xs`): the actual checkpoint work
- `wall_s` — CR-apply → `status.phase=Completed`
- `phase` — `Completed` / `Failed` / `NotReady` / `Timeout` / `DeployError` / `CRError`

Compare `gcr` vs `baseline` rows per model for `checkpoint_took` and tar size
(tars land on the worker: `ls -lh /var/lib/gcr-checkpoint/`).

## Robustness
`run.sh` never aborts the batch: any failure on one (mode, model) is recorded with
its `phase` and the run continues to the next.

## Files
- `run.sh` — driver (composes pods per mode, times checkpoints, writes CSV)
- `infer-pytorch.py`, `infer-tensorflow.py` — single-process inference servers (print `READY`, then idle)
- `gpucheckpoint.tmpl.yaml` — one-shot GPUCheckpoint CR template

## Worker prerequisite: CRIU TCP handling
Inference workloads keep TCP sockets open (model-download keep-alive, clients).
CRIU aborts on the first established socket (`Connected TCP socket ... -52`) unless
told to close them. On every GPU worker:
```bash
sudo mkdir -p /etc/criu
printf 'tcp-close\next-unix-sk\nfile-locks\n' | sudo tee /etc/criu/default.conf
```
`criu swrk` (spawned by crun during checkpoint) reads this automatically. This is
baked into `quickstart/scripts/gpu-worker-setup.sh`.

## Phase breakdown (CSV columns)
Each row now records where the checkpoint time goes:

| column | source | meaning |
|--------|--------|---------|
| `total_s` | agent `PHASE_TIMES` | whole pipeline (freeze→kubelet→store→remap) |
| `freeze_s` | agent | **Selective Interception data checkpoint** (D2H copy + free physical GPU mem). 0 in baseline |
| `kubelet_s` | agent | CRIUgpu container checkpoint via kubelet API (CRIU dump + cuda_plugin + CRI-O tar) |
| `cuda_plugin_s` | tar `dump.log` | GPU **control-state** dump inside CRIU (cuda_plugin span) |
| `cpu_dump_s` | tar `dump.log` | CRIU **CPU memory** dump (= CRIU total − cuda_plugin) |
| `crio_tar_s` | agent−dump.log | CRI-O tar/write + API overhead (= kubelet_s − CRIU dump) |
| `store_s` | agent | copy the tar to the CR backend |
| `remap_s` | agent | interceptor restore (H2D remap) so the source keeps running |
| `freeze_bytes` | pod log | bytes the interceptor moved GPU→host |
| `tar_bytes` | agent `stat` | checkpoint tar size |

`cuda_plugin_s`/`cpu_dump_s`/`crio_tar_s`/`tar_bytes` are read from the checkpoint
tar's `dump.log` via `kubectl exec` into the node-agent pod (it mounts the checkpoint
dir), so no worker SSH is needed.

### Requires the instrumented node-agent
`freeze_s/kubelet_s/store_s/remap_s/total_s` come from a `PHASE_TIMES` log line added
to the agent. Rebuild + redeploy once:
```bash
docker build -t docker.io/jeongseungjun/gpu-cr-node-agent:latest .
docker push docker.io/jeongseungjun/gpu-cr-node-agent:latest
kubectl -n gpu-cr-system rollout restart ds/gpu-cr-node-agent
kubectl -n gpu-cr-system rollout status  ds/gpu-cr-node-agent
```
(Ensure the DaemonSet's `imagePullPolicy: Always`, or bump an image tag, so the new
image is actually pulled.)


## Known workload issues (fixed / flagged)
- **PyTorch `.bin` models need torch>=2.6.** Recent `transformers` blocks `torch.load`
  of pickle weights on torch<2.6 (`check_torch_load_is_safe`). Models with safetensors
  (gpt2, gpt2-large) load on any torch; `facebook/opt-*` ship `.bin` and failed on the
  old 2.4 image. The PyTorch image is now `pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime`.
- **TensorFlow is unsupported by the interceptor (gcr mode).** Baseline TF checkpoints
  fine, but with the LD_PRELOAD VMM interceptor active TF's BFC allocator crashes at
  device init. Until the interceptor supports TF, run TF in baseline only:
  ```bash
  GCR_SKIP_FW="tensorflow" MODES="gcr baseline" TIMEOUT=1200 bash benchmark/run.sh
  ```
  `GCR_SKIP_FW` lists frameworks to skip in gcr mode (recorded as phase `SkippedGCR`).

## Choosing the checkpoint storage backend
`run.sh` builds the GPUCheckpoint CR from `STORAGE_*` env vars (mirrors `spec.storage`),
so you can benchmark any backend without editing files:

```bash
# default: node-local hostPath
bash benchmark/run.sh

# NFS (agent mounts it at runtime; no DaemonSet volume needed)
STORAGE_TYPE=nfs STORAGE_ENDPOINT=10.178.0.14 STORAGE_PATH=/mnt/nfs STORAGE_SUBPATH=gcr \
  MODES="gcr baseline" bash benchmark/run.sh

# generic file mount (NFSv4 / EFS / CIFS / CephFS ...)
STORAGE_TYPE=mount STORAGE_FSTYPE=nfs4 STORAGE_SOURCE=10.178.0.14:/mnt/nfs \
  STORAGE_OPTIONS=nfsvers=4,nolock STORAGE_SUBPATH=gcr bash benchmark/run.sh

# CSI/PVC (EBS, EFS, ...) — needs the checkpoint mover (pending)
STORAGE_TYPE=pvc STORAGE_CLAIM=my-ebs-pvc STORAGE_SUBPATH=gcr bash benchmark/run.sh
```
The `path` CSV column shows where each tar landed (for nfs/mount it is the agent's
runtime mountpoint, which is NFS/EFS-backed). Tip: keep the interceptor offload
(`GCR_DATA_DIR`) on local disk and send only the tar to remote storage, so remote
I/O doesn't skew the freeze/store timings.

## Complete checkpoint = tar + blob (GCR-aligned)
With the Selective-Interception data engine, a `gcr` checkpoint is TWO files, both
stored in the backend:
- `checkpoint-….tar`  — CPU + GPU control state (CRIUgpu)
- `checkpoint-….blob` — the GPU data buffers (interceptor offload)

The CSV now records both `tar_bytes` and `blob_bytes`. Success looks like:
- `gcr` **`tar_bytes` << `baseline` `tar_bytes`** (the model left the tar), and
- `gcr` **`blob_bytes` ≈ model GPU footprint** (the model now lives in the blob).

Both files are needed to restore. The blob dir (`GCR_DATA_DIR` = `/var/lib/gcr-data`)
is host-visible so the agent persists it next to the tar; put it on tmpfs for RAM speed.
`GCR_PERSIST_BLOB=false` keeps the blob local only (in-place resume, not restorable).

## Quick single-model check (verify the tar-shrink fix)
Test one model first — the fastest way to confirm the GCR data engine actually keeps
the model out of the tar (`ONLY="framework model"` runs a single config).

Prerequisites (once):
```bash
# 0) rebuild+redeploy the agent (interceptor munmap + disk cleanup + blob offload)
docker build -t docker.io/jeongseungjun/gpu-cr-node-agent:latest .   && docker push docker.io/jeongseungjun/gpu-cr-node-agent:latest
kubectl apply -f deploy/daemonset-crio.yaml
kubectl -n gpu-cr-system rollout restart ds/gpu-cr-node-agent
kubectl -n gpu-cr-system rollout status  ds/gpu-cr-node-agent

# 1) free worker disk if a previous run filled it (avoids disk-pressure eviction)
#    run on each GPU worker:
#      sudo rm -f  /var/lib/kubelet/checkpoints/*.tar
#      sudo rm -rf /var/lib/gcr-data/*
#      df -h /var/lib
```

Run one model, gcr vs baseline, keeping resources on failure:
```bash
# gpt2-large (1.5GB) is big enough to see the shrink; use local storage for the fastest check
ONLY="pytorch gpt2-large" MODES="gcr baseline" KEEP_FAILED=1 TIMEOUT=1800   bash benchmark/run.sh

# or straight to NFS:
STORAGE_TYPE=nfs STORAGE_ENDPOINT=10.178.0.14 STORAGE_PATH=/mnt/nfs STORAGE_SUBPATH=gcr ONLY="pytorch gpt2-large" MODES="gcr baseline" KEEP_FAILED=1 TIMEOUT=1800   bash benchmark/run.sh
```

Success = in the CSV, **`gcr` `tar_bytes` is ~model-size smaller than `baseline`**
(the model left the tar via the external blob), and **`gcr` `freeze_s` is small**
(RAM offload). If the tar did NOT shrink, CRIU is still dumping the mapping — grab the
CRIU `dump.log` from inside the tar (see the diagnostics the harness prints on failure).

`ONLY` also works with a single mode, e.g. `MODES=gcr ONLY="pytorch opt-6.7b"`.

## Offline models (avoid HuggingFace download failures)
HF's Xet CDN intermittently 403s, stalling model downloads at benchmark time. Pre-cache
the PyTorch models into the NFS share once, then load them locally:

```bash
# (optional, if public Xet 403s) create an HF token secret:
kubectl create secret generic hf-token -n default --from-literal=token=hf_xxx

# 1) download gpt2, gpt2-large, opt-1.3b, opt-6.7b into 10.178.0.14:/mnt/nfs/models/<id>
kubectl apply -f benchmark/download-models-job.yaml
kubectl logs -f job/hf-download-bench-models      # wait for "all models cached"

# 2) run the benchmark against the cache (MODELS_NFS -> load from /models/<id> offline)
MODELS_NFS=10.178.0.14:/mnt/nfs/models FRAMEWORKS=pytorch MODES="gcr baseline" RUNS=3 \
  STORAGE_TYPE=nfs STORAGE_ENDPOINT=10.178.0.14 STORAGE_PATH=/mnt/nfs STORAGE_SUBPATH=gcr \
  bash benchmark/run.sh
```
`MODELS_NFS` mounts the NFS models dir into each PyTorch pod and sets `MODEL=/models/<id>`
with `HF_HUB_OFFLINE=1`/`TRANSFORMERS_OFFLINE=1` — no runtime HuggingFace access.
(TensorFlow models are unaffected; they use keras.applications, not HF.)

> The cached model dir must contain **all** files, not just weights: `config.json`,
> tokenizer files (`tokenizer.json` or `vocab.json`+`merges.txt`, `tokenizer_config.json`),
> and for sharded models the `*.index.json`. If a tokenizer/config file is missing you'll
> see `Couldn't instantiate the backend tokenizer` or `no file named model.safetensors`;
> fetch the missing small files (e.g. `curl -fsSL https://huggingface.co/<id>/resolve/main/<file>`).
> The pods install `sentencepiece tiktoken protobuf` for tokenizer conversion.
