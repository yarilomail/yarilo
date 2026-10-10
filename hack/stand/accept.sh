#!/usr/bin/env bash
# The rollout acceptance run: deploy a tag, smoketest and imaptest on the three
# storage types, and judge every criterion a rollout must meet, PASS or FAIL.
# Every log line a criterion counts is kept verbatim: a count without its line
# explains nothing once the pods are replaced (#2183).
#
# Usage:
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/accept.sh <image-tag> <out-dir>
#
# Runs on the runner, like a window. Exits 1 when any criterion fails.

set -euo pipefail

TAG="${1:?image tag}"
OUT="${2:?output directory}"
NS="${YARILO_NS:-yarilo-sb}"
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PASSWORD='Yarilo!test1'
mkdir -p "$OUT/logs" "$OUT/lines"
: > "$OUT/verdict.txt"
FAILS=0

kube() { kubectl -n "$NS" --request-timeout=60s "$@"; }

# verdict records one criterion: its name, ok or not, and what decided it.
verdict() {
  if [ "$2" = ok ]; then
    echo "PASS $1: $3" | tee -a "$OUT/verdict.txt"
  else
    echo "FAIL $1: $3" | tee -a "$OUT/verdict.txt"
    FAILS=$((FAILS + 1))
  fi
}

# pod_state lists every pod with its uid and each container's restart count: a
# replaced pod shows as a new uid, a restarted container as a higher count.
pod_state() {
  kube get pods -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.uid}{range .status.containerStatuses[*]} {.name}={.restartCount}{end}{"\n"}{end}' | sort
}

# imap_session runs one IMAP conversation from a backend pod against the login
# service and prints the untagged and tagged replies.
imap_session() {
  kube exec yarilo-backend-0 -c yarilo-imap -- sh -c "printf '$1' | nc -w 15 yarilo-imap-login 143" | tr -d '\r'
}

# index_messages is the INBOX record count the admin API reports for a user.
index_messages() {
  kube exec yarilo-backend-0 -c yarilo-backend-api -- yarctl backend folder info "$1" INBOX 2>/dev/null |
    sed -n 's/.*"messages": *\([0-9]*\).*/\1/p'
}

# disk_messages counts a user's INBOX message files: sdbox u.*, maildir cur/ and new/.
disk_messages() {
  local user="$1" type="$2" dom="${1#*@}" home
  home="/var/mail/vhosts/$dom/$user"
  case "$type" in
    sdbox) kube exec yarilo-backend-0 -c yarilo-imap -- sh -c "ls $home/sdbox/mailboxes/INBOX/dbox-Mails 2>/dev/null | grep -c '^u\.' || true" ;;
    maildir) kube exec yarilo-backend-0 -c yarilo-imap -- sh -c "ls $home/Maildir/cur $home/Maildir/new 2>/dev/null | grep -vc -e '^\$' -e ':\$' || true" ;;
  esac
}

# aged_account finds an account of the domain whose INBOX holds at least 20
# message files changed more than a day ago: the class #2172 lost.
aged_account() {
  local dom="$1" sub="$2" pattern="$3"
  kube exec yarilo-backend-0 -c yarilo-imap -- sh -c \
    "cd /var/mail/vhosts/$dom 2>/dev/null && for u in *; do n=\$(find \"\$u/$sub\" -maxdepth 1 -name '$pattern' -cmin +1440 2>/dev/null | wc -l); [ \"\$n\" -ge 20 ] && echo \"\$u \$n\" && break; done" 2>/dev/null || true
}

# uid_pairs prints "user uid" for every UID both vanished and present, for the
# imaptest maildir accounts: a UID the server handed back after expunging it.
uid_pairs() {
  local n user uv out van pres
  for n in $(seq 53 70); do
    user="u$n@d00002.test"
    uv=$(imap_session "a LOGIN $user $PASSWORD\r\nb SELECT INBOX\r\nc LOGOUT\r\n" | sed -n 's/.*UIDVALIDITY \([0-9]*\).*/\1/p' | head -1)
    [ -n "$uv" ] || { echo "$user no-uidvalidity"; continue; }
    out=$(imap_session "a LOGIN $user $PASSWORD\r\nb ENABLE QRESYNC\r\nc SELECT INBOX (QRESYNC ($uv 1))\r\nd UID SEARCH ALL\r\ne LOGOUT\r\n")
    van=$(echo "$out" | sed -n 's/^\* VANISHED (EARLIER) //p' | tr ',' '\n' | awk -F: 'NF==1{print $1} NF==2{for(i=$1;i<=$2;i++)print i}' | sort)
    pres=$(echo "$out" | sed -n 's/^\* SEARCH //p' | tr ' ' '\n' | grep -v '^$' | sort)
    comm -12 <(echo "$van") <(echo "$pres") | grep -v '^$' | sed "s/^/$user /" || true
  done
}

