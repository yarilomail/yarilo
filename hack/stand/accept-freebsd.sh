#!/usr/bin/env bash
# The single-host acceptance run on the FreeBSD host: build mail/yarilo at a
# module version, install it, and judge the host suite the way accept.sh judges
# the stand, one PASS/FAIL/SKIP line per criterion in verdict.txt.
#
# Usage, as root on the host:
#   bash hack/stand/accept-freebsd.sh <module-version> <smoketest-binary> <out-dir>
#
# <module-version> is the Go version of github.com/yarilomail/yarilo/v2 at the
# commit under test (go list -m github.com/yarilomail/yarilo/v2@<commit>).
# <smoketest-binary> is app/smoketest built from the same commit for the host:
#   GOOS=freebsd GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o smoketest ./app/smoketest
# ACCEPT_SKIP_BUILD=1 judges the package already installed, which must be that
# version. Exits 1 when any criterion fails.

set -Eeuo pipefail
trap 'rc=$?; echo "accept-freebsd: line $LINENO: $BASH_COMMAND exited $rc" >&2' ERR

VERSION="${1:?module version}"
SMOKE_SRC="${2:?smoketest binary}"
OUT="${3:?output directory}"
PORT="${ACCEPT_PORT:-/usr/ports/mail/yarilo}"
CONF="${ACCEPT_CONF:-/usr/local/etc/yarilo/yarilo.yaml}"
RCCONF="${ACCEPT_RCCONF:-/etc/rc.conf.d/yarilo}"
SECRETS="${ACCEPT_SECRETS:-/root/run.secrets}"
CLIENT="${ACCEPT_CLIENT_USER:-support}"
CA="${ACCEPT_CA:-/home/$CLIENT/host-cert.pem}"
IMAPTEST="${ACCEPT_IMAPTEST:-/home/$CLIENT/imaptest/src/imaptest}"
MBOX="${ACCEPT_MBOX:-/home/$CLIENT/test.mbox}"
PROCS="${ACCEPT_PROCS:-17}"
CLOCK_MAX_MS="${ACCEPT_CLOCK_MAX_MS:-1000}"

# A run never writes over another run's evidence.
[ -e "$OUT" ] && { echo "accept-freebsd: $OUT exists; pick a new directory" >&2; exit 2; }
[ "$(id -u)" = 0 ] || { echo "accept-freebsd: run as root" >&2; exit 2; }
mkdir -p "$OUT/logs"
: > "$OUT/verdict.txt"
FAILS=0
SKIPS=0
PASS=$(sed -n 1p "$SECRETS")
[ -n "$PASS" ] || { echo "accept-freebsd: no password in $SECRETS" >&2; exit 2; }
SMOKE="/home/$CLIENT/yarilo-smoketest.accept"

# Rows the host cannot run, by name: no admin API, no director, no OAuth2
# provider, no ACL, no WebSocket ticket, no extra passdbs or quota account.
HOST_SKIPS='smtp MX PROXY protocol
smtp MX XCLIENT cap
oauth2 imap AUTHENTICATE OAUTHBEARER (SASL-IR) + SELECT INBOX
oauth2 pop3 AUTH OAUTHBEARER + STAT
oauth2 submission AUTH OAUTHBEARER + MAIL FROM
imap login (passwd-file passdb)
imap login (static passdb)
imap QUOTA enforcement (OVERQUOTA)
imap ACL (MYRIGHTS + SETACL round-trip)
imap ACL disclosure (shared namespace, peer vs absent mailbox)
imap FTS document is the message (u1@d00001.test)
imap FTS document is the message (u51@d00002.test)
imap FTS document is the message (u101@d00003.test)
director admin API status (authenticated)
consistency imap<->admin API quota numbers
jmap connection limit refused with 429 and Retry-After
jmap WebSocket opened with a ticket alone pushes a StateChange
jmap forged client address header from an untrusted peer is ignored'
# Skipped only while FTS is off.
NOFTS_SKIPS='consistency imap SEARCH <-> jmap Email/query over one term
jmap Email/query over full-text search'

verdict() {
  case "$2" in
    ok) echo "PASS $1: $3" | tee -a "$OUT/verdict.txt" ;;
    skip) echo "SKIP $1: $3" | tee -a "$OUT/verdict.txt"; SKIPS=$((SKIPS + 1)) ;;
    *) echo "FAIL $1: $3" | tee -a "$OUT/verdict.txt"; FAILS=$((FAILS + 1)) ;;
  esac
}

