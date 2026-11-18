#!/usr/bin/env bash
# flock between two backend pods on the shared mail volume: does the RWX
# mount give shared and exclusive holds across clients, as the maildir crypt
# lock needs (MAILCRYPT section 13.5)? Nothing of yarilo is restarted or
# changed; a scratch directory on the volume is made and removed.
#
# Usage, on the runner, from a clean checkout of merged develop:
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/flock-rwx.sh <out-dir>
#
# Rows, each PASS or FAIL in <out-dir>/flock-rwx.txt:
#   two pods on two nodes, flock present in the image;
#   a shared hold on A admits a shared take on B;
#   a shared hold on A refuses an exclusive take on B;
#   an exclusive hold on A refuses a shared take on B;
#   two shared holds on A, one closed: B's exclusive take is still refused;
#   all released: B's exclusive take goes;
#   the cost of a shared take and release on one pod, mean of 200.

set -euo pipefail

OUT="${1:?output directory}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
ROOT="${MAIL_ROOT:-/var/mail/vhosts}"
DIR="$ROOT/.yarilo-flock-check"
F="$DIR/crypt.lock"
mkdir -p "$OUT"
REPORT="$OUT/flock-rwx.txt"
: >"$REPORT"
FAILED=0

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" --request-timeout=60s "$@"; }
on() { kube exec "$1" -c yarilo-imap -- sh -c "$2"; }
verdict() { # <name> pass|fail <detail>
  local v=PASS
  if [ "$2" != pass ]; then v=FAIL; FAILED=$((FAILED + 1)); fi
  printf '%s %s: %s\n' "$v" "$1" "$3" | tee -a "$REPORT"
}
# hold <pod> <-s|-x> <seconds>: a hold in the background on pod, by a
# process of its own, that outlives the exec.
hold() { on "$1" "nohup flock $2 $F sleep $3 >/dev/null 2>&1 &"; sleep 2; }
# try <pod> <-s|-x>: 0 when the take goes at once, 1 when it is refused.
try() { if on "$1" "flock $2 -n $F true"; then echo 0; else echo 1; fi; }

echo "-- tooling: $(git -C "$REPO" log --oneline -1)" | tee -a "$REPORT"
mapfile -t PODS < <(kube get pods -l app.kubernetes.io/component=backend -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.nodeName}{"\n"}{end}')
if [ "${#PODS[@]}" -lt 2 ]; then
  verdict pods fail "${#PODS[@]} backend pods, two are needed"
  exit 1
fi
A="${PODS[0]%% *}" NA="${PODS[0]##* }"
B="${PODS[1]%% *}" NB="${PODS[1]##* }"
if [ "$NA" = "$NB" ]; then
  verdict pods fail "$A and $B are both on $NA: one client, which says nothing about the mount"
  exit 1
fi
if on "$A" 'command -v flock >/dev/null' && on "$B" 'command -v flock >/dev/null'; then
  verdict pods pass "$A on $NA, $B on $NB, flock present"
else
  verdict pods fail "flock is not in the image"
  exit 1
fi
on "$A" "grep ' $ROOT ' /proc/mounts || grep \"$(dirname "$ROOT")\" /proc/mounts" | tee -a "$REPORT" || true

cleanup() {
  on "$A" "pkill -f 'flock .* $F' 2>/dev/null; rm -rf $DIR" || true
  on "$B" "pkill -f 'flock .* $F' 2>/dev/null" || true
}
trap cleanup EXIT
on "$A" "mkdir -p $DIR && : > $F"

hold "$A" -s 8
[ "$(try "$B" -s)" = 0 ] && verdict shared-shared pass "B took it shared beside A's shared hold" ||
  verdict shared-shared fail "B could not take it shared beside A's shared hold"
[ "$(try "$B" -x)" = 1 ] && verdict shared-excl pass "B's exclusive take was refused beside A's shared hold" ||
  verdict shared-excl fail "B took it exclusive while A held it shared"
sleep 8

hold "$A" -x 8
[ "$(try "$B" -s)" = 1 ] && verdict excl-shared pass "B's shared take was refused beside A's exclusive hold" ||
  verdict excl-shared fail "B took it shared while A held it exclusive"
sleep 8

hold "$A" -s 12
on "$A" "flock -s $F true"
[ "$(try "$B" -x)" = 1 ] && verdict close-keeps pass "a second shared hold on A, closed, left the first in place" ||
  verdict close-keeps fail "closing one of A's holds dropped the other: the mount binds locks to the process"
sleep 12

[ "$(try "$B" -x)" = 0 ] && verdict released pass "B took it exclusive once A let go" ||
  verdict released fail "B still could not take it exclusive with nothing held"

cost=$(on "$A" "s=\$(date +%s%N); i=0; while [ \$i -lt 200 ]; do flock -s $F true; i=\$((i+1)); done; e=\$(date +%s%N); echo \$(( (e - s) / 200000 ))")
verdict cost pass "a shared take and release on $A: ${cost} us mean of 200 (busybox flock, a process per take)"

echo "== $(date '+%Y-%m-%d %H:%M %Z') $FAILED failed" | tee -a "$REPORT"
[ "$FAILED" = 0 ]
