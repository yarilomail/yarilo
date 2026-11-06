#!/usr/bin/env bash
# Installs stand-dns with the committed zone and points the cluster CoreDNS at
# it for test. only. Safe to run again: the forward block is replaced, not added.
#
# Usage: KUBECONFIG=~/.kube/ihorru-sbox-nc.yaml bash hack/sandbox/dns-apply.sh

set -Eeuo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
kube() { kubectl --request-timeout=30s "$@"; }

kube apply -f "$DIR/stand-dns.yaml"
kube -n stand-dns create configmap stand-dns \
  --from-file=Corefile="$DIR/stand-dns/Corefile" --from-file=test.db="$DIR/stand-dns/test.db" \
  --dry-run=client -o yaml | kube apply -f -
kube -n stand-dns rollout restart deploy/stand-dns
kube -n stand-dns rollout status deploy/stand-dns --timeout=120s

ip=$(kube -n stand-dns get svc stand-dns -o jsonpath='{.spec.clusterIP}')
[ -n "$ip" ] || { echo "dns-apply: stand-dns has no ClusterIP" >&2; exit 1; }

# The block sits between two marker lines, so a rerun swaps it whole.
current=$(kube -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')
patched=$(printf '%s\n' "$current" | awk '/^# stand-dns begin$/{skip=1} !skip{print} /^# stand-dns end$/{skip=0}')
patched=$(printf '%s\n# stand-dns begin\ntest:53 {\n    forward . %s\n}\n# stand-dns end\n' "$patched" "$ip")
if [ "$patched" = "$current" ]; then
  echo "dns-apply: Corefile already forwards test. to $ip"
  exit 0
fi
kube -n kube-system create configmap coredns --from-literal=Corefile="$patched" \
  --dry-run=client -o yaml | kube apply -f -
echo "dns-apply: Corefile forwards test. to $ip (CoreDNS reloads it within a minute)"
