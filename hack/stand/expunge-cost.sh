#!/usr/bin/env bash
# What one EXPUNGE of N messages costs in map-lock acquisitions and in wall
# time, measured on a live mdbox account (#1884).
#
# Usage:
#   KUBECONFIG=~/.kube/ihorru-sbox-nc.yaml \
#     bash hack/stand/expunge-cost.sh <arm-label> <N> <out-dir>

set -euo pipefail

ARM="${1:?arm label}"
N="${2:?message count}"
OUT="${3:?output directory}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
USER_NAME="${YARILO_USER:-u1@d00001.test}"
PASSWORD="${YARILO_PASSWORD:-Yarilo!test1}"
PORT="${YARILO_LOCAL_PORT:-11143}"

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" "$@"; }
mkdir -p "$OUT"
LOG="$OUT/expunge-$ARM-n$N.log"
: > "$LOG"

# Summed over the backends because the account is pinned to one of them and
# which one is not this measurement's business.
map_acquisitions() {
  local total=0 pod v
  for pod in $(kube get pods -l app.kubernetes.io/component=backend -o name | cut -d/ -f2); do
    v=$(kube exec "$pod" -c yarilo-imap -- sh -c \
      'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null | grep "^yarilo_locks_acquire_wait_seconds_count{resource=\"mdboxmap\"}"' 2>/dev/null \
      | awk '{print $2}')
    case "$v" in ''|*[!0-9.]*) v=0;; esac
    total=$(awk -v a="$total" -v b="$v" 'BEGIN{printf "%d", a+b}')
  done
  [ "$total" -gt 0 ] || { echo "expunge-cost: the mdboxmap counter reads zero or is missing" >&2; return 1; }
  echo "$total"
}

kube port-forward svc/yarilo-imap-login "$PORT:143" >/dev/null 2>&1 &
PF=$!
trap 'kill "$PF" 2>/dev/null || true' EXIT
for _ in 1 2 3 4 5 6 7 8 9 10; do
  (exec 9<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null && break
  sleep 1
done

exec 3<>"/dev/tcp/127.0.0.1/$PORT" || { echo "expunge-cost: no IMAP port at 127.0.0.1:$PORT" >&2; exit 1; }

# Waits for one tagged answer and fails on NO/BAD rather than reading on into
# the next command's output.
await() {
  local tag="$1" line
  while IFS= read -r -t 120 line <&3; do
    printf '%s\n' "$line" >> "$LOG"
    case "$line" in
      "$tag OK"*) return 0;;
      "$tag NO"*|"$tag BAD"*) echo "expunge-cost: $tag refused: $line" >&2; return 1;;
    esac
  done
  echo "expunge-cost: no answer to $tag" >&2
  return 1
}

send() { printf '%s\r\n' "$1" >&3; }

IFS= read -r -t 30 greeting <&3 || { echo "expunge-cost: no greeting" >&2; exit 1; }
printf '%s\n' "$greeting" >> "$LOG"

send "a1 LOGIN \"$USER_NAME\" \"$PASSWORD\""
await a1
send "a2 SELECT INBOX"
await a2

body=$'From: sender@example.com\r\nTo: '"$USER_NAME"$'\r\nSubject: expunge cost\r\n\r\nbody\r\n'
len=${#body}
for i in $(seq 1 "$N"); do
  send "b$i APPEND INBOX {$len}"
  IFS= read -r -t 120 cont <&3 || { echo "expunge-cost: no continuation for APPEND $i" >&2; exit 1; }
  printf '%s\n' "$cont" >> "$LOG"
  case "$cont" in "+"*) ;; *) echo "expunge-cost: APPEND $i refused: $cont" >&2; exit 1;; esac
  printf '%s\r\n' "$body" >&3
  await "b$i"
done

# Reopened so the appended messages are in this session's view, and counted
# from here: the appends take the map lock too, and they are not the subject.
send "c1 CLOSE"
await c1
send "c2 SELECT INBOX"
await c2
exists=$(grep -c '^\* [0-9]* EXISTS' "$LOG" || true)
[ "$exists" -gt 0 ] || { echo "expunge-cost: the mailbox never reported EXISTS" >&2; exit 1; }

send "d1 STORE 1:$N +FLAGS (\\Deleted)"
await d1

before=$(map_acquisitions)
TIMEFORMAT='%3R'
send "e1 EXPUNGE"
elapsed=$( { time await e1 >/dev/null; } 2>&1 )
after=$(map_acquisitions)

send "z1 LOGOUT"
await z1 || true
exec 3<&-

echo "$ARM n=$N map_acquisitions=$((after - before)) expunge_seconds=$elapsed"
