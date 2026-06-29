#!/usr/bin/env bash
# =============================================================================
# gpu-cr-cuda-helper — runs cuda-checkpoint on the HOST for the in-container agent
#
# Why: the GPU C/R node agent runs inside a container whose glibc differs from
# the host. cuda-checkpoint is a host binary and "stack smashing detected" /
# aborts when executed from the container (even via nsenter). Running it as a
# normal HOST process works perfectly. This helper is that host process.
#
# Protocol — files under REQ_DIR (default /var/lib/gpu-cr/cuda-req):
#   agent writes  <id>.req   one line:  "<op> <container_pid>"   op = toggle|state
#   helper writes <id>.res :  line1 = exit code (0 ok), rest = detail
#
# For each request it finds the GPU-using PIDs that are descendants of
# <container_pid> (from `nvidia-smi` compute-apps) and applies cuda-checkpoint to
# them — so it targets the real CUDA process, not the container's init PID.
# =============================================================================
set -u
REQ_DIR="${GPU_CR_CUDA_REQ_DIR:-/var/lib/gpu-cr/cuda-req}"
BIN="${CUDA_CHECKPOINT_BIN:-/usr/bin/cuda-checkpoint}"
mkdir -p "$REQ_DIR"

is_descendant() {  # anc pid  -> 0 if pid == anc or a descendant of anc
  local anc=$1 p=$2 i=0 ppid
  while [ -n "$p" ] && [ "$p" -gt 0 ] 2>/dev/null && [ "$i" -lt 64 ]; do
    [ "$p" = "$anc" ] && return 0
    ppid=$(awk '/^PPid:/{print $2; exit}' "/proc/$p/status" 2>/dev/null)
    [ -z "$ppid" ] && return 1
    p=$ppid; i=$((i+1))
  done
  return 1
}

list_descendant_pids() {  # anc -> every pid in its subtree (incl. anc)
  local anc=$1 d pid
  for d in /proc/[0-9]*; do
    pid=${d#/proc/}
    is_descendant "$anc" "$pid" && echo "$pid"
  done
}

# Find the CUDA process(es) under the container by probing cuda-checkpoint
# --get-state (exit 0 only for a real CUDA process). Unlike nvidia-smi this also
# finds an already-suspended/checkpointed process (no GPU memory) so RESUME works.
gpu_pids_under() {  # container_pid -> CUDA pids in its subtree
  local cpid=$1 pid st
  for pid in $(list_descendant_pids "$cpid"); do
    if st=$(timeout 5 "$BIN" --get-state --pid "$pid" 2>/dev/null) && [ -n "$st" ]; then
      echo "$pid"
    fi
  done
}

handle() {
  local f=$1 op cpid rc=0 out="" pids p o r
  read -r op cpid < "$f"
  pids=$(gpu_pids_under "${cpid:-0}")
  if [ -z "$pids" ]; then
    printf '3\nno GPU process under container pid %s\n' "${cpid:-?}" > "${f%.req}.res"
    return
  fi
  case "$op" in
    toggle)
      for p in $pids; do
        o=$("$BIN" --toggle --pid "$p" 2>&1); r=$?
        [ "$r" -ne 0 ] && rc=$r
        out+="pid $p toggle rc=$r ${o}"$'\n'
      done ;;
    state)
      for p in $pids; do out+="pid $p $("$BIN" --get-state --pid "$p" 2>&1)"$'\n'; done ;;
    *) rc=2; out="unknown op: $op" ;;
  esac
  printf '%s\n%s' "$rc" "$out" > "${f%.req}.res"
}

echo "[gpu-cr-cuda-helper] watching ${REQ_DIR} (bin=${BIN})"
while true; do
  shopt -s nullglob
  for f in "$REQ_DIR"/*.req; do
    handle "$f"
    rm -f "$f"
  done
  sleep 0.2
done
