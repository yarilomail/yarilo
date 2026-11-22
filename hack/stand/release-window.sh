#!/usr/bin/env bash
# The release window on the cut image: A1 (the last release with its own chart
# and values) / B (the cut) / A2 (last release, same slot as B), store-parallel
# after each arm; then the rollback: Bw writes, A2c (last release on Bw's
# store) judged with that release's smoketest and disk = index, and back to B.
#
# Usage:
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/release-window.sh <cut-tag> <last-release> <out-dir>
#
# The tooling is this checkout at YARILO_ARM_COMMIT (default HEAD); the last
# release's chart is a worktree at v<last-release> beside it. Progress lines go
# to <out-dir>/progress.txt. A failed arm saves the stand in snap-<arm>/ and
# stops the window there.

set -u
TAG="${1:?cut image tag}"
REL="${2:?last release, e.g. 2.4.7}"
W="${3:?output directory}"
NS="${YARILO_NS:-yarilo-sb}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
A="${YARILO_RELEASE_WORKTREE:-$(dirname "$REPO")/stand-v$REL}"
export YARILO_ARM_COMMIT="${YARILO_ARM_COMMIT:-$(git -C "$REPO" rev-parse HEAD)}"
[ -e "$W" ] && { echo "release-window: $W exists; a window never writes over another's evidence" >&2; exit 1; }
mkdir -p "$W"
cd "$REPO" || exit 1

kube() { kubectl -n "$NS" --request-timeout=60s "$@"; }
p() { echo "$(date -u +%H:%M:%S) $*" | tee -a "$W/progress.txt"; }
master() {
  kubectl -n redis exec valkey-probe -- sh -c 'REDISCLI_AUTH=$(cat /auth/sentinel/sentinel) valkey-cli --no-auth-warning --user sentinel -h valkey-sentinel -p 26379 SENTINEL get-master-addr-by-name mymaster' | head -1
}
unread() {
  local i
  for i in 0 1; do
    kube exec "yarilo-backend-$i" -c yarilo-imap -- sh -c 'wget -qO- http://127.0.0.1:8080/metrics' 2>/dev/null |
      grep '^imap_unreadable_messages_total' | sed "s/^/$1 backend-$i /"
  done >> "$W/unreadable.txt"
}
# snap saves the stand as it stands at a failure, before anything else runs,
# and stops the window.
snap() {
  local d="$W/snap-$1" po
  mkdir -p "$d"
  kube get pods -o wide > "$d/pods.txt" 2>&1
  kube get sts,deploy,job -o wide > "$d/workloads.txt" 2>&1
  kube get events --sort-by=.lastTimestamp > "$d/events.txt" 2>&1
  helm status yarilo -n "$NS" > "$d/helm-status.txt" 2>&1
  for po in $(kube get pods --no-headers | awk '{split($2,r,"/"); if (r[1]!=r[2] && $3!="Completed") print $1}'); do
    kube describe pod "$po" > "$d/describe-$po.txt" 2>&1
    kube logs "$po" --all-containers --timestamps > "$d/logs-$po.txt" 2>&1
    kube logs "$po" --all-containers --timestamps --previous > "$d/logs-previous-$po.txt" 2>&1
  done
  p "snapshot $1 saved in snap-$1; window stopped, stand left as it is"
  exit 1
}
run_arm() { # label tag chart [keep]
  p "arm $1 $2 ab-arm"
  YARILO_ARM_KEEP_STORE="${4:-0}" YARILO_ARM_CHART="$3" bash "$HERE/ab-arm.sh" "$1" "$2" "$W" > "$W/arm-$1.out" 2>&1 || { p "arm $1 ab-arm FAILED"; snap "$1"; }
  unread "$1"
  p "arm $1 valkey master=$(master)"
}
store() {
  p "arm $1 store-parallel"
  bash "$HERE/store-parallel.sh" "$1" "$W" > "$W/store-$1.out" 2>&1 || { p "arm $1 store FAILED"; snap "store-$1"; }
}
index_messages() {
  kube exec yarilo-backend-0 -c yarilo-backend-api -- yarctl backend folder info "$1" INBOX 2>/dev/null |
    sed -n 's/.*"messages": *\([0-9]*\).*/\1/p'
}
disk_messages() {
  local user="$1" type="$2" home="/var/mail/vhosts/${1#*@}/$1"
  case "$type" in
    sdbox) kube exec yarilo-backend-0 -c yarilo-imap -- sh -c "ls $home/sdbox/mailboxes/INBOX/dbox-Mails 2>/dev/null | grep -c '^u\.' || true" ;;
    maildir) kube exec yarilo-backend-0 -c yarilo-imap -- sh -c "ls $home/Maildir/cur $home/Maildir/new 2>/dev/null | grep -vc -e '^\$' -e ':\$' || true" ;;
  esac
}
. "$HERE/deploy.sh"

git -C "$REPO" diff --quiet HEAD || { echo "release-window: the checkout is not clean" >&2; exit 1; }
[ -d "$A" ] || git -C "$REPO" worktree add -q --detach "$A" "v$REL" || { echo "release-window: no worktree at v$REL" >&2; exit 1; }
M0=$(master)
p "start cut=$TAG release=$REL tooling=$(git -C "$REPO" rev-parse --short HEAD) $A at $(git -C "$A" describe --tags) valkey master=$M0"

run_arm A1 "$REL" "$A"; store A1
run_arm B "$TAG" "$REPO"; store B
run_arm A2 "$REL" "$A"; store A2
p "rollback: Bw writes"
run_arm Bw "$TAG" "$REPO"
p "rollback: A2c $REL on Bw's store"
run_arm A2c "$REL" "$A" 1
for user in u1@d00001.test u51@d00002.test u101@d00003.test; do
  d=$(mktemp -d)
  cp "$A/hack/smoketest/run.sh" "$d/"
  sed "s/u1@d00001\.test/$user/g" "$A/hack/smoketest/job.yaml" > "$d/job.yaml"
  NAMESPACE="$NS" bash "$d/run.sh" "$REL" > "$W/a2c-smoke-$user.log" 2>&1
  p "A2c smoke $user rc=$? $(grep -o '"checks":[0-9]*,"passed":[0-9]*,"failed":[0-9]*,"skipped":[0-9]*' "$W/a2c-smoke-$user.log" | tail -1)"
  rm -rf "$d"
done
for spec in u51@d00002.test:maildir u101@d00003.test:sdbox; do
  user=${spec%%:*}
  p "A2c disk = index $user disk=$(disk_messages "$user" "${spec##*:}") index=$(index_messages "$user")"
done
p "back to $TAG"
mkdir -p "$W/deploy-back"
stand_deploy "$W/deploy-back" "$TAG" "$REPO/helm" -f "$REPO/helm_values/values-sandbox.yaml" > "$W/back.txt" 2>&1 || p "back FAILED"
for d in $(kube get pods -o name | grep director | grep -v ring); do
  kube exec "$d" -- yarctl -O json director ring status > "$W/ring-${d#pod/}.json" 2>&1
  p "ring status at ${d#pod/}: rc=$? saved ring-${d#pod/}.json"
done
p "valkey master at the end=$(master) (start $M0)"
p "done"
