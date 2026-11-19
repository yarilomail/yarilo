#!/usr/bin/env bash
# The six re-encryption scenarios of MAILCRYPT §13.12 on one image, each a
# PASS or FAIL line in <out-dir>/crypt-scenarios.txt with the image and the
# status counts before and after.
#
# Usage:
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/crypt-scenarios.sh <image-tag> <out-dir>
#
#   mixed     a folder of each type holding plaintext and age mail is rewritten
#             to current, nothing failed
#   load      a user imaptest is working on is re-encrypted; a rerun after the
#             load leaves nothing to do and imaptest saw no error
#   kill      the pod running a job is deleted mid-run; the rerun finishes it
#   rotate    a new key beside the old, re-encrypt --all, 0 other_key and 0
#             reference, the old key removed, 0 no_key (§13.10)
#   plain     --all --to plain leaves no encrypted message on the stand
#   speed     messages a second per type, and the time of status on a user
#             holding CRYPT_BIG messages
#
# The keys are mounted for the run only. They are removed only when status
# --all shows no encrypted message; otherwise the stand stays on its keys with
# write off, the Secret is kept, and the users holding encrypted mail are named.
# That check runs however the script ends.

set -uo pipefail

. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../sandbox/creds.sh"
TAG="${1:?image tag}"
OUT="${2:?output directory}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
N="${CRYPT_N:-200}"
KILL_N="${CRYPT_KILL_N:-3000}"
BIG="${CRYPT_BIG:-100000}"
ROWS="$OUT/crypt-scenarios.txt"
SECRET=yarilo-mail-crypt-window
VALUES="$REPO/helm_values"
mkdir -p "$OUT"
: > "$ROWS"

declare -A USERS=([mdbox]=u45@d00001.test [maildir]=u95@d00002.test [sdbox]=u145@d00003.test)
KILL_USER=u96@d00002.test
BIG_USER=u46@d00001.test
LOAD_DOMAIN=d00002.test
LOAD_USERS=51-70
LOAD_USER=u51@d00002.test

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" --request-timeout=60s "$@"; }
. "$HERE/deploy.sh"

crypt() { kube exec yarilo-backend-0 -c yarilo-backend-api -- yarctl backend crypt "$@"; }

failed=0
row() {
  echo "$1 $2 image=$TAG $3" | tee -a "$ROWS"
  [ "$1" = PASS ] || failed=1
}

# counts reads a status table's TOTAL row, or for --all its ALL USERS line, as
# "plain=. current=. other_key=. reference=. no_key=. failed=.".
counts() {
  awk '
    /^ALL USERS/ { all = 1; next }
    all && $2 == "users" { p = $3; c = $4; o = $5; r = $6; n = $7; f = $8; done = 1; exit }
    !all && $1 == "TOTAL" && NF == 7 { p = $2; c = $3; o = $4; r = $5; n = $6; f = $7; done = 1 }
    END { if (done) printf "plain=%d current=%d other_key=%d reference=%d no_key=%d failed=%d", p, c, o, r, n, f; else print "unread" }'
}
status_of() { crypt status "$@" 2>&1 | tee -a "$OUT/status.log" | counts; }
field() { echo "$1" | tr ' ' '\n' | sed -n "s/^$2=//p"; }
encrypted() { echo $(( $(field "$1" current) + $(field "$1" other_key) + $(field "$1" reference) + $(field "$1" no_key) )); }

fill_to() {
  local user="$1" want="$2" n="${1%%@*}"
  YARILO_FILL_DOMAIN="${user#*@}" bash "$HERE/fill-mailboxes.sh" "${n#u}" "${n#u}" "$want" >> "$OUT/fill.log" 2>&1
}
messages() { kube exec yarilo-backend-0 -c yarilo-backend-api -- yarctl -O json backend quota show "$1" 2>/dev/null |
  awk -F'[:,]' '/"message_value"/ { gsub(/[^0-9-]/, "", $2); print $2 + 0; exit }'; }

deploy() {
  local name="$1"
  shift
  mkdir -p "$OUT/deploy-$name"
  stand_deploy "$OUT/deploy-$name" "$TAG" "$REPO/helm" -f "$VALUES/values-sandbox.yaml" "$@" ||
    { echo "crypt-scenarios: the $name deploy did not settle" >&2; return 1; }
}

restart_backend() {
  mkdir -p "$OUT/restart-$1"
  stand_watch_backend "$OUT/restart-$1/backend-downtime.txt" &
  local w=$!
  kube rollout restart sts/yarilo-backend > /dev/null && kube rollout status sts/yarilo-backend --timeout=600s > /dev/null
  local rc=$?
  kill "$w" 2>/dev/null
  wait "$w" 2>/dev/null
  return "$rc"
}

