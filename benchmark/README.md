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
