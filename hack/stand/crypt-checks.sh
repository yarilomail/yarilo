#!/usr/bin/env bash
# What a write: age arm must show, after its runs, on the first backend pod.
#
# Usage:
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/crypt-checks.sh <arm> <out-dir>
#
# PASS/FAIL lines in <out-dir>/crypt-<arm>.txt; exit 1 when any fails.
#   check     yarctl config check exits 0 in yarilo-imap: the keys read
#   metrics   mailbox_crypt_sealed_total grew, mailbox_crypt_failed_total is 0
#             (the pod started with this arm, so the counters are the arm's)
#   files     no message file of this arm is plaintext: a maildir file starts
#             with the age signature; an sdbox or mdbox file, whose record
#             starts after the dbox header, carries the signature and no line
#             of a message header
#   disk      disk = index for the maildir and sdbox accounts filled
#   size      RFC822.SIZE of a filled message is its plaintext size
#   reference a reference-format file put in a maildir INBOX reads whole and in
#             part through FETCH, against the vector's plaintext digest

set -uo pipefail

. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../sandbox/creds.sh"
ARM="${1:?arm label}"
OUT="${2:?output directory}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
VEC="$REPO/internal/mailcrypt/refstream/testdata/reference"
POD=yarilo-backend-0
PASSWORD="$(sandbox_password)" || exit 1
RESULT="$OUT/crypt-$ARM.txt"
mkdir -p "$OUT"
: > "$RESULT"
failed=0

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" --request-timeout=60s "$@"; }
in_pod() { kube exec "$POD" -c "${CTR:-yarilo-imap}" -- sh -c "$1"; }
verdict() { # <name> pass|fail <detail>
  local v=PASS
  [ "$2" = pass ] || { v=FAIL; failed=1; }
  printf '%s %s: %s\n' "$v" "$1" "$3" | tee -a "$RESULT"
}
imap_session() {
  in_pod "printf '$1' | nc -w 15 yarilo-imap-login 143"
}

# check
if out=$(in_pod 'yarctl config check' 2>&1); then
  verdict check pass "exit 0 in $POD/yarilo-imap"
else
  verdict check fail "$(echo "$out" | tail -1)"
fi

# metrics: every container's telemetry port, from one, since a pod shares
# its network namespace.
metric_sum() {
  in_pod 'for p in $(seq 8080 8090); do wget -qO- -T 5 http://127.0.0.1:$p/metrics 2>/dev/null; done' |
    awk -v m="$1" 'index($0, m) == 1 && ($1 == m || substr($1, length(m) + 1, 1) == "{") { s += $NF } END { printf "%d", s }'
}
sealed=$(metric_sum mailbox_crypt_sealed_total)
failures=$(metric_sum mailbox_crypt_failed_total)
if [ "${sealed:-0}" -gt 0 ] && [ "${failures:-1}" = 0 ]; then
  verdict metrics pass "sealed $sealed, failed 0"
else
  verdict metrics fail "sealed ${sealed:-?}, failed ${failures:-?}"
fi

# files: the arm wiped and filled these accounts, so every message file is its own.
files=$(in_pod '
  sig="age-encryption.org/v1"
  md=0; mdbad=0; db=0; dbbad=0
  for f in $(find /var/mail/vhosts/d00002.test -path "*/Maildir/cur/*" -type f -o -path "*/Maildir/new/*" -type f); do
    md=$((md + 1)); [ "$(head -c 21 "$f")" = "$sig" ] || mdbad=$((mdbad + 1))
  done
  for f in $(find /var/mail/vhosts/d00003.test -path "*/dbox-Mails/u.*" -type f; find /var/mail/vhosts/d00001.test -path "*/storage/m.*" -type f); do
    db=$((db + 1))
    if ! grep -q "$sig" "$f" || grep -qiE "^(Subject|From|Message-ID):" "$f"; then dbbad=$((dbbad + 1)); fi
  done
  echo "$md $mdbad $db $dbbad"')
set -- $files
if [ "${1:-0}" -gt 0 ] && [ "${3:-0}" -gt 0 ] && [ "${2:-1}" = 0 ] && [ "${4:-1}" = 0 ]; then
  verdict files pass "maildir $1 files start with the signature; sdbox and mdbox $3 files carry it and no header line"
else
  verdict files fail "maildir ${1:-?} (${2:-?} plaintext), sdbox and mdbox ${3:-?} (${4:-?} plaintext)"
fi

# disk = index on accounts the runs did not touch: maildir u71, sdbox u121.
disk_index() { # <user> <type>
  local user="$1" home="/var/mail/vhosts/${1#*@}/$1" disk idx
  case "$2" in
    sdbox) disk=$(in_pod "ls $home/sdbox/mailboxes/INBOX/dbox-Mails 2>/dev/null | grep -c '^u\\.' || true") ;;
    maildir) disk=$(in_pod "ls $home/Maildir/cur $home/Maildir/new 2>/dev/null | grep -vc -e '^\$' -e ':\$' || true") ;;
  esac
  idx=$(CTR=yarilo-backend-api in_pod "yarctl backend folder info $user INBOX 2>/dev/null" | sed -n 's/.*"messages": *\([0-9]*\).*/\1/p')
  echo "$disk $idx"
}
bad=""
for ut in u71@d00002.test:maildir u121@d00003.test:sdbox; do
  set -- $(disk_index "${ut%%:*}" "${ut##*:}")
  [ -n "${1:-}" ] && [ "${1:-}" != 0 ] && [ "$1" = "${2:-}" ] || bad="$bad ${ut%%:*} disk=${1:-?} index=${2:-?}"
