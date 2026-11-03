#!/usr/bin/env bash
# Builds dist/yarilo_<version>_<arch>.deb from deploy/nfpm/nfpm.yaml. Run from the repo root.
set -euo pipefail

NFPM=github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.48.0
export ARCH="${ARCH:-amd64}"
export VERSION="${VERSION:-$(sed -n 's/^appVersion: "\(.*\)"$/\1/p' helm/Chart.yaml)}"
BIN=dist/bin
MOD=github.com/yarilomail/yarilo/v2

mkdir -p "$BIN"
for t in $(sed -n 's|^  - src: dist/bin/||p' deploy/nfpm/nfpm.yaml); do
	CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath \
		-ldflags "-s -w -X $MOD/pkg/build.Version=$VERSION -X $MOD/pkg/build.ChartVersion=$VERSION" \
		-o "$BIN/$t" "./app/$t"
done
go run "$NFPM" package --config deploy/nfpm/nfpm.yaml --packager deb --target dist/
