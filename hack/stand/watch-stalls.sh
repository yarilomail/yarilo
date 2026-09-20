#!/usr/bin/env bash
# Captures, at the first stalls, what is gone by the time anyone looks:
# goroutines from every backend, the auth histogram, and the node's jobs (#1881).
#
# Usage: watch-stalls.sh <out-dir> <label> [threshold] [poll-seconds]

set -uo pipefail

OUT="${1:?output directory}"
LABEL="${2:?label}"
THRESHOLD="${3:-5}"
POLL="${4:-10}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" "$@"; }
mkdir -p "$OUT"

while kube get job imaptest >/dev/null 2>&1; do
  if kube get job imaptest -o jsonpath='{.status.conditions[*].type}' 2>/dev/null | grep -qE 'Complete|Failed'; then
    exit 0
  fi
  stalls=$(kube logs job/imaptest 2>/dev/null | grep -c 'stalled for' || true)
  if [ "${stalls:-0}" -ge "$THRESHOLD" ]; then
    stamp=$(date -u +%Y%m%dT%H%M%SZ)
    echo "watch-stalls: $stalls stalls at $stamp, capturing"
    for pod in $(kube get pods -l app.kubernetes.io/component=backend -o name | cut -d/ -f2); do
      kube exec "$pod" -c yarilo-imap -- sh -c \
        'wget -qO- "http://127.0.0.1:8080/debug/pprof/goroutine?debug=2"' \
        > "$OUT/stall-$LABEL-$stamp-$pod.goroutines" 2>&1
    done
    authpod=$({ out=$(kube get pods -l app.kubernetes.io/component=auth -o name); printf '%s' "${out%%$'\n'*}" | cut -d/ -f2; })
    [ -n "$authpod" ] && kube exec "$authpod" -- sh -c \
      'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null | grep -E "^yarilo_auth_request_seconds_"' \
      > "$OUT/stall-$LABEL-$stamp.auth" 2>&1
    kube get jobs,pods -o wide > "$OUT/stall-$LABEL-$stamp.cluster" 2>&1
    exit 0
  fi
  sleep "$POLL"
done
