#!/usr/bin/env bash
# The release window on the cut image: A1 (the last release with its own chart
# and values) / B (the cut) / A2 (last release, same slot as B), store-parallel
# after each arm, and back to B. Each A arm runs on a store its own release
# filled: before A1 every account of the test domains and the public
# namespace is emptied, since the scenarios before a window write them with
# the cut, and each arm empties the mailbox its seed fills. A rollback arm (the
# last release reading a store the cut wrote) is not run: the old release is
# not meant to read what the new one writes (#2685).
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
. "$HERE/deploy.sh"

# reset_store empties every account of the test domains and the public
# namespace, and stops the window when anything is left.
reset_store() {
  local pod left
  pod=$(kube get pods -l app.kubernetes.io/component=backend -o name | head -1 | cut -d/ -f2)
  kube exec "$pod" -c yarilo-imap -- sh -c '
    for d in /var/mail/vhosts/d00001.test /var/mail/vhosts/d00002.test /var/mail/vhosts/d00003.test /var/mail/vhosts/public; do
      [ -d "$d" ] && find "$d" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
    done' > "$W/reset-store.txt" 2>&1
  left=$(kube exec "$pod" -c yarilo-imap -- sh -c 'find /var/mail/vhosts/d00001.test /var/mail/vhosts/d00002.test /var/mail/vhosts/d00003.test /var/mail/vhosts/public -mindepth 1 2>/dev/null | wc -l')
  [ "$left" = 0 ] || { p "store reset left ${left:-unreadable} entries"; snap reset; }
  p "store reset: the test domains and the public namespace are empty"
}

git -C "$REPO" diff --quiet HEAD || { echo "release-window: the checkout is not clean" >&2; exit 1; }
[ -d "$A" ] || git -C "$REPO" worktree add -q --detach "$A" "v$REL" || { echo "release-window: no worktree at v$REL" >&2; exit 1; }
M0=$(master)
p "start cut=$TAG release=$REL tooling=$(git -C "$REPO" rev-parse --short HEAD) $A at $(git -C "$A" describe --tags) valkey master=$M0"

reset_store
run_arm A1 "$REL" "$A"; store A1
run_arm B "$TAG" "$REPO"; store B
run_arm A2 "$REL" "$A"; store A2
p "rollback: not run; the last release is not meant to read a store the cut wrote (#2685)"
p "back to $TAG"
mkdir -p "$W/deploy-back"
stand_deploy "$W/deploy-back" "$TAG" "$REPO/helm" -f "$REPO/helm_values/values-sandbox.yaml" > "$W/back.txt" 2>&1 || p "back FAILED"
for d in $(kube get pods -o name | grep director | grep -v ring); do
  kube exec "$d" -- yarctl -O json director ring status > "$W/ring-${d#pod/}.json" 2>&1
  p "ring status at ${d#pod/}: rc=$? saved ring-${d#pod/}.json"
done
p "valkey master at the end=$(master) (start $M0)"
p "done"
