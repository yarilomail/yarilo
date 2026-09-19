#!/usr/bin/env bash
# Samples CPU while a run is under way, so "the sessions no longer wait" can be
# told apart from "the stand is at its ceiling": if the backend sits at its CPU
# limit, throughput is bounded by work and no amount of waiting removed will
# move it; if the client sits there instead, the number measures imaptest
# (#1875).
#
# Usage:
#   KUBECONFIG=... bash hack/stand/watch-cpu.sh <out-dir> <label> [interval]

set -uo pipefail

OUT="${1:?output directory}"
LABEL="${2:?label}"
EVERY="${3:-10}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" --request-timeout=30s "$@"; }

mkdir -p "$OUT"
samples="$OUT/cpu-$LABEL.txt"
: > "$samples"

# The limits first, as numbers beside the samples: "at the ceiling" is a
# comparison, and the ceiling has to be in the same file.
{
  echo "# limits (cpu request/limit per container, millicores)"
  kube get pods -l app.kubernetes.io/component=backend -o \
    jsonpath='{range .items[*]}{.metadata.name}{"\t"}{range .spec.containers[*]}{.name}{"="}{.resources.requests.cpu}{"/"}{.resources.limits.cpu}{" "}{end}{"\n"}{end}' 2>/dev/null
  echo "# node allocatable"
  kubectl --kubeconfig="$KCFG" get nodes -o \
    jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.allocatable.cpu}{"\n"}{end}' 2>/dev/null
} >> "$samples"

echo "# samples: every ${EVERY}s, pod cpu from metrics-server" >> "$samples"
while :; do
  ts=$(date +%H:%M:%S)
  kube top pod --no-headers 2>/dev/null |
    awk -v ts="$ts" '$1 ~ /backend|imaptest/ { printf "%s %s %s %s\n", ts, $1, $2, $3 }' >> "$samples"
  sleep "$EVERY"
done
