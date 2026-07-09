#!/usr/bin/env bash
# GPU checkpoint benchmark across single-process inference frameworks.
# Runs on the MASTER (needs kubectl). For each (framework, model) and each MODE:
#   deploy inference pod -> wait until it prints READY (model loaded + warmed on GPU)
#   -> one-shot GPUCheckpoint -> record checkpoint time + tar path -> cleanup.
#
# MODES (this is the whole point of the benchmark):
#   gcr      = OUR system: Selective-Interception DATA freeze/remap + CRIUgpu control
#              (agent GCR_INTERCEPTION=true; pod carries the LD_PRELOAD interceptor)
#   baseline = pure CRIUgpu, no interception (agent GCR_INTERCEPTION=false; plain pod)
# Same models under both modes -> the CSV lets you compare ours vs baseline directly.
#
# Robust: ANY error on one config is caught, recorded, and the run continues.
#
#   bash benchmark/run.sh
#   MODES=gcr bash benchmark/run.sh           # only our system
#   MODES="gcr baseline" TIMEOUT=1200 OUT=res.csv bash benchmark/run.sh
set -uo pipefail                       # no -e: never abort the whole batch on one config
NS=${NS:-default}
AGENT_NS=${AGENT_NS:-gpu-cr-system}
OUT=${OUT:-bench-results.csv}
TIMEOUT=${TIMEOUT:-900}
MODES=${MODES:-"gcr baseline"}         # our system first, then baseline for comparison
here="$(cd "$(dirname "$0")" && pwd)"
now(){ date +%s.%N; }
elapsed(){ awk "BEGIN{printf \"%.1f\", $(now)-$1}"; }

# framework model   (edit freely; PyTorch model size sweeps the GPU footprint)
CONFIGS=(
  "pytorch gpt2"
  "pytorch gpt2-large"
  "pytorch facebook/opt-1.3b"
  "pytorch facebook/opt-6.7b"
  "tensorflow ResNet50"
  "tensorflow EfficientNetB7"
)

fw_image(){ case $1 in
  pytorch)    echo "pytorch/pytorch:2.4.0-cuda12.1-cudnn9-runtime";;
  tensorflow) echo "tensorflow/tensorflow:2.15.0-gpu";;
esac; }
fw_pip(){ case $1 in
  pytorch)    echo '"transformers>=4.44" accelerate sentencepiece';;
  tensorflow) echo '';;
esac; }

# make_pod MODE NAME FRAMEWORK MODEL  ->  full pod YAML on stdout
make_pod(){
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

cleanup(){ kubectl -n "$NS" delete gpucheckpoint "$1" --ignore-not-found >/dev/null 2>&1 || true
           kubectl -n "$NS" delete pod "$2" --force --grace-period=0 >/dev/null 2>&1 || true; }

set_mode(){  # switch the node-agent between our-system and baseline
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

  local r0; r0=$(now); local ready=""
  while awk "BEGIN{exit !($(elapsed "$r0")<$TIMEOUT)}"; do
    if kubectl -n "$NS" logs "$name" 2>/dev/null | grep -q '^READY'; then ready=1; break; fi
    local pp; pp=$(kubectl -n "$NS" get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    [ "$pp" = "Failed" ] || [ "$pp" = "Succeeded" ] && break
    sleep 3
  done
  local ready_s; ready_s=$(elapsed "$r0")
  if [ -z "$ready" ]; then
    echo "  NOT READY in ${ready_s}s (kubectl logs $name); skipping"
    echo "$mode,$fw,$model,$name,$ready_s,,,NotReady," >> "$OUT"; cleanup "$cr" "$name"; return 0; fi
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
  local took; took=$(kubectl -n "$AGENT_NS" logs -l app.kubernetes.io/name=gpu-cr-node-agent --tail=400 2>/dev/null | grep -oE 'took [0-9.]+[a-z]+' | tail -1 | awk '{print $2}')
  local path; path=$(kubectl -n "$NS" get gpucheckpoint "$cr" -o jsonpath='{.status.lastCheckpointPath}' 2>/dev/null || echo "")
  echo "  phase=$phase  checkpoint_took=${took:-?}  wall=${wall}s  path=$path"
  echo "$mode,$fw,$model,$name,$ready_s,${took:-},$wall,$phase,$path" >> "$OUT"
  cleanup "$cr" "$name"
  return 0
}

echo "mode,framework,model,pod,ready_s,checkpoint_took,wall_s,phase,path" > "$OUT"
for mode in $MODES; do
  set_mode "$mode"
  for c in "${CONFIGS[@]}"; do run_one "$mode" $c || echo "  (config errored, continuing)"; done
done
echo; echo "[bench] results -> $OUT"; column -t -s, "$OUT" 2>/dev/null || cat "$OUT"
echo "[bench] checkpoint tars are on the worker: ls -lh /var/lib/gcr-checkpoint/"
