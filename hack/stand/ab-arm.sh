#!/usr/bin/env bash
# One arm of a stand A/B: deploy, restore the same start, run the three storage
# types, and record the auth latency histogram either side.
#
# The same start before every arm is the point: mailbox fill moves throughput
# further than most things under test (#1733).
#
# Usage:
#   KUBECONFIG=~/.kube/ihorru-sbox-nc.yaml \
#     bash hack/stand/ab-arm.sh <arm-label> <image-tag> <out-dir>

set -euo pipefail

# An arm that ends anywhere but its own last line says so: three runs died
# inside the fill leaving only the line before it (#1875).
trap 'rc=$?; [ "$rc" = 0 ] || echo "ab-arm: arm ${ARM:-?} ended during ${STEP:-?} with status $rc" >&2' EXIT

ARM="${1:?arm label}"
TAG="${2:?image tag}"
OUT="${3:?output directory}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" --request-timeout=60s "$@"; }

# first_pod names one pod of a component. Captured whole and then cut: piping
# kubectl into head closes the pipe under its feet, and pipefail turns that
# SIGPIPE into status 141 -- which ended an arm after its fill, with no line
# naming the step (#1875).
first_pod() {
  local out
  out=$(kube get pods -l "app.kubernetes.io/component=$1" -o name 2>/dev/null) || return 1
  out=${out%%$'\n'*}
  printf '%s\n' "${out#pod/}"
}

# step names what the arm is doing, so the exit trap can say where it stopped.
step() { STEP="$1"; }
STEP="starting"
mkdir -p "$OUT"

IMAGE_REPO="${YARILO_IMAGE_REPO:-yarilomail/yarilo}"

# A tag with no image deploys, backs off, and reports "pods did not settle" ten
# minutes later -- a true sentence pointing at the wrong thing (#1881).
tag_exists() {
  local tok code
  tok=$(curl -fsS "https://ghcr.io/token?scope=repository:${IMAGE_REPO}:pull&service=ghcr.io" |
    sed 's/.*"token":"\([^"]*\)".*/\1/') || return 2
  [ -n "$tok" ] || return 2
  code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $tok" \
    -H "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.docker.distribution.manifest.v2+json" \
    "https://ghcr.io/v2/${IMAGE_REPO}/manifests/$1") || return 2
  case "$code" in 200) return 0;; 404) return 1;; *) return 2;; esac
}

last_built_tag() {
  local tok
  tok=$(curl -fsS "https://ghcr.io/token?scope=repository:${IMAGE_REPO}:pull&service=ghcr.io" |
    sed 's/.*"token":"\([^"]*\)".*/\1/') || return 1
  curl -fsS -H "Authorization: Bearer $tok" "https://ghcr.io/v2/${IMAGE_REPO}/tags/list?n=1000" |
    tr ',' '\n' | grep -o '[0-9]\+\.[0-9]\+\.[0-9]\+-dev\.[0-9]\+' | sort -t. -k4 -n | tail -1
}

# The node keeps images the registry no longer lists, and an A arm is taken
# from that cache by rule: pullPolicy is IfNotPresent, so a cached tag starts.
# Checked on the node itself, not in the registry (#1875).
NODE_SSH="${YARILO_NODE_SSH:-ssh -J ncjump -o BatchMode=yes -o ConnectTimeout=15 root@10.50.80.24}"
# node_reachable says whether the node can be asked at all. From a laptop with
# the jump host open it can; from the runner, which has no such access, it
# cannot -- and "cannot ask" must not read as "the image is not there".
node_reachable() {
  $NODE_SSH "true" >/dev/null 2>&1
}

cached_on_node() {
  # Anchored on both ends of the reference: ctr prints one full reference per
  # line, and an unanchored match answers "yes" for dev.676 when asked about
  # dev.67 -- a check that accepts a neighbour reports another arm's number.
  local pat
  pat=$(printf '%s' "${IMAGE_REPO}:$1" | sed 's/[.]/\\./g')
  $NODE_SSH "microk8s.ctr images ls -q 2>/dev/null | grep -qE '(^|/)${pat}\$'" >/dev/null 2>&1
}

tag_exists "$TAG" && rc=0 || rc=$?
if [ "$rc" != 0 ]; then
  if ! node_reachable; then
    # The rollout is the authoritative check: an image the node does not have
    # leaves the pods in ImagePullBackOff, and the wait below fails on it.
    echo "-- tag $TAG is not in the registry and the node cannot be asked from here; the rollout decides" |
      tee "$OUT/tag-$ARM-unverified.txt"
  elif cached_on_node "$TAG"; then
    echo "-- tag $TAG is not in the registry and is cached on the node; the arm runs from the cache" |
      tee "$OUT/tag-$ARM-from-node-cache.txt"
  else
    echo "ab-arm: no image for tag $TAG, in the registry or on the node; the last built tag is $(last_built_tag)" >&2
    exit 1
  fi
