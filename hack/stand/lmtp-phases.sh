#!/usr/bin/env bash
# Times one LMTP session command by command, so a slow fill can be placed: the
# RCPT answer (lookup, quota, concurrency) against the end-of-DATA answer (the
# delivery itself). lmtp_delivery_seconds covers only the second.
#
# Usage:
#   KUBECONFIG=... bash hack/stand/lmtp-phases.sh <user> [messages]

set -uo pipefail

USER_ADDR="${1:?recipient}"
COUNT="${2:-20}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
LMTP_HOST="${LMTP_HOST:-yarilo-lmtp-login}"
LMTP_PORT="${LMTP_PORT:-24}"
NOW=$(command -v gdate || command -v date)

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" --request-timeout=60s "$@"; }
ms() { "$NOW" +%s%3N; }

pod=$(kube get pods -l app.kubernetes.io/component=backend -o name | head -1 | cut -d/ -f2)
[ -n "$pod" ] || { echo "phases: no backend pod in $NS" >&2; exit 1; }

# Named pipes rather than coproc: the bash a Mac ships is 3.2.
dir=$(mktemp -d); trap 'rm -rf "$dir"' EXIT
mkfifo "$dir/in" "$dir/out"
kube exec -i "$pod" -c yarilo-imap -- nc -w 60 "$LMTP_HOST" "$LMTP_PORT" <"$dir/in" >"$dir/out" 2>&1 &
exec 3>"$dir/in" 4<"$dir/out"

# answer reads one reply, multi-line included, and prints its last line.
answer() {
  local line
  while IFS= read -r -t 60 line <&4; do
    line="${line%$'\r'}"
    case "$line" in
      [0-9][0-9][0-9]-*) continue ;;
      *) printf '%s\n' "$line"; return 0 ;;
    esac
  done
  echo "timeout"
  return 1
}
say() { printf '%s\r\n' "$1" >&3; }

answer >/dev/null # greeting
say "LHLO phases.invalid"; answer >/dev/null

for i in $(seq 1 "$COUNT"); do
  # NOOP is the round trip alone (laptop, API server, pod), to subtract.
  n0=$(ms); say "NOOP"; answer >/dev/null; n1=$(ms)
  say "MAIL FROM:<phases@test.invalid>"; answer >/dev/null
  t0=$(ms); say "RCPT TO:<$USER_ADDR>"; r=$(answer); t1=$(ms)
  say "DATA"; answer >/dev/null
  {
    printf 'Subject: phases %s\r\nFrom: <phases@test.invalid>\r\nTo: <%s>\r\n' "$i" "$USER_ADDR"
    printf 'Message-ID: <phases-%s-%s-%s@test.invalid>\r\n\r\n' "$t0" "$i" "$USER_ADDR"
    for _ in $(seq 1 24); do printf 'filler text for a mailbox that is not empty\r\n'; done
  } >&3
  t2=$(ms); say "."; d=$(answer); t3=$(ms)
  echo "phase: user=$USER_ADDR n=$i noop_ms=$((n1 - n0)) rcpt_ms=$((t1 - t0)) data_ms=$((t3 - t2)) rcpt=${r%% *} data=${d%% *}"
done
say "QUIT"; answer >/dev/null
