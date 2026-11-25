#!/usr/bin/env bash
# lmtp-rate.sh <ns> <pod> <container|-> <host> <port> <rcpt> <n>
# The client of docs-internal#318, kept here so a run is repeatable.
# Pipelines n of the fill's message over one LMTP connection from a pod and
# reads every reply; prints acked messages and their rate between the first
# and the last final 250.
set -u
ns=$1 pod=$2 ctr=$3 host=$4 port=$5 rcpt=$6 n=$7
now() { date +%s.%N; }
c=(); [ "$ctr" != - ] && c=(-c "$ctr")
dir=$(mktemp -d); mkfifo "$dir/to" "$dir/from"
kubectl -n "$ns" exec -i "$pod" ${c[@]+"${c[@]}"} -- nc "$host" "$port" < "$dir/to" > "$dir/from" 2>&1 &
conn=$!
exec 3> "$dir/to" 4< "$dir/from"
{
  printf 'LHLO rate.invalid\r\n'
  for i in $(seq 1 "$n"); do
    printf 'MAIL FROM:<fill@test.invalid>\r\nRCPT TO:<%s>\r\nDATA\r\n' "$rcpt"
    printf 'Subject: fill %s\r\nFrom: <fill@test.invalid>\r\nTo: <%s>\r\nDate: Thu, 18 Sep 2026 12:00:00 +0000\r\nMessage-ID: <rate-%s-%s@test.invalid>\r\n\r\n' "$i" "$rcpt" "$i$RANDOM" "$rcpt"
    awk 'BEGIN { for (i = 0; i < 24; i++) printf "%s\r\n", "filler text for a mailbox that is not empty" }'
    printf '.\r\n'
  done
  printf 'QUIT\r\n'
} >&3 &
writer=$!
want() { local l; while IFS= read -r -t 120 l <&4; do l=${l%$'\r'}; case "$l" in "$1-"*) continue ;; "$1 "*) return 0 ;; *) echo "refused: $l (waiting for $1)" >&2; return 1 ;; esac; done; echo "no reply in 120 s (waiting for $1)" >&2; return 1; }
acked=0 first="" last=""
if want 220 && want 250; then
  for i in $(seq 1 "$n"); do
    want 250 && want 250 && want 354 && want 250 || break
    acked=$((acked + 1)); [ -z "$first" ] && first=$(now)
  done
  last=$(now)
fi
kill $writer 2>/dev/null; exec 3>&- 4<&-; wait $conn 2>/dev/null; rm -rf "$dir"
awk -v a="$acked" -v f="${first:-0}" -v l="${last:-0}" -v r="$rcpt" 'BEGIN {
  span = l - f
  printf "rcpt=%s acked=%d span_s=%.1f per_s=%.1f ms_per=%.0f\n", r, a, span,
    (span > 0 ? (a - 1) / span : 0), (a > 1 ? span * 1000 / (a - 1) : 0)
}'
