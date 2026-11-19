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
#   load-<type>  on each type, a user imaptest is working on is re-encrypted
#             while imaptest runs (checked, not assumed); a rerun after the load
#             leaves nothing to do and imaptest saw no error
#   kill      the pod running a job is deleted mid-run; the rerun finishes it
#   rotate    a new key beside the old, re-encrypt --all, 0 other_key and 0
#             reference, the old key removed, 0 no_key (§13.10)
#   plain     --all --to plain leaves no encrypted message on the stand
#   speed     messages a second per type and source state (plain, the old key,
#             age back to plain), and the time of status on a user holding
#             CRYPT_BIG messages
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

TYPES="mdbox maildir sdbox"
user_of() { case "$1" in mdbox) echo u45@d00001.test ;; maildir) echo u95@d00002.test ;; sdbox) echo u145@d00003.test ;; esac; }
# The load users are the first of each imaptest range accept.sh runs.
load_of() { case "$1" in mdbox) echo "u3@d00001.test d00001.test 3-20" ;; maildir) echo "u53@d00002.test d00002.test 53-70" ;; sdbox) echo "u103@d00003.test d00003.test 103-120" ;; esac; }
LOAD_N="${CRYPT_LOAD_N:-1000}"
KILL_USER=u96@d00002.test
BIG_USER=u46@d00001.test

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
    if [ "$left" = unread ]; then
      row FAIL cleanup "status --all did not answer; the stand is left as it is, Secret $SECRET kept"
    elif [ "$(encrypted "$left")" = 0 ]; then
      if ! deploy restore; then
        row FAIL cleanup "no mail is encrypted, but the restore to the stand's values did not settle; Secret $SECRET kept"
      elif ! bash "$HERE/mailcrypt-keys.sh" delete; then
        row FAIL cleanup "the stand is back on its values, but Secret $SECRET was not deleted"
      else
        echo "PASS cleanup image=$TAG no encrypted message; the stand is on its values, the keys are gone" | tee -a "$ROWS"
      fi
    else
      where=$(crypt status --all 2>&1 | awk -F': ' '/^[^ ]+: / && / (current|other_key|reference|no_key) [1-9]/ { print $1 }' | tr '\n' ' ')
      row FAIL cleanup "encrypted mail is left: $left; users: ${where:-see $OUT/status.log}; the stand stays on its keys with write off, Secret $SECRET kept"
      deploy keys-kept -f "$VALUES/values-sandbox-mailcrypt-keys.yaml" ||
        row FAIL cleanup "the stand did not settle on its keys with write off"
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
for t in $TYPES; do u=$(user_of "$t"); fill_to "$u" $(( $(messages "$u") + N )); done

echo "== write: age, checked live"
deploy write -f "$VALUES/values-sandbox-mailcrypt-write.yaml" || exit 1
crypt keys > "$OUT/keys.txt" 2>&1 || { row FAIL live "backend-api does not answer crypt keys"; exit 1; }
awk '$1 ~ /^age1pq1/ && $3 == "yes" { ok = 1 } END { exit !ok }' "$OUT/keys.txt" || { row FAIL live "no key new mail is sealed to: $(tr '\n' ' ' < "$OUT/keys.txt" | cut -c1-200)"; exit 1; }
probe=$(user_of maildir)
before=$(status_of "$probe")
fill_to "$probe" $(( $(messages "$probe") + 1 ))
after=$(status_of "$probe")
[ "$(field "$after" current)" -gt "$(field "$before" current)" ] ||
  { row FAIL live "a delivery under write: age was not sealed: before $before after $after"; exit 1; }
for t in $TYPES; do u=$(user_of "$t"); fill_to "$u" $(( $(messages "$u") + N )); done

echo "== mixed, and speed per type"
# timed runs crypt reencrypt and adds "<label> <messages> <seconds>" to speed.txt.
timed() {
  local label="$1" moved="$2" start code
  shift 2
  start=$(date +%s)
  crypt reencrypt "$@" >> "$OUT/reencrypt.log" 2>&1
  code=$?
  echo "$label $moved $(( $(date +%s) - start ))" >> "$OUT/speed.txt"
  return "$code"
}

for t in $TYPES; do
  u=$(user_of "$t")
  before=$(status_of "$u")
  timed "$t:plain" "$(field "$before" plain)" "$u" --rate 0
  code=$?
  after=$(status_of "$u")
  if [ "$code" = 0 ] && [ "$(field "$before" plain)" -gt 0 ] && [ "$(field "$before" current)" -gt 0 ] &&
    [ "$(field "$after" plain)" = 0 ] && [ "$(field "$after" failed)" = 0 ] && [ "$(field "$after" other_key)" = 0 ]; then
    row PASS "mixed-$t" "$u before[$before] after[$after]"
  else
    row FAIL "mixed-$t" "$u exit $code before[$before] after[$after]"
  fi
