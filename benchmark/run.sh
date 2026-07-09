#!/usr/bin/env bash
# GPU checkpoint benchmark: OUR system (GCR interception + CRIUgpu) vs baseline CRIUgpu.
# Runs on the MASTER (needs kubectl). Single-GPU aware, self-diagnosing.
#
# MODES:
#   gcr      = OUR system: interceptor DATA freeze/remap + CRIUgpu control
#   baseline = pure CRIUgpu (no interceptor)
#
# Env:
#   MODES="gcr baseline"  TIMEOUT=900  OUT=bench-results.csv
#   KEEP_FAILED=1   -> do NOT delete pod/CR when a checkpoint fails (inspect it)
#   PRECLEAN_GPU=1  -> also delete ANY pod requesting nvidia.com/gpu before starting
set -uo pipefail
NS=${NS:-default}
AGENT_NS=${AGENT_NS:-gpu-cr-system}
OUT=${OUT:-bench-results.csv}
TIMEOUT=${TIMEOUT:-900}
MODES=${MODES:-"gcr baseline"}
KEEP_FAILED=${KEEP_FAILED:-0}
PRECLEAN_GPU=${PRECLEAN_GPU:-0}
here="$(cd "$(dirname "$0")" && pwd)"
now(){ date +%s.%N; }
elapsed(){ awk "BEGIN{printf \"%.1f\", $(now)-$1}"; }

CONFIGS=(
  "pytorch gpt2"
  "pytorch gpt2-large"
  "pytorch facebook/opt-1.3b"
  "pytorch facebook/opt-6.7b"
  "tensorflow ResNet50"
  "tensorflow EfficientNetB7"
)
[ -n "${ONLY:-}" ] && CONFIGS=("$ONLY")   # ONLY="pytorch gpt2" runs a single config

fw_image(){ case $1 in
  pytorch)    echo "pytorch/pytorch:2.4.0-cuda12.1-cudnn9-runtime";;
  tensorflow) echo "tensorflow/tensorflow:2.15.0-gpu";;
esac; }
fw_pip(){ case $1 in
  pytorch)    echo '"transformers>=4.44" accelerate sentencepiece';;
  tensorflow) echo '';;
esac; }

make_pod(){  # MODE NAME FRAMEWORK MODEL
  local mode=$1 name=$2 fw=$3 model=$4
  local image pip pyb64; image=$(fw_image "$fw"); pip=$(fw_pip "$fw")
  pyb64=$(base64 -w0 "$here/infer-$fw.py")
  local pipline=":"; [ -n "$pip" ] && pipline="pip -q install $pip >/dev/null 2>&1 || true"
  local env_extra="" vmounts="" vols=""
  if [ "$mode" = gcr ]; then
    env_extra=$'        - { name: GCR_VMM_ALLOC, value: "1" }\n        - { name: LD_PRELOAD, value: /opt/gpu-cr/libgcr-interceptor.so }\n        - { name: GCR_HOME, value: /opt/gpu-cr }\n        - name: GCR_POD_UID\n          valueFrom: { fieldRef: { fieldPath: metadata.uid } }\n        - { name: GCR_CONTROL_DIR, value: /var/lib/gpu-cr/run }\n        - { name: GCR_DATA_DIR, value: /var/lib/gcr-checkpoint }'
    vmounts=$'      volumeMounts:\n        - { name: gpu-cr-lib, mountPath: /opt/gpu-cr, readOnly: true }\n        - { name: gpu-cr-run, mountPath: /var/lib/gpu-cr/run }\n        - { name: gcr-checkpoint, mountPath: /var/lib/gcr-checkpoint }'
    vols=$'  volumes:\n    - name: gpu-cr-lib\n      hostPath: { path: /var/lib/gpu-cr/lib, type: Directory }\n    - name: gpu-cr-run\n      hostPath: { path: /var/lib/gpu-cr/run, type: DirectoryOrCreate }\n    - name: gcr-checkpoint\n      hostPath: { path: /var/lib/gcr-checkpoint, type: DirectoryOrCreate }'
  fi
  cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
  namespace: $NS
  labels: { app: $name, bench: gpu-cr, mode: $mode }
spec:
  nodeSelector: { nvidia.com/gpu.present: "true" }
  restartPolicy: Never
  containers:
    - name: cuda-app
      image: $image
      env:
        - { name: MODEL, value: "$model" }
        - { name: HF_HOME, value: /root/.cache/huggingface }
$env_extra
      command: ["bash","-lc"]
      args:
        - |
          set -e
          $pipline
          echo "$pyb64" | base64 -d > /tmp/run.py
          python /tmp/run.py
      resources: { limits: { nvidia.com/gpu: "1" } }
$vmounts
$vols
EOF
}