as_client() { su -m "$CLIENT" -c "$*"; }
running() { service yarilo status 2>&1 | grep -c 'is running' || true; }
imap_start() { ps -axo lstart,command | grep '[l]ibexec/yarilo/yarilo-imap$' | head -1; }

# imaps sends the IMAP stream on stdin and prints the replies. The stream comes
# on stdin, never through $(...), which drops the last line's LF (docs-internal#207).
imaps() {
  { cat; sleep "${1:-3}"; } | timeout 30 openssl s_client -quiet -connect 127.0.0.1:993 2>/dev/null | tr -d '\r' || true
}

smoke_run() {
  local user="$1" peer="$2" log="$3"
  as_client "env SSL_CERT_FILE=$CA $SMOKE \
    -host=127.0.0.1 -tls-server-name=localhost -timeout=20s -telemetry=http://127.0.0.1:18092 \
    -imap-port=993 -imap-user=$user -imap-pass='$PASS' \
    -smtp-sub -smtp-sub-send -smtp-sub-port=587 -lmtp-login -lmtp-login-port=24 \
    -pop3s -pop3s-port=995 -pop3-user=$user -pop3-pass='$PASS' \
    -managesieve -managesieve-port=4190 -managesieve-user=$user -managesieve-pass='$PASS' \
    -sieve -delivery-host=127.0.0.1 -delivery-port=24 -delivery-proto=lmtp \
    -scram-user=scram@d00001.test -scram-pass='$PASS' -quota-user=$user -quota-pass='$PASS' \
    -acl-user=$user -acl-pass='$PASS' -acl-peer-user=$peer -acl-peer-pass='$PASS' -acl-shared-prefix=user/$user/ \
    -envelope-user=$user -envelope-pass='$PASS' -fts-user=$user -fts-pass='$PASS' \
    -fts-copy-users=u1@d00001.test:'$PASS',u51@d00002.test:'$PASS',u101@d00003.test:'$PASS' \
    -smtp-mx -smtp-mx-host=127.0.0.1 -smtp-mx-port=25 \
    -jmap -jmap-host=127.0.0.1 -jmap-port=8443 -jmap-user=$user -jmap-pass='$PASS'" > "$log" 2>&1 || true
}

# smoke_judge: nothing failed and the skipped rows are exactly the named ones.
smoke_judge() {
  local summary failed skips want
  summary=$(grep -o '"checks":[0-9]*,"passed":[0-9]*,"failed":[0-9]*,"skipped":[0-9]*' "$1" | tail -1 || true)
  failed=$(echo "$summary" | sed -n 's/.*"failed":\([0-9]*\).*/\1/p')
  skips=$(grep '"smoke: SKIP"' "$1" | grep -o '"check":"[^"]*"' | sed 's/^"check":"//; s/"$//' | sort || true)
  want=$(printf '%s\n' "$2" | grep . | sort)
  if [ -n "$summary" ] && [ "$failed" = 0 ] && [ "$skips" = "$want" ]; then
    echo "ok ${summary//\"/}"
  else
    echo "fail ${summary:-no summary}; skips not expected: $(comm -23 <(echo "$skips") <(echo "$want") | tr '\n' ';') missing: $(comm -13 <(echo "$skips") <(echo "$want") | tr '\n' ';') $(grep '"smoke: FAIL"' "$1" | head -3 | cut -c1-200 | tr '\n' ' ')"
  fi
}

smoke_part() {
  local part="$1" skips="$2" spec user judged
  for spec in u1@d00001.test:u2@d00001.test u51@d00002.test:u52@d00002.test u101@d00003.test:u102@d00003.test; do
    user="${spec%%:*}"
    smoke_run "$user" "${spec#*:}" "$OUT/logs/smoke-$part-$user.log"
    judged=$(smoke_judge "$OUT/logs/smoke-$part-$user.log" "$skips")
    case "$judged" in
      ok*) verdict "smoketest $part $user" ok "${judged#ok }" ;;
      *) verdict "smoketest $part $user" fail "${judged#fail }" ;;
    esac
  done
}

date -u +"start %FT%TZ" > "$OUT/times"
cp "$CONF" "$OUT/yarilo.yaml.start"
cp "$RCCONF" "$OUT/rc.conf.start"
restore() {
  install -m 0644 -o root -g wheel "$OUT/yarilo.yaml.start" "$CONF"
  install -m 0644 "$OUT/rc.conf.start" "$RCCONF"
  service yarilo restart > /dev/null 2>&1 || true
}
trap restore EXIT

