#!/usr/bin/env bash
# Seeds the sandbox matrix: u1-50 mdbox, u51-100 maildir, u101-150 sdbox, plus
# the over-quota account. It owns those rows, and no other one (#1806).
#
# Usage:
#   KUBECONFIG=~/.kube/ihorru-sbox-nc.yaml bash hack/db/seed-sandbox.sh

set -euo pipefail

DB_NS="${DB_NS:-db}"
DB_POD="${DB_POD:-mysql-0}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
OVER_USER="${OVER_USER:-over@d00001.test}"
SCRAM_USER="${SCRAM_USER:-scram@d00001.test}"

mysql_do() {
  kubectl --kubeconfig="$KCFG" exec -i -n "$DB_NS" "$DB_POD" -- \
    mysql -u yarilo -psandbox-secret yarilo "$@"
}

echo "Generating SHA512-CRYPT hash ..."
PLAIN_HASH=$(kubectl --kubeconfig="$KCFG" exec -n "$DB_NS" "$DB_POD" -- \
  openssl passwd -6 'Yarilo!test1')

echo "Ensuring quota_clone mapped table (quota) exists ..."
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
QUOTA_OUT=$(mysql_do < "$SCRIPT_DIR/quota-mapped.sql" 2>&1) || {
  echo "$QUOTA_OUT" >&2
  echo "seed: the quota table step failed" >&2
  exit 1
}
echo "$QUOTA_OUT" | grep -v Warning || true

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

# Assert, do not print: a printed count reads normal when a format is missing
# (#1806), and a query that never ran is no verdict about the matrix (#1817).
echo "Verifying the matrix ..."
RAW=$(mysql_do -N -B -e "
SELECT CONCAT(mbtype, '=', COUNT(*)) FROM mailbox
WHERE username REGEXP '^u[0-9]+@d00001[.]test\$'
  AND CAST(SUBSTRING_INDEX(SUBSTRING(username, 2), '@', 1) AS UNSIGNED) BETWEEN 1 AND 150
GROUP BY mbtype ORDER BY mbtype;" 2>&1) || {
  echo "$RAW" >&2
  echo "seed: the matrix query failed; nothing was verified" >&2
  exit 1
}
GOT=$({ printf '%s' "$RAW" | grep -v Warning || true; } | tr '\n' ' ' | sed 's/ *$//')

WANT="maildir=50 mdbox=50 sdbox=50"
if [ "$GOT" != "$WANT" ]; then
  echo "seed: the matrix is [$GOT], want [$WANT]" >&2
  exit 1
fi
echo "matrix: $GOT"

# Failing here must not read as "no other rows", which is what an empty
# listing looks like.
echo "Rows this script does not own, left alone:"
OTHERS=$(mysql_do -e "
SELECT mbtype, COUNT(*) AS cnt FROM mailbox
WHERE username NOT IN ('$OVER_USER', '$SCRAM_USER')
  AND NOT (username REGEXP '^u[0-9]+@d00001[.]test\$'
  AND CAST(SUBSTRING_INDEX(SUBSTRING(username, 2), '@', 1) AS UNSIGNED) BETWEEN 1 AND 150)
GROUP BY mbtype;" 2>&1) || {
  echo "$OTHERS" >&2
  echo "seed: could not list the rows it does not own" >&2
  exit 1
}
echo "$OTHERS" | grep -v Warning || true

# Provisioned outside u1-u150 so no imaptest run empties or fills it, which is
# what makes the smoketest OVERQUOTA row repeatable (#1855).
OVER_MBTYPE="${OVER_MBTYPE:-maildir}"
OVER_LIMIT=65536
FILL_BYTES=16384
FILL_TRIES=8
YARILO_NS="${YARILO_NS:-yarilo-sb}"
LMTP_HOST="${LMTP_HOST:-yarilo-lmtp-login}"
LMTP_PORT="${LMTP_PORT:-24}"

kube() { kubectl --kubeconfig="$KCFG" -n "$YARILO_NS" "$@"; }

# A SCRAM-SHA-256 verifier for the sandbox password, so the service has a
# mechanism to announce; generated with sasl.GenerateScramSha256Credentials.
SCRAM_PASSWORD='{SCRAM-SHA-256}4096,rVWY5tn9RRHylcRuVMDh+Q==,CDuPU6P3lQho2V+PqMFpgA+StYaqyyUoBqoZRkhQa/k=,ZQl7BB7E33Klr/aYgxpFiof+ISzx8uUewXhLo1CiJTw='

echo "Seeding $SCRAM_USER (SCRAM-SHA-256 verifier) ..."
SCRAM_OUT=$(mysql_do <<SQL 2>&1
INSERT INTO mailbox (username, password, mbtype, home, maildir, quota_bytes, local_part, domain, active, mpath)
VALUES ('$SCRAM_USER', '$SCRAM_PASSWORD', 'maildir', '/var/mail/vhosts/', 'Maildir', 1073741824,
        'scram', 'd00001.test', 1, 'd00001.test/$SCRAM_USER')
ON DUPLICATE KEY UPDATE
  password = VALUES(password), mbtype = VALUES(mbtype), home = VALUES(home),
  maildir = VALUES(maildir), quota_bytes = VALUES(quota_bytes), local_part = VALUES(local_part),
  domain = VALUES(domain), active = VALUES(active), mpath = VALUES(mpath);
SQL
) || { echo "$SCRAM_OUT" >&2; echo "seed: the scram row failed" >&2; exit 1; }
echo "$SCRAM_OUT" | grep -v Warning || true

