#!/usr/bin/env bash
# Writes stand-dns/test.db from the stand's dkim table: one TXT per row with a
# public key, nothing else but SOA and NS. No A or MX: mail to .test domains is
# routed by the relays' transport maps and must stay so.
#
# Usage: KUBECONFIG=~/.kube/ihorru-sbox-nc.yaml bash hack/sandbox/dns-zone.sh

set -Eeuo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ZONE="$DIR/stand-dns/test.db"

rows=$(kubectl -n db --request-timeout=30s exec mysql-0 -- sh -c \
  'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" yarilo -N -B -e "SELECT lower(trim(domain)), selector, public_key FROM dkim WHERE public_key IS NOT NULL AND public_key <> \"\" ORDER BY 1, 2"' \
  2> >(grep -v 'Using a password' >&2))
[ -n "$rows" ] || { echo "dns-zone: no dkim row with a public key" >&2; exit 1; }

{
  echo '$ORIGIN test.'
  echo '$TTL 300'
  echo "@ IN SOA ns.test. hostmaster.test. ( $(date -u +%Y%m%d%H) 3600 600 86400 300 )"
  echo '@ IN NS ns.test.'
  while IFS=$'\t' read -r domain selector key; do
    # A raw 32-byte key is Ed25519 (44 base64 characters); anything else is an RSA SPKI.
    if [ "${#key}" -eq 44 ]; then kt=ed25519; else kt=rsa; fi
    record="v=DKIM1; k=$kt; p=$key"
    # One TXT string holds at most 255 bytes.
    parts=""
    while [ -n "$record" ]; do
      parts="$parts \"${record:0:255}\""
      record="${record:255}"
    done
    echo "$selector._domainkey.${domain%.test} IN TXT ($parts )"
  done <<< "$rows"
} > "$ZONE"

echo "dns-zone: $(grep -c ' IN TXT ' "$ZONE") TXT records in $ZONE"
