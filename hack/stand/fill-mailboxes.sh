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

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" "$@"; }

pod=$(kube get pods -l app.kubernetes.io/component=backend -o name | head -1 | cut -d/ -f2)
[ -n "$pod" ] || { echo "fill: no backend pod in $NS" >&2; exit 1; }

# One connection per user, every message in it: a connection per message spends
# the whole fill in handshakes.
deliver_user() {
  local user="$1"
  {
    printf 'LHLO fill.invalid\r\n'
    for i in $(seq 1 "$COUNT"); do
      printf 'MAIL FROM:<fill@test.invalid>\r\nRCPT TO:<%s>\r\nDATA\r\n' "$user"
      printf 'Subject: fill %s\r\nFrom: <fill@test.invalid>\r\nTo: <%s>\r\nDate: Thu, 18 Sep 2026 12:00:00 +0000\r\nMessage-ID: <fill-%s-%s@test.invalid>\r\n\r\n' \
        "$i" "$user" "$i" "$user"
      awk -v n="$BODY_LINES" 'BEGIN { for (i = 0; i < n; i++) printf "%s\r\n", "filler text for a mailbox that is not empty" }'
      printf '.\r\n'
    done
    printf 'QUIT\r\n'
  } | kube exec -i "$pod" -c yarilo-imap -- nc -w 60 "$LMTP_HOST" "$LMTP_PORT" 2>&1
}

delivered=0
refused=0
for n in $(seq "$FROM" "$TO"); do
  user="u${n}@${DOMAIN}"
  out=$(deliver_user "$user") || true
  # Counted from the answers, not from what was sent: a mailbox that refused
  # is not a mailbox that was filled.
  ok=$(printf '%s\n' "$out" | grep -c '^250 2\.0\.0' || true)
  delivered=$((delivered + ok))
  refused=$((refused + COUNT - ok))
done

echo "fill: delivered=$delivered refused=$refused users=$((TO - FROM + 1)) per_user=$COUNT"
[ "$refused" = "0" ] || { echo "fill: $refused deliveries were not accepted" >&2; exit 1; }