done
[ -z "$bad" ] && verdict disk pass "u71 maildir, u121 sdbox" || verdict disk fail "$bad"

# size: the fill's messages are plaintext the size of their first line onward;
# RFC822.SIZE against the length FETCH returns for the same message.
size_out=$(imap_session "a LOGIN u72@d00002.test $PASSWORD\r\nb SELECT INBOX\r\nc FETCH 1 (RFC822.SIZE BODY.PEEK[])\r\nd LOGOUT\r\n")
want=$(echo "$size_out" | sed -n 's/.*RFC822.SIZE \([0-9]*\).*/\1/p' | head -1)
got=$(echo "$size_out" | sed -n 's/.*BODY\[\] {\([0-9]*\)}.*/\1/p' | head -1)
if [ -n "$want" ] && [ "$want" = "$got" ]; then
  verdict size pass "RFC822.SIZE $want = the body FETCH returns"
else
  verdict size fail "RFC822.SIZE ${want:-?}, body ${got:-?}"
fi

# reference: the vector into u99's INBOX (filled, outside every run's range),
# then FETCH whole and a slice; the server's plaintext is digested in the pod.
vec="X25519-gcm-1000.bin"
digest=$(awk -v f="$vec" '$1 == f { print $3 }' "$VEC/MANIFEST")
home=/var/mail/vhosts/d00002.test/u99@d00002.test/Maildir
kube exec -i "$POD" -c yarilo-imap -- sh -c "cat > $home/new/reference-window.$ARM" < "$VEC/$vec"
ref=$(in_pod "
  printf 'a LOGIN u99@d00002.test $PASSWORD\r\nb SELECT INBOX\r\nc UID SEARCH ALL\r\nd LOGOUT\r\n' | nc -w 15 yarilo-imap-login 143 > /tmp/ref.s
  uid=\$(tr -d '\r' < /tmp/ref.s | sed -n 's/^\* SEARCH .* \([0-9]*\)\$/\1/p')
  printf \"a LOGIN u99@d00002.test $PASSWORD\r\nb SELECT INBOX\r\nc UID FETCH \$uid (RFC822.SIZE BODY.PEEK[])\r\nd UID FETCH \$uid (BODY.PEEK[]<100.50>)\r\ne LOGOUT\r\n\" | nc -w 15 yarilo-imap-login 143 > /tmp/ref.f
  off() { grep -abo \"\$1\" /tmp/ref.f | head -1 | cut -d: -f1; }
  w=\$(off 'BODY\[\] {1000}'); p=\$(off 'BODY\[\]<100> {50}')
  whole=\$(dd if=/tmp/ref.f bs=1 skip=\$((w + 15)) count=1000 2>/dev/null | sha256sum | cut -d' ' -f1)
  slice=\$(dd if=/tmp/ref.f bs=1 skip=\$((p + 18)) count=50 2>/dev/null | sha256sum | cut -d' ' -f1)
  ref=\$(dd if=/tmp/ref.f bs=1 skip=\$((w + 15 + 100)) count=50 2>/dev/null | sha256sum | cut -d' ' -f1)
  size=\$(tr -d '\r' < /tmp/ref.f | sed -n 's/.*RFC822.SIZE \([0-9]*\).*/\1/p' | head -1)
  rm -f /tmp/ref.s /tmp/ref.f
  echo \"\$size \$whole \$slice \$ref\"")
set -- $ref
if [ "${1:-}" = 1000 ] && [ "${2:-}" = "$digest" ] && [ -n "${3:-}" ] && [ "$3" = "${4:-}" ]; then
  verdict reference pass "RFC822.SIZE 1000, BODY[] digest is the vector's, BODY[]<100.50> is bytes 100-149 of it"
else
  verdict reference fail "size ${1:-?}, digest ${2:-?} (want $digest), slice ${3:-?} vs ${4:-?}"
fi
in_pod "rm -f $home/new/reference-window.$ARM $home/cur/reference-window.$ARM*" || true

exit "$failed"
