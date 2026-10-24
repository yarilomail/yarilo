#!/usr/bin/env bash
# Time from a delivery's LMTP 250 on one backend pod to its StateChange on a
# JMAP push stream held open on the other: the cross-pod path of push events.
# YARILO_PUSH_TRANSPORT picks the stream: eventsource (curl through a
# port-forward) or websocket (yarilo-smoketest -push-listen in the JMAP login
# pod, the hop a client's WebSocket takes).
#
# Usage (on the runner; bash 5 for EPOCHREALTIME):
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/push-latency.sh <label> <out-dir>
#
# Runs against whatever image is deployed, never alongside a window arm.

set -euo pipefail

LABEL="${1:?label}"
OUT="${2:?output directory}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
STREAM_POD="${YARILO_PUSH_POD:-}"
CANDIDATES="${YARILO_PUSH_USERS:-u1@d00001.test u51@d00002.test u101@d00003.test u2@d00001.test u52@d00002.test u102@d00003.test}"
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../sandbox/creds.sh"
PASS="${YARILO_PUSH_PASS:-$(sandbox_password)}"
COUNT="${YARILO_PUSH_COUNT:-50}"
PAUSE="${YARILO_PUSH_PAUSE:-2}"
WAIT="${YARILO_PUSH_WAIT:-10}"
SERVER_NAME="${YARILO_INTERNAL_NAME:-yarilo-internal}"
TRANSPORT="${YARILO_PUSH_TRANSPORT:-eventsource}"
JMAP_PORT=18443
LMTP_PORT=18024

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" --request-timeout=60s "$@"; }

if [ -z "${EPOCHREALTIME:-}" ]; then
  echo "push-latency: needs bash 5 (EPOCHREALTIME); run it on the runner" >&2
  exit 1
fi
mkdir -p "$OUT"
LOG="$OUT/push-$LABEL.log"
EVENTS="$OUT/push-$LABEL-events.txt"
: > "$LOG"
: > "$EVENTS"

TMP=$(mktemp -d)
chmod 700 "$TMP"
PIDS=()
cleanup() {
  for pid in ${PIDS[@]+"${PIDS[@]}"}; do kill "$pid" 2>/dev/null || true; done
  rm -rf "$TMP"
}
trap cleanup EXIT

# pin is the backend IP the director pins a user to; the map is a peek and
# places nobody, so an unplaced user is logged in once over IMAP first.
pin() {
  local at
  at=$(kube exec "$1" -- yarctl -O json director map --user "$2" 2>/dev/null |
    sed -n 's/.*"backend": *"\([^":]*\).*/\1/p' || true)
  if [ -z "$at" ]; then
    kube exec yarilo-backend-0 -c yarilo-imap -- sh -c \
      "printf 'a LOGIN $2 $PASS\r\nb LOGOUT\r\n' | nc -w 15 yarilo-imap-login 143" >> "$LOG" 2>&1 || true
    at=$(kube exec "$1" -- yarctl -O json director map --user "$2" 2>/dev/null |
      sed -n 's/.*"backend": *"\([^":]*\).*/\1/p' || true)
  fi
  echo "$at"
}

# pinned_elsewhere prints a user, the backend IP the director pins it to, and
# a backend pod other than that one for the stream: two pods, by construction.
pinned_elsewhere() {
  local director u at pod ip
  director=$(kube get pods -l app.kubernetes.io/component=director -o jsonpath='{.items[0].metadata.name}' || true)
  [ -n "$director" ] || return 1
  for u in $CANDIDATES; do
    at=$(pin "$director" "$u")
    echo "candidate $u pinned to ${at:-nothing}" >> "$LOG"
    [ -n "$at" ] || continue
    while read -r pod ip; do
      if [ "$ip" != "$at" ] && { [ -z "$STREAM_POD" ] || [ "$pod" = "$STREAM_POD" ]; }; then
        echo "$u $at $pod"
        return 0
      fi
    done < <(kube get pods -l app.kubernetes.io/component=backend \
      -o jsonpath='{range .items[*]}{.metadata.name} {.status.podIP}{"\n"}{end}')
  done
  return 1
}

# forward holds a port-forward open until the script ends and waits for it.
forward() {
  kube port-forward "$1" "$2:$3" >>"$LOG" 2>&1 &
  PIDS+=($!)
  local deadline=$(( $(date +%s) + 30 ))
  until (exec 9<>"/dev/tcp/127.0.0.1/$2") 2>/dev/null; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "push-latency: the forward to $1 on $2 never came up" >&2
      return 1
    fi
    sleep 0.2
  done
}

# The stream speaks to the backend as the JMAP login does: over internal mTLS
# with the forwarded identity, so the login pod is not on the path measured.
client_cert() {
  local secret
  secret=$(kube get deploy yarilo-jmap-login -o jsonpath='{range .spec.template.spec.volumes[*]}{.secret.secretName}{"\n"}{end}' |
    grep -- '-internal-tls$' | head -1 || true)
  [ -n "$secret" ] || { echo "push-latency: the JMAP login has no internal certificate; is internal TLS on?" >&2; return 1; }
  for k in tls.crt tls.key ca.crt; do
    kube get secret "$secret" -o jsonpath="{.data.${k//./\\.}}" | base64 -d > "$TMP/$k"
    [ -s "$TMP/$k" ] || { echo "push-latency: $secret has no $k" >&2; return 1; }
  done
}

# lmtp reads one reply, multi-line or not, and prints its code.
lmtp_reply() {
  local line
  while IFS= read -r -t 30 line <&3; do
    line=${line%$'\r'}
    echo "$line" >> "$LOG"
    case "$line" in
      [0-9][0-9][0-9]-*) continue ;;
      *) echo "${line:0:3}"; return 0 ;;
    esac
  done
  return 1
}