echo "== clock"
off=$(ntpq -pn 2>/dev/null | awk '/^\*/{print $9}')
if [ -n "$off" ] && awk -v o="$off" -v m="$CLOCK_MAX_MS" 'BEGIN{exit !(o < m && o > -m)}'; then
  verdict "clock in sync" ok "ntpd offset ${off} ms"
else
  verdict "clock in sync" fail "ntpd offset '${off:-none}' ms, date $(date -u +%FT%TZ)"
fi

echo "== port"
if [ "${ACCEPT_SKIP_BUILD:-0}" != 1 ]; then
  dist="/usr/ports/distfiles/go/mail_yarilo/yarilo-v$VERSION"
  cp "$PORT/Makefile" "$OUT/Makefile.before"
  mkdir -p "$dist"
  for ext in mod zip; do
    [ -s "$dist/v$VERSION.$ext" ] || fetch -q -o "$dist/v$VERSION.$ext" "https://proxy.golang.org/github.com/yarilomail/yarilo/v2/@v/v$VERSION.$ext"
  done
  sed -i '' "s/^DISTVERSION=.*/DISTVERSION=	$VERSION/" "$PORT/Makefile"
  rc=0
  (cd "$PORT" && make BATCH=yes clean makesum build stage check-plist stage-qa) > "$OUT/logs/port-build.log" 2>&1 || rc=$?
  [ "$rc" = 0 ] && verdict "port builds" ok "make build stage check-plist stage-qa" || verdict "port builds" fail "rc=$rc (logs/port-build.log)"
  grep -q 'No pkg-plist issues found' "$OUT/logs/port-build.log" && verdict "check-plist clean" ok "no pkg-plist issues" ||
    verdict "check-plist clean" fail "logs/port-build.log"
  notes=$(grep -E '^(Warning|Error):' "$OUT/logs/port-build.log" | grep -v 'you might not need LIB_DEPENDS on libxapian.so' || true)
  [ -z "$notes" ] && verdict "stage-qa notes" ok "only the libxapian LIB_DEPENDS note" ||
    verdict "stage-qa notes" fail "$(echo "$notes" | tr '\n' ' ' | cut -c1-300)"
  rc=0
  (cd "$PORT" && make BATCH=yes deinstall install) > "$OUT/logs/port-install.log" 2>&1 || rc=$?
  [ "$rc" = 0 ] && verdict "port installs" ok "deinstall install" || verdict "port installs" fail "rc=$rc (logs/port-install.log)"
fi
want_pkg="yarilo-$(echo "$VERSION" | tr -- '-' '.')"
got_pkg=$(pkg info -x '^yarilo-[0-9]' 2>/dev/null | head -1)
[ "$got_pkg" = "$want_pkg" ] && verdict "installed version" ok "$got_pkg" || verdict "installed version" fail "installed '$got_pkg', want '$want_pkg'"
install -o "$CLIENT" -m 755 "$SMOKE_SRC" "$SMOKE"

echo "== restart on the installed binaries"
service yarilo restart > /dev/null 2>&1 || true
sleep 6
n=$(running)
[ "$n" = "$PROCS" ] && verdict "processes running" ok "$n" || verdict "processes running" fail "$n of $PROCS: $(service yarilo status 2>&1 | grep -v 'is running' | tr '\n' ' ')"
imap_start > "$OUT/imap-start.fts"

echo "== smoketest with FTS"
smoke_part fts "$HOST_SKIPS"

echo "== mail between mailboxes"
CURL="curl -s --cacert $CA --connect-to localhost:587:127.0.0.1:587 --connect-to localhost:993:127.0.0.1:993"
sent=0; got=0; bcc=0
for pair in u1@d00001.test:u51@d00002.test u51@d00002.test:u101@d00003.test u101@d00003.test:u1@d00001.test u1@d00001.test:u101@d00003.test; do
  from=${pair%%:*}; to=${pair#*:}; m="xcross-$(date +%s)-$$-$sent"
  eml="$OUT/cross-$sent.eml"
  printf 'From: <%s>\r\nTo: <%s>\r\nBcc: <%s>\r\nSubject: %s\r\nMessage-ID: <%s@accept>\r\n\r\ncross-type delivery\r\n' "$from" "$to" "$to" "$m" "$m" > "$eml"
  chmod 644 "$eml"
  as_client "$CURL --ssl-reqd --url smtp://localhost:587 --user '$from:$PASS' --mail-from '$from' --mail-rcpt '$to' --upload-file $eml" && sent=$((sent + 1))
  r=""
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    r=$(as_client "$CURL --url 'imaps://localhost:993/INBOX' --user '$to:$PASS' -X 'UID SEARCH HEADER SUBJECT $m'" || true)
    case "$r" in *SEARCH\ [0-9]*) got=$((got + 1)); break ;; esac
    sleep 1
  done
  b=$(as_client "$CURL --url 'imaps://localhost:993/INBOX' --user '$to:$PASS' -X 'UID SEARCH HEADER SUBJECT $m HEADER BCC \"\"'" || true)
  case "$b" in *SEARCH\ [0-9]*) bcc=$((bcc + 1)) ;; esac
  echo "$from -> $to: $(echo "$r" | tr -d '\r' | tr '\n' ' ')" >> "$OUT/logs/cross.log"
