#!/usr/bin/env bash
# Runs the auth policy rows of #2317 (go test -tags wforce) against the
# sandbox's wforce and its yarilo with the wforce overlay deployed.
#
# Usage:
#   KUBECONFIG=~/.kube/ihorru-sbox-nc.yaml bash hack/stand/policy-wforce.sh [go test args]
#
# Needs go, kubectl and a route to the sandbox's IMAP address.

set -euo pipefail

NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
. "$REPO/hack/sandbox/creds.sh"
PORT=18084
IMAP_PORT=18993

kube() { kubectl --kubeconfig="$KCFG" --request-timeout=60s "$@"; }

url=$(kube -n "$NS" get configmap yarilo -o jsonpath='{.data.yarilo\.yaml}' |
  sed -n 's/^ *auth_policy_server_url: *"\{0,1\}\([^"]*\)"\{0,1\}$/\1/p')
if [ -z "$url" ]; then
  echo "policy-wforce: the deployed config has no auth_policy_server_url; deploy helm_values/values-sandbox-wforce.yaml first" >&2
  exit 1
fi

pids=()
trap 'for p in ${pids[@]+"${pids[@]}"}; do kill "$p" 2>/dev/null || true; done' EXIT

# forward holds a port-forward open until the script ends and waits for it.
forward() { # <namespace> <target> <local> <remote>
  kube -n "$1" port-forward "$2" "$3:$4" >/dev/null 2>&1 &
  pids+=($!)
  local deadline=$(( $(date +%s) + 30 ))
  until (exec 9<>"/dev/tcp/127.0.0.1/$3") 2>/dev/null; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "policy-wforce: the forward to $2 never came up" >&2
      return 1
    fi
    sleep 0.2
  done
}

forward wforce svc/wforce "$PORT" 8084
if [ -z "${YARILO_IMAP_ADDR:-}" ]; then
  forward "$NS" svc/yarilo-imap-login "$IMAP_PORT" 993
  YARILO_IMAP_ADDR="127.0.0.1:$IMAP_PORT"
fi

WFORCE_URL="http://127.0.0.1:$PORT/" \
WFORCE_PASSWORD="$(sandbox_wforce_password)" \
YARILO_PASSWORD="$(sandbox_password)" \
YARILO_IMAP_ADDR="$YARILO_IMAP_ADDR" \
YARILO_IMAP_NAME="${YARILO_IMAP_NAME:-mail-sb.seconddns.com}" \
KUBECONFIG="$KCFG" \
  go -C "$REPO" test -tags wforce -count=1 -timeout 5m ./internal/auth/policy/ "$@"
