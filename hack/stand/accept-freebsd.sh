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
# The port's DISTVERSION carries no v (DISTVERSIONPREFIX adds it), and the
# proxy paths below add their own: a v given here is taken off once.
VERSION="${VERSION#v}"
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
# yarilo processes: the components, the log process and yarilo-master (#2715).
PROCS="${ACCEPT_PROCS:-19}"
# CPU of the supervisor and of the log process over the imaptest part, at
# info: measured on the host at 0.4-0.7 % and 2.1-4.3 % (2026-11-26).
MASTER_CPU_MAX="${ACCEPT_MASTER_CPU_MAX:-3}"
LOG_CPU_MAX="${ACCEPT_LOG_CPU_MAX:-15}"
# The hard open-files limit a session process must keep: by default the
# kernel's per-process limit, read at the run, since the kernel sets it at
# boot from the memory (116928 before a reboot of the host, 116919 after).
MIN_NOFILE="${ACCEPT_MIN_NOFILE:-$(sysctl -n kern.maxfilesperproc)}"
MASTER_METRICS="${ACCEPT_MASTER_METRICS:-http://127.0.0.1:18079/metrics}"
CLOCK_MAX_MS="${ACCEPT_CLOCK_MAX_MS:-1000}"
# Both whole-server stalls on the host came with 12 % free and none of the
# runs with 44 % free (#2715); no run lies between.
DISK_MIN_FREE_PCT="${ACCEPT_DISK_MIN_FREE_PCT:-25}"

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
# Components the supervisor reports running; status lines are tab-separated
# fields: name, state, pid, uptime, restarts, why.
# status exits 1 while a component is down; under pipefail that must not end
# the run, so each reader below ends in "|| true".
running() { { service yarilo status 2>/dev/null || true; } | awk -F'\t' '$2 == "running"' | wc -l | tr -d ' '; }
# The supervisor is /usr/local/sbin/yarilo-master; daemon(8) writes its pid.
MASTER_PIDFILE="${ACCEPT_MASTER_PIDFILE:-/var/run/yarilo/master.pid}"
# Empty while there is none, as just after a kill; never a failure, which
# set -e would end the run on.
master_pid() {
  local pid
  pid=$(cat "$MASTER_PIDFILE" 2>/dev/null || true)
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then echo "$pid"; fi
  return 0
}
log_pid() { pgrep -f '^/usr/local/sbin/yarilo-master log' | head -1 || true; }
# Every yarilo process: the components in libexec, the log process, and the
# supervisor.
yarilo_procs() {
  local n
  n=$(ps -axo command | grep -cE '^/usr/local/libexec/yarilo/yarilo-' || true)
  [ -n "$(master_pid)" ] && n=$((n + 1))
  [ -n "$(log_pid)" ] && n=$((n + 1))
  echo "$n"
}
cpu_s() { ps -o time= -p "$1" 2>/dev/null | awk '{n=split($1,a,":"); s=0; for(i=1;i<=n;i++) s=s*60+a[i]; print s+0}'; }
# starts: every component process with its start time, one a line.
starts() { ps -axo pid,lstart,command | grep -E '/usr/local/libexec/yarilo/yarilo-' | grep -v grep | sort; }
# comp_uptime name: seconds the component has run, from status.
comp_uptime() { { service yarilo status "$1" 2>/dev/null || true; } | awk -F'\t' -v c="$1" '$1 == c {print $4}'; }
comp_pid() { { service yarilo status "$1" 2>/dev/null || true; } | awk -F'\t' -v c="$1" '$1 == c {print $3}'; }
restarts_metric() {
  { curl -s "$MASTER_METRICS" || true; } | awk -v c="$1" -v r="$2" \
    'index($0, "yarilo_master_restarts_total{") == 1 && index($0, "component=\"" c "\"") && index($0, "reason=\"" r "\"") {print $2}' | head -1
}
# up_within N name...: every named component is running within N seconds.
up_within() {
  local n="$1" c ok; shift
  for _ in $(seq 1 $((n * 10))); do
    ok=1
    for c in "$@"; do service yarilo status "$c" > /dev/null 2>&1 || ok=0; done
    [ "$ok" = 1 ] && return 0
    sleep 0.1
  done
  return 1
}
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
# The owner and mode go back as they were: yarilo.yaml is root:yarilo-auth
# 0640, and a run that left it 0644 failed the next run's log-process row.
set -- $(stat -f '%Su %Sg %Lp' "$CONF")
conf_owner=$1 conf_group=$2 conf_mode=$3
set -- $(stat -f '%Su %Sg %Lp' "$RCCONF")
rc_owner=$1 rc_group=$2 rc_mode=$3
restore() {
  install -m "$conf_mode" -o "$conf_owner" -g "$conf_group" "$OUT/yarilo.yaml.start" "$CONF"
  install -m "$rc_mode" -o "$rc_owner" -g "$rc_group" "$OUT/rc.conf.start" "$RCCONF"
  service yarilo restart > /dev/null 2>&1 || true
}
trap restore EXIT