agent_log(){ kubectl -n "$AGENT_NS" logs -l app.kubernetes.io/name=gpu-cr-node-agent --tail="${1:-60}" 2>/dev/null; }

diag(){  # NAME CR  -> dump why it failed
  local name=$1 cr=$2
  echo "  ---------------- DIAG $name ----------------"
  kubectl -n "$NS" get pod "$name" -o wide 2>/dev/null | sed 's/^/  /'
  echo "  -- pod events --"
  kubectl -n "$NS" describe pod "$name" 2>/dev/null | sed -n '/Events:/,$p' | tail -12 | sed 's/^/  /'
  echo "  -- container logs (tail) --"
  kubectl -n "$NS" logs "$name" --tail=20 2>/dev/null | sed 's/^/  /'
  if [ -n "$cr" ]; then
    echo "  -- GPUCheckpoint status --"
    kubectl -n "$NS" get gpucheckpoint "$cr" -o jsonpath='{.status.phase}{"\n"}{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}' 2>/dev/null | sed 's/^/  /'
  fi
  echo "  -- node-agent logs (tail) --"
  agent_log 40 | tail -40 | sed 's/^/  /'
  echo "  --------------------------------------------"
}

gpu_used(){ # count pods currently requesting a GPU in NS
  kubectl -n "$NS" get pods -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.containers[*].resources.limits.nvidia\.com/gpu}{"\n"}{end}' 2>/dev/null | awk 'NF==2{n++} END{print n+0}'; }

preflight(){
  echo "[preflight] cleaning previous bench pods/CRs ..."
  kubectl -n "$NS" delete gpucheckpoint --all --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete pod -l bench=gpu-cr --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
  if [ "$PRECLEAN_GPU" = 1 ]; then
    echo "[preflight] PRECLEAN_GPU=1: deleting ALL GPU-requesting pods in $NS"
    for p in $(kubectl -n "$NS" get pods -o json 2>/dev/null | python3 -c 'import sys,json;[print(i["metadata"]["name"]) for i in json.load(sys.stdin)["items"] if any(c.get("resources",{}).get("limits",{}).get("nvidia.com/gpu") for c in i["spec"]["containers"])]' 2>/dev/null); do
      kubectl -n "$NS" delete pod "$p" --force --grace-period=0 >/dev/null 2>&1 || true; done
  fi
  echo "[preflight] node GPU capacity/allocatable:"
  kubectl get nodes -o custom-columns='NODE:.metadata.name,GPU_CAP:.status.capacity.nvidia\.com/gpu,GPU_ALLOC:.status.allocatable.nvidia\.com/gpu' 2>/dev/null | sed 's/^/  /'
  # wait for GPU to be free (single-GPU node)
  local t0; t0=$(now)
  while [ "$(gpu_used)" != "0" ]; do
    echo "[preflight] waiting for GPU to free up (pods holding GPU: $(gpu_used)) ..."
    kubectl -n "$NS" get pods 2>/dev/null | sed 's/^/  /'
    awk "BEGIN{exit !($(elapsed "$t0")<120)}" || { echo "[preflight] GPU still busy after 120s; a foreign pod holds it. Delete it or set PRECLEAN_GPU=1."; break; }
    sleep 5
  done
  echo "[preflight] done."
}

cleanup(){ kubectl -n "$NS" delete gpucheckpoint "$1" --ignore-not-found >/dev/null 2>&1 || true
           kubectl -n "$NS" delete pod "$2" --force --grace-period=0 >/dev/null 2>&1 || true; }

