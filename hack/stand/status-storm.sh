#!/usr/bin/env bash
# The imap OOM of #2661 as numbers: one maildir folder of STORM_FILES messages,
# a file put into its cur/ from outside before each round, then STORM_SESSIONS
# sessions asking STATUS at once. Reports, per backend, the peak memory of the
# imap container during the rounds, the whole-folder walks they cost and the
# CPU those took. Runs on the runner, after a deploy, outside a window.
#
# Usage:
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/status-storm.sh <out-dir>
set -euo pipefail
NS="${YARILO_NS:-yarilo-sb}"
OUT="${1:?output directory}"
FILES="${STORM_FILES:-12204}"
SESSIONS="${STORM_SESSIONS:-40}"
ROUNDS="${STORM_ROUNDS:-5}"
# Inside the maildir arm's range, which an arm wipes before it fills: no run
# measures this account but this one.
ACCOUNT="${STORM_ACCOUNT:-u100@d00002.test}"
mkdir -p "$OUT"
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../sandbox/creds.sh"
PASSWORD="$(sandbox_password)"
kube() { kubectl -n "$NS" --request-timeout=60s "$@"; }
HOMEDIR="/var/mail/vhosts/${ACCOUNT#*@}/$ACCOUNT"
PODS=$(kube get pods -l app.kubernetes.io/component=backend -o name | cut -d/ -f2)
first=${PODS%%$'\n'*}

# metric sums one imap_maildir_sync_total result over the pod's imap process.
metric() {
  kube get --raw "/api/v1/namespaces/$NS/pods/$1:8080/proxy/metrics" 2>/dev/null |
    awk -v r="result=\"$2\"" 'index($0, "imap_maildir_sync_total{") == 1 && index($0, r) { s += $NF } END { print s + 0 }'
}
cpu_usec() { kube exec "$1" -c yarilo-imap -- sh -c "sed -n 's/^usage_usec //p' /sys/fs/cgroup/cpu.stat" 2>/dev/null || echo 0; }
mem_now() { kube exec "$1" -c yarilo-imap -- cat /sys/fs/cgroup/memory.current 2>/dev/null || echo 0; }

echo "== folder: $ACCOUNT INBOX, $FILES messages"
kube exec "$first" -c yarilo-imap -- sh -c "
  rm -rf '$HOMEDIR' && mkdir -p '$HOMEDIR/Maildir/cur' '$HOMEDIR/Maildir/new' '$HOMEDIR/Maildir/tmp' &&
  cd '$HOMEDIR/Maildir/cur' && i=0 &&
  while [ \$i -lt $FILES ]; do
    printf 'From: a@b\r\nSubject: storm %d\r\n\r\nbody\r\n' \$i > \"17000\$(printf %05d \$i).M\${i}P1.storm,S=40,W=42:2,S\"
    i=\$((i+1))
  done && ls | wc -l" | sed 's/^/files on disk: /' | tee "$OUT/folder.txt"
# The request lands in the pod once, through stdin: a password inside the
# shell string would break on its first quote.
printf 'a LOGIN %s %s\r\nb STATUS INBOX (MESSAGES UNSEEN)\r\nc LOGOUT\r\n' "$ACCOUNT" "$PASSWORD" |
  kube exec -i "$first" -c yarilo-imap -- sh -c 'cat > /tmp/storm.req'
storm() {
  kube exec "$first" -c yarilo-imap -- sh -c "
    i=0; while [ \$i -lt $SESSIONS ]; do
      nc -w 30 yarilo-imap-login 143 < /tmp/storm.req > /tmp/storm.\$i 2>&1 &
      i=\$((i+1))
    done; wait; grep -h 'STATUS INBOX' /tmp/storm.[0-9]* | head -1; rm -f /tmp/storm.[0-9]*" | tr -d '\r'
}
echo "-- adoption: $(storm | head -1)"

for p in $PODS; do echo "$(metric "$p" scanned) $(cpu_usec "$p")" > "$OUT/before.$p"; done
( while :; do for p in $PODS; do echo "$p $(mem_now "$p")"; done; sleep 1; done ) > "$OUT/memory.txt" &
sampler=$!
trap 'kill $sampler 2>/dev/null || true' EXIT
for r in $(seq 1 "$ROUNDS"); do
  kube exec "$first" -c yarilo-imap -- sh -c "printf 'x\r\n' > '$HOMEDIR/Maildir/cur/1800000$r.M${r}P1.outside:2,'"
  echo "-- round $r: $(storm | head -1)"
done
kill $sampler 2>/dev/null || true
kube exec "$first" -c yarilo-imap -- rm -f /tmp/storm.req
for p in $PODS; do
  max=$(awk -v p="$p" '$1 == p && $2 > m { m = $2 } END { print m + 0 }' "$OUT/memory.txt")
  read -r w0 c0 < "$OUT/before.$p"
  echo "storm: pod=$p sessions=$SESSIONS rounds=$ROUNDS files=$FILES peak_imap_mib=$((max / 1048576)) walks=$(( $(metric "$p" scanned) - w0 )) cpu_ms=$(( ($(cpu_usec "$p") - c0) / 1000 ))"
done | tee "$OUT/result.txt"