# count_lines keeps every line matching the pattern from the run's logs in
# lines/<name>.txt and prints how many there were.
count_lines() {
  grep -ahE "$2" "$OUT"/logs/*.log > "$OUT/lines/$1.txt" 2>/dev/null || true
  wc -l < "$OUT/lines/$1.txt" | tr -d ' '
}

echo "-- tooling: $(git -C "$REPO" log --oneline -1) tag=$TAG"

echo "== deploy $(date -u +%FT%TZ)"
helm upgrade yarilo "$REPO/helm" -n "$NS" -f "$REPO/helm_values/values-sandbox.yaml" \
  --set image.tag="$TAG" --wait --timeout 10m > "$OUT/deploy.txt" 2>&1
kube rollout status sts/yarilo-backend --timeout=280s >> "$OUT/deploy.txt" 2>&1
START=$(date -u +%FT%TZ)
pod_state > "$OUT/pods-start.txt"
echo "-- pods settled; the run's window starts $START"

echo "== aged mailboxes before"
SDBOX_AGED=$(aged_account d00003.test sdbox/mailboxes/INBOX/dbox-Mails 'u.*')
MAILDIR_AGED=$(aged_account d00002.test Maildir/cur '*')
echo "sdbox: ${SDBOX_AGED:-none}; maildir: ${MAILDIR_AGED:-none}" | tee "$OUT/aged-before.txt"
SDBOX_AGED_DISK=""; MAILDIR_AGED_DISK=""
[ -n "$SDBOX_AGED" ] && SDBOX_AGED_DISK=$(disk_messages "${SDBOX_AGED%% *}" sdbox)
[ -n "$MAILDIR_AGED" ] && MAILDIR_AGED_DISK=$(disk_messages "${MAILDIR_AGED%% *}" maildir)

uid_pairs | sort > "$OUT/uid-pairs-before.txt"

echo "== smoketest"
for user in u1@d00001.test u51@d00002.test u101@d00003.test; do
  dir=$(mktemp -d)
  cp "$REPO/hack/smoketest/run.sh" "$dir/"
  sed "s/u1@d00001\.test/$user/g" "$REPO/hack/smoketest/job.yaml" > "$dir/job.yaml"
  NAMESPACE="$NS" bash "$dir/run.sh" "$TAG" > "$OUT/smoke-$user.log" 2>&1 || true
  summary=$(grep -o '"checks":[0-9]*,"passed":[0-9]*,"failed":[0-9]*' "$OUT/smoke-$user.log" | tail -1)
  case "$summary" in
    '"checks":46,"passed":46,"failed":0') verdict "smoketest $user" ok "$summary" ;;
    *) verdict "smoketest $user" fail "${summary:-no summary}; $(grep '"smoke: FAIL"' "$OUT/smoke-$user.log" | head -3 | cut -c1-200 | tr '\n' ' ')" ;;
  esac
done

for spec in u51@d00002.test:maildir u101@d00003.test:sdbox; do
  user=${spec%%:*}
  disk=$(disk_messages "$user" "${spec##*:}")
  index=$(index_messages "$user")
  [ -n "$disk" ] && [ "$disk" = "$index" ] && verdict "disk = index $user" ok "$disk" || verdict "disk = index $user" fail "disk=${disk:-?} index=${index:-?}"
done

echo "== imaptest, one type at a time"
for spec in d00001.test:3-20 d00002.test:53-70 d00003.test:103-120; do
  dom=${spec%%:*}
  kube delete job imaptest --ignore-not-found > /dev/null
  sed -e "s/u%d@d00001\.test/u%d@$dom/" -e "s/users=1-20/users=${spec##*:}/" "$REPO/hack/imaptest/job.yaml" | kube apply -f - > /dev/null
  kube wait --for=condition=complete --timeout=300s job/imaptest > /dev/null 2>&1 || true
  kube logs job/imaptest -c imaptest > "$OUT/imaptest-$dom.log" 2>&1 || true
  errors=$(grep -ac '^Error' "$OUT/imaptest-$dom.log" || true)
  stalls=$(grep -ac 'stalled' "$OUT/imaptest-$dom.log" || true)
  logins=$(tail -1 "$OUT/imaptest-$dom.log" | awk '{print $1}')
  [ "$errors" = 0 ] && [ "$stalls" = 0 ] && [ -n "$logins" ] &&
    verdict "imaptest $dom" ok "logins=$logins" ||
    verdict "imaptest $dom" fail "errors=$errors stalled=$stalls logins=${logins:-?}"
done
kube delete job imaptest --ignore-not-found > /dev/null

echo "== aged mailboxes after an open"
for spec in "${SDBOX_AGED:-}:sdbox:$SDBOX_AGED_DISK" "${MAILDIR_AGED:-}:maildir:$MAILDIR_AGED_DISK"; do
  acct=${spec%%:*}; rest=${spec#*:}; type=${rest%%:*}; before=${rest#*:}
  if [ -z "$acct" ]; then
    verdict "aged $type mailbox survives an open" fail "no $type account with 20+ files older than a day on the stand"
    continue
  fi
  user=${acct%% *}
  imap_session "a LOGIN $user $PASSWORD\r\nb SELECT INBOX\r\nc LOGOUT\r\n" > /dev/null
  sleep 3
  after=$(disk_messages "$user" "$type")
  [ "$after" = "$before" ] && verdict "aged $type mailbox survives an open" ok "$user $after files" ||
    verdict "aged $type mailbox survives an open" fail "$user before=$before after=$after"
done

uid_pairs | sort > "$OUT/uid-pairs-after.txt"
new_pairs=$(comm -13 "$OUT/uid-pairs-before.txt" "$OUT/uid-pairs-after.txt" | tee "$OUT/lines/uid-pairs-new.txt" | wc -l | tr -d ' ')
[ "$new_pairs" = 0 ] && verdict "no new vanished-and-present uid" ok "0" || verdict "no new vanished-and-present uid" fail "$new_pairs (lines/uid-pairs-new.txt)"

echo "== logs of every pod since $START"
pod_state > "$OUT/pods-end.txt"
for pod in $(kube get pods -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'); do
  for c in $(kube get pod "$pod" -o jsonpath='{.spec.containers[*].name}'); do
    kube logs "$pod" -c "$c" --since-time="$START" > "$OUT/logs/$pod.$c.log" 2>/dev/null || true
    kube logs "$pod" -c "$c" --previous > "$OUT/logs/$pod.$c.previous.log" 2>/dev/null || rm -f "$OUT/logs/$pod.$c.previous.log"
  done
done

changed=$(diff "$OUT/pods-start.txt" "$OUT/pods-end.txt" | grep '^[<>]' || true)
[ -z "$changed" ] && verdict "no pod restarted or replaced" ok "$(wc -l < "$OUT/pods-end.txt" | tr -d ' ') pods" ||
  verdict "no pod restarted or replaced" fail "$(echo "$changed" | tr '\n' ' ' | cut -c1-400)"

for spec in \
  "panic|panic: |fatal error: " \
  "stopped-naming|the list stopped naming this record" \
  "reconcile-unfinished|the reconcile did not finish" \
  "append-failed|handling APPEND command" \
  "list-rename|uidlist: rename" \
  "reactive-heal|reactive heal\"|corrupt message flagged" \
  "message-swept|removed a save that never got a name.*\"file\":\"u\\."; do
  name=${spec%%|*}
  n=$(count_lines "$name" "${spec#*|}")
  [ "$n" = 0 ] && verdict "no $name lines" ok "0" || verdict "no $name lines" fail "$n (lines/$name.txt)"
done

verdict "explicit domain rebalance" fail "not run: the director has no trigger for one yet"

echo "== $(date -u +%FT%TZ) $FAILS criteria failed" | tee -a "$OUT/verdict.txt"
[ "$FAILS" = 0 ]
