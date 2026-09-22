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

# A dirty file stops a checkout, the runner stays on the commit before it, and
# the window then measures tooling nobody asked for (#1875).
step "checkout"
dirty=$(git -C "$REPO" status --porcelain 2>/dev/null)
head=$(git -C "$REPO" rev-parse HEAD 2>/dev/null)
if [ -n "$dirty" ]; then
  echo "ab-arm: the checkout at $REPO is not clean, so the arm would run tooling nobody reviewed:" >&2
  printf '%s\n' "$dirty" >&2
  exit 1
fi
if [ -n "${YARILO_ARM_COMMIT:-}" ] && [ "$head" != "$YARILO_ARM_COMMIT" ]; then
  echo "ab-arm: the checkout is at $head, and the window was asked for $YARILO_ARM_COMMIT" >&2
  exit 1
fi
echo "-- tooling: $(git -C "$REPO" log --oneline -1 2>/dev/null)"

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
# OVERLAY_NAME picks a second values file, and the arm records which one it ran
# with: an arm whose config nobody can name is not a comparison (#1875).
OVERLAY_NAME="${YARILO_ARM_OVERLAY:-}"
[ "$BLOCKPROFILE" = "1" ] && OVERLAY_NAME="blockprofile"
overlay_args=()

# The keys each overlay is allowed to carry, as an extended regex over the
# "top.second" path. Anything else and the arm stops.
overlay_allows() {
  case "$1" in
    blockprofile) echo '^telemetry(\.pprof)?$' ;;
    fsync-never) echo '^storage(\.mail_fsync)?$' ;;
    *) return 1 ;;
  esac
}