done
[ "$sent" = 4 ] && [ "$got" = 4 ] && [ "$bcc" = 0 ] && verdict "mail between mailboxes" ok "4 sent, 4 delivered, no Bcc kept" ||
  verdict "mail between mailboxes" fail "sent=$sent delivered=$got with_bcc=$bcc"

echo "== METADATA through yarilo-dict"
mark="accept-$(date +%s)"
printf 'a LOGIN u1@d00001.test %s\r\nb SETMETADATA INBOX (/private/comment "%s")\r\nc GETMETADATA INBOX /private/comment\r\nd GETMETADATA (DEPTH infinity) INBOX (/private)\r\nz LOGOUT\r\n' "$PASS" "$mark" | imaps > "$OUT/logs/metadata.log"
oks=$(grep -cE '^[bcdz] OK' "$OUT/logs/metadata.log" || true)
grep -q "\"$mark\"" "$OUT/logs/metadata.log" && [ "$oks" = 4 ] && verdict "METADATA round trip" ok "SET, GET and DEPTH infinity OK" ||
  verdict "METADATA round trip" fail "ok=$oks of 4 (LOGOUT included), value $(grep -c "$mark" "$OUT/logs/metadata.log" || true)"

echo "== imaptest, one type at a time"
for spec in d00001.test:3-20 d00002.test:53-70 d00003.test:103-120; do
  dom=${spec%%:*}
  as_client "$IMAPTEST host=127.0.0.1 port=143 user=u%d@$dom users=${spec##*:} pass='$PASS' clients=50 msgs=500 secs=90 mbox=$MBOX" > "$OUT/logs/imaptest-$dom.log" 2>&1 || true
  errors=$(grep -ac '^Error' "$OUT/logs/imaptest-$dom.log" || true)
  stalls=$(grep -ac 'stalled' "$OUT/logs/imaptest-$dom.log" || true)
  logins=$(tail -1 "$OUT/logs/imaptest-$dom.log" | awk '{print $1}')
  [ "$errors" = 0 ] && [ "$stalls" = 0 ] && [ -n "$logins" ] && verdict "imaptest $dom" ok "logins=$logins" ||
    verdict "imaptest $dom" fail "errors=$errors stalled=$stalls logins=${logins:-?}"
done

echo "== FTS short tokens"
U=u101@d00003.test; T=$(date +%s)
for s in 7 1 77; do
  printf 'LHLO accept\r\nMAIL FROM:<a@d00001.test>\r\nRCPT TO:<%s>\r\nDATA\r\nSubject: xtok-%s-%s\r\nMessage-ID: <xtok-%s-%s@accept>\r\n\r\nshort token\r\n.\r\nQUIT\r\n' "$U" "$T" "$s" "$T" "$s" | timeout 30 nc -N 127.0.0.1 24 > /dev/null || true
done
sleep 5
search() { printf 'a LOGIN %s %s\r\nb SELECT INBOX\r\nc UID SEARCH SUBJECT %s\r\nz LOGOUT\r\n' "$U" "$PASS" "$1" | imaps 4 | sed -n 's/^\* SEARCH *//p' | wc -w | tr -d ' '; }
t7=$(search "xtok-$T-7"); t1=$(search "xtok-$T-1"); t77=$(search "xtok-$T-77")
echo "T=$T xtok-T-7=$t7 xtok-T-1=$t1 xtok-T-77=$t77" > "$OUT/logs/fts-tokens.log"
[ "$t7" = 2 ] && [ "$t1" = 1 ] && [ "$t77" = 1 ] && verdict "FTS short tokens" ok "-7 finds 2, -1 finds 1, -77 finds 1" ||
  verdict "FTS short tokens" fail "-7=$t7 (want 2), -1=$t1 (want 1), -77=$t77 (want 1)"

