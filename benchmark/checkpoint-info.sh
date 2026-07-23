#!/usr/bin/env bash
# checkpoint-info.sh — list every GPU checkpoint under a directory with its tar
# info, matching .blob, and the Pod UID that produced it.
#
# The Pod UID is recovered from the CRI-O checkpoint metadata inside each tar
# (spec.dump / config.dump -> "io.kubernetes.pod.uid"), so it works even after
# the Pods have been deleted.
#
# Usage:
#   bash benchmark/checkpoint-info.sh [DIR]
#     DIR   directory holding checkpoint-*.tar (default: $CKPT_DIR or /mnt/nfs/gcr)
#   CSV=out.csv bash benchmark/checkpoint-info.sh /mnt/nfs/gcr   # also write CSV
set -uo pipefail

DIR="${1:-${CKPT_DIR:-/mnt/nfs/gcr}}"
if [ ! -d "$DIR" ]; then
  echo "usage: $0 <checkpoint-dir>   (directory containing checkpoint-*.tar)" >&2
  echo "  tried: $DIR" >&2
  exit 1
fi

hsize() { [ -e "$1" ] && du -h --apparent-size "$1" 2>/dev/null | cut -f1 || echo "-"; }

pod_uid() {
  # Read only the small metadata members (tar stops at first match) and grep the
  # kube pod-uid annotation. Try both CRI-O metadata files.
  local tar="$1" uid=""
  for m in spec.dump config.dump; do
    uid=$(tar --occurrence=1 -xOf "$tar" "$m" 2>/dev/null \
          | grep -oE '"io\.kubernetes\.pod\.uid" *: *"[^"]+"' \
          | head -1 | sed -E 's/.*: *"([^"]+)"/\1/')
    [ -n "$uid" ] && { echo "$uid"; return; }
  done
  echo "?"
}

parse_name() {  # checkpoint-<pod>_<ns-container-epoch>.tar -> POD  EPOCH
  local b="$1"; b="${b#checkpoint-}"; b="${b%.tar}"
  local pod="${b%%_*}" rest="${b#*_}"
  local epoch="${rest##*-}"
  printf '%s\t%s' "$pod" "$epoch"
}

CSV="${CSV:-}"
[ -n "$CSV" ] && echo "pod,pod_uid,tar_bytes,blob_bytes,timestamp,tar_path" > "$CSV"

printf '%s\n' "checkpoints under: $DIR"
{
  printf 'POD\tTAR\tBLOB\tWHEN\tPOD_UID\tPATH\n'
  # Sort by name so replicas/models group together.
  while IFS= read -r tar; do
    base="$(basename "$tar")"
    IFS=$'\t' read -r pod epoch < <(parse_name "$base")
    blob="${tar%.tar}.blob"
    when="-"; [[ "$epoch" =~ ^[0-9]+$ ]] && when="$(date -d "@$epoch" '+%Y-%m-%d %H:%M:%S' 2>/dev/null || echo "$epoch")"
    uid="$(pod_uid "$tar")"
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' \
      "$pod" "$(hsize "$tar")" "$(hsize "$blob")" "$when" "$uid" "$tar"
    if [ -n "$CSV" ]; then
      tb=$(stat -c %s "$tar" 2>/dev/null || echo 0)
      bb=$(stat -c %s "$blob" 2>/dev/null || echo 0)
      echo "$pod,$uid,$tb,$bb,$when,$tar" >> "$CSV"
    fi
  done < <(find "$DIR" -type f -name 'checkpoint-*.tar' | sort)
} | column -t -s$'\t'

[ -n "$CSV" ] && echo "CSV written: $CSV"
