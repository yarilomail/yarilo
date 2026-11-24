#!/usr/bin/env bash
# pprof.sh: profile captures from every backend pod at once, for the stand
# scripts that source it. The caller defines kube and OUT.

# A forward that never came up is the flake this retries: status 2 is that
# and only that, so a genuinely unprofilable pod still stops the arm.
retry_capture() {
  local kind="$1" label="$2" secs="$3" rc
  pprof_capture "$kind" "$label" "$secs" && return 0
  rc=$?
  [ "$rc" = 2 ] || return "$rc"
  echo "pprof: the $kind forward did not come up; taking it once more" >&2
  pprof_capture "$kind" "$label" "$secs"
}

# pprof_port is the first local port a kind's forwards take: captures of two
# kinds run at once, and on shared ports the second rode the first's forward
# and lost it when the first closed it.
pprof_port() {
  case "$1" in
    profile) echo 18080 ;;
    block) echo 18180 ;;
    mutex) echo 18280 ;;
    *) echo 18380 ;;
  esac
}

pprof_capture() {
  local kind="$1" label="$2" secs="${3:-40}" pod pid pids=() files=() rc=0 base port
  base=$(pprof_port "$kind")
  port=$base
  local pods
  pods=$(kube get pods -l app.kubernetes.io/component=backend -o name | cut -d/ -f2)
  [ -n "$pods" ] || { echo "pprof: no backend pod to profile" >&2; return 1; }
  for pod in $pods; do
    kube port-forward "pod/$pod" "$port:8080" >/dev/null 2>&1 &
    pids+=($!)
    files+=("$OUT/$kind-$label-$pod-req${secs}s.pprof")
    port=$((port + 1))
  done
  # Ready, not slept for: a fixed pause captured one pod and missed the other,
  # and a forward that is not up yet writes no profile at all.
  port=$base
  for pod in $pods; do
    local deadline=$(( $(date +%s) + 30 ))
    until curl -fsS -o /dev/null "http://127.0.0.1:$port/healthz" 2>/dev/null; do
      if [ "$(date +%s)" -ge "$deadline" ]; then
        echo "pprof: the forward to $pod on $port never came up" >&2
        for pid in ${pids[@]+"${pids[@]}"}; do kill "$pid" 2>/dev/null || true; done
        return 2
      fi
      sleep 1
    done
    port=$((port + 1))
  done

  port=$base
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
      echo "pprof: the $kind profile for $(basename "$f") is empty; that pod was not profiled" >&2
      rc=1
    fi
  done
  return "$rc"
}
