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
#   (info)    key cache hits and misses over the backend pods
#   files     no message file the arm filled is plaintext (its account ranges
#             only: other accounts on these domains predate the window); a
#             maildir file starts
#             with the age signature; an sdbox or mdbox file, whose record
#             starts after the dbox header, carries the signature and no line
#             of a message header
#   disk      disk = index for the maildir and sdbox accounts filled
#   size      RFC822.SIZE of a filled message is its plaintext size
#   reference a reference-format file put in a maildir INBOX reads whole and in
#             part through FETCH; the server sends the plaintext with CRLF line
#             ends, so size and digests come from testdata SERVED, which a unit
#             test holds to the vector. The literal is found on the runner, as
#             the image's grep has no byte offsets; a marker not found is FAIL

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

# key cache: hits and misses over both backend pods (the arm's sessions
# land on either), since each started with this arm. A line, not a verdict.
hits=0; misses=0
for pod in $(kube get pods -l app.kubernetes.io/component=backend -o name | cut -d/ -f2); do
  set -- $(POD="$pod" in_pod 'for p in $(seq 8080 8090); do wget -qO- -T 5 http://127.0.0.1:$p/metrics 2>/dev/null; done' |
    awk '$1 == "mailbox_crypt_key_cache_total{result=\"hit\"}" { h += $2 } $1 == "mailbox_crypt_key_cache_total{result=\"miss\"}" { m += $2 } END { printf "%d %d", h, m }')
  hits=$((hits + ${1:-0})); misses=$((misses + ${2:-0}))
done
share="-"
[ $((hits + misses)) -gt 0 ] && share="$((hits * 100 / (hits + misses)))%"
echo "INFO key cache: hit $hits, miss $misses ($share hits)" | tee -a "$RESULT"

# files: the arm wiped and filled these ranges (ab-arm.sh type_domain), so
# every message file in them is its own; accounts outside them are older.
files=$(in_pod '
  sig="age-encryption.org/v1"
  md=0; mdbad=0; db=0; dbbad=0
  users() { for n in $(seq "$2" "$3"); do echo "/var/mail/vhosts/$1/u$n@$1"; done; }
  for f in $(find $(users d00002.test 51 100) -path "*/Maildir/cur/*" -type f -o -path "*/Maildir/new/*" -type f 2>/dev/null); do
    md=$((md + 1)); [ "$(head -c 21 "$f")" = "$sig" ] || mdbad=$((mdbad + 1))
  done
  for f in $(find $(users d00003.test 101 150) -path "*/dbox-Mails/u.*" -type f 2>/dev/null; find $(users d00001.test 1 50) -path "*/storage/m.*" -type f 2>/dev/null); do
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
# then FETCH whole and a slice; the response comes back to the runner whole.
vec="X25519-gcm-1000.bin"
set -- $(awk -v f="$vec" '$1 == f { print $3, $4, $5, $6, $7 }' "$VEC/SERVED")
want_size="${1:-}" want_whole="${2:-}" s_off="${3:-}" s_len="${4:-}" want_slice="${5:-}"
home=/var/mail/vhosts/d00002.test/u99@d00002.test/Maildir
kube exec -i "$POD" -c yarilo-imap -- sh -c "cat > $home/new/reference-window.$ARM" < "$VEC/$vec"
resp="$OUT/reference-$ARM.imap"
in_pod "
  uid=\$(printf 'a LOGIN u99@d00002.test $PASSWORD\r\nb SELECT INBOX\r\nc UID SEARCH ALL\r\nd LOGOUT\r\n' | nc -w 15 yarilo-imap-login 143 | tr -d '\r' | sed -n 's/^\* SEARCH .* \([0-9]*\)\$/\1/p')
  printf \"a LOGIN u99@d00002.test $PASSWORD\r\nb SELECT INBOX\r\nc UID FETCH \$uid (RFC822.SIZE BODY.PEEK[])\r\nd UID FETCH \$uid (BODY.PEEK[]<$s_off.$s_len>)\r\ne LOGOUT\r\n\" | nc -w 15 yarilo-imap-login 143" > "$resp"
size=$(tr -d '\r' < "$resp" | sed -n 's/.*RFC822.SIZE \([0-9]*\).*/\1/p' | head -1)
# The literal starts after "{n}" and its CRLF; the offset is GNU grep's.
literal_at() { # <marker>
  local at
  at=$(LC_ALL=C grep -abo -F "$1" "$resp" | head -1 | cut -d: -f1)
  [ -n "$at" ] && echo $((at + ${#1} + 2))
}
w=$(literal_at "BODY[] {$want_size}")
p=$(literal_at "BODY[]<$s_off> {$s_len}")
if [ -z "$want_size" ]; then
  verdict reference fail "$vec is not in $VEC/SERVED"
elif [ -z "$w" ] || [ -z "$p" ]; then
  verdict reference fail "size ${size:-?}: marker not found (BODY[] {$want_size}: ${w:-none}, BODY[]<$s_off> {$s_len}: ${p:-none}), response in $resp"
else
  whole=$(tail -c +$((w + 1)) "$resp" | head -c "$want_size" | sha256sum | cut -d' ' -f1)
  slice=$(tail -c +$((p + 1)) "$resp" | head -c "$s_len" | sha256sum | cut -d' ' -f1)
  if [ "$size" = "$want_size" ] && [ "$whole" = "$want_whole" ] && [ "$slice" = "$want_slice" ]; then
    verdict reference pass "RFC822.SIZE $size, BODY[] and BODY[]<$s_off.$s_len> digests are the served vector's"
  else
    verdict reference fail "size ${size:-?} (want $want_size), whole $whole (want $want_whole), slice $slice (want $want_slice)"
  fi
fi
in_pod "rm -f $home/new/reference-window.$ARM $home/cur/reference-window.$ARM*" || true

exit "$failed"
