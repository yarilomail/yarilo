# The stand's one rollout step, sourced by every script that deploys a tag.
#
#   stand_deploy <out-dir> <tag> <chart-dir> [helm args...]
#
# 1. A release left pending by a cut-off helm stops the step: clearing it is a
#    manual act, recorded in the run's row.
# 2. The images the release renders are pulled onto every node that runs its
#    pods, by a pod per node and image asking no CPU, before the upgrade: the
#    node never has room for an old and a new backend pod together, and a pull
#    the new pods waited on held a backend down for minutes (2026-11-19).
# 3. How many seconds each backend pod was not Ready during the upgrade goes to
#    <out-dir>/backend-downtime.txt, one "<pod> <seconds>" line each, and how
#    long the new pod waited to be scheduled and to be Ready to
#    <out-dir>/backend-pending.txt: a StatefulSet stops a pod before it starts
#    its successor, so part of the gap is the restart, part a wait for CPU.
#
# Expects NS and a kube() function (kubectl in the namespace); helm takes KCFG,
# else KUBECONFIG.

stand_deploy() {
  local out="$1" tag="$2" chart="$3"
  shift 3
  local status
  status=$(helm --kubeconfig="${KCFG:-$KUBECONFIG}" history yarilo -n "$NS" --max 1 -o json 2>/dev/null | sed -n 's/.*"status":"\([a-z-]*\)".*/\1/p')
  case "$status" in
    pending-*)
      echo "stand_deploy: release yarilo is $status; a helm run was cut off. Clear it by hand and record it in the run's row." >&2
      return 1 ;;
  esac
  stand_prepull "$out" "$chart" "$@" --set image.tag="$tag" || return 1
  rm -f "$out/backend-downtime.txt"
  stand_watch_backend "$out/backend-downtime.txt" &
  local watcher=$! rc=0 i
  # Its first write comes after its trap: an upgrade that ends before then
  # would stop a watcher that leaves nothing behind.
  for i in $(seq 1 100); do [ -s "$out/backend-downtime.txt" ] && break; sleep 0.1; done
  helm --kubeconfig="${KCFG:-$KUBECONFIG}" upgrade yarilo "$chart" -n "$NS" "$@" --set image.tag="$tag" --wait --timeout 10m > "$out/deploy.txt" 2>&1 || rc=1
  kube rollout status sts/yarilo-backend --timeout=280s >> "$out/deploy.txt" 2>&1 || rc=1
  kill "$watcher" 2>/dev/null
  wait "$watcher" 2>/dev/null
  stand_backend_pending > "$out/backend-pending.txt"
  echo "-- backend pods, seconds pending and to Ready: $(tr '\n' ' ' < "$out/backend-pending.txt")"
  if [ -s "$out/backend-downtime.txt" ]; then
    echo "-- backend not Ready during the rollout: $(tr '\n' ' ' < "$out/backend-downtime.txt")"
  else
    echo "-- backend not Ready during the rollout: unmeasured, the watcher wrote nothing" >&2
    rc=1
  fi
  return "$rc"
}

# stand_prepull pulls every image the release renders onto every node its pods
# run on, and removes the pods that did it.
stand_prepull() {
  local out="$1" chart="$2"
  shift 2
  local images nodes img node name i=0 rc=0 deadline phase why
  images=$(helm --kubeconfig="${KCFG:-$KUBECONFIG}" template yarilo "$chart" -n "$NS" "$@" 2>/dev/null | sed -n 's/^ *image: *"\{0,1\}\([^" ]*\)"\{0,1\}$/\1/p' | sort -u)
  nodes=$(kube get pods -l app.kubernetes.io/instance=yarilo -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | sort -u | grep .)
  [ -n "$images" ] || { echo "stand_prepull: the chart renders no image" >&2; return 1; }
  [ -n "$nodes" ] || { echo "stand_prepull: no node runs a yarilo pod" >&2; return 1; }
  : > "$out/prepull.txt"
  for node in $nodes; do
    for img in $images; do
      i=$((i + 1))
      name="yarilo-prepull-$i"
      kube delete pod "$name" --ignore-not-found --wait=true >/dev/null 2>&1
      kube apply -f - >/dev/null <<EOF || { rc=1; continue; }
apiVersion: v1
kind: Pod
metadata:
  name: $name
  labels: {app.kubernetes.io/name: yarilo-prepull}
spec:
  nodeName: $node
  restartPolicy: Never
  containers:
    - name: pull
      image: $img
      imagePullPolicy: IfNotPresent
      command: ["/bin/true"]
      resources: {requests: {cpu: "0", memory: "0"}}
EOF
      local start
      start=$(date +%s)
      deadline=$((start + 900))
      while :; do
        phase=$(kube get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null)
        why=$(kube get pod "$name" -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null)
        case "$phase:$why" in
          Succeeded:*|Failed:*) break ;;
          *:ErrImagePull|*:ImagePullBackOff|*:InvalidImageName)
            echo "stand_prepull: $img on $node: $why" >&2; rc=1; break ;;
        esac
        [ "$(date +%s)" -lt "$deadline" ] || { echo "stand_prepull: $img on $node not pulled in 15 min" >&2; rc=1; break; }
        sleep 2
      done
      echo "$node $img $phase $(( $(date +%s) - start ))s" >> "$out/prepull.txt"
      kube delete pod "$name" --ignore-not-found --wait=false >/dev/null 2>&1
    done
  done
  echo "-- prepulled: $(awk '{print $1, $4}' "$out/prepull.txt" | tr '\n' ' ')"
  return "$rc"
}

# stand_watch_backend counts, once a second until killed, the seconds each
# backend pod is not Ready; a pod that does not exist counts as not Ready.
# Plain arrays only: bash 3.2, a laptop's, has no associative ones.
stand_watch_backend() {
  local file="$1" replicas n ready prev now
  local -a pods=() down=()
  replicas=$(kube get sts yarilo-backend -o jsonpath='{.spec.replicas}' 2>/dev/null)
  for n in $(seq 0 $(( ${replicas:-1} - 1 ))); do pods[n]="yarilo-backend-$n"; down[n]=0; done
  write() {
    : > "$file"
    for n in "${!pods[@]}"; do echo "${pods[n]} ${down[n]}" >> "$file"; done
  }
  trap 'write; exit 0' TERM
  write
  prev=$(date +%s)
  while :; do
    now=$(date +%s)
    for n in "${!pods[@]}"; do
      ready=$(kube get pod "${pods[n]}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
      # The time since the last look, not one per look: a look takes a while.
      [ "$ready" = "True" ] || down[n]=$(( ${down[n]} + now - prev ))
    done
    prev=$now
    sleep 1 &
    wait $!
  done
}

# stand_backend_pending prints "<pod> pending=<s> ready=<s>" for each backend
# pod: from its creation to being scheduled, and to being Ready.
stand_backend_pending() {
  kube get pods -l app.kubernetes.io/component=backend -o json | python3 -c '
import datetime, json, sys
t = lambda s: datetime.datetime.strptime(s, "%Y-%m-%dT%H:%M:%SZ")
for p in json.load(sys.stdin)["items"]:
    c = {k["type"]: k.get("lastTransitionTime") for k in p["status"].get("conditions", [])}
    made = t(p["metadata"]["creationTimestamp"])
    out = [p["metadata"]["name"]]
    for name, cond in (("pending", "PodScheduled"), ("ready", "Ready")):
        out.append("%s=%s" % (name, int((t(c[cond]) - made).total_seconds()) if c.get(cond) else "-"))
    print(" ".join(out))'
}
