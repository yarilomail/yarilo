#!/usr/bin/env bash
# Seeds the sandbox matrix: u1-50 mdbox, u51-100 maildir, u101-150 sdbox.
#
# It owns those 150 rows and nothing else. Fixtures (static@, conv*, mda*) and
# any account added by hand keep their rows: an upsert on username leaves what
# the script did not write, where TRUNCATE deleted every sdbox account and put
# none back (#1806).
#
# Usage:
#   KUBECONFIG=~/.kube/ihorru-sbox-nc.yaml bash hack/db/seed-sandbox.sh

set -euo pipefail

DB_NS="${DB_NS:-db}"
DB_POD="${DB_POD:-mysql-0}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"

mysql_do() {
  kubectl --kubeconfig="$KCFG" exec -i -n "$DB_NS" "$DB_POD" -- \
    mysql -u yarilo -psandbox-secret yarilo "$@"
}

echo "Generating SHA512-CRYPT hash ..."
PLAIN_HASH=$(kubectl --kubeconfig="$KCFG" exec -n "$DB_NS" "$DB_POD" -- \
  openssl passwd -6 'Yarilo!test1')

echo "Ensuring quota_clone mapped table (quota) exists ..."
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
mysql_do < "$SCRIPT_DIR/quota-mapped.sql" 2>&1 | grep -v Warning || true

# range <mbtype> <maildir> <from> <to>
range() {
  cat <<SQL
INSERT INTO mailbox (username, password, mbtype, home, maildir, quota_bytes, local_part, domain, active, mpath)
SELECT CONCAT('u', n, '@d00001.test'), '$PLAIN_HASH', '$1', '/var/mail/vhosts/', '$2', 1073741824,
       CONCAT('u', n), 'd00001.test', 1, CONCAT('d00001.test/u', n, '@d00001.test')
FROM (SELECT a.N + b.N*10 + c.N*100 + 1 AS n
      FROM (SELECT 0 AS N UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4
            UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9) a,
           (SELECT 0 AS N UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4
            UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9) b,
           (SELECT 0 AS N UNION SELECT 1) c
      HAVING n BETWEEN $3 AND $4) nums
ON DUPLICATE KEY UPDATE
  password = VALUES(password), mbtype = VALUES(mbtype), home = VALUES(home),
  maildir = VALUES(maildir), quota_bytes = VALUES(quota_bytes), local_part = VALUES(local_part),
  domain = VALUES(domain), active = VALUES(active), mpath = VALUES(mpath);
SQL
}

echo "Seeding u1-u150@d00001.test in $DB_NS/$DB_POD ..."
# Not piped into grep: a failing INSERT there is invisible, which is how the
# previous version emptied the table and reported nothing (#1806).
SEED_OUT=$({ range mdbox mdbox 1 50; range maildir Maildir 51 100; range sdbox sdbox 101 150; } | mysql_do 2>&1) || {
  echo "$SEED_OUT" >&2
  echo "seed: the insert failed; nothing was changed" >&2
  exit 1
}
echo "$SEED_OUT" | grep -v Warning || true

# The assertion, not a printout: a count that merely prints reads like a normal
# result when a format is missing (#1806).
echo "Verifying the matrix ..."
GOT=$(mysql_do -N -B -e "
SELECT CONCAT(mbtype, '=', COUNT(*)) FROM mailbox
WHERE username REGEXP '^u[0-9]+@d00001[.]test\$'
  AND CAST(SUBSTRING_INDEX(SUBSTRING(username, 2), '@', 1) AS UNSIGNED) BETWEEN 1 AND 150
GROUP BY mbtype ORDER BY mbtype;" 2>/dev/null | tr '\n' ' ' | sed 's/ *$//')

WANT="maildir=50 mdbox=50 sdbox=50"
if [ "$GOT" != "$WANT" ]; then
  echo "seed: the matrix is [$GOT], want [$WANT]" >&2
  exit 1
fi
echo "matrix: $GOT"

echo "Rows this script does not own, left alone:"
mysql_do -e "
SELECT mbtype, COUNT(*) AS cnt FROM mailbox
WHERE NOT (username REGEXP '^u[0-9]+@d00001[.]test\$'
  AND CAST(SUBSTRING_INDEX(SUBSTRING(username, 2), '@', 1) AS UNSIGNED) BETWEEN 1 AND 150)
GROUP BY mbtype;" 2>&1 | grep -v Warning || true

echo "Done."