echo "Seeding $OVER_USER ($OVER_MBTYPE, limit $OVER_LIMIT bytes) ..."
# The limit is written absolutely, never as an increment: a second run must
# leave the row measuring the same thing.
OVER_OUT=$(mysql_do <<SQL 2>&1
INSERT INTO mailbox (username, password, mbtype, home, maildir, quota_bytes, local_part, domain, active, mpath)
VALUES ('$OVER_USER', '$PLAIN_HASH', '$OVER_MBTYPE', '/var/mail/vhosts/', 'Maildir', $OVER_LIMIT,
        'over', 'd00001.test', 1, 'd00001.test/$OVER_USER')
ON DUPLICATE KEY UPDATE
  password = VALUES(password), mbtype = VALUES(mbtype), home = VALUES(home),
  maildir = VALUES(maildir), quota_bytes = VALUES(quota_bytes), local_part = VALUES(local_part),
  domain = VALUES(domain), active = VALUES(active), mpath = VALUES(mpath);
SQL
) || { echo "$OVER_OUT" >&2; echo "seed: the over-quota row failed" >&2; exit 1; }
echo "$OVER_OUT" | grep -v Warning || true

BACKEND_POD=$(kube get pods -l app.kubernetes.io/component=backend -o name 2>/dev/null | head -1 | cut -d/ -f2)
if [ -z "$BACKEND_POD" ]; then
  echo "seed: no backend pod in $YARILO_NS; nothing was filled or verified" >&2
  exit 1
fi

# deliver_filler prints the LMTP transcript of one delivery. The body is lines,
# not one run of bytes: a line over 1000 octets is a protocol error, not a test.
deliver_filler() {
  local lines=$((FILL_BYTES / 64))
  {
    printf 'LHLO seed.invalid\r\nMAIL FROM:<seed@test.invalid>\r\nRCPT TO:<%s>\r\nDATA\r\n' "$OVER_USER"
    printf 'Subject: over-quota filler %s\r\nFrom: <seed@test.invalid>\r\nTo: <%s>\r\n\r\n' "$1" "$OVER_USER"
    awk -v n="$lines" 'BEGIN { for (i = 0; i < n; i++) printf "%s\r\n", "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" }'
    printf '.\r\nQUIT\r\n'
  } | kube exec -i "$BACKEND_POD" -c yarilo-imap -- nc -w 10 "$LMTP_HOST" "$LMTP_PORT" 2>&1
}

# usage_bytes prints the storage usage as a number, or fails with what yarctl
# said: a read swallowed under pipefail kills the seed with no line naming it.
usage_bytes() {
  local out
  out=$(kube exec "$BACKEND_POD" -c yarilo-backend-api -- yarctl -O json backend quota show "$1" 2>&1) || {
    printf '%s\n' "$out" >&2
    return 1
  }
  # storage_value is KiB, as the endpoint reports it.
  printf '%s\n' "$out" | awk -F'[:,]' '/"storage_value"/ { gsub(/[^0-9-]/, "", $2); printf "%d\n", $2 * 1024; exit }'
}

echo "Filling $OVER_USER past $OVER_LIMIT bytes ..."
# Before the first delivery the mailbox does not exist yet, and no usage is not
# a broken read; after one it is, and the loop below says so.
HAVE=$(usage_bytes "$OVER_USER") || HAVE=0
[ -n "$HAVE" ] || HAVE=0
i=0
while [ "$HAVE" -le "$OVER_LIMIT" ] && [ "$i" -lt "$FILL_TRIES" ]; do
  i=$((i + 1))
  TRANSCRIPT=$(deliver_filler "$i") || {
    echo "$TRANSCRIPT" >&2
    echo "seed: the LMTP session to $LMTP_HOST:$LMTP_PORT failed" >&2
    exit 1
  }
  # The reply to the final dot, not any 250 in the session: LHLO, MAIL and RCPT
  # answer 250 too, so a refused delivery passes a transcript-wide match.
  STATUS=$(printf '%s' "$TRANSCRIPT" | grep -E '^[0-9]{3} ' | grep -v '^221 ' | tail -1)
  case "$STATUS" in
    250\ *) ;;
    *)
      echo "$TRANSCRIPT" >&2
      echo "seed: filler delivery $i was answered [$STATUS], want 250" >&2
      exit 1
      ;;
  esac
  HAVE=$(usage_bytes "$OVER_USER") || {
    echo "seed: the usage of $OVER_USER could not be read back after delivery $i" >&2
    exit 1
  }
done

if [ "$HAVE" -le "$OVER_LIMIT" ]; then
  echo "seed: $OVER_USER holds $HAVE bytes, still within the $OVER_LIMIT-byte limit" >&2
  exit 1
fi
echo "over quota: $HAVE bytes against a $OVER_LIMIT-byte limit (+$i delivered now)"

echo "Done."