set_mode(){
  local mode=$1 flag=true; [ "$mode" = baseline ] && flag=false
  echo "[bench] mode=$mode -> agent GCR_INTERCEPTION=$flag"
  kubectl -n "$AGENT_NS" set env ds/gpu-cr-node-agent GCR_INTERCEPTION=$flag >/dev/null 2>&1 || true
  kubectl -n "$AGENT_NS" rollout status ds/gpu-cr-node-agent --timeout=180s >/dev/null 2>&1 || true
}

run_one(){
  local mode=$1 fw=$2 model=$3
  local slug; slug=$(echo "$model" | tr '/.:' '---' | tr '[:upper:]' '[:lower:]')
  local name="b-${mode}-${fw}-${slug}"; name="${name:0:60}"
  local cr="ckpt-${name}"; cr="${cr:0:62}"
  echo "=== [$mode] $fw / $model  ($name) ==="

  if ! make_pod "$mode" "$name" "$fw" "$model" | kubectl apply -f - >/dev/null 2>&1; then
    echo "  deploy failed; skipping"; echo "$mode,$fw,$model,$name,,,,DeployError," >> "$OUT"; cleanup "$cr" "$name"; return 0; fi

  local r0; r0=$(now); local ready="" pp=""
  while awk "BEGIN{exit !($(elapsed "$r0")<$TIMEOUT)}"; do
    if kubectl -n "$NS" logs "$name" 2>/dev/null | grep -q '^READY'; then ready=1; break; fi
    pp=$(kubectl -n "$NS" get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    if [ "$pp" = "Failed" ] || [ "$pp" = "Succeeded" ]; then break; fi
    sleep 3
  done
  local ready_s; ready_s=$(elapsed "$r0")
  if [ -z "$ready" ]; then
    echo "  NOT READY in ${ready_s}s (pod phase=${pp:-unknown})"
    diag "$name" ""
    echo "$mode,$fw,$model,$name,$ready_s,,,NotReady," >> "$OUT"
    [ "$KEEP_FAILED" = 1 ] || cleanup "$cr" "$name"
    return 0; fi
  echo "  $(kubectl -n "$NS" logs "$name" 2>/dev/null | grep '^READY' | tail -1)  (ready ${ready_s}s)"

  if ! sed -e "s|__CR__|$cr|g" -e "s|__NAME__|$name|g" "$here/gpucheckpoint.tmpl.yaml" | kubectl apply -f - >/dev/null 2>&1; then
    echo "  CR apply failed; skipping"; echo "$mode,$fw,$model,$name,$ready_s,,,CRError," >> "$OUT"; cleanup "$cr" "$name"; return 0; fi
  local c0; c0=$(now); local phase=""
  while awk "BEGIN{exit !($(elapsed "$c0")<$TIMEOUT)}"; do
    phase=$(kubectl -n "$NS" get gpucheckpoint "$cr" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    [ "$phase" = "Completed" ] && break
    [ "$phase" = "Failed" ] && break
    sleep 1
  done
  [ -z "$phase" ] && phase="Timeout"
  local wall; wall=$(elapsed "$c0")
  local took; took=$(agent_log 400 | grep -oE 'took [0-9.]+[a-z]+' | tail -1 | awk '{print $2}')
  local path; path=$(kubectl -n "$NS" get gpucheckpoint "$cr" -o jsonpath='{.status.lastCheckpointPath}' 2>/dev/null || echo "")
  echo "  phase=$phase  checkpoint_took=${took:-?}  wall=${wall}s  path=$path"
  echo "$mode,$fw,$model,$name,$ready_s,${took:-},$wall,$phase,$path" >> "$OUT"
  if [ "$phase" != "Completed" ]; then
    diag "$name" "$cr"
    [ "$KEEP_FAILED" = 1 ] && { echo "  KEEP_FAILED=1: leaving $name / $cr for inspection"; return 0; }
  fi
  cleanup "$cr" "$name"
  return 0
}

preflight
echo "mode,framework,model,pod,ready_s,checkpoint_took,wall_s,phase,path" > "$OUT"
for mode in $MODES; do
  set_mode "$mode"
  for c in "${CONFIGS[@]}"; do run_one "$mode" $c || echo "  (config errored, continuing)"; done
done
echo; echo "[bench] results -> $OUT"; column -t -s, "$OUT" 2>/dev/null || cat "$OUT"
echo "[bench] checkpoint tars are on the worker: ls -lh /var/lib/gcr-checkpoint/"
