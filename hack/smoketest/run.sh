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
BACKEND_LABEL="${SMOKE_BACKEND_LABEL:-app.kubernetes.io/component=backend}"
BACKEND_STS="${SMOKE_BACKEND_STS:-yarilo-backend}"
# A rolling update deletes and recreates one pod at a time: between the two,
# the pods listed below are all ready while the set is not.
kubectl -n "$NAMESPACE" rollout status "sts/$BACKEND_STS" --timeout="${READY_TIMEOUT}s"
waited=0
while :; do
  pods=$(kubectl -n "$NAMESPACE" get pods -l "$BACKEND_LABEL" \
           -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
  # No pod is not "every pod is ready": a wrong label or namespace would
  # otherwise skip the wait silently and report readiness as a broken build.
  if [ -z "$pods" ]; then
    echo "smoketest: no pod matches $BACKEND_LABEL in namespace $NAMESPACE" >&2
    exit 1
  fi
  missing=""
  for pod in $pods; do
    # A pod still starting refuses the exec, and that is a pod to wait for,
    # not a reason to end the script under set -e.
    body=$(kubectl -n "$NAMESPACE" exec "$pod" -c yarilo-backend-reg -- \
             sh -c 'wget -qO- http://127.0.0.1:8080/readyz 2>/dev/null' 2>/dev/null || true)
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

# A ready backend is not yet a routed one: it registers with the directors
# after /readyz, and a login in between is answered TRYLATER. Wait until every
# director lists every backend pod, by its current address, as up.
DIRECTOR_LABEL="${SMOKE_DIRECTOR_LABEL:-app.kubernetes.io/component=director}"
waited=0
while :; do
  ips=$(kubectl -n "$NAMESPACE" get pods -l "$BACKEND_LABEL" \
          -o jsonpath='{range .items[*]}{.status.podIP}{"\n"}{end}' 2>/dev/null || true)
  directors=$(kubectl -n "$NAMESPACE" get pods -l "$DIRECTOR_LABEL" \
          -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
  if [ -z "$directors" ]; then
    break # no director in this deployment: logins route without one
  fi
  missing=""
  for d in $directors; do
    list=$(kubectl -n "$NAMESPACE" exec "$d" -- yarctl director backends list 2>/dev/null || true)
    for ip in $ips; do
      echo "$list" | grep -Eq "^${ip}[[:space:]].*[[:space:]]up[[:space:]]" || missing="$missing $d:$ip"
    done
  done
  [ -n "$ips" ] && [ -z "$missing" ] && break
  if [ "$waited" -ge "$READY_TIMEOUT" ]; then
    echo "smoketest: directors still do not route to every backend after ${READY_TIMEOUT}s:$missing" >&2
    exit 1
  fi
  sleep 5
  waited=$((waited + 5))
done
[ "$waited" -gt 0 ] && echo "smoketest: waited ${waited}s for the directors to route to every backend"

kubectl -n "$NAMESPACE" delete job smoketest --ignore-not-found
sed "s|__IMAGE_TAG__|${TAG}|" "$(dirname "$0")/job.yaml" | kubectl -n "$NAMESPACE" apply -f -
kubectl -n "$NAMESPACE" wait --for=condition=complete --timeout=300s job/smoketest
kubectl -n "$NAMESPACE" logs job/smoketest
