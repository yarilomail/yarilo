#!/usr/bin/env bash
# LMTP delivery, yarilo against the reference, the way docs-internal#318 was
# measured on 2026-11-25: three runs of 300 messages to u49 (mdbox), u99
# (maildir) and u149 (sdbox) on each server, CPU a message from the node's
# cadvisor over the whole namespace less the same namespace idle for 30 s.
# yarilo counts every tier (logins, director, locks, fts); the reference is
# its one pod.
#
# Usage:
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/lmtp-pair.sh <label> <out-dir>
#
# Runs against whatever yarilo is deployed, right after an ab-arm.sh arm (with
# YARILO_ARM_OVERLAY=lmtp-nocpu for the row without the CPU limit). yarilo's
# backends are scaled to 0 while the reference runs and back after; the
# reference is scaled to 0 again. The lmtp container's CPU limit as deployed is
# written beside the figures, so a row says which limit it ran under.

set -euo pipefail

LABEL="${1:?label}"
OUT="${2:?output directory}"
NS="${YARILO_NS:-yarilo-sb}"
REF_NS="${YARILO_REF_NS:-dovecot}"
NODE="${YARILO_NODE:-sb-k8s-01}"
RUNS="${YARILO_LMTP_RUNS:-3}"
MSGS="${YARILO_LMTP_MSGS:-300}"
USERS="${YARILO_LMTP_USERS:-u49@d00001.test u99@d00002.test u149@d00003.test}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
mkdir -p "$OUT"
L="$OUT/lmtp-$LABEL.log"

k() { kubectl --request-timeout=60s "$@"; }

# Pod-level CPU seconds of a namespace, from the node's cadvisor.
ns_cpu() {
  k get --raw "/api/v1/nodes/$NODE/proxy/metrics/cadvisor" | awk -v n="namespace=\"$1\"" '
    /^container_cpu_usage_seconds_total\{/ && index($0, n) && index($0, "container=\"\",") && index($0, "name=\"\"") { s += $2 }
    END { printf "%.2f", s }'
}

idle_rate() {
  local a b
  a=$(ns_cpu "$1"); sleep 30; b=$(ns_cpu "$1")
  awk -v a="$a" -v b="$b" 'BEGIN { printf "%.3f", (b - a) / 30 }'
}

# runs ns label pod container host
runs() {
  local base a b t0 t1 r u
  base=$(idle_rate "$1")
  echo "== $2 idle ${base} cpu/s" >> "$L"
  for r in $(seq 1 "$RUNS"); do
    a=$(ns_cpu "$1"); t0=$(date +%s)
    for u in $USERS; do
      bash "$HERE/lmtp-rate.sh" "$1" "$3" "$4" "$5" 24 "$u" "$MSGS" >> "$L" 2>&1 || echo "rcpt=$u failed" >> "$L"
    done
    sleep 20 # what the delivery queued (indexing, fts) lands in the window
    b=$(ns_cpu "$1"); t1=$(date +%s)
    awk -v a="$a" -v b="$b" -v base="$base" -v d=$((t1 - t0)) -v r="$r" -v n="$(($(echo "$USERS" | wc -w) * MSGS))" 'BEGIN {
      used = b - a - base * d
      printf "  run %d: cpu_s=%.1f over %ds, less idle %.1f, ms a message %.1f\n", r, b - a, d, used, used * 1000 / n
    }' >> "$L"
  done
}

# login_metrics <file>: yarilo-lmtp-login's metrics page, through a forward.
login_metrics() {
  local pod pf
  pod=$(k -n "$NS" get pods -o name | grep -m1 'lmtp-login' | cut -d/ -f2)
  k -n "$NS" port-forward "pod/$pod" 18680:8080 > /dev/null 2>&1 &
  pf=$!
  for _ in $(seq 1 30); do curl -fsS -o "$1" http://127.0.0.1:18680/metrics 2>/dev/null && break; sleep 1; done
  kill "$pf" 2>/dev/null || true
}

# phases <before> <after> <messages>: each phase of the proxy's RCPT and
# backend histograms as ms a message, and the share of resumed handshakes.
phases() {
  awk -v n="$3" '
    FNR == 1 { f++ }
    /^yarilo_lmtp_login_(rcpt|backend)_phase_seconds_sum\{/ {
      match($0, /phase="[^"]*"/); ph = substr($0, RSTART + 7, RLENGTH - 8)
      kind = ($0 ~ /backend_phase/) ? "backend " : ""
      s[kind ph] += (f == 1 ? -$2 : $2)
    }
    /^yarilo_lmtp_login_backend_tls_handshakes_total\{/ {
      r = ($0 ~ /resumed="true"/) ? "yes" : "no"
      h[r] += (f == 1 ? -$2 : $2)
    }
    END {
      line = "  proxy phases, ms a message:"
      for (k in s) line = line sprintf(" %s=%.2f", k, s[k] * 1000 / n)
      t = h["yes"] + h["no"]
      line = line sprintf("; backend TLS handshakes %d, resumed %d (%.0f %%)", t, h["yes"], t > 0 ? 100 * h["yes"] / t : 0)
      print line
    }' "$1" "$2"
}

limit=$(k -n "$NS" get pod yarilo-backend-0 -o jsonpath='{.spec.containers[?(@.name=="yarilo-lmtp")].resources.limits.cpu}')
image=$(k -n "$NS" get pod yarilo-backend-0 -o jsonpath='{.spec.containers[0].image}')
echo "== $LABEL yarilo $image, yarilo-lmtp cpu limit ${limit:-none}" | tee "$L"

login_metrics "$OUT/lmtp-login-$LABEL-before.prom"
runs "$NS" yarilo yarilo-backend-0 yarilo-imap yarilo-lmtp-login
login_metrics "$OUT/lmtp-login-$LABEL-after.prom"
phases "$OUT/lmtp-login-$LABEL-before.prom" "$OUT/lmtp-login-$LABEL-after.prom" "$((RUNS * MSGS * $(echo "$USERS" | wc -w)))" >> "$L"

restore() {
  k -n "$REF_NS" delete pod lmtpc --ignore-not-found --wait=false > /dev/null 2>&1 || true
  k -n "$REF_NS" scale deploy dovecot --replicas=0 > /dev/null 2>&1 || true
  k -n "$NS" scale sts yarilo-backend --replicas=2 > /dev/null
  k -n "$NS" rollout status sts yarilo-backend --timeout=600s > /dev/null && echo "backends back" >> "$L"
}
trap restore EXIT
k -n "$NS" scale sts yarilo-backend --replicas=0 > /dev/null
k -n "$NS" wait --for=delete pod/yarilo-backend-0 pod/yarilo-backend-1 --timeout=300s > /dev/null 2>&1 || true
k -n "$REF_NS" scale deploy dovecot --replicas=1 > /dev/null
k -n "$REF_NS" rollout status deploy dovecot --timeout=300s > /dev/null
k -n "$REF_NS" run lmtpc --image=busybox:1.37 --restart=Never -- sleep 3600 > /dev/null
k -n "$REF_NS" wait --for=condition=Ready pod/lmtpc --timeout=180s > /dev/null
echo "== reference $(k -n "$REF_NS" get deploy dovecot -o jsonpath='{.spec.template.spec.containers[0].image}')" >> "$L"
runs "$REF_NS" reference lmtpc - dovecot
trap - EXIT
restore
echo "done" >> "$L"
