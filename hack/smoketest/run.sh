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
kubectl -n "$NAMESPACE" delete job smoketest --ignore-not-found
sed "s|__IMAGE_TAG__|${TAG}|" "$(dirname "$0")/job.yaml" | kubectl -n "$NAMESPACE" apply -f -
kubectl -n "$NAMESPACE" wait --for=condition=complete --timeout=300s job/smoketest
kubectl -n "$NAMESPACE" logs job/smoketest