echo "== clock"
# chronyd when it runs (the host steps a paused VM's clock with it), else ntpd.
if command -v chronyc > /dev/null 2>&1 && chronyc -c tracking > /dev/null 2>&1; then
  src=chronyd
  off=$(chronyc -c tracking | awk -F, '$14 == "Normal" {printf "%.3f", $5 * 1000}')
else
  src=ntpd
  off=$(ntpq -pn 2>/dev/null | awk '/^\*/{print $9}')
fi
if [ -n "$off" ] && awk -v o="$off" -v m="$CLOCK_MAX_MS" 'BEGIN{exit !(o < m && o > -m)}'; then
  verdict "clock in sync" ok "$src offset ${off} ms"
else
  verdict "clock in sync" fail "$src offset '${off:-none}' ms, date $(date -u +%FT%TZ)"
fi

echo "== disk"
# Every file system the mail, the state and the logs live on, and / (the
# figures in #2715 are df /), each once.
free=$(df -kP / /var/mail/vhosts /var/db/yarilo /var/log/yarilo 2>/dev/null | awk 'NR > 1 && !seen[$6]++ {
  printf "%s%s %d%%", sep, $6, 100 - $5; sep = ", "
  if (100 - $5 < min || min == "") min = 100 - $5
} END {print "|" min}')
if [ -n "${free#*|}" ] && [ "${free#*|}" -ge "$DISK_MIN_FREE_PCT" ]; then
  verdict "free space at least ${DISK_MIN_FREE_PCT} %" ok "${free%|*} free"
else
  verdict "free space at least ${DISK_MIN_FREE_PCT} %" fail "${free%|*} free"
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
# The port names the package: it splits a version at letter/digit borders.
port_ver=$(cd "$PORT" && make -V DISTVERSION)
want_pkg=$(cd "$PORT" && make -V PKGNAME)
got_pkg=$(pkg info -x '^yarilo-[0-9]' 2>/dev/null | head -1)
[ "$port_ver" = "$VERSION" ] && [ "$got_pkg" = "$want_pkg" ] && verdict "installed version" ok "$got_pkg" ||
  verdict "installed version" fail "port DISTVERSION '$port_ver' (want '$VERSION'), installed '$got_pkg', port names '$want_pkg'"
install -o "$CLIENT" -m 755 "$SMOKE_SRC" "$SMOKE"

echo "== restart on the installed binaries"
service yarilo restart > /dev/null 2>&1 || true
sleep 6
n=$(running); p=$(yarilo_procs)
[ "$n" = "$((PROCS - 1))" ] && [ "$p" = "$PROCS" ] && verdict "processes running" ok "$n components and yarilo-master" ||
  verdict "processes running" fail "$n components running of $((PROCS - 1)), $p yarilo processes of $PROCS: $(service yarilo status 2>&1 | awk -F'\t' '$2 != "running"' | tr '\n' ' ')"

echo "== yarilo-master"
service yarilo status > "$OUT/logs/master-status.start" 2>&1 || true
mpid=$(master_pid)
# No component runs as root, real or effective; only yarilo-master does.
rootc=$(ps -axo uid,ruid,command | awk '$3 ~ /^\/usr\/local\/libexec\/yarilo\/yarilo-/ && ($1 == 0 || $2 == 0)' | wc -l | tr -d ' ')
[ "$rootc" = 0 ] && [ -n "$mpid" ] && [ "$(ps -o uid= -p "$mpid" | tr -d ' ')" = 0 ] &&
  verdict "only yarilo-master runs as root" ok "master pid $mpid" ||
  verdict "only yarilo-master runs as root" fail "components as root: $rootc, master pid '${mpid:-none}'"

# The log process runs as yarilo-log, which may not read yarilo.yaml.
lpid=$(log_pid)
luser=$(ps -o user= -p "${lpid:-0}" 2>/dev/null | tr -d ' ')
if [ -n "$lpid" ] && [ "$luser" = yarilo-log ] && ! su -m yarilo-log -c "cat $CONF" > /dev/null 2>&1; then
  verdict "the log process cannot read yarilo.yaml" ok "pid $lpid as yarilo-log, $CONF refused"