echo "== disk = index, every mailbox"
mkdir -p "$OUT/index-raw"
for uk in u51@d00002.test:maildir u101@d00003.test:sdbox; do
  u=${uk%%:*}; kind=${uk#*:}; R="$OUT/index-raw/$u"
  printf 'a LOGIN %s %s\r\nb LIST "" "*"\r\nz LOGOUT\r\n' "$u" "$PASS" | imaps 2 > "$R.list"
  grep '^\* LIST' "$R.list" | grep -v Noselect | sed 's/.*"[./]" //' > "$R.boxes" || true
  nb=$(wc -l < "$R.boxes" | tr -d ' ')
  awk -v u="$u" -v p="$PASS" 'BEGIN{printf "a LOGIN %s %s\r\n", u, p} {printf "s%d STATUS %s (MESSAGES)\r\n", NR, $0} END{printf "z LOGOUT\r\n"}' "$R.boxes" | imaps > "$R.status"
  ns=$(grep -c '^\* STATUS' "$R.status" || true)
  bad=$(grep -cE '^s[0-9]+ (NO|BAD)' "$R.status" || true)
  index=$(grep '^\* STATUS' "$R.status" | sed 's/.*MESSAGES \([0-9]*\).*/\1/' | awk '{s+=$1} END{print s+0}')
  home="/var/mail/vhosts/${u#*@}/${u%@*}"
  case "$kind" in
    maildir) disk=$(find "$home/Maildir" \( -path '*/cur/*' -o -path '*/new/*' \) -type f | wc -l | tr -d ' ') ;;
    sdbox) disk=$(find "$home/sdbox" -name 'u.*' -type f | wc -l | tr -d ' ') ;;
  esac
  [ "$nb" -gt 0 ] && [ "$ns" = "$nb" ] && [ "$bad" = 0 ] && grep -q '^z OK' "$R.status" && [ "$disk" = "$index" ] &&
    verdict "disk = index $u" ok "$nb mailboxes, $disk = $index" ||
    verdict "disk = index $u" fail "mailboxes=$nb status=$ns no_bad=$bad disk=$disk index=$index"
done

imap_start > "$OUT/imap-start.fts-end"
cmp -s "$OUT/imap-start.fts" "$OUT/imap-start.fts-end" && verdict "no restart during the FTS part" ok "yarilo-imap $(cut -c1-24 "$OUT/imap-start.fts")" ||
  verdict "no restart during the FTS part" fail "yarilo-imap started $(cat "$OUT/imap-start.fts-end")"

echo "== smoketest without FTS"
# Only the fts block's own enabled line: any other section keeps its value.
awk '/^[^[:space:]#]/ { in_fts = ($0 ~ /^fts:/) } in_fts && /^  enabled: true$/ { sub(/true$/, "false") } { print }' \
  "$OUT/yarilo.yaml.start" > "$CONF"
changed=$(diff "$OUT/yarilo.yaml.start" "$CONF" | grep -c '^[<>]' || true)
[ "$changed" = 2 ] && verdict "FTS off changes one line" ok "fts.enabled false" ||
  verdict "FTS off changes one line" fail "$changed changed lines, want 2 (one out, one in)"
sed 's/ fts / /' "$OUT/rc.conf.start" > "$RCCONF"
service yarilo stop fts > /dev/null 2>&1 || true
service yarilo restart > /dev/null 2>&1 || true
sleep 5
imap_start > "$OUT/imap-start.nofts"
smoke_part nofts "$(printf '%s\n%s\n' "$HOST_SKIPS" "$NOFTS_SKIPS")"
imap_start > "$OUT/imap-start.nofts-end"
cmp -s "$OUT/imap-start.nofts" "$OUT/imap-start.nofts-end" && verdict "no restart during the no-FTS part" ok "yarilo-imap $(cut -c1-24 "$OUT/imap-start.nofts")" ||
  verdict "no restart during the no-FTS part" fail "yarilo-imap started $(cat "$OUT/imap-start.nofts-end")"

echo "== restore"
trap - EXIT
restore
sleep 6
n=$(running)
cmp -s "$OUT/yarilo.yaml.start" "$CONF" && [ "$n" = "$PROCS" ] && verdict "run config restored" ok "$n running" ||
  verdict "run config restored" fail "running=$n config $(cmp -s "$OUT/yarilo.yaml.start" "$CONF" && echo same || echo differs)"
date -u +"end %FT%TZ" >> "$OUT/times"

echo "accept-freebsd: version=$VERSION fails=$FAILS skips=$SKIPS" | tee -a "$OUT/verdict.txt"
if [ "$FAILS" -gt 0 ]; then exit 1; fi
exit 0
