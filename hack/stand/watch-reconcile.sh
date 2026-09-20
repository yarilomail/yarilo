#!/usr/bin/env bash
# Samples the reconcile counters while a run is under way, so a walk driven by
# a cold cache can be told apart from one driven by a change: the shares only
# separate over time -- first-seen belongs to the first minutes after a rollout
# and has to fade, and what the fill left behind shows up as token-moved (#1875).
#
# Usage:
#   KUBECONFIG=... bash hack/stand/watch-reconcile.sh <out-dir> <label> [interval]

set -uo pipefail

OUT="${1:?output directory}"
LABEL="${2:?label}"
EVERY="${3:-10}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" --request-timeout=30s "$@"; }

mkdir -p "$OUT"
samples="$OUT/reconcile-$LABEL.txt"
: > "$samples"
echo "# samples: every ${EVERY}s, imap_maildir_sync_total summed over the backends" >> "$samples"

while :; do
  ts=$(date +%H:%M:%S)
  pods=$(kube get pods -l app.kubernetes.io/component=backend -o name 2>/dev/null | cut -d/ -f2)
  # Summed over the pods, not one of them: the director spreads the load and a
  # single pod's curve is the shape of its share, not of the run.
  for pod in $pods; do
    kube exec "$pod" -c yarilo-imap -- sh -c \
      'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null' 2>/dev/null |
      grep '^imap_maildir_sync_total{'
  done | awk -v ts="$ts" '
      { n = $NF; sub(/^imap_maildir_sync_total/, "", $1); sum[$1] += n }
      END { for (k in sum) printf "%s %s %d\n", ts, k, sum[k] }
    ' | sort -k2 >> "$samples"
  sleep "$EVERY"
done
