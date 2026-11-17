#!/usr/bin/env bash
# The MAILCRYPT window's keys: a one-time hybrid age key and the test key of
# the reference-format vector, as the Secret the mailcrypt overlays mount.
#
# Usage:
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/mailcrypt-keys.sh create
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/mailcrypt-keys.sh delete
#
# The age key is made here, never printed, and lives only in the Secret: the
# temporary directory goes when the script ends, the Secret when the window
# does. A row or a PR names the key's type alone.

set -euo pipefail

NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
SECRET=yarilo-mail-crypt-window
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
REF_KEY="$REPO/internal/mailcrypt/refstream/testdata/reference/X25519.pem"

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" --request-timeout=60s "$@"; }

case "${1:?create or delete}" in
  create)
    # age-keygen from PATH, else the one of the age version go.mod pins.
    if command -v age-keygen >/dev/null; then
      keygen() { age-keygen "$@"; }
    elif command -v go >/dev/null; then
      keygen() { (cd "$REPO" && go run filippo.io/age/cmd/age-keygen "$@"); }
    else
      echo "mailcrypt-keys: neither age-keygen (1.3 or later) nor go is on PATH" >&2
      exit 1
    fi
    if kube get secret "$SECRET" >/dev/null 2>&1; then
      echo "mailcrypt-keys: Secret $SECRET is there already; delete it first, a window key is used once" >&2
      exit 1
    fi
    umask 077
    dir=$(mktemp -d)
    trap 'rm -rf "$dir"' EXIT
    keygen -pq -o "$dir/private" 2>/dev/null
    grep -q '^AGE-SECRET-KEY-PQ-' "$dir/private" || { echo "mailcrypt-keys: age-keygen -pq made no hybrid key" >&2; exit 1; }
    keygen -y "$dir/private" > "$dir/public"
    kube create secret generic "$SECRET" \
      --from-file=public="$dir/public" --from-file=private="$dir/private" \
      --from-file=reference.pem="$REF_KEY" >/dev/null
    echo "mailcrypt-keys: Secret $SECRET made: hybrid age key (mlkem768x25519), reference test key X25519"
    ;;
  delete)
    kube delete secret "$SECRET" --ignore-not-found >/dev/null
    echo "mailcrypt-keys: Secret $SECRET deleted"
    ;;
  *) echo "mailcrypt-keys: create or delete" >&2; exit 1 ;;
esac