keys_mounted=0
plain_done=0
finish() {
  local rc=$? left where
  trap - EXIT
  if [ "$keys_mounted" = 1 ]; then
    if [ "$plain_done" = 0 ]; then
      echo "== cleanup: everything back to plaintext"
      crypt reencrypt --all --to plain --rate 0 >> "$OUT/cleanup.log" 2>&1
    fi
    left=$(status_of --all)
    if [ "$(encrypted "$left")" = 0 ]; then
      deploy restore && bash "$HERE/mailcrypt-keys.sh" delete
    else
      where=$(crypt status --all 2>&1 | awk -F': ' '/^[^ ]+: / && / (current|other_key|reference|no_key) [1-9]/ { print $1 }' | tr '\n' ' ')
      row FAIL cleanup "encrypted mail is left: $left; users: ${where:-see $OUT/status.log}; the stand stays on its keys with write off, Secret $SECRET kept"
      deploy keys-kept -f "$VALUES/values-sandbox-mailcrypt-keys.yaml"
    fi
  fi
  echo "crypt-scenarios: $(grep -c '^PASS' "$ROWS") passed, $(grep -c '^FAIL' "$ROWS") failed"
  [ "$failed" = 0 ] && [ "$rc" = 0 ] && exit 0
  exit 1
}

echo "-- tooling: $(git -C "$REPO" log --oneline -1) tag=$TAG n=$N kill_n=$KILL_N big=$BIG"

echo "== keys and plaintext mail"
bash "$HERE/mailcrypt-keys.sh" create || exit 1
keys_mounted=1
trap finish EXIT
deploy keys -f "$VALUES/values-sandbox-mailcrypt-keys.yaml" || exit 1
for t in "${!USERS[@]}"; do fill_to "${USERS[$t]}" $(( $(messages "${USERS[$t]}") + N )); done

echo "== write: age, checked live"
deploy write -f "$VALUES/values-sandbox-mailcrypt-write.yaml" || exit 1
crypt keys > "$OUT/keys.txt" 2>&1 || { row FAIL live "backend-api does not answer crypt keys"; exit 1; }
awk '$1 ~ /^age1pq1/ && $3 == "yes" { ok = 1 } END { exit !ok }' "$OUT/keys.txt" || { row FAIL live "no key new mail is sealed to: $(tr '\n' ' ' < "$OUT/keys.txt" | cut -c1-200)"; exit 1; }
probe="${USERS[maildir]}"
before=$(status_of "$probe")
fill_to "$probe" $(( $(messages "$probe") + 1 ))
after=$(status_of "$probe")
[ "$(field "$after" current)" -gt "$(field "$before" current)" ] ||
  { row FAIL live "a delivery under write: age was not sealed: before $before after $after"; exit 1; }
for t in "${!USERS[@]}"; do fill_to "${USERS[$t]}" $(( $(messages "${USERS[$t]}") + N )); done

echo "== mixed, and speed per type"
for t in mdbox maildir sdbox; do
  u="${USERS[$t]}"
  before=$(status_of "$u")
  start=$(date +%s)
  crypt reencrypt "$u" --rate 0 >> "$OUT/reencrypt-$t.log" 2>&1
  code=$?
  secs=$(( $(date +%s) - start ))
  after=$(status_of "$u")
  moved=$(field "$before" plain)
  if [ "$code" = 0 ] && [ "$(field "$before" plain)" -gt 0 ] && [ "$(field "$before" current)" -gt 0 ] &&
    [ "$(field "$after" plain)" = 0 ] && [ "$(field "$after" failed)" = 0 ] && [ "$(field "$after" other_key)" = 0 ]; then
    row PASS "mixed-$t" "$u before[$before] after[$after]"
  else
    row FAIL "mixed-$t" "$u exit $code before[$before] after[$after]"
  fi
  echo "speed-$t $moved messages in ${secs}s" >> "$OUT/speed.txt"
done

echo "== speed: status on $BIG messages"
fill_to "$BIG_USER" "$BIG"
start=$(date +%s)
big=$(status_of "$BIG_USER")
secs=$(( $(date +%s) - start ))
total=$(( $(field "$big" plain) + $(encrypted "$big") ))
if [ "$total" -ge "$BIG" ]; then
  row PASS speed "$(tr '\n' ';' < "$OUT/speed.txt") status of $BIG_USER ($total messages) ${secs}s"
else
  row FAIL speed "$BIG_USER holds $total of $BIG: $big"
fi

