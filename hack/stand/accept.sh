#!/usr/bin/env bash
# The rollout acceptance run: deploy a tag, smoketest and imaptest on the three
# storage types, and judge every criterion a rollout must meet, PASS or FAIL.
# Every log line a criterion counts is kept verbatim: a count without its line
# explains nothing once the pods are replaced (#2183).
#
# Usage:
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/accept.sh <image-tag> <out-dir>
#
# Runs on the runner, like a window. Exits 1 when any criterion fails; a SKIP
# (a criterion that cannot run yet) is counted apart and does not.

set -Eeuo pipefail
# A command that prints nothing must not end the run without a word: two runs
# died that way inside uid_pairs.
trap 'rc=$?; echo "accept: line $LINENO: $BASH_COMMAND exited $rc" >&2' ERR

TAG="${1:?image tag}"
OUT="${2:?output directory}"
NS="${YARILO_NS:-yarilo-sb}"
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PASSWORD='Yarilo!test1'
mkdir -p "$OUT/logs" "$OUT/lines"
: > "$OUT/verdict.txt"
FAILS=0
SKIPS=0

kube() { kubectl -n "$NS" --request-timeout=60s "$@"; }

# verdict records one criterion: its name, ok, fail or skip, and what decided it.
verdict() {
  case "$2" in
    ok) echo "PASS $1: $3" | tee -a "$OUT/verdict.txt" ;;
    skip)
      echo "SKIP $1: $3" | tee -a "$OUT/verdict.txt"
      SKIPS=$((SKIPS + 1))
      ;;
    *)
      echo "FAIL $1: $3" | tee -a "$OUT/verdict.txt"
      FAILS=$((FAILS + 1))
      ;;
  esac
}

# pod_state lists every pod with its uid and each container's restart count: a
# replaced pod shows as a new uid, a restarted container as a higher count.
pod_state() {
  kube get pods -l '!job-name' -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.uid}{range .status.containerStatuses[*]} {.name}={.restartCount}{end}{"\n"}{end}' | sort
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
# message files modified over a day ago: the class #2172's sweep judged by mtime.
aged_account() {
  local dom="$1" sub="$2" pattern="$3"
  kube exec yarilo-backend-0 -c yarilo-imap -- sh -c \
    "cd /var/mail/vhosts/$dom 2>/dev/null && for u in *; do n=\$(find \"\$u/$sub\" -maxdepth 1 -name '$pattern' -mmin +1440 2>/dev/null | wc -l); [ \"\$n\" -ge 20 ] && echo \"\$u \$n\" && break; done" 2>/dev/null || true
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
    pres=$(echo "$out" | sed -n 's/^\* SEARCH //p' | tr ' ' '\n' | { grep -v '^$' || true; } | sort)
    comm -12 <(echo "$van") <(echo "$pres") | grep -v '^$' | sed "s/^/$user /" || true
  done
}

# director_pin is the backend IP one director pins a user to (a peek: it
# places nobody).
director_pin() {
  kube exec "$1" -- yarctl -O json director map --user "$2" 2>/dev/null |
    sed -n 's/.*"backend": *"\([^"]*\)".*/\1/p' || true
}

