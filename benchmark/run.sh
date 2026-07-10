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
GCR_SKIP_FW=${GCR_SKIP_FW:-}   # frameworks to skip in gcr mode (interceptor unsupported), e.g. "tensorflow"
# --- checkpoint storage backend (mirrors spec.storage) ---
STORAGE_TYPE=${STORAGE_TYPE:-hostPath}     # hostPath | mount | nfs | pvc
STORAGE_PATH=${STORAGE_PATH:-/var/lib/gcr-checkpoint}   # hostPath dir, or subdir for mount/pvc
STORAGE_ENDPOINT=${STORAGE_ENDPOINT:-}     # nfs server
STORAGE_FSTYPE=${STORAGE_FSTYPE:-}         # mount fsType: nfs4|cifs|ceph|...
STORAGE_SOURCE=${STORAGE_SOURCE:-}         # mount source, e.g. 10.178.0.15:/mnt/nfs
STORAGE_OPTIONS=${STORAGE_OPTIONS:-}       # mount -o options
STORAGE_SUBPATH=${STORAGE_SUBPATH:-}       # subdir under the backend
STORAGE_CLAIM=${STORAGE_CLAIM:-}           # pvc claimName
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
  pytorch)    echo "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime";;
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
    env_extra=$'        - { name: GCR_VMM_ALLOC, value: "1" }\n        - { name: LD_PRELOAD, value: /opt/gpu-cr/libgcr-interceptor.so }\n        - { name: GCR_HOME, value: /opt/gpu-cr }\n        - name: GCR_POD_UID\n          valueFrom: { fieldRef: { fieldPath: metadata.uid } }\n        - { name: GCR_CONTROL_DIR, value: /var/lib/gpu-cr/run }\n        - { name: GCR_DATA_DIR, value: /var/lib/gcr-data }'
    vmounts=$'      volumeMounts:\n        - { name: gpu-cr-lib, mountPath: /opt/gpu-cr, readOnly: true }\n        - { name: gpu-cr-run, mountPath: /var/lib/gpu-cr/run }\n        - { name: gcr-checkpoint, mountPath: /var/lib/gcr-checkpoint }\n        - { name: gcr-data, mountPath: /var/lib/gcr-data }'
    vols=$'  volumes:\n    - name: gpu-cr-lib\n      hostPath: { path: /var/lib/gpu-cr/lib, type: Directory }\n    - name: gpu-cr-run\n      hostPath: { path: /var/lib/gpu-cr/run, type: DirectoryOrCreate }\n    - name: gcr-checkpoint\n      hostPath: { path: /var/lib/gcr-checkpoint, type: DirectoryOrCreate }\n    - name: gcr-data\n      hostPath: { path: /var/lib/gcr-data, type: DirectoryOrCreate }'
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
getnum(){ echo "$1" | grep -oE "$2=[0-9.]+" | head -1 | cut -d= -f2; }
row(){ local IFS=,; echo "$*" >> "$OUT"; }   # 17 fields, empties allowed
storage_block(){   # emit the `storage:` mapping for the current STORAGE_* env
  case "$STORAGE_TYPE" in
    hostPath|"") echo "  storage: { type: hostPath, path: $STORAGE_PATH }";;
    nfs)         echo "  storage: { type: nfs, endpoint: $STORAGE_ENDPOINT, path: $STORAGE_PATH${STORAGE_SUBPATH:+, subPath: $STORAGE_SUBPATH} }";;
    mount)       echo "  storage: { type: mount, fsType: $STORAGE_FSTYPE, source: $STORAGE_SOURCE${STORAGE_OPTIONS:+, options: \"$STORAGE_OPTIONS\"}${STORAGE_SUBPATH:+, subPath: $STORAGE_SUBPATH} }";;
    pvc)         echo "  storage: { type: pvc, claimName: $STORAGE_CLAIM${STORAGE_SUBPATH:+, subPath: $STORAGE_SUBPATH} }";;
    *)           echo "  storage: { type: $STORAGE_TYPE, path: $STORAGE_PATH }";;
  esac
}
make_cr(){   # CR YAML for one checkpoint: make_cr <crName> <podName>
  cat <<EOF
apiVersion: gpu-cr.io/v1alpha1
kind: GPUCheckpoint
metadata: { name: $1, namespace: $NS }
spec:
  workloadRef: { kind: Pod, namespace: $NS, name: $2, container: cuda-app }
$(storage_block)
  schedule: ""
EOF
}

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
  if [ "$mode" = gcr ]; then case " $GCR_SKIP_FW " in *" $fw "*)
    echo "  [skip] $fw not supported by the interceptor in gcr mode"
    row "$mode" "$fw" "$model" "$name" "" "" "" "" "" "" "" "" "" "" "" SkippedGCR ""; return 0;; esac; fi

  if ! make_pod "$mode" "$name" "$fw" "$model" | kubectl apply -f - >/dev/null 2>&1; then
    echo "  deploy failed; skipping"; row "$mode" "$fw" "$model" "$name" "" "" "" "" "" "" "" "" "" "" "" DeployError ""; cleanup "$cr" "$name"; return 0; fi

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
    row "$mode" "$fw" "$model" "$name" "$ready_s" "" "" "" "" "" "" "" "" "" "" NotReady ""
    [ "$KEEP_FAILED" = 1 ] || cleanup "$cr" "$name"
    return 0; fi
  echo "  $(kubectl -n "$NS" logs "$name" 2>/dev/null | grep '^READY' | tail -1)  (ready ${ready_s}s)"

  if ! make_cr "$cr" "$name" | kubectl apply -f - >/dev/null 2>&1; then
    echo "  CR apply failed; skipping"; row "$mode" "$fw" "$model" "$name" "$ready_s" "" "" "" "" "" "" "" "" "" "" CRError ""; cleanup "$cr" "$name"; return 0; fi
  local c0; c0=$(now); local phase=""
  while awk "BEGIN{exit !($(elapsed "$c0")<$TIMEOUT)}"; do
    phase=$(kubectl -n "$NS" get gpucheckpoint "$cr" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    [ "$phase" = "Completed" ] && break
    [ "$phase" = "Failed" ] && break
    sleep 1
  done
  [ -z "$phase" ] && phase="Timeout"
  local wall; wall=$(elapsed "$c0")
  local path; path=$(kubectl -n "$NS" get gpucheckpoint "$cr" -o jsonpath='{.status.lastCheckpointPath}' 2>/dev/null || echo "")

  # locate the node-agent that ran this checkpoint (same node as the pod)
  local node agentpod
  node=$(kubectl -n "$NS" get pod "$name" -o jsonpath='{.spec.nodeName}' 2>/dev/null)
  agentpod=$(kubectl -n "$AGENT_NS" get pods -l app.kubernetes.io/name=gpu-cr-node-agent -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | awk -v n="$node" '$2==n{print $1; exit}')

  # agent-side phase breakdown for THIS pod (freeze/kubelet/store/remap/total)
  local pt=""
  [ -n "$agentpod" ] && pt=$(kubectl -n "$AGENT_NS" logs "$agentpod" --tail=1500 2>/dev/null | grep -F "PHASE_TIMES pod=$NS/$name " | tail -1)
  [ -z "$pt" ] && pt=$(agent_log 1500 | grep -F "PHASE_TIMES pod=$NS/$name " | tail -1)
  local freeze_s kubelet_s store_s remap_s total_s
  freeze_s=$(getnum "$pt" freeze_s); kubelet_s=$(getnum "$pt" kubelet_s)
  store_s=$(getnum "$pt" store_s);   remap_s=$(getnum "$pt" remap_s); total_s=$(getnum "$pt" total_s)

  # bytes moved by the Selective Interception freeze (from the pod's own interceptor log)
  local freeze_bytes; freeze_bytes=$(kubectl -n "$NS" logs "$name" 2>/dev/null | grep -oE '[0-9]+ bytes copied to host' | grep -oE '^[0-9]+' | tail -1)

  # intra-CRIUgpu split from the tar's dump.log (agent mounts /var/lib/gcr-checkpoint)
  local tar_bytes cuda_s criu_s cpu_s crio_tar_s
  if [ "$phase" = Completed ] && [ -n "$agentpod" ] && [ -n "$path" ]; then
    tar_bytes=$(kubectl -n "$AGENT_NS" exec "$agentpod" -- sh -c "stat -c %s '$path' 2>/dev/null" 2>/dev/null | tr -dc '0-9')
    local dl; dl=$(kubectl -n "$AGENT_NS" exec "$agentpod" -- sh -c "tar tf '$path' 2>/dev/null | grep -m1 -E 'dump\.log$'" 2>/dev/null | tr -d '\r')
    if [ -n "$dl" ]; then
      local tmp; tmp=$(mktemp)
      kubectl -n "$AGENT_NS" exec "$agentpod" -- sh -c "tar xOf '$path' '$dl' 2>/dev/null" > "$tmp" 2>/dev/null
      criu_s=$(awk -F'[()]' '/^\([0-9]/{t=$2} END{if(t!="")printf "%.3f", t}' "$tmp")
      local cs ce
      cs=$(grep -m1 cuda_plugin "$tmp" | sed -nE 's/^\(([0-9.]+)\).*/\1/p')
      ce=$(grep cuda_plugin "$tmp" | tail -1 | sed -nE 's/^\(([0-9.]+)\).*/\1/p')
      [ -n "$cs" ] && [ -n "$ce" ] && cuda_s=$(awk "BEGIN{printf \"%.3f\", $ce-$cs}")
      [ -n "$criu_s" ] && [ -n "$cuda_s" ] && cpu_s=$(awk "BEGIN{v=$criu_s-$cuda_s; if(v<0)v=0; printf \"%.3f\", v}")
      [ -n "$kubelet_s" ] && [ -n "$criu_s" ] && crio_tar_s=$(awk "BEGIN{v=$kubelet_s-$criu_s; if(v<0)v=0; printf \"%.3f\", v}")
      rm -f "$tmp"
    fi
  fi

  echo "  phase=$phase total=${total_s:-$wall}s | freeze=${freeze_s:-0} cuda_plugin=${cuda_s:-?} cpu_dump=${cpu_s:-?} crio_tar=${crio_tar_s:-?} store=${store_s:-0} remap=${remap_s:-0} | tar=${tar_bytes:-?}B path=$path"
  row "$mode" "$fw" "$model" "$name" "$ready_s" "${total_s:-$wall}" "${freeze_s:-}" "${kubelet_s:-}" "${cuda_s:-}" "${cpu_s:-}" "${crio_tar_s:-}" "${store_s:-}" "${remap_s:-}" "${freeze_bytes:-}" "${tar_bytes:-}" "$phase" "$path"
  if [ "$phase" != "Completed" ]; then
    diag "$name" "$cr"
    [ "$KEEP_FAILED" = 1 ] && { echo "  KEEP_FAILED=1: leaving $name / $cr for inspection"; return 0; }
  fi
  cleanup "$cr" "$name"
  return 0
}

preflight
echo "[bench] storage: type=$STORAGE_TYPE ${STORAGE_SOURCE:+source=$STORAGE_SOURCE }${STORAGE_ENDPOINT:+endpoint=$STORAGE_ENDPOINT }${STORAGE_CLAIM:+claim=$STORAGE_CLAIM }path=$STORAGE_PATH"
echo "mode,framework,model,pod,ready_s,total_s,freeze_s,kubelet_s,cuda_plugin_s,cpu_dump_s,crio_tar_s,store_s,remap_s,freeze_bytes,tar_bytes,phase,path" > "$OUT"
for mode in $MODES; do
  set_mode "$mode"
  for c in "${CONFIGS[@]}"; do run_one "$mode" $c || echo "  (config errored, continuing)"; done
done
echo; echo "[bench] results -> $OUT"; column -t -s, "$OUT" 2>/dev/null || cat "$OUT"
echo "[bench] checkpoint tars: storage=$STORAGE_TYPE (see the path column; for hostPath: ls -lh $STORAGE_PATH on the worker)"