done

echo "== status on $BIG messages"
fill_to "$BIG_USER" "$BIG"
start=$(date +%s)
big=$(status_of "$BIG_USER")
big_secs=$(( $(date +%s) - start ))

echo "== under imaptest, each type"
job_done() { [ "$(kube get job imaptest -o jsonpath='{.status.succeeded}' 2>/dev/null)" = 1 ]; }
load_running() { kube get pods -l job-name=imaptest -o jsonpath='{.items[0].status.phase}' 2>/dev/null | grep -q Running; }
for t in $TYPES; do
  set -- $(load_of "$t")
  lu="$1" dom="$2" range="$3"
  fill_to "$lu" $(( $(messages "$lu") + LOAD_N ))
  crypt reencrypt "$lu" --to plain --rate 0 >> "$OUT/load.log" 2>&1
  before=$(status_of "$lu")
  kube delete job imaptest --ignore-not-found --wait=true > /dev/null
  sed -e "s/u%d@d00001\.test/u%d@$dom/" -e "s/users=1-20/users=$range/" "$REPO/hack/imaptest/job.yaml" | kube apply -f - > /dev/null
  for _ in $(seq 1 60); do load_running && break; sleep 2; done
  sleep 10
  # The overlap is the row: imaptest running when the job starts and still
  # running when it ends.
  load_running && started_under=1 || started_under=0
  # About 40 s of work, inside imaptest's 90: a fixed rate overran it on a
  # mailbox earlier runs had grown.
  rate=$(( $(field "$before" plain) / 40 + 1 ))
  crypt reencrypt "$lu" --rate "$rate" >> "$OUT/load.log" 2>&1
  during=$?
  job_done && ended_under=0 || ended_under=1
  kube wait --for=condition=complete --timeout=300s job/imaptest > /dev/null 2>&1
  kube logs job/imaptest -c imaptest > "$OUT/imaptest-load-$t.log" 2>&1
  errors=$(grep -ac '^Error' "$OUT/imaptest-load-$t.log" || true)
  crypt reencrypt "$lu" --rate 0 >> "$OUT/load.log" 2>&1
  rerun=$?
  after=$(status_of "$lu")
  if [ "$started_under$ended_under" != 11 ]; then
    row FAIL "load-$t" "$lu: the job did not run inside imaptest (running at start $started_under, at end $ended_under); before[$before] after[$after]"
  elif [ "$rerun" = 0 ] && [ "${errors:-1}" = 0 ] && [ "$(field "$after" plain)" = 0 ] && [ "$(field "$after" failed)" = 0 ]; then
    row PASS "load-$t" "$lu under imaptest, run exit $during, imaptest errors 0, before[$before] after[$after]"
  else
    row FAIL "load-$t" "$lu run exit $during, rerun exit $rerun, imaptest errors ${errors:-unread}, before[$before] after[$after]"
  fi
done

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
for t in $TYPES; do
  u=$(user_of "$t")
  timed "$t:old-key" "$(field "$(status_of "$u")" other_key)" "$u" --rate 0
done
crypt reencrypt --all --jobs 2 --rate 0 >> "$OUT/rotate.log" 2>&1
code=$?
mid=$(status_of --all)
# The old key goes only once nothing needs it: dropped after a run that did
# not finish, it takes with it every message still sealed to it.
if [ "$code" != 0 ] || [ "$(field "$mid" other_key)" != 0 ] || [ "$(field "$mid" reference)" != 0 ]; then
  row FAIL rotate "reencrypt exit $code; before[$before] after-reencrypt[$mid]; both keys kept"
  exit 1
fi
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
for t in $TYPES; do
  u=$(user_of "$t")
  timed "$t:to-plain" "$(encrypted "$(status_of "$u")")" "$u" --to plain --rate 0
done
crypt reencrypt --all --to plain --jobs 2 --rate 0 >> "$OUT/plain.log" 2>&1
code=$?
after=$(status_of --all)
if [ "$code" = 0 ] && [ "$(encrypted "$after")" = 0 ] && [ "$(field "$after" failed)" = 0 ]; then
  plain_done=1
  row PASS plain "before[$before] after[$after]"
else
  row FAIL plain "exit $code; before[$before] after[$after]"
fi

echo "== speed"
rates=$(awk '{ printf "%s %d msgs %ds (%s/s); ", $1, $2, $3, ($3 > 0 ? int($2 / $3) : "inf") }' "$OUT/speed.txt")
total=$(( $(field "$big" plain) + $(encrypted "$big") ))
if [ "$(wc -l < "$OUT/speed.txt")" -ge 9 ] && [ "$total" -ge "$BIG" ]; then
  row PASS speed "$rates status of $BIG_USER ($total messages) ${big_secs}s"
else
  row FAIL speed "$rates status of $BIG_USER: $total of $BIG messages"
fi
