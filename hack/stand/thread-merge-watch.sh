# Runs in a backend pod (busybox sh): from the thread log passing the tail size
# to the new base, in hundredths of a second of uptime.
# Usage: sh thread-merge-watch.sh <log> <base> <tail-size>
L="$1"; B="$2"; T="$3"; end=$(( $(date +%s) + 900 ))
UPTIME="${UPTIME_FILE:-/proc/uptime}"
# busybox date has no %N and busybox awk's %d is 32-bit: the digits of the
# uptime, whose two decimals make it hundredths.
now() { read -r up _ < "$UPTIME"; echo "${up%.*}${up#*.}"; }
b0=$(stat -c %y "$B" 2>/dev/null); t0=""
while [ "$(date +%s)" -lt "$end" ]; do
  s=$(stat -c %s "$L" 2>/dev/null || echo 0)
  if [ -z "$t0" ] && [ "$s" -gt "$T" ]; then t0=$(now); fi
  b=$(stat -c %y "$B" 2>/dev/null)
  if [ "$b" != "$b0" ]; then
    echo "trigger_seen_cs=${t0:-none} base_seen_cs=$(now) base_size=$(stat -c %s "$B") log_after=$(stat -c %s "$L")"
    exit 0
  fi
  usleep 5000 2>/dev/null || sleep 0.005
done
echo "watch: no new base in 900 s (log $(stat -c %s "$L"))"
