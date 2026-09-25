#!/bin/sh
# Weekly cleanup of what the CI runner itself made. The numbers are printed
# before and after: a timer whose effect is not in the journal is a timer
# nobody knows fired.
set -eu

KEEP_HOURS="${KEEP_HOURS:-168}"
IMAGE_REPO="${IMAGE_REPO:-ghcr.io/yarilomail/yarilo}"

echo "runner-prune: before"
docker system df

# BuildKit evicts by last use, so a layer every build reuses stays warm however
# old it is; the age filter only reaches what nothing has touched in a week.
docker builder prune -af --filter "until=${KEEP_HOURS}h"

# Dangling images are leftovers of this runner's own builds.
docker image prune -f

# Tagged images: only this project's, and only those older than the window.
# Base images (golang, alpine) are left alone: they are old by construction, and
# removing them turns the next build into a cold one.
cutoff=$(date -u -d "${KEEP_HOURS} hours ago" +%s 2>/dev/null || date -u -v-"${KEEP_HOURS}"H +%s)
docker images --format '{{.ID}} {{.Repository}} {{.CreatedAt}}' |
	while read -r id repo created_at; do
		[ "$repo" = "$IMAGE_REPO" ] || continue
		created=$(date -u -d "$created_at" +%s 2>/dev/null || echo 0)
		[ "$created" -gt 0 ] && [ "$created" -lt "$cutoff" ] || continue
		docker rmi "$id" >/dev/null 2>&1 || true
	done

echo "runner-prune: after"
docker system df
