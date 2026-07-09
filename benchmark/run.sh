#!/usr/bin/env bash
# GPU checkpoint benchmark across single-process inference frameworks.
# Runs on the MASTER (needs kubectl). For each (framework, model): deploy the
# inference pod, wait until it prints READY (model loaded + warmed up on the GPU),
# take a one-shot GPUCheckpoint, and record the checkpoint time + the produced
# tar path. Uses PURE CRIUgpu (GCR_INTERCEPTION=false) so it is framework-agnostic.
#
#   bash benchmark/run.sh
#   OUT=my.csv TIMEOUT=900 bash benchmark/run.sh
set -Eeuo pipefail
NS=${NS:-default}
AGENT_NS=${AGENT_NS:-gpu-cr-system}
OUT=${OUT:-bench-results.csv}
TIMEOUT=${TIMEOUT:-900}
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

echo "[bench] agent -> pure CRIUgpu (GCR_INTERCEPTION=false)"
kubectl -n "$AGENT_NS" set env ds/gpu-cr-node-agent GCR_INTERCEPTION=false >/dev/null
kubectl -n "$AGENT_NS" rollout status ds/gpu-cr-node-agent --timeout=180s >/dev/null || true

echo "framework,model,pod,ready_s,checkpoint_took,wall_s,phase,path" > "$OUT"

run_one(){
  local fw=$1 model=$2
  local name="bench-${fw}-$(echo "$model" | tr '/.:' '---' | tr '[:upper:]' '[:lower:]')"; name="${name:0:60}"
  local cr="ckpt-${name}"; cr="${cr:0:62}"
  local tmpl="$here/infer-${fw}.tmpl.yaml"
  [ -f "$tmpl" ] || { echo "  no template $tmpl; skip"; return; }
  echo "=== $fw / $model  ($name) ==="
  sed -e "s|__NAME__|$name|g" -e "s|__MODEL__|$model|g" "$tmpl" | kubectl apply -f - >/dev/null

  # wait for READY (model loaded + warmed up)
  local r0; r0=$(now); local ready=""
  while awk "BEGIN{exit !($(elapsed "$r0")<$TIMEOUT)}"; do
    kubectl -n "$NS" logs "$name" 2>/dev/null | grep -q '^READY' && { ready=1; break; }
    kubectl -n "$NS" get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null | grep -qE 'Failed' && break
    sleep 3
  done
  local ready_s; ready_s=$(elapsed "$r0")
  if [ -z "$ready" ]; then echo "  NOT READY in ${ready_s}s; skipping"; kubectl -n "$NS" delete pod "$name" --force --grace-period=0 >/dev/null 2>&1 || true; return; fi
  echo "  $(kubectl -n "$NS" logs "$name" | grep '^READY' | tail -1)  (ready ${ready_s}s)"

  # one-shot checkpoint + timing
  sed -e "s|__CR__|$cr|g" -e "s|__NAME__|$name|g" "$here/gpucheckpoint.tmpl.yaml" | kubectl apply -f - >/dev/null
  local c0; c0=$(now); local phase=""
  while awk "BEGIN{exit !($(elapsed "$c0")<$TIMEOUT)}"; do
    phase=$(kubectl -n "$NS" get gpucheckpoint "$cr" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    [ "$phase" = Completed ] && break
    [ "$phase" = Failed ] && break
    sleep 1
  done
  local wall; wall=$(elapsed "$c0")
  local took; took=$(kubectl -n "$AGENT_NS" logs -l app.kubernetes.io/name=gpu-cr-node-agent --tail=400 2>/dev/null | grep -oE 'took [0-9.]+[a-z]+' | tail -1 | awk '{print $2}')
  local path; path=$(kubectl -n "$NS" get gpucheckpoint "$cr" -o jsonpath='{.status.lastCheckpointPath}' 2>/dev/null || true)
  echo "  phase=$phase  checkpoint_took=${took:-?}  wall=${wall}s  path=$path"
  echo "$fw,$model,$name,$ready_s,${took:-},$wall,$phase,$path" >> "$OUT"

  kubectl -n "$NS" delete gpucheckpoint "$cr" --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete pod "$name" --force --grace-period=0 >/dev/null 2>&1 || true
}

for c in "${CONFIGS[@]}"; do run_one $c; done
echo; echo "[bench] results -> $OUT"; column -t -s, "$OUT" 2>/dev/null || cat "$OUT"
echo "[bench] tar sizes are on the worker: ls -lh /var/lib/gcr-checkpoint/"
