#!/usr/bin/env bash
# Makes or updates every Secret of the sandbox from the environment, so no
# password of it is in git. Run it before the manifests that read them, and
# again to change a value. Pods read a Secret at start: restart them after, the
# relay's (namespace yarilo-relay) included.
#
# Usage:
#   KUBECONFIG=~/.kube/ihorru-sbox-nc.yaml \
#   YARILO_PASSWORD=... YARILO_DB_PASSWORD=... YARILO_DB_ROOT_PASSWORD=... \
#   KC_ADMIN_PASSWORD=... KC_CLIENT_SECRET=... KC_HS256_KEY=... \
#     bash hack/sandbox/secrets.sh
#
# Needs kubectl and jq. KC_HS256_KEY is base64url of 64 bytes: openssl rand 64 | basenc --base64url -w0 | tr -d =

set -euo pipefail

: "${YARILO_PASSWORD:?set YARILO_PASSWORD, the password of the test accounts}"
: "${YARILO_DB_PASSWORD:?set YARILO_DB_PASSWORD, the password of the database user yarilo}"
: "${YARILO_DB_ROOT_PASSWORD:?set YARILO_DB_ROOT_PASSWORD, the MySQL root password}"
: "${KC_ADMIN_PASSWORD:?set KC_ADMIN_PASSWORD, the Keycloak admin password}"
: "${KC_CLIENT_SECRET:?set KC_CLIENT_SECRET, the secret of the Keycloak client yarilo-auth}"
: "${KC_HS256_KEY:?set KC_HS256_KEY, the HS256 key behind client yarilo-test-hs256}"

KCFG="${KUBECONFIG:-$HOME/.kube/config}"

apply_secret() { # <namespace> <name> --from-literal=... ...
  local ns=$1 name=$2
  shift 2
  kubectl --kubeconfig="$KCFG" create namespace "$ns" --dry-run=client -o yaml |
    kubectl --kubeconfig="$KCFG" apply -f - >/dev/null
  kubectl --kubeconfig="$KCFG" -n "$ns" create secret generic "$name" "$@" --dry-run=client -o yaml |
    kubectl --kubeconfig="$KCFG" apply -f -
}

apply_secret db mysql \
  --from-literal=MYSQL_DATABASE=yarilo \
  --from-literal=MYSQL_USER=yarilo \
  --from-literal=MYSQL_PASSWORD="$YARILO_DB_PASSWORD" \
  --from-literal=MYSQL_ROOT_PASSWORD="$YARILO_DB_ROOT_PASSWORD"

# The relay's Postfix looks recipients up with these; its other keys are left as they are.
jq -n '{stringData: {MYSQL_PASSWORD: env.YARILO_DB_PASSWORD, MYSQL_ROOT_PASSWORD: env.YARILO_DB_ROOT_PASSWORD}}' |
  kubectl --kubeconfig="$KCFG" -n yarilo-relay patch secret yarilo-mysql --type merge --patch-file /dev/stdin

apply_secret yarilo-sb yarilo-mysql \
  --from-literal=dsn="yarilo:${YARILO_DB_PASSWORD}@tcp(mysql.db.svc:3306)/yarilo?parseTime=true&charset=utf8mb4"

apply_secret yarilo-sb yarilo-postgres \
  --from-literal=dsn="postgres://yarilo:${YARILO_DB_PASSWORD}@yarilo-postgres:5432/yarilo?sslmode=disable" \
  --from-literal=POSTGRES_DB=yarilo \
  --from-literal=POSTGRES_USER=yarilo \
  --from-literal=POSTGRES_PASSWORD="$YARILO_DB_PASSWORD"

apply_secret keycloak keycloak \
  --from-literal=admin_password="$KC_ADMIN_PASSWORD" \
  --from-literal=client_secret="$KC_CLIENT_SECRET" \
  --from-literal=hs256_key="$KC_HS256_KEY" \
  --from-literal=user_password="$YARILO_PASSWORD"

apply_secret yarilo-sb yarilo-oauth2-sandbox \
  --from-literal=client_secret="$KC_CLIENT_SECRET" \
  --from-literal=hs256_key="$KC_HS256_KEY"

# Read by the jobs in hack/ as SANDBOX_PASSWORD.
apply_secret yarilo-sb sandbox-test-account \
  --from-literal=password="$YARILO_PASSWORD"
