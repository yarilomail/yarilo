# Sourced by the stand and seed scripts. Each password comes from its variable
# when set, else from the Secret hack/sandbox/secrets.sh made, so the runner
# needs no copy of it.

_sandbox_secret() { # <namespace> <secret> <key>
  local v
  v=$(kubectl --kubeconfig="${KUBECONFIG:-$HOME/.kube/config}" -n "$1" get secret "$2" \
    -o go-template="{{with index .data \"$3\"}}{{. | base64decode}}{{end}}" 2>/dev/null) || v=
  if [ -z "$v" ]; then
    echo "no $3 in Secret $1/$2 and no variable set; see hack/sandbox/secrets.sh" >&2
    return 1
  fi
  printf '%s' "$v"
}

# The test accounts' password.
sandbox_password() {
  if [ -n "${YARILO_PASSWORD:-}" ]; then printf '%s' "$YARILO_PASSWORD"; return; fi
  _sandbox_secret "${YARILO_NS:-yarilo-sb}" sandbox-test-account password
}

# The database user yarilo's password.
sandbox_db_password() {
  if [ -n "${YARILO_DB_PASSWORD:-}" ]; then printf '%s' "$YARILO_DB_PASSWORD"; return; fi
  _sandbox_secret "${DB_NS:-db}" mysql MYSQL_PASSWORD
}

# The policy server's API password.
sandbox_wforce_password() {
  if [ -n "${WFORCE_PASSWORD:-}" ]; then printf '%s' "$WFORCE_PASSWORD"; return; fi
  _sandbox_secret wforce wforce password
}
