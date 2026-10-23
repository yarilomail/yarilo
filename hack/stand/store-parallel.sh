#!/usr/bin/env bash
# One account writes flags into two folders at once: the profile in which a
# per-account lock on the write path serialises what folders kept apart (#714).
#
# Usage:
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/store-parallel.sh <label> <out-dir>
#
# Runs against whatever image is deployed, right after an ab-arm.sh arm.

set -euo pipefail

LABEL="${1:?label}"
OUT="${2:?output directory}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TYPES="${YARILO_STORE_TYPES:-mdbox maildir sdbox}"
CLIENTS="${YARILO_STORE_CLIENTS:-5}"
SECS="${YARILO_STORE_SECS:-60}"
MSGS="${YARILO_STORE_MSGS:-100}"
PASS="${YARILO_STORE_PASS:-Yarilo!test1}"
IMAPTEST_IMAGE="${YARILO_IMAPTEST_IMAGE:-dovecot/imaptest:latest}"

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" --request-timeout=60s "$@"; }
mkdir -p "$OUT"

# An account the arm fills but its imaptest runs never log in as.
account() {
  case "$1" in
    mdbox) echo "u45@${YARILO_MDBOX_DOMAIN:-d00001.test}" ;;
    maildir) echo "u95@${YARILO_MAILDIR_DOMAIN:-d00002.test}" ;;
    sdbox) echo "u145@${YARILO_SDBOX_DOMAIN:-d00003.test}" ;;
    *) return 1 ;;
  esac
}

imap() {
  kube exec yarilo-backend-0 -c yarilo-imap -- sh -c "printf '$1' | nc -w 30 yarilo-imap-login 143" | tr -d '\r'
}

# Both folders start with the same MSGS messages, copied from INBOX.
prepare() {
  local user="$1" out
  out=$(imap "a LOGIN $user $PASS\r\nb DELETE PushA\r\nc DELETE PushB\r\nd CREATE PushA\r\ne CREATE PushB\r\nf SELECT INBOX\r\ng COPY 1:$MSGS PushA\r\nh COPY 1:$MSGS PushB\r\ni STATUS PushA (MESSAGES)\r\nj STATUS PushB (MESSAGES)\r\nk LOGOUT\r\n")
  printf '%s\n' "$out" > "$OUT/store-$LABEL-$2-prepare.log"
  for box in PushA PushB; do
    if ! printf '%s\n' "$out" | grep -Eq "STATUS \"?$box\"? \(MESSAGES $MSGS\)"; then
      echo "store-parallel: $box of $user does not hold $MSGS messages; see store-$LABEL-$2-prepare.log" >&2
      return 1
    fi
  done
}

# The two halves run as two containers of one pod, so they start together.
manifest() {
  local user="$1" box c=""
  cat <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: store-parallel
  namespace: $NS
spec:
  ttlSecondsAfterFinished: 600
  template:
    spec:
      restartPolicy: Never
      containers:
EOF
  for box in PushA PushB; do
    cat <<EOF
      - name: $(echo "$box" | tr '[:upper:]' '[:lower:]')
        image: $IMAPTEST_IMAGE
        args: [host=yarilo-imap-login.$NS.svc.cluster.local, port=143, user=$user, "pass=$PASS",
               clients=$CLIENTS, secs=$SECS, box=$box, msgs=$MSGS,
               select=100, store=100, fetch=0, fetch2=0, search=0, sort=0, thread=0, copy=0,
               list=0, status=0, append=0, expunge=0, delete=0, logout=0]
EOF
  done
}

backend_metrics() {
  local pod
  for pod in $(kube get pods -l app.kubernetes.io/component=backend -o name | cut -d/ -f2); do
    kube exec "$pod" -c yarilo-imap -- sh -c 'wget -qO- http://127.0.0.1:8080/metrics' 2>/dev/null |
      grep -E '^(imap_command_seconds_bucket\{command="Store"|yarilo_locks_acquire_wait_seconds_bucket\{resource="mbox"|counters_bump_total )' || true
  done
}

