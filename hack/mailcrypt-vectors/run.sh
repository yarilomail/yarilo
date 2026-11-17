#!/bin/sh
# Builds gen.c against the reference library in alpine:3.24 and writes the
# vectors into $1. Run on a host with docker; CI sets YARILO_REFSTREAM_VECTORS.
set -eu
out=$(cd "$1" && pwd)
here=$(cd "$(dirname "$0")" && pwd)
docker run --rm -v "$here:/src:ro" -v "$out:/out" alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf sh -euc '
  apk add --no-cache build-base dovecot dovecot-dev >/dev/null
  cc -o /tmp/gen /src/gen.c -include /usr/include/dovecot/config.h -I/usr/include/dovecot \
    -L/usr/lib/dovecot -Wl,-rpath,/usr/lib/dovecot -ldovecot
  /tmp/gen /out
  chown -R '"$(id -u):$(id -g)"' /out
'
wc -l < "$out/MANIFEST"
