#!/usr/bin/env bash
# /tmp use of every pod across a window, for the tmpSizeLimit default.
#
# Usage:
#   KUBECONFIG=~/.kube/sbox.yaml bash hack/stand/watch-tmp.sh <out-dir> &
#   ... the window ...
#   kill %1    # the summary is written on the way out
#
# A pod's /tmp is one emptyDir for all its containers, so one container a pod
# is read, every 15 s: the reference-format cache keeps a body 60 s, so a peak
# is not missed, and the sampler costs the same in every arm of the window.
# <out-dir>/tmp-samples.tsv has one line a pod a sample (epoch, pod, KiB) and
# one a subdirectory (epoch, pod/dir, KiB); tmp-peak.txt is each pod's peak.

set -uo pipefail

OUT="${1:?output directory}"
NS="${YARILO_NS:-yarilo-sb}"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
EVERY="${YARILO_TMP_EVERY:-15}"
mkdir -p "$OUT"
SAMPLES="$OUT/tmp-samples.tsv"

kube() { kubectl --kubeconfig="$KCFG" -n "$NS" --request-timeout=30s "$@"; }

summary() {
  awk -F'\t' 'index($2, "/") == 0 { if ($3 > peak[$2]) peak[$2] = $3 }
    END { for (p in peak) printf "%s\t%d KiB\n", p, peak[p] }' "$SAMPLES" 2>/dev/null |
    sort -t$'\t' -k2,2nr > "$OUT/tmp-peak.txt"
  echo "watch-tmp: peaks in $OUT/tmp-peak.txt" >&2
  exit 0
}
trap summary TERM INT

while :; do
  now=$(date +%s)
  # Running pods with their first container; a Job's finished pod has no /tmp to read.
  kube get pods --field-selector=status.phase=Running \
    -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.containers[0].name}{"\n"}{end}' 2>/dev/null |
    while read -r pod ctr; do
      [ -n "$pod" ] || continue
      kube exec "$pod" -c "$ctr" -- sh -c 'du -sk /tmp 2>/dev/null; du -sk /tmp/* 2>/dev/null' 2>/dev/null |
        awk -v t="$now" -v p="$pod" -F'\t' '
          $2 == "/tmp" { printf "%s\t%s\t%s\n", t, p, $1; next }
          { sub("^/tmp/", "", $2); printf "%s\t%s/%s\t%s\n", t, p, $2, $1 }'
    done >> "$SAMPLES"
  sleep "$EVERY" &
  wait $!
done