fi
# Lock acquisitions by resource class, summed over the backends: the number that
# says which lock a change moved, which throughput alone cannot (#1884).
# A pod that has taken no lock yet has no such metric, and the grep that finds
# none exits 1 -- which under set -e ended the arm before its first run.
# Nothing acquired is an answer, and an empty file is how it is written.
lock_classes() {
  local pod page
  for pod in $(kube get pods -l app.kubernetes.io/component=backend -o name | cut -d/ -f2); do
    # The page first, and it is never empty -- runtime series are always there.
    # An empty one means the endpoint is unreadable, which is not the same
    # answer as "this pod has taken no lock yet", and must not be written as
    # one: only the grep for the lock series may come back with nothing.
    page=$(kube exec "$pod" -c yarilo-imap -- sh -c \
      'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null' 2>/dev/null)
    if [ -z "$page" ]; then
      echo "ab-arm: $pod does not answer on /metrics; lock classes cannot be counted" >&2
      return 1
    fi
    printf '%s\n' "$page" | grep "^yarilo_locks_acquire_wait_seconds_count{" || true
  done | awk -F'"' '{split($0,f," "); sum[$2]+=f[length(f)]} END{for (c in sum) printf "%s %d\n", c, sum[c]}' | sort
}



# The profiling overlay, for an arm that measures waiting rather than
# throughput. Off unless the arm asks: accounting for every blocking operation
# changes the pod being measured (#1875).
BLOCKPROFILE="${YARILO_ARM_BLOCKPROFILE:-0}"
OVERLAY="$REPO/helm_values/values-sandbox-blockprofile.yaml"
overlay_args=()
if [ "$BLOCKPROFILE" = "1" ]; then
  [ -f "$OVERLAY" ] || { echo "ab-arm: $OVERLAY is missing" >&2; exit 1; }
  # Only the profiling keys live there. A file that has grown a second purpose
  # is a stand running on something nobody reviewed.
  # Both levels the header promises: telemetry at the top, pprof under it. A
  # guard that reads only the first level lets telemetry.metrics_enabled in,
  # and the file then says one thing while the arm does another.
  stray=$(awk '
    /^[[:space:]]*#/ || /^[[:space:]]*$/ { next }
    /^[^[:space:]]/ { key=$1; sub(":.*","",key); top=key; if (key != "telemetry") print key; next }
    /^[[:space:]][[:space:]][^[:space:]]/ {
      key=$1; sub(":.*","",key)
      if (top == "telemetry" && key != "pprof") print "telemetry." key
    }
  ' "$OVERLAY")
  if [ -n "$stray" ]; then
    echo "ab-arm: $OVERLAY carries keys outside telemetry.pprof.*: $stray" >&2
    exit 1
  fi
  overlay_args=(-f "$OVERLAY")
  echo "-- arm $ARM runs with the profiling overlay: this is a latency arm" | tee "$OUT/overlay-$ARM.txt"
fi

# FILL is how many messages each user starts with. An empty mailbox is a state
# no deployment is in, and on this stand it is worth about four times the
# throughput of a filled one -- so a window on empty mailboxes measures
# something nobody runs (#1875). Set 0 for the empty mode, which is still the
# right start for a question about the login path alone.
FILL="${YARILO_ARM_FILL:-200}"

# dict_ops prints the dict service's operation counters, one per line. Read
# either side of a run, the difference is what that run asked of the service.
dict_ops() {
  local pod page deadline
  # A blink of the network reads exactly like a silent pod, and it costs the
  # arm its fill -- half an hour of deliveries. So the read is retried to a
  # deadline, and the two causes are told apart before giving up.
  deadline=$(( $(date +%s) + 60 ))
  while :; do
    pod=$(first_pod dict)
    if [ -n "$pod" ]; then
      page=$(kube exec "$pod" -- sh -c 'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null' 2>/dev/null)
      [ -n "$page" ] && break
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      if kube get nodes >/dev/null 2>&1; then
        echo "ab-arm: ${pod:-the dict pod} does not answer on /metrics; the dict counters cannot be read" >&2
      else
        echo "ab-arm: the cluster API is unreachable; the dict counters cannot be read" >&2
      fi
      return 1
    fi
    sleep 5
  done
  # Only the absence of these series is an answer -- a dict nobody asked
  # anything of. The page itself is never empty.
  printf '%s\n' "$page" | grep "^yarilo_dict_operations_total{" || true
}

# dict_delta writes what one run cost the dict service, per dict and verb.
dict_delta() {
  local before="$1" after="$2" out="$3"
  awk '
    FNR==NR { was[$1]=$2; next }
    { d = $2 - (($1 in was) ? was[$1] : 0); if (d != 0) printf "%s %d\n", $1, d }
  ' "$before" "$after" | sort > "$out"
}

# block_profile captures both backends while the run is under way: waiting is
# not a CPU sample, and a profile taken after the load measures silence. Which
# pod carried the load is only visible afterwards, from the sample seconds in
# the file name.
# The file name carries the window asked for, not the sample seconds -- those
# are inside the profile, and naming them here would be a promise this script
# cannot keep without reading the file back.
block_profile() {
  local label="$1" secs="${2:-40}" pod port=18080 pid pids=() files=() rc=0
  local pods
  pods=$(kube get pods -l app.kubernetes.io/component=backend -o name | cut -d/ -f2)
  [ -n "$pods" ] || { echo "ab-arm: no backend pod to profile" >&2; return 1; }
  for pod in $pods; do
    kube port-forward "pod/$pod" "$port:8080" >/dev/null 2>&1 &
    pids+=($!)
    files+=("$OUT/block-$label-$pod-req${secs}s.pprof")
    port=$((port + 1))
  done
  # Ready, not slept for: a fixed pause captured one pod and missed the other,
  # and a forward that is not up yet writes no profile at all.
  port=18080
  for pod in $pods; do
    local deadline=$(( $(date +%s) + 30 ))
    until curl -fsS -o /dev/null "http://127.0.0.1:$port/healthz" 2>/dev/null; do
      if [ "$(date +%s)" -ge "$deadline" ]; then
        echo "ab-arm: the forward to $pod on $port never came up" >&2
        for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
        return 1
      fi
      sleep 1
    done
    port=$((port + 1))
  done

  port=18080
  local curls=()
  for pod in $pods; do
    curl -fsS -o "$OUT/block-$label-$pod-req${secs}s.pprof" \
      "http://127.0.0.1:$port/debug/pprof/block?seconds=$secs" &
    curls+=($!)
    port=$((port + 1))
  done
  # Only the captures: a bare wait also waits for the port-forwards, which
  # never exit, and the arm stops there for ever (seen: 80 minutes).
  for pid in "${curls[@]}"; do wait "$pid" || rc=1; done
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  # An empty file is a capture that did not happen, and it reads exactly like
  # a pod where nobody waited.
  local f
  for f in "${files[@]}"; do
    if [ ! -s "$f" ]; then
      echo "ab-arm: the block profile for $(basename "$f") is empty; that pod was not profiled" >&2
      rc=1
    fi
  done
  return "$rc"
}

step "deploy"
echo "== arm $ARM: $TAG"
helm --kubeconfig="$KCFG" upgrade yarilo "$REPO/helm" -n "$NS" \
  -f "$REPO/helm_values/values-sandbox.yaml" "${overlay_args[@]}" --set image.tag="$TAG" --timeout 10m >/dev/null

deadline=$(( $(date +%s) + 600 ))
until [ "$(kube get pods --no-headers | grep -cv 'Running\|Completed')" = "0" ]; do
  [ "$(date +%s)" -lt "$deadline" ] || { echo "ab-arm: pods did not settle" >&2; exit 1; }
  sleep 10
done

# What actually runs, read from the pods: a rollout that quietly left the old
# pod in place, or a tag that resolved elsewhere, is otherwise invisible -- and
# on a path where the image could not be verified beforehand, this is the
# proof (#1875).
running_images() {
  kube get pods -l app.kubernetes.io/component=backend -o \
    jsonpath='{range .items[*]}{.metadata.name}{"\t"}{range .status.containerStatuses[?(@.name=="yarilo-imap")]}{.image}{"\t"}{.imageID}{end}{"\n"}{end}' 2>/dev/null
}
running_images > "$OUT/image-$ARM.txt"
echo "-- running image: $(head -1 "$OUT/image-$ARM.txt" | awk '{print $2}')"
# Every backend, not any: a rollout that left one pod behind is the case this
# exists for, and half a window is not a window.
if ! [ -s "$OUT/image-$ARM.txt" ]; then
  echo "ab-arm: no backend reported a running image" >&2
  exit 1
fi
wrong=$(grep -v ":$TAG[[:space:]]" "$OUT/image-$ARM.txt" | grep -v ":$TAG$" || true)
if [ -n "$wrong" ]; then
  echo "ab-arm: not every backend runs $TAG:" >&2
  printf '%s\n' "$wrong" >&2
  exit 1
fi

step "wipe"
echo "-- same start: emptying u1-u150"
for pod in $(kube get pods -l app.kubernetes.io/component=backend -o name | cut -d/ -f2); do
  kube exec "$pod" -c yarilo-imap -- sh -c \
    'for n in $(seq 1 150); do rm -rf "/var/mail/vhosts/d00001.test/u${n}@d00001.test"; done' >/dev/null
done

# The start as a number, not as a step that ran: a mailbox left behind moves
# throughput further than anything under test, and an arm that starts bigger
# than the one before it is not a comparison (#1875).
start_inventory() {
  local pod
  pod=$(first_pod backend)
  kube exec "$pod" -c yarilo-imap -- sh -c '
    f=0; k=0
    for n in $(seq 1 150); do
      d="/var/mail/vhosts/d00001.test/u${n}@d00001.test"
      [ -d "$d" ] || continue
      f=$((f+$(find "$d" -type f 2>/dev/null | wc -l)))
      k=$((k+$(du -sk "$d" 2>/dev/null | cut -f1)))
    done
    echo "files=$f du_kb=$k"' 2>/dev/null
}

# probe_inventory counts the one mailbox the seed itself fills.
probe_inventory() {
  local pod
  pod=$(first_pod backend)
  kube exec "$pod" -c yarilo-imap -- sh -c '
    d="/var/mail/vhosts/d00001.test/over@d00001.test"
    echo "files=$(find "$d" -type f 2>/dev/null | wc -l) du_kb=$(du -sk "$d" 2>/dev/null | cut -f1)"' 2>/dev/null
}

left=$(start_inventory)
echo "-- start after wipe: ${left:-unreadable}" | tee "$OUT/start-$ARM-wiped.txt"
case "$left" in
  "files=0 du_kb=0") ;;
  "") echo "ab-arm: could not read the start inventory" >&2; exit 1 ;;
  *) echo "ab-arm: the wipe left $left behind; this arm would start from more than the last one" >&2; exit 1 ;;