echo "== under imaptest"
crypt reencrypt "$LOAD_USER" --to plain --rate 0 >> "$OUT/load.log" 2>&1
before=$(status_of "$LOAD_USER")
kube delete job imaptest --ignore-not-found > /dev/null
sed -e "s/u%d@d00001\.test/u%d@$LOAD_DOMAIN/" -e "s/users=1-20/users=$LOAD_USERS/" "$REPO/hack/imaptest/job.yaml" | kube apply -f - > /dev/null
sleep 20
crypt reencrypt "$LOAD_USER" --rate 0 >> "$OUT/load.log" 2>&1
during=$?
kube wait --for=condition=complete --timeout=300s job/imaptest > /dev/null 2>&1
kube logs job/imaptest -c imaptest > "$OUT/imaptest-load.log" 2>&1
errors=$(grep -ac '^Error' "$OUT/imaptest-load.log" || true)
crypt reencrypt "$LOAD_USER" --rate 0 >> "$OUT/load.log" 2>&1
rerun=$?
after=$(status_of "$LOAD_USER")
if [ "$rerun" = 0 ] && [ "${errors:-1}" = 0 ] && [ "$(field "$after" plain)" = 0 ] && [ "$(field "$after" failed)" = 0 ]; then
  row PASS load "$LOAD_USER during-run exit $during, imaptest errors 0, before[$before] after[$after]"
else
  row FAIL load "$LOAD_USER during-run exit $during, rerun exit $rerun, imaptest errors ${errors:-unread}, before[$before] after[$after]"
fi

echo "== pod killed mid-run"
fill_to "$KILL_USER" "$KILL_N"
crypt reencrypt "$KILL_USER" --to plain --rate 0 >> "$OUT/kill.log" 2>&1
before=$(status_of "$KILL_USER")
crypt reencrypt "$KILL_USER" --rate 50 --detach >> "$OUT/kill.log" 2>&1
for _ in $(seq 1 60); do
  crypt job "$KILL_USER" 2>&1 | grep -q 'running, [1-9]' && break
  sleep 2
done
ip=$(kube exec "$(kube get pods -l app.kubernetes.io/component=director -o name | head -1 | cut -d/ -f2)" -- \
  yarctl -O json director map --user "$KILL_USER" 2>/dev/null | sed -n 's/.*"backend": *"\([^":]*\).*/\1/p')
victim=$(kube get pods -l app.kubernetes.io/component=backend -o jsonpath='{range .items[*]}{.metadata.name} {.status.podIP}{"\n"}{end}' | awk -v ip="$ip" '$2 == ip { print $1 }')
mid=$(status_of "$KILL_USER")
if [ -z "$victim" ]; then
  row FAIL kill "no backend pod holds $KILL_USER (director answered '${ip:-nothing}')"
else
  mkdir -p "$OUT/kill"
  stand_watch_backend "$OUT/kill/backend-downtime.txt" &
  w=$!
  kube delete pod "$victim" --wait=true > /dev/null
  kube rollout status sts/yarilo-backend --timeout=600s > /dev/null
  kill "$w" 2>/dev/null
  wait "$w" 2>/dev/null
  crypt reencrypt "$KILL_USER" --rate 0 >> "$OUT/kill.log" 2>&1
  rerun=$?
  after=$(status_of "$KILL_USER")
  if [ "$rerun" = 0 ] && [ "$(field "$mid" plain)" -gt 0 ] && [ "$(field "$mid" current)" -gt 0 ] &&
    [ "$(field "$after" plain)" = 0 ] && [ "$(field "$after" failed)" = 0 ]; then
    row PASS kill "$KILL_USER $victim deleted mid-run; before[$before] mid[$mid] after[$after]; down $(tr '\n' ' ' < "$OUT/kill/backend-downtime.txt")"
  else
    row FAIL kill "$KILL_USER rerun exit $rerun; before[$before] mid[$mid] after[$after]"
  fi
fi

echo "== rotation (§13.10)"
bash "$HERE/mailcrypt-keys.sh" rotate-add && restart_backend rotate-add || { row FAIL rotate "the new key did not reach the pods"; exit 1; }
before=$(status_of --all)
crypt reencrypt --all --jobs 2 --rate 0 >> "$OUT/rotate.log" 2>&1
code=$?
mid=$(status_of --all)
bash "$HERE/mailcrypt-keys.sh" rotate-drop && restart_backend rotate-drop || { row FAIL rotate "the old key was not removed from the pods"; exit 1; }
after=$(status_of --all)
if [ "$code" = 0 ] && [ "$(field "$before" other_key)" -gt 0 ] && [ "$(field "$mid" other_key)" = 0 ] &&
  [ "$(field "$mid" reference)" = 0 ] && [ "$(field "$after" no_key)" = 0 ] && [ "$(field "$after" failed)" = 0 ]; then
  row PASS rotate "before[$before] after-reencrypt[$mid] old-key-removed[$after]"
else
  row FAIL rotate "reencrypt exit $code; before[$before] after-reencrypt[$mid] old-key-removed[$after]"
fi

echo "== back to plaintext"
before=$(status_of --all)
crypt reencrypt --all --to plain --jobs 2 --rate 0 >> "$OUT/plain.log" 2>&1
code=$?
after=$(status_of --all)
if [ "$code" = 0 ] && [ "$(encrypted "$after")" = 0 ] && [ "$(field "$after" failed)" = 0 ]; then
  plain_done=1
  row PASS plain "before[$before] after[$after]"
else
  row FAIL plain "exit $code; before[$before] after[$after]"
fi
