#!/usr/bin/env bash
# The MAILCRYPT window's keys: a one-time hybrid age key and the test key of
# the reference-format vector, as the Secret the mailcrypt overlays mount.
#
# Usage:
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/mailcrypt-keys.sh create
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/mailcrypt-keys.sh rotate-add
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/mailcrypt-keys.sh rotate-drop
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/mailcrypt-keys.sh delete
#
# rotate-add puts a new key beside the old one and makes it the one new mail is
# sealed to; rotate-drop leaves the new key alone (MAILCRYPT §13.10).
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

umask 077
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT

# newkey writes a new hybrid key to $1/private and its recipient to $1/public.
newkey() {
  if command -v age-keygen >/dev/null; then
    age-keygen -pq -o "$1/private" 2>/dev/null
    age-keygen -y "$1/private" > "$1/public"
  elif command -v go >/dev/null; then
    (cd "$REPO" && go run filippo.io/age/cmd/age-keygen -pq -o "$1/private" 2>/dev/null)
    (cd "$REPO" && go run filippo.io/age/cmd/age-keygen -y "$1/private") > "$1/public"
  else
    echo "mailcrypt-keys: neither age-keygen (1.3 or later) nor go is on PATH" >&2
    exit 1
  fi
  grep -q '^AGE-SECRET-KEY-PQ-' "$1/private" || { echo "mailcrypt-keys: no hybrid key was made" >&2; exit 1; }
  grep -q '^age1pq1' "$1/public" || { echo "mailcrypt-keys: no hybrid recipient was made" >&2; exit 1; }
}

# replace rewrites the Secret's public and private keys, the reference key kept.
replace() {
  kube get secret "$SECRET" -o jsonpath='{.data.reference\.pem}' | base64 -d > "$dir/reference.pem"
  kube create secret generic "$SECRET" --dry-run=client -o yaml \
    --from-file=public="$1" --from-file=private="$2" --from-file=reference.pem="$dir/reference.pem" |
    kube apply -f - >/dev/null
}

case "${1:?create, rotate-add, rotate-drop or delete}" in
  create)
    if kube get secret "$SECRET" >/dev/null 2>&1; then
      echo "mailcrypt-keys: Secret $SECRET is there already; delete it first, a window key is used once" >&2
      exit 1
    fi
    newkey "$dir"
    kube create secret generic "$SECRET" \
      --from-file=public="$dir/public" --from-file=private="$dir/private" \
      --from-file=reference.pem="$REF_KEY" >/dev/null
    echo "mailcrypt-keys: Secret $SECRET made: hybrid age key (mlkem768x25519), reference test key X25519"
    ;;
  rotate-add)
    kube get secret "$SECRET" -o jsonpath='{.data.private}' | base64 -d > "$dir/old"
    [ "$(grep -c '^AGE-SECRET-KEY-' "$dir/old")" = 1 ] || { echo "mailcrypt-keys: rotate-add wants a Secret holding one key" >&2; exit 1; }
    newkey "$dir"
    cat "$dir/old" "$dir/private" > "$dir/both"
    replace "$dir/public" "$dir/both"
    echo "mailcrypt-keys: Secret $SECRET holds the old key and a new hybrid key; new mail is sealed to the new one"
    ;;
  rotate-drop)
    kube get secret "$SECRET" -o jsonpath='{.data.private}' | base64 -d > "$dir/both"
    kube get secret "$SECRET" -o jsonpath='{.data.public}' | base64 -d > "$dir/public"
    [ "$(grep -c '^AGE-SECRET-KEY-' "$dir/both")" = 2 ] || { echo "mailcrypt-keys: rotate-drop wants a Secret holding two keys" >&2; exit 1; }
    grep '^AGE-SECRET-KEY-' "$dir/both" | tail -1 > "$dir/new"
    replace "$dir/public" "$dir/new"
    echo "mailcrypt-keys: Secret $SECRET holds the new key alone"
    ;;
  delete)
    kube delete secret "$SECRET" --ignore-not-found >/dev/null
    echo "mailcrypt-keys: Secret $SECRET deleted"
    ;;
  *) echo "mailcrypt-keys: create, rotate-add, rotate-drop or delete" >&2; exit 1 ;;
esac
