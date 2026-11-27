#!/usr/bin/env bash
# The MAILCRYPT A→B→A window: A1 B1 A2 B2 A3 in one slot, on one image, so
# each change of write mode is crossed twice in each direction.
#
# Usage, on the runner, from a clean checkout:
#   KUBECONFIG=~/.kube/sbox.yaml YARILO_ARM_COMMIT=$(git rev-parse HEAD) \
#     bash hack/stand/mailcrypt-window.sh <image-tag> <out-dir>
#
# Every arm mounts the same keys (mailcrypt-keys overlay); B arms write age
# (mailcrypt-write overlay), which differs in write alone. Every arm wipes and
# fills its accounts, so each starts from the same empty store and reads what
# it wrote: A2 and A3 read no ciphertext from the B arm before them.
#
# The window makes its one-time key at the start and deletes it at the end,
# after putting the stand back on its own values, so no pod is left mounting
# a Secret that is gone. /tmp is sampled across all five arms alike.

set -euo pipefail

TAG="${1:?image tag}"
OUT="${2:?output directory}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
mkdir -p "$OUT"

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" --request-timeout=60s "$@"; }
. "$HERE/deploy.sh"

watcher=""
checks_failed=0
finish() {
  rc=$?
  [ -n "$watcher" ] && { kill "$watcher" 2>/dev/null || true; wait "$watcher" 2>/dev/null || true; }
  echo "== restore: the stand's own values on $TAG"
  mkdir -p "$OUT/deploy-restore"
  if stand_deploy "$OUT/deploy-restore" "$TAG" "$REPO/helm" -f "$REPO/helm_values/values-sandbox.yaml"; then
    bash "$HERE/mailcrypt-keys.sh" delete
  else
    echo "mailcrypt-window: the restore did not settle; Secret yarilo-mail-crypt-window is left for the pods that still mount it, delete it after them" >&2
    rc=1
  fi
  echo "mailcrypt-window: ended with status $rc"
  exit "$rc"
}

bash "$HERE/mailcrypt-keys.sh" create
trap finish EXIT

bash "$HERE/watch-tmp.sh" "$OUT" &
watcher=$!

for arm in A1 B1 A2 B2 A3; do
  overlay=mailcrypt-keys
  case "$arm" in B*) overlay=mailcrypt-write ;; esac
  echo "== window arm $arm ($overlay) from $(date -u +%H:%M:%S)"
  YARILO_ARM_OVERLAY="$overlay" bash "$HERE/ab-arm.sh" "$arm" "$TAG" "$OUT" 2>&1 | tee "$OUT/arm-$arm.log"
  # A failed check is a finding, not a reason to lose the slot: the arms go on.
  case "$arm" in B*) bash "$HERE/crypt-checks.sh" "$arm" "$OUT" || checks_failed=1 ;; esac
done
echo "== window done at $(date -u +%H:%M:%S)"
[ "$checks_failed" = 0 ] || { echo "mailcrypt-window: a crypt check failed, see crypt-B*.txt" >&2; exit 1; }