# move_domain places a domain on a backend through the operator's command and
# prints "<from> <to> <moved>" from its reply.
move_domain() {
  local out
  out=$(kube exec "$DIRECTOR" -- yarctl -O json director domains move "$1" "$2" 2>&1) || { echo "error: $out" | tr '\n' ' '; return; }
  echo "$(echo "$out" | sed -n 's/.*"from": *"\([^:"]*\).*/\1/p') $(echo "$out" | sed -n 's/.*"to": *"\([^:"]*\).*/\1/p') $(echo "$out" | sed -n 's/.*"moved": *"\([a-z]*\)".*/\1/p')"
}

# login_lands logs a user in through the login service and asserts every
# director pinned that login to the expected backend: a pin left elsewhere
# would split the domain between two backends.
login_lands() {
  local name="$1" user="$2" want="$3" d pin bad=""
  sleep 3
  imap_session "a LOGIN $user $PASSWORD\r\nb SELECT INBOX\r\nc LOGOUT\r\n" > "$OUT/rebalance-${name// /-}.imap"
  grep -q '^b OK' "$OUT/rebalance-${name// /-}.imap" || { verdict "$name" fail "$user SELECT not OK after the move"; return; }
  for d in $DIRECTORS; do
    pin=$(director_pin "$d" "$user")
    echo "$name: $d pins $user to ${pin:-none}" >> "$OUT/rebalance.txt"
    [ "$pin" = "$want" ] || bad="$bad $d=${pin:-none}"
  done
  [ -z "$bad" ] && verdict "$name" ok "$user on $want at every director" ||
    verdict "$name" fail "want $want, $user pinned at$bad"
}

# lock_hold prints maildir_lock_hold_seconds from every backend's IMAP and FTS
# containers: both reconcile the same folders and take the same list lock.
lock_hold() {
  local pod
  for pod in $(kube get pods -l app.kubernetes.io/component=backend -o name | cut -d/ -f2); do
    kube exec "$pod" -c yarilo-imap -- sh -c 'wget -qO- http://127.0.0.1:8080/metrics' 2>/dev/null | { grep -E '^maildir_lock_hold_seconds_bucket\{' || true; }
    kube exec "$pod" -c yarilo-fts -- sh -c 'wget -qO- http://127.0.0.1:8085/metrics' 2>/dev/null | { grep -E '^maildir_lock_hold_seconds_bucket\{' || true; }
  done
}

# hold_quantiles reads the run's bucket delta and prints, per site, the count
# and the bucket bounds p50 and p99 fall under.
hold_quantiles() {
  awk '
    FILENAME == ARGV[1] { if ($1 ~ /_bucket\{/) start[$1] += $2; next }
    $1 ~ /_bucket\{/ {
      site = $1; sub(/.*site="/, "", site); sub(/".*/, "", site)
      le = $1; sub(/.*le="/, "", le); sub(/".*/, "", le)
      n[site, le] += $2 - start[$1]; start[$1] = 0; les[le] = 1; sites[site] = 1
    }
    END {
      m = 0
      for (l in les) if (l != "+Inf") b[++m] = l + 0
      for (i = 1; i <= m; i++) for (j = i + 1; j <= m; j++) if (b[j] < b[i]) { x = b[i]; b[i] = b[j]; b[j] = x }
      for (s in sites) {
        total = n[s, "+Inf"]; p50 = "-"; p99 = "-"
        for (i = 1; i <= m; i++) {
          c = 0
          for (l in les) if (l != "+Inf" && l + 0 == b[i]) c = n[s, l]
          if (p50 == "-" && total > 0 && c >= 0.5 * total) p50 = b[i]
          if (p99 == "-" && total > 0 && c >= 0.99 * total) p99 = b[i]
        }
        if (total > 0 && p50 == "-") p50 = "+Inf"
        if (total > 0 && p99 == "-") p99 = "+Inf"
        print s, total, p50, p99
      }
    }' "$1" "$2" | sort
}

# watch_stopped_naming copies a user's list, lock file, index and the named
# record's stat into evidence/ at the first stopped-naming line for that
# folder: the line alone does not say why the row went missing (#2183).
watch_stopped_naming() {
  set +e
  trap - ERR
  local seen="$OUT/evidence/.seen" pod c line user folder base dom home md ix
  mkdir -p "$OUT/evidence"; : > "$seen"
  while :; do
    for pod in $(kube get pods -l app.kubernetes.io/component=backend -o name 2>/dev/null | cut -d/ -f2); do
      for c in yarilo-imap yarilo-fts; do
        kube logs "$pod" -c "$c" --since-time="$START" 2>/dev/null | grep -a 'the list stopped naming this record' |
          while IFS= read -r line; do
            user=$(echo "$line" | sed -n 's/.*"user":"\([^"]*\)".*/\1/p')
            folder=$(echo "$line" | sed -n 's/.*"folder":"\([^"]*\)".*/\1/p')
            base=$(echo "$line" | sed -n 's/.*"base":"\([^"]*\)".*/\1/p')
            grep -qxF "$user $folder" "$seen" && continue
            echo "$user $folder" >> "$seen"
            dom=${user#*@}; home="/var/mail/vhosts/$dom/$user"
            md="Maildir"; ix="index"
            [ "$folder" = INBOX ] || { md="Maildir/.$folder"; ix="index/.$folder"; }
            d="$OUT/evidence/$user.${folder//\//_}"; mkdir -p "$d"
            echo "$line" > "$d/line.json"
            kube exec "$pod" -c yarilo-imap -- sh -c "cd $home && tar -cf - $md/yarilo-uidlist $md/yarilo-uidlist.lock $ix/yarilo.index $ix/yarilo.index.log $ix/yarilo.index.cache 2>/dev/null" > "$d/files.tar"
            kube exec "$pod" -c yarilo-imap -- sh -c "cd $home && stat $md/yarilo-uidlist $md/yarilo-uidlist.lock $md/cur/${base%%,*}* $md/new/${base%%,*}* 2>&1; ls -la --full-time $md/tmp 2>&1" > "$d/stat.txt"
            echo "-- evidence for $user $folder from $pod/$c in $d"
          done
      done
    done
    sleep 5
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
lock_hold > "$OUT/lock-hold-start.txt"
watch_stopped_naming &
WATCHER=$!
trap 'kill "$WATCHER" 2>/dev/null || true' EXIT

echo "== aged mailboxes before"
SDBOX_AGED=$(aged_account d00003.test sdbox/mailboxes/INBOX/dbox-Mails 'u.*')
MAILDIR_AGED=$(aged_account d00002.test Maildir/cur '*')
echo "sdbox: ${SDBOX_AGED:-none}; maildir: ${MAILDIR_AGED:-none}" | tee "$OUT/aged-before.txt"

uid_pairs | sort > "$OUT/uid-pairs-before.txt"

echo "== smoketest"
for user in u1@d00001.test u51@d00002.test u101@d00003.test; do
  dir=$(mktemp -d)
  cp "$REPO/hack/smoketest/run.sh" "$dir/"
  sed "s/u1@d00001\.test/$user/g" "$REPO/hack/smoketest/job.yaml" > "$dir/job.yaml"
  NAMESPACE="$NS" bash "$dir/run.sh" "$TAG" > "$OUT/smoke-$user.log" 2>&1 || true
  summary=$(grep -o '"checks":[0-9]*,"passed":[0-9]*,"failed":[0-9]*' "$OUT/smoke-$user.log" | tail -1 || true)
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
for spec in "${SDBOX_AGED:-}:sdbox" "${MAILDIR_AGED:-}:maildir"; do
  acct=${spec%%:*}; type=${spec#*:}
  if [ -z "$acct" ]; then
    verdict "aged $type mailbox survives an open" fail "no $type account with 20+ files older than a day on the stand"
    continue
  fi
  user=${acct%% *}
  # Counted right before the open: the smoketest appends to these accounts.
  before=$(disk_messages "$user" "$type")
  imap_session "a LOGIN $user $PASSWORD\r\nb SELECT INBOX\r\nc LOGOUT\r\n" > /dev/null
  sleep 3
  after=$(disk_messages "$user" "$type")
  [ "$after" = "$before" ] && verdict "aged $type mailbox survives an open" ok "$user $after files" ||
    verdict "aged $type mailbox survives an open" fail "$user before=$before after=$after"
done

uid_pairs | sort > "$OUT/uid-pairs-after.txt"
new_pairs=$(comm -13 "$OUT/uid-pairs-before.txt" "$OUT/uid-pairs-after.txt" | tee "$OUT/lines/uid-pairs-new.txt" | wc -l | tr -d ' ')
[ "$new_pairs" = 0 ] && verdict "no new vanished-and-present uid" ok "0" || verdict "no new vanished-and-present uid" fail "$new_pairs (lines/uid-pairs-new.txt)"

echo "== explicit domain rebalance"
DIRECTORS=$(kube get pods -l app.kubernetes.io/component=director -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' || true)
DIRECTOR=${DIRECTORS%% *}
UP=$(kube exec "$DIRECTOR" -- yarctl director backends list 2>/dev/null | awk '$0 ~ /[[:space:]]up[[:space:]]/ {print $1}' | sort || true)
: > "$OUT/rebalance.txt"
if [ -z "$DIRECTOR" ] || [ "$(echo "$UP" | grep -c .)" -lt 2 ]; then
  verdict "explicit domain rebalance" fail "need a director and two up backends: directors='$DIRECTORS' up='$(echo $UP)'"
else
  # The move names its source in the reply; a no-op means the domain already
  # sits on the backend asked for, so the other one is the move.
  first=$(echo "$UP" | sed -n 1p); second=$(echo "$UP" | sed -n 2p)
  reply=$(move_domain d00001.test "$second")
  case "$reply" in *" false") reply=$(move_domain d00001.test "$first") ;; esac
  echo "move: $reply" >> "$OUT/rebalance.txt"
  set -- $reply
  if [ "${3:-}" != true ]; then
    verdict "explicit domain rebalance" fail "d00001.test did not move: $reply"
  else
    login_lands "explicit domain rebalance" u1@d00001.test "$2"
    back=$(move_domain d00001.test "$1")
    echo "back: $back" >> "$OUT/rebalance.txt"
    login_lands "domain moved back" u1@d00001.test "$1"
  fi
fi

kill "$WATCHER" 2>/dev/null || true

echo "== list lock hold over the run"
lock_hold > "$OUT/lock-hold-end.txt"
hold_quantiles "$OUT/lock-hold-start.txt" "$OUT/lock-hold-end.txt" | tee "$OUT/lock-hold.txt"
# The list wait gives up at 10 s: every site's p99 must sit well under it.
over=$(awk '$4 == "+Inf" || ($4 != "-" && $4 + 0 >= 10) { print $1 "=" $4 "s" }' "$OUT/lock-hold.txt" | tr '\n' ' ')
if [ ! -s "$OUT/lock-hold.txt" ]; then
  verdict "list hold p99 under the 10 s wait" fail "no lock hold histogram read"
elif [ -n "$over" ]; then
  verdict "list hold p99 under the 10 s wait" fail "$over"
else
  verdict "list hold p99 under the 10 s wait" ok "$(awk '{ printf "%s n=%s p50<=%ss p99<=%ss; ", $1, $2, $3, $4 }' "$OUT/lock-hold.txt")"
fi

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


# The directors on their own: #2187 was a director panic on a domain move.
n=0
for d in $DIRECTORS; do
  n=$((n + $(cat "$OUT"/logs/"$d".*.log 2>/dev/null | grep -acE "panic: |fatal error: " || true)))
done
[ "$n" = 0 ] && verdict "no director panic" ok "0 in $(echo $DIRECTORS | wc -w | tr -d ' ') directors" ||
  verdict "no director panic" fail "$n (lines/panic.txt)"

echo "== $(date -u +%FT%TZ) $FAILS failed, $SKIPS skipped" | tee -a "$OUT/verdict.txt"
[ "$FAILS" = 0 ]
