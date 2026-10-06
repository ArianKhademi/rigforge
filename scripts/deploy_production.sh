#!/usr/bin/env bash
# Deploy Rigforge to a single Ubuntu VPS running k3s, from scratch.
#
#   ACME_EMAIL=you@example.com scripts/deploy_production.sh root@203.0.113.10
#
# What it does, in order (every step is idempotent; run it again after a change):
#   1. installs k3s on the host over ssh if it is not there (Traefik ingress
#      and local-path storage come with it)
#   2. copies the cluster's kubeconfig to tmp/kubeconfig-production
#   3. installs cert-manager and a Let's Encrypt ClusterIssuer
#   4. applies deploy/k8s/overlays/production
#   5. waits for the rollout and the certificate, then runs the smoke test
#
# Before running
#   - DNS: an A record for DOMAIN pointing at the host
#   - deploy/k8s/overlays/production/{config.env,secret.env,issuer-key.pem}
#     (see the kustomization there and docs/r2-setup.md)
#   - the four images on GHCR must be pullable (public packages, or an image
#     pull secret named ghcr-pull in the rigforge namespace)
#   - ssh access to the host as a user who can sudo without a password
#
# Environment
#   ACME_EMAIL   required; Let's Encrypt sends expiry notices here
#   DOMAIN       default rigforge.khademi.tech
#   CERT_MANAGER_VERSION   default: latest release
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TARGET="${1:?usage: ACME_EMAIL=... deploy_production.sh user@host}"
DOMAIN="${DOMAIN:-rigforge.khademi.tech}"
ACME_EMAIL="${ACME_EMAIL:?set ACME_EMAIL to the address for the Let’s Encrypt account}"
OVERLAY="$ROOT/deploy/k8s/overlays/production"
KUBECONFIG_FILE="$ROOT/tmp/kubeconfig-production"
HOST="${TARGET#*@}"
# k3s puts the node's own IP in the API server certificate; connect by IP.
IP="$(python3 -c "import socket,sys; print(socket.gethostbyname(sys.argv[1]))" "$HOST")"

export KUBECONFIG="$KUBECONFIG_FILE"
k() { kubectl "$@"; }
remote() { ssh -o BatchMode=yes -o ConnectTimeout=15 "$TARGET" "$@"; }

for f in config.env secret.env issuer-key.pem; do
  [ -f "$OVERLAY/$f" ] || { echo "missing $OVERLAY/$f (see the kustomization there)" >&2; exit 1; }
done

echo "==> 1/5 k3s on $TARGET"
# --tls-san adds the domain and public IP to the API server certificate.
remote "command -v k3s >/dev/null || curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC='--tls-san $DOMAIN --tls-san $IP' sh -"
remote "sudo k3s kubectl wait --for=condition=Ready node --all --timeout=180s" >/dev/null

echo "==> 2/5 kubeconfig -> ${KUBECONFIG_FILE#"$ROOT"/}"
mkdir -p "$(dirname "$KUBECONFIG_FILE")"
(umask 077 && remote "sudo cat /etc/rancher/k3s/k3s.yaml" | sed "s/127.0.0.1/$IP/" >"$KUBECONFIG_FILE")
k get nodes

echo "==> 3/5 cert-manager"
if [ -n "${CERT_MANAGER_VERSION:-}" ]; then
  manifest="https://github.com/cert-manager/cert-manager/releases/download/$CERT_MANAGER_VERSION/cert-manager.yaml"
else
  manifest="https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml"
fi
k apply -f "$manifest" >/dev/null
k -n cert-manager rollout status deployment/cert-manager-webhook --timeout=300s >/dev/null
# The webhook needs a moment after "rolled out" before it admits ClusterIssuers.
for _ in $(seq 1 30); do
  if sed "s/you@example.com/$ACME_EMAIL/" "$OVERLAY/cluster-issuer.yaml" | k apply -f - >/dev/null 2>&1; then break; fi
  sleep 5
done

echo "==> 4/5 manifests"
k apply -k "$OVERLAY"

echo "==> 5/5 waiting"
for workload in statefulset/postgres deployment/redis deployment/issuer deployment/api deployment/worker deployment/web; do
  k -n rigforge rollout status "$workload" --timeout=600s
done
# The certificate is requested when the Ingress appears; HTTP-01 needs DNS
# to already point here.
k -n rigforge wait certificate/rigforge-tls --for=condition=Ready --timeout=600s
k get pods -n rigforge
echo
"$ROOT/scripts/smoke_test.sh" "https://$DOMAIN"
echo
echo "Rigforge is live at https://$DOMAIN"
