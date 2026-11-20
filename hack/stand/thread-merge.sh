#!/usr/bin/env bash
# Times the thread log's merge into its base on the stand (#2621): per
# account, a backend restart (so the memory peak is this account's), the log
# filled to just short of threading_tail_size, then one delivery at a time
# until one crosses it and 30 more while the merge runs. A watcher in each
# backend pod gives the time from the log passing the size to the new base.
#
# Usage:
#   KUBECONFIG=... bash hack/stand/thread-merge.sh <out-dir> <merges> <user> <thread-dir> [<user> <thread-dir> ...]
#
# <thread-dir> is the directory holding the account's `threads` file in the
# pods. The stand's tag is the one deployed; this script deploys nothing.

set -uo pipefail

OUT="${1:?out dir}"
MERGES="${2:?merges per account}"
shift 2
[ $# -ge 2 ] && [ $(($# % 2)) -eq 0 ] || { echo "thread-merge: pairs of <user> <thread-dir>" >&2; exit 2; }
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
TAIL="${YARILO_THREAD_TAIL:-131072}"
HERE="$(cd "$(dirname "$0")" && pwd)"
PODS="yarilo-backend-0 yarilo-backend-1"

mkdir -p "$OUT"
kube() { kubectl --kubeconfig="$KCFG" -n "$NS" --request-timeout=60s "$@"; }
log() { echo "$(date -u +%FT%TZ) $*" | tee -a "$OUT/run.log"; }
size() { kube exec yarilo-backend-0 -c yarilo-lmtp -- stat -c %s "$1" 2>/dev/null || echo 0; }

# memory prints the lmtp container's peak, and its anon and file now: the peak
# counts page cache, so the split says what of it is the process.
memory() {
  local p
  for p in $PODS; do
    echo "$1 $p peak=$(kube exec "$p" -c yarilo-lmtp -- cat /sys/fs/cgroup/memory.peak)" \
      "$(kube exec "$p" -c yarilo-lmtp -- grep -E '^(anon|file) ' /sys/fs/cgroup/memory.stat | tr ' \n' '= ')"
  done >> "$OUT/mem.txt"
}

# watchers copies the watcher into every pod and checks it arrived: exec
# without -i passes no stdin, and an empty watcher measures nothing.
watchers() {
  local p
  for p in $PODS; do
    kube exec -i "$p" -c yarilo-lmtp -- sh -c 'cat > /tmp/thread-merge-watch.sh' < "$HERE/thread-merge-watch.sh"
    [ "$(kube exec "$p" -c yarilo-lmtp -- stat -c %s /tmp/thread-merge-watch.sh)" -gt 0 ] || { log "watcher not copied to $p"; exit 1; }
  done
}

merge_once() {
  local u="$1" dir="$2" label="$3" L="$2/threads" B="$2/threads.base" p i sz before after out
  log "$u $label: log $(size "$L") B"
  # A batch of 50 adds about 10 KB: batches stop 20 KB short of the size, so
  # none crosses it before the watchers run.
  while [ "$(size "$L")" -lt $((TAIL - 20000)) ]; do
    bash "$HERE/lmtp-phases.sh" "$u" 50 >> "$OUT/fill.txt"
  done
  for p in $PODS; do
    kube exec "$p" -c yarilo-lmtp -- sh /tmp/thread-merge-watch.sh "$L" "$B" "$TAIL" > "$OUT/watch-$label-$u-$p.txt" 2>&1 &
  done
  echo "== $u $label" >> "$OUT/phases.txt"
  before=$(size "$L"); i=0; after=-1
  while :; do
    out=$(bash "$HERE/lmtp-phases.sh" "$u" 1)
    sz=$(size "$L")
    echo "$out log_before=$before log_after=$sz" >> "$OUT/phases.txt"
    if [ "$after" -lt 0 ] && { [ "$before" -le "$TAIL" ] && [ "$sz" -gt "$TAIL" ] || [ "$sz" -lt "$before" ]; }; then after=0; fi
    [ "$after" -ge 0 ] && after=$((after + 1))
    [ "$after" -gt 30 ] && break
    i=$((i + 1))
    [ "$i" -gt 400 ] && { log "$u $label: no crossing in 400 deliveries"; break; }
    before=$sz
  done
  wait
  for p in $PODS; do
    log "$u $label $p: $(cat "$OUT/watch-$label-$u-$p.txt")"
  done
}

while [ $# -gt 0 ]; do
  user="$1" dir="$2"
  shift 2
  log "$user: backend restart"
  kube rollout restart sts/yarilo-backend >> "$OUT/run.log" 2>&1
  kube rollout status sts/yarilo-backend >> "$OUT/run.log" 2>&1
  watchers
  for k in $(seq 1 "$MERGES"); do
    merge_once "$user" "$dir" "merge-$k"
  done
  memory "$user"
done
log "done"
bash "$HERE/thread-merge-summary.sh" "$OUT" | tee "$OUT/summary.txt"
