#!/usr/bin/env bash
# Delivers the same number of messages to every user in a range, over LMTP, so
# an arm starts from a mailbox a deployment would recognise. An empty mailbox
# is a state no deployment is in, and on this stand its throughput is about
# four times a filled one's (#1875), so a window on empty mailboxes measures
# something nobody runs.
#
# Usage:
#   KUBECONFIG=... bash hack/stand/fill-mailboxes.sh <from> <to> <count>

set -euo pipefail

FROM="${1:?first user number}"
TO="${2:?last user number}"
COUNT="${3:?messages per user}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
DOMAIN="${YARILO_FILL_DOMAIN:-d00001.test}"
LMTP_HOST="${LMTP_HOST:-yarilo-lmtp-login}"
LMTP_PORT="${LMTP_PORT:-24}"
BODY_LINES="${YARILO_FILL_LINES:-24}"

# --request-timeout so a call that hangs becomes an error instead of a script
# that waits for ever: an arm stood six hours on one (#1875).
kube() { kubectl --kubeconfig="$KCFG" -n "$NS" --request-timeout=60s "$@"; }

pod=$({ out=$(kube get pods -l app.kubernetes.io/component=backend -o name); printf '%s' "${out%%$'\n'*}" | cut -d/ -f2; })
# The index, not the transcript, answers whether a mailbox is filled.
[ -n "$pod" ] || { echo "fill: no backend pod in $NS" >&2; exit 1; }

# messages_in prints how many messages the index holds for one mailbox.
# A failed read answers "unknown", not "the script is over": under set -e a
# command substitution that fails ends the run with no line in the log, which
# is how an arm died silently mid-fill (#1875).
messages_in() {
  local out rc
  out=$(kube exec "$pod" -c yarilo-backend-api -- yarctl -O json backend quota show "$1" 2>&1) || rc=$?
  if [ "${rc:-0}" != "0" ]; then
    case "$out" in
      *"no mail home"*)
        # Not an error: a wiped mailbox has no home until the first delivery.
        echo "0"
        return 0
        ;;
    esac
    echo "fill: could not read the index for $1: ${out%%$'\n'*}" >&2
    echo "-1"
    return 0
  fi
  printf '%s\n' "$out" |
    awk -F'[:,]' '/"message_value"/ { gsub(/[^0-9-]/, "", $2); print $2 + 0; exit }'
}

# One connection per user, every message in it: a connection per message spends
# the whole fill in handshakes.
deliver_user() {
  local user="$1" want="${2:-$COUNT}"
  {
    printf 'LHLO fill.invalid\r\n'
    for i in $(seq 1 "$want"); do
      printf 'MAIL FROM:<fill@test.invalid>\r\nRCPT TO:<%s>\r\nDATA\r\n' "$user"
      printf 'Subject: fill %s\r\nFrom: <fill@test.invalid>\r\nTo: <%s>\r\nDate: Thu, 18 Sep 2026 12:00:00 +0000\r\nMessage-ID: <fill-%s-%s@test.invalid>\r\n\r\n' \
        "$i" "$user" "$i" "$user"
      awk -v n="$BODY_LINES" 'BEGIN { for (i = 0; i < n; i++) printf "%s\r\n", "filler text for a mailbox that is not empty" }'
      printf '.\r\n'
    done
    printf 'QUIT\r\n'
  } | kube exec -i "$pod" -c yarilo-imap -- nc -w 60 "$LMTP_HOST" "$LMTP_PORT" 2>&1
}

# A range that is already filled is left alone: the fill is the expensive part
# of an arm -- thirty thousand deliveries -- and a run that lost its connection
# should resume, not start over (#1875).
already=1
for n in $(seq "$FROM" "$TO"); do
  have=$(messages_in "u${n}@${DOMAIN}")
  if [ "${have:-0}" != "$COUNT" ]; then
    already=0
    break
  fi
done
if [ "$already" = "1" ]; then
  echo "fill: in_index=$(( (TO - FROM + 1) * COUNT )) acked=0 users=$((TO - FROM + 1)) per_user=$COUNT (already filled)"
  exit 0
fi

acked=0
for n in $(seq "$FROM" "$TO"); do
  user="u${n}@${DOMAIN}"
  have=$(messages_in "$user")
  have=${have:-0}
  if [ "$have" -lt 0 ]; then
    # Unknown: deliver the whole count and let the proof below decide.
    have=0
  fi
  if [ "$have" -eq "$COUNT" ]; then
    continue
  elif [ "$have" -gt "$COUNT" ]; then
    # Not ours to correct: a mailbox past the number was filled by something
    # else, and topping up or ignoring it both make the arm a liar.
    echo "fill: $user holds $have messages, more than the $COUNT asked for" >&2
    exit 1
  fi
  # The remainder only: the mailbox a lost connection left half full is the
  # one that most needs resuming, and a second full delivery would break the
  # proof exactly there.
  out=$(deliver_user "$user" "$((COUNT - have))") || true
  # Informational: what the transport said, for a failure to be readable.
  acked=$((acked + $(printf '%s\n' "$out" | grep -o '250 2\.0\.0' | wc -l | tr -d ' ')))
done

short=0
have_total=0
for n in $(seq "$FROM" "$TO"); do
  user="u${n}@${DOMAIN}"
  have=$(messages_in "$user")
  have=${have:-0}
  have_total=$((have_total + have))
  if [ "$have" != "$COUNT" ]; then
    echo "fill: $user holds $have messages, want $COUNT" >&2
    short=$((short + 1))
  fi
done

echo "fill: in_index=$have_total acked=$acked users=$((TO - FROM + 1)) per_user=$COUNT"
[ "$short" = "0" ] || { echo "fill: $short mailboxes are not filled to $COUNT" >&2; exit 1; }