esac

# The seed delivers over LMTP, so it needs the pods serving, not merely Running.
for attempt in 1 2 3 4 5; do
  if KUBECONFIG="$KCFG" bash "$REPO/hack/db/seed-sandbox.sh" >/dev/null 2>&1; then break; fi
  [ "$attempt" -lt 5 ] || { echo "ab-arm: the seed did not take" >&2; exit 1; }
  sleep 20
done

if [ "$FILL" != "0" ]; then
  step "fill"
  echo "-- filling u1-u150 with $FILL messages each"
  for range in "1 50" "51 100" "101 150"; do
    set -- $range
    KUBECONFIG="$KCFG" YARILO_NS="$NS" bash "$REPO/hack/stand/fill-mailboxes.sh" "$1" "$2" "$FILL" |
      tee -a "$OUT/fill-$ARM.txt"
  done
fi

seeded=$(start_inventory)
echo "-- start after seed: ${seeded:-unreadable}" | tee "$OUT/start-$ARM-seeded.txt"
# u1-150 are empty at the start by design: the seed puts them in the database
# and imaptest fills them during the run. What the seed does deliver is the
# over-quota probe, so that is what proves the inventory reads a real volume
# rather than answering zero from the wrong path.
probe=$(probe_inventory)
echo "-- seeded probe mailbox: ${probe:-unreadable}" | tee "$OUT/start-$ARM-probe.txt"
case "$probe" in
  files=0\ *|"") echo "ab-arm: the inventory does not see the mailbox the seed delivered (${probe:-unreadable}); it is reading the wrong place" >&2; exit 1 ;;