if [ -n "$OVERLAY_NAME" ]; then
  allow=$(overlay_allows "$OVERLAY_NAME") || {
    echo "ab-arm: no overlay is named $OVERLAY_NAME" >&2
    exit 1
  }
  OVERLAY="$REPO/helm_values/values-sandbox-$OVERLAY_NAME.yaml"
  [ -f "$OVERLAY" ] || { echo "ab-arm: $OVERLAY is missing" >&2; exit 1; }
  # Both levels the header promises: a guard that reads only the first lets a
  # sibling key in, and the file then says one thing while the arm does another.
  stray=$(awk '
    /^[[:space:]]*#/ || /^[[:space:]]*$/ { next }
    /^[^[:space:]]/ { key=$1; sub(":.*","",key); top=key; print key; next }
    /^[[:space:]][[:space:]][^[:space:]]/ { key=$1; sub(":.*","",key); print top "." key }
  ' "$OVERLAY" | grep -Ev "$allow" || true)
  if [ -n "$stray" ]; then
    echo "ab-arm: $OVERLAY carries keys the $OVERLAY_NAME overlay may not: $stray" >&2
    exit 1
  fi
  overlay_args=(-f "$OVERLAY")
  echo "-- arm $ARM runs with the $OVERLAY_NAME overlay" | tee "$OUT/overlay-$ARM.txt"
fi

# FILL is how many messages each user starts with. An empty mailbox is a state
# no deployment is in, and on this stand it is worth about four times the
# throughput of a filled one -- so a window on empty mailboxes measures
# something nobody runs (#1875). Set 0 for the empty mode, which is still the
# right start for a question about the login path alone.
FILL="${YARILO_ARM_FILL:-200}"

# KEEP_STORE carries the previous arm's mailboxes in, for the question a wiped
# arm cannot ask: the first listing over older state (#1714).
KEEP_STORE="${YARILO_ARM_KEEP_STORE:-0}"

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

# backend_counters prints the per-run counters of the imap container, summed
# over the backends: with two pods serving one load, a per-pod number answers
# only which pod the director picked (#1875).
backend_counters() {
  local pods pod page deadline total=""
  deadline=$(( $(date +%s) + 60 ))
  while :; do
    pods=$(kube get pods -l app.kubernetes.io/component=backend -o name 2>/dev/null | cut -d/ -f2)
    if [ -n "$pods" ]; then
      total=""
      for pod in $pods; do
        page=$(kube exec "$pod" -c yarilo-imap -- sh -c 'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null' 2>/dev/null)
        # One silent backend makes the sum a different question, so the whole
        # read is retried rather than answered from the pods that did reply.
        [ -n "$page" ] || { total=""; break; }
        total+=$(printf '%s\n' "$page" |
          grep -E "^(imap_maildir_sync_total\{|imap_maildir_sync_seconds_(count|sum)\{|maildir_partial_pass_empty_total|quota_folders_opened_total|quota_usage_count_total\{|index_cache_record_crc_mismatch_total|mailbox_message_opened_total\{|fileindex_journal_write_failed_total\{|mailbox_write_failed_total\{)" || true)
        total+=$'\n'
      done
      [ -n "${total//[$'\n']/}" ] && break
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      if kube get nodes >/dev/null 2>&1; then
        echo "ab-arm: a backend does not answer on /metrics; the reconcile counters cannot be read" >&2
      else
        echo "ab-arm: the cluster API is unreachable; the reconcile counters cannot be read" >&2
      fi
      return 1
    fi
    sleep 5
  done
  printf '%s\n' "$total" | awk 'NF == 2 { sum[$1] += $2 } END { for (k in sum) printf "%s %d\n", k, sum[k] }' | sort
}

# dict_delta writes what one run cost the dict service, per dict and verb. The
# shape is a counter page either side, so the reconcile counters share it.
dict_delta() {
  local before="$1" after="$2" out="$3"
  awk '
    FNR==NR { was[$1]=$2; next }
    {
      d = $2 - (($1 in was) ? was[$1] : 0)
      if (d == 0) next
      # Seconds are fractions: printing them as integers reports a walk that
      # took 0.4s as zero, and the average is then a division by a lie.
      if ($1 ~ /_seconds_sum/) printf "%s %.6f\n", $1, d
      else printf "%s %d\n", $1, d
    }
  ' "$before" "$after" | sort > "$out"
}

# block_profile captures both backends while the run is under way: waiting is
# not a CPU sample, and a profile taken after the load measures silence. Which
# pod carried the load is only visible afterwards, from the sample seconds in
# the file name.
# The file name carries the window asked for, not the sample seconds -- those
# are inside the profile, and naming them here would be a promise this script
# cannot keep without reading the file back.
# A forward that never came up is the flake this retries: status 2 is that
# and only that, so a genuinely unprofilable pod still stops the arm.
retry_capture() {
  local kind="$1" label="$2" secs="$3" rc
  pprof_capture "$kind" "$label" "$secs" && return 0
  rc=$?
  [ "$rc" = 2 ] || return "$rc"
  echo "ab-arm: the $kind forward did not come up; taking it once more" >&2
  pprof_capture "$kind" "$label" "$secs"
}

block_profile() { retry_capture block "$1" "${2:-40}"; }

# cpu_profile is the same capture against the CPU endpoint, which needs no
# overlay: pprof is on in the sandbox values with the block rate at zero.
cpu_profile() { retry_capture profile "$1" "${2:-40}"; }

pprof_capture() {
  local kind="$1" label="$2" secs="${3:-40}" pod port=18080 pid pids=() files=() rc=0
  local pods
  pods=$(kube get pods -l app.kubernetes.io/component=backend -o name | cut -d/ -f2)
  [ -n "$pods" ] || { echo "ab-arm: no backend pod to profile" >&2; return 1; }
  for pod in $pods; do
    kube port-forward "pod/$pod" "$port:8080" >/dev/null 2>&1 &
    pids+=($!)
    files+=("$OUT/$kind-$label-$pod-req${secs}s.pprof")
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
        for pid in ${pids[@]+"${pids[@]}"}; do kill "$pid" 2>/dev/null || true; done
        return 2
      fi
      sleep 1
    done
    port=$((port + 1))
  done

  port=18080
  local curls=()
  for pod in $pods; do
    curl -fsS -o "$OUT/$kind-$label-$pod-req${secs}s.pprof" \
      "http://127.0.0.1:$port/debug/pprof/$kind?seconds=$secs" &
    curls+=($!)
    port=$((port + 1))
  done
  # Only the captures: a bare wait also waits for the port-forwards, which
  # never exit, and the arm stops there for ever (seen: 80 minutes).
  # Empty arrays are "unbound" under set -u on bash 3.2, which is what macOS
  # ships: a window with no pod to profile must say so, not die here.
  for pid in ${curls[@]+"${curls[@]}"}; do wait "$pid" || rc=1; done
  for pid in ${pids[@]+"${pids[@]}"}; do kill "$pid" 2>/dev/null || true; done
  # An empty file is a capture that did not happen, and it reads exactly like
  # a pod where nobody waited.
  local f
  for f in ${files[@]+"${files[@]}"}; do
    if [ ! -s "$f" ]; then
      echo "ab-arm: the $kind profile for $(basename "$f") is empty; that pod was not profiled" >&2
      rc=1
    fi
  done
  return "$rc"
}

step "deploy"
echo "== arm $ARM: $TAG"
helm --kubeconfig="$KCFG" upgrade yarilo "$REPO/helm" -n "$NS" \
  -f "$REPO/helm_values/values-sandbox.yaml" ${overlay_args[@]+"${overlay_args[@]}"} \
  --set image.tag="$TAG" --timeout 10m >/dev/null

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

# One domain per storage type (#1943): a single domain puts the whole stand on
# one backend under assignment_policy: domain, which is the policy working and
# a window measuring half a cluster. The ranges are the accounts the sandbox
# holds for each.
MDBOX_DOMAIN="${YARILO_MDBOX_DOMAIN:-d00001.test}"
MAILDIR_DOMAIN="${YARILO_MAILDIR_DOMAIN:-d00002.test}"
SDBOX_DOMAIN="${YARILO_SDBOX_DOMAIN:-d00003.test}"

# message_inventory is the start as the INDEX sees it: a cache rewritten in
# place moves the file count while the mailbox is unchanged (#1714).
message_inventory() {
  local pod total=0 n out
  pod=$(first_pod backend-api)
  [ -n "$pod" ] || pod=$(first_pod backend)
  for t in mdbox maildir sdbox; do
    set -- $(type_domain "$t")
    for n in $(seq "$2" "$3"); do
      out=$(kube exec "$pod" -c yarilo-backend-api -- yarctl -O json backend quota show "u${n}@$1" 2>/dev/null |
        awk -F'[:,]' '/"message_value"/ { gsub(/[^0-9-]/, "", $2); print $2 + 0; exit }')
      total=$((total + ${out:-0}))
    done
  done
  echo "messages=$total"
}

# type → domain and the range that type's accounts live in.
type_domain() {
  case "$1" in
    mdbox) echo "$MDBOX_DOMAIN 1 50" ;;
    maildir) echo "$MAILDIR_DOMAIN 51 100" ;;
    sdbox) echo "$SDBOX_DOMAIN 101 150" ;;
    *) return 1 ;;
  esac
}