lmtp_send() { printf '%s\r\n' "$@" >&3; printf '> %s\n' "$@" >> "$LOG"; }

read -r USER_NAME PIN STREAM_POD <<<"$(pinned_elsewhere)" || true
if [ -z "${USER_NAME:-}" ]; then
  echo "push-latency: no candidate is pinned away from a backend pod${STREAM_POD:+ ($STREAM_POD)}; see $LOG" >&2
  exit 1
fi
forward svc/yarilo-lmtp-login "$LMTP_PORT" 24
echo "-- push-latency $LABEL: image $(kube get pod "$STREAM_POD" -o jsonpath='{.spec.containers[0].image}'), user=$USER_NAME on $PIN, $TRANSPORT on $STREAM_POD, count=$COUNT"

case "$TRANSPORT" in
eventsource)
  client_cert
  forward "pod/$STREAM_POD" "$JMAP_PORT" 10443
  curl -sSN --http1.1 --cert "$TMP/tls.crt" --key "$TMP/tls.key" --cacert "$TMP/ca.crt" \
    --resolve "$SERVER_NAME:$JMAP_PORT:127.0.0.1" \
    -H "X-Yarilo-User: $USER_NAME" -H "X-Session-ID: pushlatency00000" -H "X-Proxy-TTL: 4" \
    "https://$SERVER_NAME:$JMAP_PORT/jmap/eventsource/?types=Email&ping=0" 2>>"$LOG" |
    while IFS= read -r line; do
      case "$line" in data:*'"Email"'*) echo "$EPOCHREALTIME" >> "$EVENTS" ;; esac
    done &
  PIDS+=($!)
  sleep 2 # the stream's base is taken before the first delivery
  ;;
websocket)
  stream_ip=$(kube get pod "$STREAM_POD" -o jsonpath='{.status.podIP}')
  READY="$TMP/ready"
  kube exec -i deploy/yarilo-jmap-login -- yarilo-smoketest \
    -push-listen "wss://$stream_ip:10443/jmap/ws/" -push-listen-user "$USER_NAME" \
    -internal-server-name "$SERVER_NAME" 2>>"$LOG" |
    while IFS= read -r line; do
      case "$line" in
        ready) : > "$READY" ;;
        "state Email") echo "$EPOCHREALTIME" >> "$EVENTS" ;;
      esac
    done &
  PIDS+=($!)
  # ready comes once push is enabled, so no delivery is folded into the base.
  deadline=$(( $(date +%s) + 30 ))
  until [ -e "$READY" ]; do
    [ "$(date +%s)" -lt "$deadline" ] || { echo "push-latency: the WebSocket never became ready; see $LOG" >&2; exit 1; }
    sleep 0.1
  done
  ;;
*)
  echo "push-latency: YARILO_PUSH_TRANSPORT must be eventsource or websocket, not $TRANSPORT" >&2
  exit 1
  ;;
esac

exec 3<>"/dev/tcp/127.0.0.1/$LMTP_PORT"
[ "$(lmtp_reply)" = 220 ] || { echo "push-latency: no LMTP greeting; see $LOG" >&2; exit 1; }
lmtp_send "LHLO push-latency.invalid"
[ "$(lmtp_reply)" = 250 ] || { echo "push-latency: LHLO refused; see $LOG" >&2; exit 1; }

LAT="$OUT/push-$LABEL-ms.txt"
: > "$LAT"
lost=0
for i in $(seq 1 "$COUNT"); do
  seen=$(wc -l < "$EVENTS")
  for step in "MAIL FROM:<push-latency@test.invalid> 250" "RCPT TO:<$USER_NAME> 250" "DATA 354"; do
    lmtp_send "${step% *}"
    code=$(lmtp_reply) || code=none
    [ "$code" = "${step##* }" ] || { echo "push-latency: delivery $i answered $code to ${step% *}; see $LOG" >&2; exit 1; }
  done
  lmtp_send "Subject: push-latency $LABEL $i" "" "x" "."
  [ "$(lmtp_reply)" = 250 ] || { echo "push-latency: delivery $i not accepted; see $LOG" >&2; exit 1; }
  t0=$EPOCHREALTIME
  deadline=$(( ${t0%.*} + WAIT ))
  until [ "$(wc -l < "$EVENTS")" -gt "$seen" ]; do
    if [ "$(date +%s)" -ge "$deadline" ]; then break; fi
    sleep 0.005
  done
  if [ "$(wc -l < "$EVENTS")" -gt "$seen" ]; then
    t1=$(sed -n "$((seen + 1))p" "$EVENTS")
    awk -v a="$t0" -v b="$t1" 'BEGIN { printf "%.1f\n", (b - a) * 1000 }' >> "$LAT"
  else
    lost=$((lost + 1))
  fi
  sleep "$PAUSE"
done
lmtp_send "QUIT"
exec 3>&-

sort -n "$LAT" | awk -v label="$LABEL" -v lost="$lost" -v n="$COUNT" '
  { v[NR] = $1 }
  END {
    if (NR == 0) { printf "push_latency label=%s count=0 lost=%d\n", label, lost; exit 1 }
    p50 = v[int((NR - 1) * 0.50) + 1]; p99 = v[int((NR - 1) * 0.99) + 1]
    printf "push_latency label=%s count=%d lost=%d p50_ms=%s p99_ms=%s max_ms=%s\n", label, NR, lost, p50, p99, v[NR]
  }'
[ "$lost" -eq 0 ] || { echo "push-latency: $lost of $COUNT deliveries brought no StateChange within ${WAIT}s" >&2; exit 1; }