esac

step "auth histogram"
authpod=$(first_pod auth)
[ -n "$authpod" ] || { echo "ab-arm: no auth pod to read the histogram from" >&2; exit 1; }
kube exec "$authpod" -- sh -c \
  'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null | grep -E "^yarilo_auth_request_seconds_(bucket|count|sum)\{.*verb=\"AUTH\""' \
  > "$OUT/auth-$ARM-before.txt" || true

for pair in "mdbox 1-20" "maildir 51-70" "sdbox 101-120"; do
  set -- $pair
  name=$1; range=$2
  step "run $name"
  # Checked, not assumed: if the literal in job.yaml ever moves, an unchecked
  # sed runs mdbox three times and reports three types.
  manifest="$OUT/job-$ARM-$name.yaml"
  sed "s/- users=1-20/- users=$range/" "$REPO/hack/imaptest/job.yaml" > "$manifest"
  if ! grep -q -- "- users=$range" "$manifest"; then
    echo "ab-arm: the user range did not substitute; hack/imaptest/job.yaml no longer carries 'users=1-20'" >&2
    exit 1
  fi
  # The watcher runs beside the job: a stall captured after the run is one
  # nobody can explain (#1881).
  KUBECONFIG="$KCFG" YARILO_NS="$NS" bash "$REPO/hack/stand/watch-stalls.sh" "$OUT" "$ARM-$name" &
  watcher=$!
  # CPU beside the run: waiting removed does not move a number bounded by work,
  # and the ceiling has to be in the window rather than in someone's memory.
  KUBECONFIG="$KCFG" YARILO_NS="$NS" bash "$REPO/hack/stand/watch-cpu.sh" "$OUT" "$ARM-$name" 10 &
  cpuwatch=$!
  lock_classes > "$OUT/locks-$ARM-$name-before.txt"
  if [ "$BLOCKPROFILE" = "1" ]; then
    dict_ops > "$OUT/dict-$ARM-$name-before.txt" || exit 1
    ( sleep 20; block_profile "$ARM-$name" 40 ) &
    capture=$!
  fi
  KUBECONFIG="$KCFG" YARILO_NS="$NS" bash "$REPO/hack/stand/run-job.sh" imaptest "$manifest" "$OUT/ab-$ARM-$name.log" 900
  lock_classes > "$OUT/locks-$ARM-$name-after.txt"
  if [ "$BLOCKPROFILE" = "1" ]; then
    if ! wait "$capture"; then
      echo "ab-arm: the block capture failed for $name; this arm has no latency number" >&2
      exit 1
    fi
    dict_ops > "$OUT/dict-$ARM-$name-after.txt" || exit 1
    dict_delta "$OUT/dict-$ARM-$name-before.txt" "$OUT/dict-$ARM-$name-after.txt" "$OUT/dict-$ARM-$name-delta.txt"
  fi
  wait "$watcher" 2>/dev/null || true
  kill "$cpuwatch" 2>/dev/null || true
  # The peak each side reached, so the reading does not need the whole file.
  # Per container against its own limit: imap is the one under load, and its
  # limit is one CPU whatever the pod totals say.
  # The limit comes from the file's own header, not from a line in this script:
  # the stand's limit has already changed once under a hardcoded "1000m".
  peak=$(awk '/^yarilo-backend/ && /yarilo-imap=/ {
                for (i = 1; i <= NF; i++) if ($i ~ /^yarilo-imap=/) { split($i, kv, "/"); lim = kv[2] }
              }
              /^[0-9][0-9]:/ {
                cpu=$4; sub("m","",cpu); cpu+=0
                if ($3 == "yarilo-imap" && cpu > i) i=cpu
                if ($2 ~ /imaptest/ && cpu > c) c=cpu
                if ($1 != last) { last=$1; delete pod }
                if ($2 ~ /backend/) { pod[$1"/"$2]+=cpu; if (pod[$1"/"$2] > p) p=pod[$1"/"$2] }
              }
              /^[0-9][0-9]:/ && $2 == "NODE" { n=$5; sub("%","",n); if (n+0 > nodepct) nodepct=n+0 }
              END { if (lim == "") lim = "?"
                    else if (lim !~ /m$/) lim = (lim * 1000) "m"   # "2" is two cores
                    printf "imap_peak=%dm (limit %s) backend_pod_peak=%dm imaptest_peak=%dm node_peak=%d%%", i, lim, p, c, nodepct }' \
        "$OUT/cpu-$ARM-$name.txt" 2>/dev/null)
  echo "$ARM $name cpu: ${peak:-unreadable}"
  logins=$(grep -A 3 '^Logi' "$OUT/ab-$ARM-$name.log" | tail -1 | awk '{print $1}')
  stalls=$(grep -c 'stalled for' "$OUT/ab-$ARM-$name.log" || true)
  echo "$ARM $name logins=${logins:-?} stalls=$stalls"
done

kube exec "$authpod" -- sh -c \
  'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null | grep -E "^yarilo_auth_request_seconds_(bucket|count|sum)\{.*verb=\"AUTH\""' \
  > "$OUT/auth-$ARM-after.txt"
echo "== arm $ARM done"