# The start as a number, not as a step that ran: a mailbox left behind moves
# throughput further than anything under test, and an arm that starts bigger
# than the one before it is not a comparison (#1875).
start_inventory() {
  local pod f=0 k=0 out
  pod=$(first_pod backend)
  for t in mdbox maildir sdbox; do
    set -- $(type_domain "$t")
    out=$(kube exec "$pod" -c yarilo-imap -- sh -c "
      f=0; k=0
      for n in \$(seq $2 $3); do
        d=\"/var/mail/vhosts/$1/u\${n}@$1\"
        [ -d \"\$d\" ] || continue
        f=\$((f+\$(find \"\$d\" -type f 2>/dev/null | wc -l)))
        k=\$((k+\$(du -sk \"\$d\" 2>/dev/null | cut -f1)))
      done
      echo \"\$f \$k\"" 2>/dev/null)
    set -- $out
    f=$((f + ${1:-0}))
    k=$((k + ${2:-0}))
  done
  echo "files=$f du_kb=$k"
}
# probe_inventory counts the one mailbox the seed itself fills.
probe_inventory() {
  local pod
  pod=$(first_pod backend)
  kube exec "$pod" -c yarilo-imap -- sh -c '
    d="/var/mail/vhosts/'"$MDBOX_DOMAIN"'/over@'"$MDBOX_DOMAIN"'"
    echo "files=$(find "$d" -type f 2>/dev/null | wc -l) du_kb=$(du -sk "$d" 2>/dev/null | cut -f1)"' 2>/dev/null
}
if [ "$KEEP_STORE" = "1" ]; then
  step "carry"
  # Nothing is emptied, seeded or filled: a wipe would remove the very
  # question this arm is here to ask (#1714).
  carried=$(message_inventory)
  echo "-- start carried over: ${carried:-unreadable}" | tee "$OUT/start-$ARM-carried.txt"
  case "$carried" in
    "messages=0"|"") echo "ab-arm: nothing was carried over (${carried:-unreadable}); this arm has nothing to list over" >&2; exit 1 ;;
  esac
  # The arm before must have ended on this number, or the two arms did not
  # start from the same place and the comparison is not one.
  if [ -f "$OUT/messages-last-arm.txt" ]; then
    before=$(cat "$OUT/messages-last-arm.txt")
    [ "$before" = "$carried" ] || {
      echo "ab-arm: the arm before left $before and this one starts from $carried" >&2
      exit 1
    }
    echo "-- same start as the arm before: $carried"
  else
    echo "ab-arm: no arm recorded its messages, so a carried start cannot be proven" >&2
    exit 1
  fi
else

step "wipe"
echo "-- same start: emptying every type's accounts"
for pod in $(kube get pods -l app.kubernetes.io/component=backend -o name | cut -d/ -f2); do
  for t in mdbox maildir sdbox; do
    set -- $(type_domain "$t")
    kube exec "$pod" -c yarilo-imap -- sh -c \
      "for n in \$(seq $2 $3); do rm -rf \"/var/mail/vhosts/$1/u\${n}@$1\"; done" >/dev/null
  done
done



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
  echo "-- filling every type's accounts with $FILL messages each"
  for t in mdbox maildir sdbox; do
    set -- $(type_domain "$t")
    KUBECONFIG="$KCFG" YARILO_NS="$NS" YARILO_FILL_DOMAIN="$1" \
      bash "$REPO/hack/stand/fill-mailboxes.sh" "$2" "$3" "$FILL" |
      tee -a "$OUT/fill-$ARM.txt"
  done
fi
fi

if [ "$KEEP_STORE" = "1" ]; then
  seeded="$carried"
else
  seeded=$(start_inventory)
  echo "-- start after seed: ${seeded:-unreadable}" | tee "$OUT/start-$ARM-seeded.txt"
fi
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
  domain=$(type_domain "$name" | cut -d' ' -f1)
  step "run $name"
  # Checked, not assumed: if either literal in job.yaml ever moves, an
  # unchecked sed runs one type three times and reports three.
  manifest="$OUT/job-$ARM-$name.yaml"
  sed -e "s/- users=1-20/- users=$range/" \
      -e "s/- user=u%d@d00001.test/- user=u%d@$domain/" \
      "$REPO/hack/imaptest/job.yaml" > "$manifest"
  if ! grep -q -- "- users=$range" "$manifest" || ! grep -q -- "- user=u%d@$domain" "$manifest"; then
    echo "ab-arm: the range or the domain did not substitute; hack/imaptest/job.yaml no longer carries 'users=1-20' and 'user=u%d@d00001.test'" >&2
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
  # The reconcile curve beside the CPU one: a cold cache and a changed folder
  # are the same total, and only their shapes over time tell them apart (#1875).
  KUBECONFIG="$KCFG" YARILO_NS="$NS" bash "$REPO/hack/stand/watch-reconcile.sh" "$OUT" "$ARM-$name" 10 &
  recwatch=$!
  # Sessions are counted while they exist: read after the run they are all
  # zero, because every client has disconnected (#1943).
  KUBECONFIG="$KCFG" YARILO_NS="$NS" bash "$REPO/hack/stand/watch-sessions.sh" "$OUT" "$ARM-$name" 5 &
  sesswatch=$!
  lock_classes > "$OUT/locks-$ARM-$name-before.txt"
  # Taken on every run, not only under the profiling overlay: these are the
  # numbers a change to the open path is judged by (#1875).
  backend_counters > "$OUT/backend-$ARM-$name-before.txt" || exit 1
  # The CPU profile is taken on every arm: what a read spends on the checksum
  # is a share of this file, and a share needs no second arm to compare with.
  ( sleep 20; cpu_profile "$ARM-$name" 40 ) &
  cpucapture=$!
  if [ "$BLOCKPROFILE" = "1" ]; then
    dict_ops > "$OUT/dict-$ARM-$name-before.txt" || exit 1
    ( sleep 70; block_profile "$ARM-$name" 40 ) &
    capture=$!
  fi
  KUBECONFIG="$KCFG" YARILO_NS="$NS" bash "$REPO/hack/stand/run-job.sh" imaptest "$manifest" "$OUT/ab-$ARM-$name.log" 900
  lock_classes > "$OUT/locks-$ARM-$name-after.txt"
  backend_counters > "$OUT/backend-$ARM-$name-after.txt" || exit 1
  dict_delta "$OUT/backend-$ARM-$name-before.txt" "$OUT/backend-$ARM-$name-after.txt" \
    "$OUT/backend-$ARM-$name-delta.txt"
  # A CPU profile that did not land is an arm with no read cost in it, which
  # is the number this window is for.
  if ! wait "$cpucapture"; then
    echo "ab-arm: the CPU capture failed for $name; this arm cannot price a read" >&2
    exit 1
  fi
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
  kill "$recwatch" 2>/dev/null || true
  kill "$sesswatch" 2>/dev/null || true
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
  # The peak, not the last sample: a run ends with every client gone. A nought
  # is printed too -- it is not the same answer as an absent backend (#1931).
  spread=$(awk '/^[0-9]/ { if (!($2 in peak) || $3 > peak[$2]) peak[$2] = $3 }
                END { for (ip in peak) printf "%s=%d ", ip, peak[ip] }' \
    "$OUT/spread-$ARM-$name.txt" 2>/dev/null)
  echo "$ARM $name sessions (peak): ${spread:-unreadable}"
  # What drove the walks, and how the cold share fades: the number A2 is
  # decided by. Read from the delta, with the curve beside it in the file.
  echo "$ARM $name $(awk '
      # This series and each result exactly: the seconds histogram carries the
      # same label, and "scanned" is a prefix of two more (#1875, #1952).
      $1 !~ /^imap_maildir_sync_total\{/ { next }
      $1 ~ /result="scanned"/            { full += $2 }
      $1 ~ /result="scanned-partial"/    { partial += $2 }
      $1 ~ /result="scanned-untokened"/  { untokened += $2 }
      $1 ~ /reason="first-seen"/ && $1 ~ /result="scanned"/ { cold += $2 }
      END { walks = full + partial + untokened
            printf "walks: full=%d partial=%d untokened=%d first-seen=%d", full, partial, untokened, cold
            if (walks + 0 > 0) printf " partial_share=%.1f%%", 100 * partial / walks
            if (full + 0 > 0) printf " cold_share=%.1f%%", 100 * cold / full }
    ' "$OUT/backend-$ARM-$name-delta.txt")"
  # What each kind of walk cost, and how many arrivals passes had nothing to
  # move: the two numbers that price the trade the partial pass makes (#1952).
  echo "$ARM $name $(awk '
      $1 ~ /_seconds_sum\{/ && $1 ~ /result="scanned"/ { fullSum += $2 }
      $1 ~ /_seconds_count\{/ && $1 ~ /result="scanned"/ { fullN += $2 }
      $1 ~ /_seconds_sum\{/ && $1 ~ /result="scanned-partial"/ { partSum += $2 }
      $1 ~ /_seconds_count\{/ && $1 ~ /result="scanned-partial"/ { partN += $2 }
      $1 == "maildir_partial_pass_empty_total" { empty += $2 }
      END { printf "walk cost: full_ms=%s partial_ms=%s partial_empty=%d of %d",
              (fullN > 0 ? sprintf("%.3f", 1000 * fullSum / fullN) : "-"),
              (partN > 0 ? sprintf("%.3f", 1000 * partSum / partN) : "-"),
              empty, partN }
    ' "$OUT/backend-$ARM-$name-delta.txt")"
  # Per login, because that is the unit the arm already reports: a raw delta
  # says nothing without the load that produced it.
  echo "$ARM $name $(awk -v logins="${logins:-0}" '
      # Matched on the label, not on where it sits: a second label (reason,
      # #1875) sorts before result and an anchored pattern then reads zero.
      $1 ~ /^imap_maildir_sync_total\{/ && $1 ~ /result="scanned"/ { scanned += $2 }
      $1 ~ /^imap_maildir_sync_total\{/ && $1 ~ /result="scanned-partial"/ { partial += $2 }
      $1 ~ /^imap_maildir_sync_total\{/ && $1 ~ /result="skipped"/ { skipped += $2 }
      $1 == "quota_folders_opened_total" { opened += $2 }
      END { printf "reconcile: scanned=%d partial=%d skipped=%d folders_opened=%d",
              scanned, partial, skipped, opened
            if (logins + 0 > 0) printf " scanned_per_login=%.3f partial_per_login=%.3f folders_per_login=%.3f",
              scanned / logins, partial / logins, opened / logins }
    ' "$OUT/backend-$ARM-$name-delta.txt")"
  # Every counter this arm collects has a line, or it is a number nobody reads
  # (#1964). Zero is the expected reading for three of these four.
  echo "$ARM $name $(awk -v logins="${logins:-0}" '
      $1 == "index_cache_record_crc_mismatch_total" { crc += $2 }
      $1 ~ /^mailbox_message_opened_total\{/ { opened += $2 }
      $1 ~ /^fileindex_journal_write_failed_total\{/ { journal += $2 }
      $1 ~ /^mailbox_write_failed_total\{/ { writes += $2 }
      END { printf "cache: crc_mismatch=%d bodies_opened=%d journal_write_failed=%d store_write_failed=%d",
              crc, opened, journal, writes
            if (logins + 0 > 0) printf " opens_per_login=%.3f", opened / logins }
    ' "$OUT/backend-$ARM-$name-delta.txt")"
done

kube exec "$authpod" -- sh -c \
  'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null | grep -E "^yarilo_auth_request_seconds_(bucket|count|sum)\{.*verb=\"AUTH\""' \
  > "$OUT/auth-$ARM-after.txt"

# What the next arm must find if it carries this state over.
message_inventory > "$OUT/messages-last-arm.txt"
echo "-- this arm ends on $(cat "$OUT/messages-last-arm.txt")" | tee "$OUT/messages-$ARM-end.txt"
echo "== arm $ARM done"