else
  verdict "the log process cannot read yarilo.yaml" fail "log pid '${lpid:-none}' as '${luser:-?}', or yarilo-log reads $CONF"
fi

# kill -KILL of the log process: it is back within 1 s, and no component
# restarted, their start times all unchanged.
starts > "$OUT/logs/starts-before-log-kill.txt"
kill -KILL "$lpid" 2>/dev/null || true
back=""
for _ in $(seq 1 10); do
  back=$(log_pid)
  [ -n "$back" ] && [ "$back" != "$lpid" ] && break
  sleep 0.1
done
sleep 1
starts > "$OUT/logs/starts-after-log-kill.txt"
if [ -n "$back" ] && [ "$back" != "$lpid" ] && cmp -s "$OUT/logs/starts-before-log-kill.txt" "$OUT/logs/starts-after-log-kill.txt"; then
  verdict "log process killed comes back" ok "pid $lpid -> $back within 1 s; every component kept its start time"
else
  verdict "log process killed comes back" fail "log pid '${back:-none}' (was $lpid); components $(diff "$OUT/logs/starts-before-log-kill.txt" "$OUT/logs/starts-after-log-kill.txt" | grep -c '^>') changed"
fi

ipid=$(comp_pid imap)
procstat -e "$ipid" > "$OUT/logs/imap-env.txt" 2>&1 || true
procstat -l "$ipid" > "$OUT/logs/imap-limits.txt" 2>&1 || true
# master_environment as "NAME=value" lines, from a block map or a one-line
# flow map; any other form stops the run, not a criterion.
master_env() {
  awk '
    /^[^[:space:]#]/ { m = ($0 ~ /^master:/); e = 0 }
    m && /^[[:space:]]+master_environment:/ {
      line = $0; sub(/^[^:]*:[[:space:]]*/, "", line)
      if (line ~ /^\{.*\}[[:space:]]*$/) {
        gsub(/^\{|\}[[:space:]]*$/, "", line); n = split(line, kv, ",")
        for (i = 1; i <= n; i++) { k = kv[i]; v = kv[i]; sub(/:.*/, "", k); sub(/^[^:]*:/, "", v)
          gsub(/[[:space:]"'"'"']/, "", k); gsub(/^[[:space:]]+|[[:space:]]+$|["'"'"']/, "", v); if (k != "") print k "=" v }
        next
      }
      if (line == "" ) { e = 1; next }
      print "UNREADABLE"; next
    }
    e && /^[[:space:]][[:space:]][[:space:]][[:space:]][A-Za-z_][A-Za-z0-9_]*:/ { k = $0; v = $0; sub(/:.*/, "", k); sub(/^[^:]*:/, "", v)
      gsub(/[[:space:]]/, "", k); gsub(/^[[:space:]]+|[[:space:]]+$|["'"'"']/, "", v); print k "=" v; next }
    e && !/^[[:space:]][[:space:]][[:space:]][[:space:]]/ { e = 0 }
  ' "$CONF"
}
menv=$(master_env)
case "$menv" in *UNREADABLE*) echo "accept-freebsd: master_environment in $CONF is neither a block nor a one-line map" >&2; exit 2 ;; esac
want_gmp=$(printf '%s\n' "$menv" | sed -n 's/^GOMAXPROCS=//p' | head -1)
# Unset in master_environment, the supervisor gives the default, 2.
gmp_from="master_environment"
[ -z "$want_gmp" ] && { want_gmp=2; gmp_from="the default"; }
if grep -qw "GOMAXPROCS=$want_gmp" "$OUT/logs/imap-env.txt"; then
  verdict "GOMAXPROCS from master_environment, else 2" ok "GOMAXPROCS=$want_gmp in yarilo-imap, from $gmp_from"
else
  verdict "GOMAXPROCS from master_environment, else 2" fail "yarilo-imap lacks GOMAXPROCS=$want_gmp from $gmp_from (logs/imap-env.txt)"
fi
# Only the variables yarilo-master gives: its fixed four, GOMAXPROCS, which it
# always gives, and master_environment.
# Each name once: GOMAXPROCS set in master_environment is the same variable.
allowed=$(printf '%s\n' PATH CONFIG LOG_LEVEL TELEMETRY_LISTEN GOMAXPROCS $(printf '%s\n' "$menv" | cut -d= -f1) |
  awk 'NF && !seen[$0]++' | tr '\n' ' ')
extra=$(tail -n +2 "$OUT/logs/imap-env.txt" | tr ' ' '\n' | grep '=' | cut -d= -f1 | sort -u | while read -r v; do
  case " $allowed " in (*" $v "*) ;; (*) printf '%s ' "$v" ;; esac
done)
[ -z "$extra" ] && verdict "a component's environment is its own" ok "only: $allowed" ||
  verdict "a component's environment is its own" fail "yarilo-imap also has: $extra"
