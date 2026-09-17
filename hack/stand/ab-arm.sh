#!/usr/bin/env bash
# One arm of a stand A/B: deploy, restore the same start, run the three storage
# types, and record the auth latency histogram either side.
#
# The same start before every arm is the point: mailbox fill moves throughput
# further than most things under test (#1733).
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

IMAGE_REPO="${YARILO_IMAGE_REPO:-yarilomail/yarilo}"

# A tag with no image deploys, backs off, and reports "pods did not settle" ten
# minutes later -- a true sentence pointing at the wrong thing (#1881).
tag_exists() {
  local tok code
  tok=$(curl -fsS "https://ghcr.io/token?scope=repository:${IMAGE_REPO}:pull&service=ghcr.io" |
    sed 's/.*"token":"\([^"]*\)".*/\1/') || return 2
  [ -n "$tok" ] || return 2
  code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $tok" \
    -H "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.docker.distribution.manifest.v2+json" \
    "https://ghcr.io/v2/${IMAGE_REPO}/manifests/$1") || return 2
  case "$code" in 200) return 0;; 404) return 1;; *) return 2;; esac
}

last_built_tag() {
  local tok
  tok=$(curl -fsS "https://ghcr.io/token?scope=repository:${IMAGE_REPO}:pull&service=ghcr.io" |
    sed 's/.*"token":"\([^"]*\)".*/\1/') || return 1
  curl -fsS -H "Authorization: Bearer $tok" "https://ghcr.io/v2/${IMAGE_REPO}/tags/list?n=1000" |
    tr ',' '\n' | grep -o '[0-9]\+\.[0-9]\+\.[0-9]\+-dev\.[0-9]\+' | sort -t. -k4 -n | tail -1
}

tag_exists "$TAG" && rc=0 || rc=$?
if [ "$rc" = 1 ]; then
  echo "ab-arm: no image for tag $TAG; the last built tag is $(last_built_tag)" >&2
  exit 1
elif [ "$rc" != 0 ]; then
  echo "ab-arm: could not read the registry to check tag $TAG" >&2
  exit 1
fi
# Lock acquisitions by resource class, summed over the backends: the number that
# says which lock a change moved, which throughput alone cannot (#1884).
lock_classes() {
  local pod
  for pod in $(kube get pods -l app.kubernetes.io/component=backend -o name | cut -d/ -f2); do
    kube exec "$pod" -c yarilo-imap -- sh -c \
      'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null | grep "^yarilo_locks_acquire_wait_seconds_count{"' 2>/dev/null
  done | awk -F'"' '{split($0,f," "); sum[$2]+=f[length(f)]} END{for (c in sum) printf "%s %d\n", c, sum[c]}' | sort
}



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
  # Checked, not assumed: if the literal in job.yaml ever moves, an unchecked
  # sed runs mdbox three times and reports three types.
  manifest="$OUT/job-$ARM-$name.yaml"
  sed "s/- users=1-20/- users=$range/" "$REPO/hack/imaptest/job.yaml" > "$manifest"
  if ! grep -q -- "- users=$range" "$manifest"; then
    echo "ab-arm: the user range did not substitute; hack/imaptest/job.yaml no longer carries 'users=1-20'" >&2
    exit 1
  fi
  # The watcher runs beside the job: a stall captured after the run is one
  # nobody can explain (#1881).
  KUBECONFIG="$KCFG" YARILO_NS="$NS" bash "$REPO/hack/stand/watch-stalls.sh" "$OUT" "$ARM-$name" &
  watcher=$!
  lock_classes > "$OUT/locks-$ARM-$name-before.txt"
  KUBECONFIG="$KCFG" YARILO_NS="$NS" bash "$REPO/hack/stand/run-job.sh" imaptest "$manifest" "$OUT/ab-$ARM-$name.log" 900
  lock_classes > "$OUT/locks-$ARM-$name-after.txt"
  wait "$watcher" 2>/dev/null || true
  logins=$(grep -A 3 '^Logi' "$OUT/ab-$ARM-$name.log" | tail -1 | awk '{print $1}')
  stalls=$(grep -c 'stalled for' "$OUT/ab-$ARM-$name.log" || true)
  echo "$ARM $name logins=${logins:-?} stalls=$stalls"
done

kube exec "$authpod" -- sh -c \
  'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null | grep -E "^yarilo_auth_request_seconds_(bucket|count|sum)\{.*verb=\"AUTH\""' \
  > "$OUT/auth-$ARM-after.txt"
echo "== arm $ARM done"
