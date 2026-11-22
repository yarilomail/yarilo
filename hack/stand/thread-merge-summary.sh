#!/usr/bin/env bash
# Summary of a thread-merge.sh run: per merge its time, the delivery that
# triggered it and the 30 after; memory per account; deliveries not 250.
# Usage: bash hack/stand/thread-merge-summary.sh <out-dir>
D="${1:?dir}"
echo "--- merges"
sed -n 's/^[^ ]* \([^ ]*\) \(merge-[0-9]*\) \([^:]*\): trigger_seen_cs=\([0-9]*\) base_seen_cs=\([0-9]*\).*base_size=\([0-9]*\).*/\1 \2 \3 \4 \5 \6/p' "$D/run.log" |
  awk '{printf "%s %s %s merge %.2f s, base %d B\n", $1, $2, $3, ($5-$4)/100, $6}'
grep -h 'watch: no new base\|no crossing\|not copied' "$D/run.log" || true
echo "--- deliveries"
awk '
/^== /{h=$2" "$3; f=0; n=0; d=""; next}
/log_after/{
  for(i=1;i<=NF;i++){split($i,kv,"="); v[kv[1]]=kv[2]}
  if(!f && (v["log_after"]+0>tail+0 || v["log_after"]+0<v["log_before"]+0)){f=1; print h, "trigger data_ms=" v["data_ms"]; next}
  if(f && n<30){d=d" "v["data_ms"]; n++; if(n==30) print h, "after:" d}
}' tail="${YARILO_THREAD_TAIL:-131072}" "$D/phases.txt"
echo "--- memory"
cat "$D/mem.txt" 2>/dev/null
# grep -c exits 1 on a count of none: the count decides the exit, not grep.
bad=$(cat "$D/fill.txt" "$D/phases.txt" 2>/dev/null | grep 'phase:' | grep -vc 'data=250' || true)
echo "--- deliveries not 250: ${bad:-0}"
[ "${bad:-0}" -eq 0 ]
