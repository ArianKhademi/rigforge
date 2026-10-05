#!/usr/bin/env bash
# Bring the whole of Rigforge up on a local kind (Kubernetes in Docker) cluster.
#
#   scripts/deploy_kind.sh          create the cluster if needed, build, deploy
#   scripts/deploy_kind.sh status   show the pods and the URL
#
# What it does, in order:
#   1. creates the kind cluster "rigforge" (deploy/kind/cluster.yaml)
#   2. builds the four images and loads them into the cluster's node
#   3. generates throwaway secrets for this cluster (git-ignored)
#   4. applies the manifests (deploy/k8s/overlays/kind)
#   5. waits for every workload to become ready and prints the URL
#
# Needs docker, kind, kubectl and openssl. Safe to run again: every step is
# idempotent, and a second run redeploys freshly built images.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CLUSTER="rigforge"
CONTEXT="kind-$CLUSTER"
OVERLAY="$ROOT/deploy/k8s/overlays/kind"
URL="http://localhost:8880"
IMAGES=(rigforge-api:dev rigforge-issuer:dev rigforge-worker:dev rigforge-web:dev)

k() { kubectl --context "$CONTEXT" "$@"; }

status() {
  k get pods -n rigforge -o wide
  echo
  echo "Rigforge is running at $URL"
}

if [ "${1:-}" = "status" ]; then
  status
  exit 0
fi

echo "==> 1/5 cluster"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "kind cluster \"$CLUSTER\" already exists"
else
  kind create cluster --config "$ROOT/deploy/kind/cluster.yaml" --wait 120s
fi

echo "==> 2/5 images"
docker build -q -t rigforge-api:dev --target api "$ROOT/api"
docker build -q -t rigforge-issuer:dev --target issuer "$ROOT/api"
docker build -q -t rigforge-worker:dev "$ROOT/worker"
docker build -q -t rigforge-web:dev "$ROOT/web"
# kind's node has its own image store, separate from Docker's; this copies
# the images in, so the cluster needs no registry.
kind load docker-image "${IMAGES[@]}" --name "$CLUSTER"

echo "==> 3/5 secrets"
# Generated once per checkout and git-ignored. These are credentials for the
# Postgres and MinIO that live inside this throwaway cluster, nothing else.
if [ ! -f "$OVERLAY/secret.env" ]; then
  (
    umask 077
    {
      echo "POSTGRES_PASSWORD=$(openssl rand -hex 16)"
      echo "S3_ACCESS_KEY_ID=rigforge"
      echo "S3_SECRET_ACCESS_KEY=$(openssl rand -hex 20)"
    } >"$OVERLAY/secret.env"
  )
  echo "generated $OVERLAY/secret.env"
fi
if [ ! -f "$OVERLAY/issuer-key.pem" ]; then
  (umask 077 && openssl genrsa -out "$OVERLAY/issuer-key.pem" 2048 2>/dev/null)
  echo "generated the dev issuer's signing key"
fi

echo "==> 4/5 manifests"
# Loading gigabytes of images keeps the single node busy; give the API server
# a moment to answer again before talking to it.
for _ in $(seq 1 60); do
  if k get --raw /readyz >/dev/null 2>&1; then break; fi
  sleep 2
done
REDEPLOY=0
if k -n rigforge get deployment api >/dev/null 2>&1; then REDEPLOY=1; fi
k apply -k "$OVERLAY"
if [ "$REDEPLOY" -eq 1 ]; then
  # The image tag (:dev) does not change between builds, so on a redeploy
  # the Deployments have to be told to pick up the freshly loaded images.
  k -n rigforge rollout restart deployment/api deployment/worker deployment/web deployment/issuer >/dev/null
fi

echo "==> 5/5 waiting for rollout"
for workload in statefulset/postgres deployment/redis deployment/minio deployment/traefik \
  deployment/issuer deployment/api deployment/worker deployment/web; do
  k -n rigforge rollout status "$workload" --timeout=300s
done

# The ingress is only useful once it actually routes: wait for the api's
# health endpoint through it.
for _ in $(seq 1 60); do
  if curl -fsS "$URL/api/health" >/dev/null 2>&1; then break; fi
  sleep 2
done
curl -fsS "$URL/api/health" >/dev/null || {
  echo "the api is not reachable through the ingress at $URL" >&2
  exit 1
}

echo
status
