#!/usr/bin/env bash
# Answers whether an image tag is in the registry, for the check a rollout makes
# before it pins one (#1807).
set -euo pipefail

IMAGE="${IMAGE:-yarilomail/yarilo}"
TAG="${1:-}"
if [ -z "$TAG" ]; then
  echo "usage: $(basename "$0") <tag>   (IMAGE=$IMAGE)" >&2
  exit 2
fi

TOKEN=$(curl -fsS "https://ghcr.io/token?scope=repository:${IMAGE}:pull" |
  sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
if [ -z "$TOKEN" ]; then
  echo "registry: no pull token for ${IMAGE}" >&2
  exit 3
fi

# ghcr answers a manifest request that does not accept an index with 404.
ACCEPT='application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json'
CODE=$(curl -s -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer ${TOKEN}" -H "Accept: ${ACCEPT}" \
  "https://ghcr.io/v2/${IMAGE}/manifests/${TAG}")

case "$CODE" in
  200) echo "${IMAGE}:${TAG} present"; exit 0 ;;
  404) echo "${IMAGE}:${TAG} absent" >&2; exit 1 ;;
  # Anything else is the registry saying something other than yes or no, and a
  # rollout must not read that as either.
  *) echo "registry: HTTP ${CODE} for ${IMAGE}:${TAG}" >&2; exit 4 ;;
esac
