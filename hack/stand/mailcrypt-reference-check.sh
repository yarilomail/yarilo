#!/usr/bin/env bash
# The reference-format check alone: the stand reads the reference format
# (mailcrypt-keys overlay: reference_keys set, write off) and FETCH of the
# vector is checked whole and in part, without a whole A→B→A window.
#
# Usage, on the runner, from a clean checkout:
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/mailcrypt-reference-check.sh <image-tag> <out-dir>
#
# The one-time key is made at the start and deleted at the end, after the
# stand is back on its own values. Result in <out-dir>/crypt-reference.txt.

set -euo pipefail

TAG="${1:?image tag}"
OUT="${2:?output directory}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
mkdir -p "$OUT"

deploy() { # [extra values file]
  helm --kubeconfig="$KCFG" upgrade yarilo "$REPO/helm" -n "$NS" \
    -f "$REPO/helm_values/values-sandbox.yaml" ${1:+-f "$1"} \
    --set image.tag="$TAG" --timeout 10m --wait >/dev/null
}

finish() {
  rc=$?
  echo "== restore: the stand's own values on $TAG"
  if deploy; then
    bash "$HERE/mailcrypt-keys.sh" delete
  else
    echo "mailcrypt-reference-check: the restore did not settle; Secret yarilo-mail-crypt-window is left for the pods that still mount it, delete it after them" >&2
    rc=1
  fi
  echo "mailcrypt-reference-check: ended with status $rc"
  exit "$rc"
}

echo "-- tooling: $(git -C "$REPO" log --oneline -1)"
bash "$HERE/mailcrypt-keys.sh" create
trap finish EXIT
echo "== deploy $TAG with the mailcrypt-keys overlay"
deploy "$REPO/helm_values/values-sandbox-mailcrypt-keys.yaml"
CRYPT_CHECKS=reference bash "$HERE/crypt-checks.sh" reference "$OUT"
