#!/usr/bin/env bash
# Applies the smoketest Job against the image the namespace is actually running.
# A tag written into job.yaml is a number in git that rots at the next bump.
set -euo pipefail

NAMESPACE="${NAMESPACE:-yarilo-sb}"
DEPLOYMENT="${DEPLOYMENT:-yarilo-imap-login}"
TAG="${1:-}"

if [ -z "$TAG" ]; then
  # What is deployed, not what the chart intends: a post-deploy check that
  # tests another build reports about nothing.
  IMAGE=$(kubectl -n "$NAMESPACE" get deploy "$DEPLOYMENT" \
    -o jsonpath='{.spec.template.spec.containers[0].image}')
  TAG="${IMAGE##*:}"
fi
if [ -z "$TAG" ] || [ "$TAG" = "__IMAGE_TAG__" ]; then
  echo "no image tag: pass one as \$1 or check $NAMESPACE/$DEPLOYMENT" >&2
  exit 1
fi

echo "smoketest image tag: $TAG"

# A rollout that finished is not a backend a client can reach: backend-reg
# withholds its heartbeat until every protocol answers, and a run started in
# that window reports "backend unavailable" about readiness, not about code.
READY_TIMEOUT="${SMOKE_READY_TIMEOUT:-180}"
waited=0
while :; do
  missing=""
  for pod in $(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=backend \
                 -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null); do
    body=$(kubectl -n "$NAMESPACE" exec "$pod" -c yarilo-backend-reg -- \
             sh -c 'wget -qO- http://127.0.0.1:8080/readyz 2>/dev/null' 2>/dev/null)
    case "$body" in
      *'"ready":true'*) ;;
      *) missing="$missing $pod" ;;
    esac
  done
  [ -z "$missing" ] && break
  if [ "$waited" -ge "$READY_TIMEOUT" ]; then
    echo "smoketest: backends still not ready after ${READY_TIMEOUT}s:$missing" >&2
    exit 1
  fi
  sleep 5
  waited=$((waited + 5))
done
[ "$waited" -gt 0 ] && echo "smoketest: waited ${waited}s for the backends to report ready"

kubectl -n "$NAMESPACE" delete job smoketest --ignore-not-found
sed "s|__IMAGE_TAG__|${TAG}|" "$(dirname "$0")/job.yaml" | kubectl -n "$NAMESPACE" apply -f -
kubectl -n "$NAMESPACE" wait --for=condition=complete --timeout=300s job/smoketest
kubectl -n "$NAMESPACE" logs job/smoketest
