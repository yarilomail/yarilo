#!/usr/bin/env bash
# One arm of a stand A/B: deploy a tag, put the mailboxes back to the same
# start, run imaptest over the three storage types, and record the auth
# latency histogram of the arm.
#
# The start is identical for every arm on purpose: mailbox fill moves
# throughput further than anything usually under test, and two arms that
# started differently diverged per type in opposite directions (#1733).
#
# Usage:
#   KUBECONFIG=~/.kube/ihorru-sbox-nc.yaml \
#     bash hack/stand/ab-arm.sh <arm-label> <image-tag> <out-dir>

set -euo pipefail

ARM="${1:?arm label}"
TAG="${2:?image tag}"
OUT="${3:?output directory}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" "$@"; }
mkdir -p "$OUT"

echo "== arm $ARM: $TAG"
helm --kubeconfig="$KCFG" upgrade yarilo "$REPO/helm" -n "$NS" \
  -f "$REPO/helm_values/values-sandbox.yaml" --set image.tag="$TAG" --timeout 10m >/dev/null

deadline=$(( $(date +%s) + 600 ))
until [ "$(kube get pods --no-headers | grep -cv 'Running\|Completed')" = "0" ]; do
  [ "$(date +%s)" -lt "$deadline" ] || { echo "ab-arm: pods did not settle" >&2; exit 1; }
  sleep 10
done

echo "-- same start: emptying u1-u150"
for pod in $(kube get pods -l app.kubernetes.io/component=backend -o name | cut -d/ -f2); do
  kube exec "$pod" -c yarilo-imap -- sh -c \
    'for n in $(seq 1 150); do rm -rf "/var/mail/vhosts/d00001.test/u${n}@d00001.test"; done' >/dev/null
done

# The seed delivers over LMTP, so it needs the pods serving, not merely Running.
for attempt in 1 2 3 4 5; do
  if KUBECONFIG="$KCFG" bash "$REPO/hack/db/seed-sandbox.sh" >/dev/null 2>&1; then break; fi
  [ "$attempt" -lt 5 ] || { echo "ab-arm: the seed did not take" >&2; exit 1; }
  sleep 20
done

authpod=$(kube get pods -l app.kubernetes.io/component=auth -o name | head -1 | cut -d/ -f2)
[ -n "$authpod" ] || { echo "ab-arm: no auth pod to read the histogram from" >&2; exit 1; }
kube exec "$authpod" -- sh -c \
  'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null | grep -E "^yarilo_auth_request_seconds_(bucket|count|sum)\{.*verb=\"AUTH\""' \
  > "$OUT/auth-$ARM-before.txt" || true

for pair in "mdbox 1-20" "maildir 51-70" "sdbox 101-120"; do
  set -- $pair
  name=$1; range=$2
  # The watcher runs beside the job: a stall captured after the run is a stall
  # nobody can explain, which is how one cost two windows (#1733).
  KUBECONFIG="$KCFG" YARILO_NS="$NS" bash "$REPO/hack/stand/watch-stalls.sh" "$OUT" "$ARM-$name" &
  watcher=$!
  sed "s/- users=1-20/- users=$range/" "$REPO/hack/imaptest/job.yaml" |
    KUBECONFIG="$KCFG" YARILO_NS="$NS" bash "$REPO/hack/stand/run-job.sh" imaptest - "$OUT/ab-$ARM-$name.log" 900
  wait "$watcher" 2>/dev/null || true
  logins=$(grep -A 3 '^Logi' "$OUT/ab-$ARM-$name.log" | tail -1 | awk '{print $1}')
  stalls=$(grep -c 'stalled for' "$OUT/ab-$ARM-$name.log" || true)
  echo "$ARM $name logins=${logins:-?} stalls=$stalls"
done

kube exec "$authpod" -- sh -c \
  'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null | grep -E "^yarilo_auth_request_seconds_(bucket|count|sum)\{.*verb=\"AUTH\""' \
  > "$OUT/auth-$ARM-after.txt"
echo "== arm $ARM done"