# quantiles prints, per series, the count and the bucket bounds p50 and p99
# fall under, from the delta between two pages.
quantiles() {
  awk '
    FILENAME == ARGV[1] { start[$1] += $2; next }
    {
      s = $1; le = $1
      sub(/,?le="[^"]*"/, "", s); sub(/.*le="/, "", le); sub(/".*/, "", le)
      n[s, le] += $2 - start[$1]; start[$1] = 0; les[le] = 1; series[s] = 1
    }
    END {
      m = 0
      for (l in les) if (l != "+Inf") b[++m] = l + 0
      for (i = 1; i <= m; i++) for (j = i + 1; j <= m; j++) if (b[j] < b[i]) { x = b[i]; b[i] = b[j]; b[j] = x }
      for (s in series) {
        total = n[s, "+Inf"]; p50 = "-"; p99 = "-"
        for (i = 1; i <= m; i++) {
          c = 0
          for (l in les) if (l != "+Inf" && l + 0 == b[i]) c = n[s, l]
          if (p50 == "-" && total > 0 && c >= 0.5 * total) p50 = b[i]
          if (p99 == "-" && total > 0 && c >= 0.99 * total) p99 = b[i]
        }
        if (total > 0 && p50 == "-") p50 = "+Inf"
        if (total > 0 && p99 == "-") p99 = "+Inf"
        if (total > 0) print s, "count=" total, "p50<=" p50, "p99<=" p99
      }
    }' "$1" "$2" | sort
}

# stores is the STORE total imaptest reports for one half.
stores() {
  awk '/^Totals:/ { t = 1; next }
       t && /Stor/ { for (i = 1; i <= NF; i++) if ($i == "Stor") col = i; next }
       t && col && $col ~ /^[0-9]+$/ { print $col; exit }' "$1"
}

echo "-- store-parallel $LABEL: image $(kube get pod yarilo-backend-0 -o jsonpath='{.spec.containers[0].image}'), clients=$CLIENTS per folder, secs=$SECS, msgs=$MSGS"
for t in $TYPES; do
  user=$(account "$t") || { echo "store-parallel: $t is no storage type this stand has" >&2; exit 1; }
  prepare "$user" "$t"
  backend_metrics > "$OUT/store-$LABEL-$t-before.txt"
  manifest "$user" > "$OUT/store-$LABEL-$t.yaml"
  KUBECONFIG="$KCFG" YARILO_NS="$NS" bash "$REPO/hack/stand/run-job.sh" store-parallel \
    "$OUT/store-$LABEL-$t.yaml" "$OUT/store-$LABEL-$t-job.log" $(( SECS + 300 ))
  for box in pusha pushb; do
    kube logs job/store-parallel -c "$box" > "$OUT/store-$LABEL-$t-$box.log" 2>&1
  done
  backend_metrics > "$OUT/store-$LABEL-$t-after.txt"
  a=$(stores "$OUT/store-$LABEL-$t-pusha.log"); b=$(stores "$OUT/store-$LABEL-$t-pushb.log")
  if [ -z "$a" ] || [ -z "$b" ]; then
    echo "store-parallel: no STORE total in the imaptest logs of $t; see store-$LABEL-$t-push{a,b}.log" >&2
    exit 1
  fi
  echo "store_parallel type=$t user=$user stores=$(( a + b )) per_sec=$(( (a + b) / SECS )) pusha=$a pushb=$b"
  quantiles "$OUT/store-$LABEL-$t-before.txt" "$OUT/store-$LABEL-$t-after.txt" | sed "s/^/store_latency type=$t /"
  bumps=$(awk 'FILENAME == ARGV[1] && $1 == "counters_bump_total" { b += $2; next } $1 == "counters_bump_total" { a += $2 } END { print a - b }' \
    "$OUT/store-$LABEL-$t-before.txt" "$OUT/store-$LABEL-$t-after.txt")
  echo "store_bumps type=$t bumps=$bumps per_store=$(awk -v b="$bumps" -v s=$(( a + b )) 'BEGIN { if (s > 0) printf "%.2f", b / s }')"
done
