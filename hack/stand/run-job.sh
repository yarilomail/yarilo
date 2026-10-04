#!/usr/bin/env bash
# Runs one kubectl Job from a manifest and saves its log. Every wait is bounded
# and every step checks that what it waits for exists (#1733 measurement).
#
# Usage:
#   KUBECONFIG=~/.kube/ihorru-sbox-nc.yaml \
#     bash hack/stand/run-job.sh <name> <manifest> <log-path> [timeout-seconds]

set -euo pipefail

NAME="${1:?job name}"
MANIFEST="${2:?manifest path or - for stdin}"
LOGPATH="${3:?where to save the log}"
TIMEOUT="${4:-600}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" "$@"; }

kube delete job "$NAME" --ignore-not-found >/dev/null 2>&1 || true

if [ "$MANIFEST" = "-" ]; then
  kube apply -f - >/dev/null
else
  kube apply -f "$MANIFEST" >/dev/null
fi

# Waiting for a job that was never created is how a run hangs for ever instead
# of failing: check it is there before waiting on it.
if ! kube get job "$NAME" >/dev/null 2>&1; then
  echo "run-job: $NAME was not created by the apply" >&2
  exit 1
fi

deadline=$(( $(date +%s) + TIMEOUT ))
until kube get job "$NAME" -o jsonpath='{.status.conditions[*].type}' 2>/dev/null | grep -qE 'Complete|Failed'; do
  if [ "$(date +%s)" -ge "$deadline" ]; then
    kube logs "job/$NAME" > "$LOGPATH" 2>&1 || true
    echo "run-job: $NAME did not finish within ${TIMEOUT}s; partial log in $LOGPATH" >&2
    exit 1
  fi
  sleep 5
done

kube logs "job/$NAME" > "$LOGPATH" 2>&1
if kube get job "$NAME" -o jsonpath='{.status.conditions[*].type}' | grep -q Failed; then
  echo "run-job: $NAME failed; log in $LOGPATH" >&2
  exit 1
fi
echo "run-job: $NAME finished, log in $LOGPATH"