nofile=$(awk '$3 == "openfiles" {print $5}' "$OUT/logs/imap-limits.txt" | head -1)
if [ -n "$nofile" ] && { [ "$nofile" = infinity ] || [ "$nofile" -ge "$MIN_NOFILE" ]; }; then
  verdict "open-files limit kept" ok "hard $nofile >= $MIN_NOFILE"
else
  verdict "open-files limit kept" fail "hard ${nofile:-?} < $MIN_NOFILE (logs/imap-limits.txt)"
fi

# A kill of a component that has worked: back at once (within 1 s), under the
# same supervisor, counted once. Then a kill of the same one, now young: back
# after the first backoff (2 s), within 4 s.
for c in pop3-login imap; do
  for _ in $(seq 1 90); do [ "$(comp_uptime "$c")" -ge 61 ] 2>/dev/null && break; sleep 1; done
  before=$(restarts_metric "$c" signal); before=${before:-0}
  pid=$(comp_pid "$c")
  kill -KILL "$pid" 2>/dev/null || true
  if up_within 1 "$c" && [ "$(comp_pid "$c")" != "$pid" ]; then
    after=$(restarts_metric "$c" signal); after=${after:-0}
    [ "$(master_pid)" = "$mpid" ] && [ "$((after - before))" = 1 ] &&
      verdict "$c that worked comes back at once" ok "within 1 s of kill -KILL, same supervisor, restarts{signal} +1" ||
      verdict "$c that worked comes back at once" fail "supervisor $(master_pid) (was $mpid), restarts{signal} $before -> $after"
  else
    verdict "$c that worked comes back at once" fail "not running 1 s after kill -KILL of $pid (uptime was 60 s+): $(service yarilo status "$c" 2>&1)"
  fi
  pid=$(comp_pid "$c")
  t0=$(date +%s)
  kill -KILL "$pid" 2>/dev/null || true
  if up_within 4 "$c" && [ "$(comp_pid "$c")" != "$pid" ]; then
    waited=$(( $(date +%s) - t0 ))
    [ "$waited" -ge 1 ] && verdict "$c that died young waits" ok "back after ${waited} s" ||
      verdict "$c that died young waits" fail "back after ${waited} s; the 2 s backoff was skipped"
  else
    verdict "$c that died young waits" fail "not running 4 s after the second kill: $(service yarilo status "$c" 2>&1)"
  fi
done

# stop keeps one down, start brings it back.
service yarilo stop imap > /dev/null 2>&1 || true
sleep 3
if ! service yarilo status imap > /dev/null 2>&1 && ! pgrep -qf '^/usr/local/libexec/yarilo/yarilo-imap$'; then
  service yarilo start imap > /dev/null 2>&1 || true
  up_within 10 imap && verdict "stop and start one component" ok "imap stayed down, then came back" ||
    verdict "stop and start one component" fail "imap not back after start: $(service yarilo status imap 2>&1)"
else
  verdict "stop and start one component" fail "imap still up after stop: $(service yarilo status imap 2>&1)"
fi

# reopen: after a rename the log goes to a new file.
mv /var/log/yarilo/imap-login.log /var/log/yarilo/imap-login.log.accept
service yarilo reopen > /dev/null 2>&1 || true
printf 'a LOGIN u1@d00001.test %s\r\nz LOGOUT\r\n' "$PASS" | imaps 2 > /dev/null
sleep 2
[ -s /var/log/yarilo/imap-login.log ] && verdict "reopen writes a new log file" ok "imap-login.log written after the rename" ||
  verdict "reopen writes a new log file" fail "no new imap-login.log after reopen"
cat /var/log/yarilo/imap-login.log.accept >> "$OUT/logs/imap-login.before-reopen.log" 2>/dev/null || true
rm -f /var/log/yarilo/imap-login.log.accept

# The supervisor killed: daemon -r starts a new one, which stops what the old
# one left and starts one of everything.
kill -KILL "$mpid" 2>/dev/null || true
new=""
for _ in $(seq 1 30); do
  new=$(master_pid)
  [ -n "$new" ] && [ "$new" != "$mpid" ] && [ "$(running)" = "$((PROCS - 1))" ] && break
  sleep 1
done
sleep 2
p=$(yarilo_procs)
[ -n "$new" ] && [ "$new" != "$mpid" ] && [ "$(running)" = "$((PROCS - 1))" ] && [ "$p" = "$PROCS" ] &&
  verdict "yarilo-master killed comes back" ok "new supervisor $new, $p yarilo processes, none doubled" ||
  verdict "yarilo-master killed comes back" fail "supervisor '${new:-none}' (was $mpid), running $(running), processes $p of $PROCS"
