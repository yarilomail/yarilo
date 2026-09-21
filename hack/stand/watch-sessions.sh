#!/usr/bin/env bash
# Samples where the sessions sit while a run is under way. Read after the run
# they are all zero -- the clients have disconnected -- so the question "which
# backend carried this load" can only be answered from inside it (#1943).
#
# Usage:
#   KUBECONFIG=... bash hack/stand/watch-sessions.sh <out-dir> <label> [interval]

set -uo pipefail

OUT="${1:?output directory}"
LABEL="${2:?label}"
EVERY="${3:-5}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" --request-timeout=30s "$@"; }

mkdir -p "$OUT"
samples="$OUT/spread-$LABEL.txt"
: > "$samples"
echo "# samples: every ${EVERY}s, sessions per backend from the director" >> "$samples"

while :; do
  ts=$(date +%H:%M:%S)
  pod=$(kube get pods -l app.kubernetes.io/component=director -o name 2>/dev/null | head -1 | cut -d/ -f2)
  if [ -n "$pod" ]; then
    page=$(kube exec "$pod" -- yarctl -O json director backends list 2>/dev/null)
    # yarctl indents its JSON, so the colon carries a space; the pair is read in
    # order because the port sits between the two fields.
    printf '%s\n' "$page" |
      grep -oE '"ip": *"[^"]*"|"sessions": *[0-9]+' |
      sed 's/"//g; s/ip: *//; s/sessions: *//' | paste - - |
      awk -v ts="$ts" 'NF == 2 { printf "%s %s %s\n", ts, $1, $2 }' >> "$samples"
  fi
  sleep "$EVERY"
done
