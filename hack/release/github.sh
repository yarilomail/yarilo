#!/usr/bin/env bash
# GitHub REST calls for the release job, with curl and jq only: the runner has no gh.
# GH_TOKEN goes in a header read from stdin, never in a URL, an argument or the log.
set -euo pipefail

API="${GITHUB_API_URL:-https://api.github.com}"
UPLOADS="https://uploads.github.com"
: "${GH_TOKEN:?GH_TOKEN is not set}"

auth() { printf 'Authorization: Bearer %s\n' "$GH_TOKEN"; }

# call METHOD URL [curl args...]: the body on stdout; an HTTP error fails with it.
call() {
	local method=$1 url=$2
	shift 2
	auth | curl -sS --fail-with-body -X "$method" -H @- \
		-H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' \
		"$@" "$url"
}

delete_asset() {
	local repo=$1 tag=$2 name=$3 id old
	id=$(call GET "$API/repos/$repo/releases/tags/$tag" | jq -er .id)
	for old in $(call GET "$API/repos/$repo/releases/$id/assets?per_page=100" |
		jq -r --arg n "$name" '.[] | select(.name == $n) | .id'); do
		call DELETE "$API/repos/$repo/releases/assets/$old" >/dev/null
	done
}

case "${1:-}" in
status)
	# PATH: the HTTP status alone, 000 when nothing answered; the caller decides.
	auth | curl -s -o /dev/null -w '%{http_code}' -H @- \
		-H 'Accept: application/vnd.github+json' "$API/$2" || true
	;;
upload-asset)
	# REPO TAG FILE: replaces an asset of the same name, as gh release upload --clobber.
	repo=$2 tag=$3 file=$4 name=$(basename "$4")
	delete_asset "$repo" "$tag" "$name"
	id=$(call GET "$API/repos/$repo/releases/tags/$tag" | jq -er .id)
	call POST "$UPLOADS/repos/$repo/releases/$id/assets?name=$name" \
		-H 'Content-Type: application/octet-stream' --data-binary "@$file" |
		jq -er '"uploaded \(.name), \(.size) bytes"'
	;;
assets)
	# REPO TAG: the release's asset names, one a line.
	call GET "$API/repos/$2/releases/tags/$3" | jq -r '.assets[].name'
	;;
delete-asset)
	# REPO TAG NAME
	delete_asset "$2" "$3" "$4"
	echo "deleted $4 from $3"
	;;
tag-main)
	# REPO TAG: tags the head of main. DRY_RUN=1 stops before the write.
	repo=$2 tag=$3
	sha=$(call GET "$API/repos/$repo/git/ref/heads/main" | jq -er .object.sha)
	if [ "${DRY_RUN:-}" = 1 ]; then
		echo "would tag $repo@${sha::7} as $tag"
		exit 0
	fi
	call POST "$API/repos/$repo/git/refs" \
		-d "$(jq -nc --arg ref "refs/tags/$tag" --arg sha "$sha" '{ref: $ref, sha: $sha}')" >/dev/null
	echo "Tagged $repo@${sha::7} as $tag"
	;;
delete-tag)
	# REPO TAG
	call DELETE "$API/repos/$2/git/refs/tags/$3" >/dev/null
	echo "deleted tag $3 from $2"
	;;
*)
	echo "usage: $0 status PATH | upload-asset REPO TAG FILE | assets REPO TAG | delete-asset REPO TAG NAME | tag-main REPO TAG | delete-tag REPO TAG" >&2
	exit 2
	;;
esac