service yarilo status > "$OUT/logs/master-status.end" 2>&1 || true
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
cm=$(master_pid); cl=$(log_pid); m0=$(cpu_s "$cm"); l0=$(cpu_s "$cl"); t0=$(date +%s)
for spec in d00001.test:3-20 d00002.test:53-70 d00003.test:103-120; do
  dom=${spec%%:*}
  as_client "$IMAPTEST host=127.0.0.1 port=143 user=u%d@$dom users=${spec##*:} pass='$PASS' clients=50 msgs=500 secs=90 mbox=$MBOX" > "$OUT/logs/imaptest-$dom.log" 2>&1 || true
  errors=$(grep -ac '^Error' "$OUT/logs/imaptest-$dom.log" || true)
  stalls=$(grep -ac 'stalled' "$OUT/logs/imaptest-$dom.log" || true)
  logins=$(tail -1 "$OUT/logs/imaptest-$dom.log" | awk '{print $1}')
  [ "$errors" = 0 ] && [ "$stalls" = 0 ] && [ -n "$logins" ] && verdict "imaptest $dom" ok "logins=$logins" ||
    verdict "imaptest $dom" fail "errors=$errors stalled=$stalls logins=${logins:-?}"
done
t1=$(date +%s)
mc=$(awk -v a="$m0" -v b="$(cpu_s "$cm")" -v t=$((t1 - t0)) 'BEGIN { printf "%.1f", (t > 0 ? (b - a) / t * 100 : 0) }')
lc=$(awk -v a="$l0" -v b="$(cpu_s "$cl")" -v t=$((t1 - t0)) 'BEGIN { printf "%.1f", (t > 0 ? (b - a) / t * 100 : 0) }')
# A process that went away during the part reads as no CPU at all: a restart
# under load is what the run is there to catch, so a changed pid fails.
if [ -z "$cm" ] || [ "$(master_pid)" != "$cm" ]; then
  verdict "supervisor CPU during imaptest" fail "the supervisor changed or went away during imaptest: $cm -> $(master_pid)"
elif awk -v c="$mc" -v m="$MASTER_CPU_MAX" 'BEGIN { exit !(c >= 0 && c < m) }'; then
  verdict "supervisor CPU during imaptest" ok "$mc % (< $MASTER_CPU_MAX %)"
else
  verdict "supervisor CPU during imaptest" fail "$mc %, limit $MASTER_CPU_MAX %"
fi
if [ -z "$cl" ] || [ "$(log_pid)" != "$cl" ]; then
  verdict "log process CPU during imaptest" fail "the log process changed or went away during imaptest: $cl -> $(log_pid)"
elif awk -v c="$lc" -v m="$LOG_CPU_MAX" 'BEGIN { exit !(c >= 0 && c < m) }'; then
  verdict "log process CPU during imaptest" ok "$lc % (< $LOG_CPU_MAX %)"
else
  verdict "log process CPU during imaptest" fail "$lc %, limit $LOG_CPU_MAX %"
fi

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
# The component list is master_components now: fts leaves it, as a flow list
# (one line) or a block list (a "- fts" item).
awk '/^[^[:space:]#]/ { in_master = ($0 ~ /^master:/) }
  in_master && /master_components:.*\[/ { gsub(/, *fts *,/, ","); gsub(/\[ *fts *, */, "["); gsub(/, *fts *\]/, "]") }
  in_master && /^[[:space:]]+- *fts[[:space:]]*$/ { next }
  { print }' "$CONF" > "$CONF.nofts" && cat "$CONF.nofts" > "$CONF" && rm -f "$CONF.nofts"
grep -A30 '^master:' "$CONF" | grep -qw fts && verdict "fts out of master_components" fail "fts still listed" ||
  verdict "fts out of master_components" ok "fts removed"
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
cmp -s "$OUT/yarilo.yaml.start" "$CONF" && [ "$n" = "$((PROCS - 1))" ] && verdict "run config restored" ok "$n running" ||
  verdict "run config restored" fail "running=$n config $(cmp -s "$OUT/yarilo.yaml.start" "$CONF" && echo same || echo differs)"
date -u +"end %FT%TZ" >> "$OUT/times"

echo "accept-freebsd: version=$VERSION fails=$FAILS skips=$SKIPS" | tee -a "$OUT/verdict.txt"
if [ "$FAILS" -gt 0 ]; then exit 1; fi
exit 0
