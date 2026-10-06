#!/usr/bin/env bash
# The on-demand hosted deployment on this Mac: a k3s VM (Lima) reached
# through a Cloudflare Tunnel at https://rigforge.khademi.tech.
#
#   scripts/hosted.sh up        create or start the VM, deploy, verify
#   scripts/hosted.sh down      stop the VM (state is kept; "up" is fast)
#   scripts/hosted.sh status    VM, pods and tunnel
#   scripts/hosted.sh destroy   delete the VM and its data
#
# Needs: lima (brew install lima), kubectl, .env with the R2 values
# (docs/r2-setup.md), and deploy/k8s/overlays/tunnel/tunnel-token from
# Cloudflare Zero Trust (docs/production.md). Without the token everything
# still deploys and is checked from this machine; only the public URL waits.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VM="rigforge"
CPUS=4
MEMORY_GB=6
DISK_GB=40
DOMAIN="rigforge.khademi.tech"
OVERLAY="$ROOT/deploy/k8s/overlays/tunnel"
KUBECONFIG_FILE="$ROOT/tmp/kubeconfig-hosted"
LOCAL_PORT=8890 # host port forwarded to the cluster's ingress for local checks

export KUBECONFIG="$KUBECONFIG_FILE"
k() { kubectl "$@"; }

vm_state() { limactl list --format '{{.Name}} {{.Status}}' 2>/dev/null | awk -v vm="$VM" '$1 == vm {print $2}'; }

# Read one KEY from .env (quotes stripped). Empty if absent.
dotenv() { sed -n "s/^$1=//p" "$ROOT/.env" 2>/dev/null | head -1 | tr -d '"'"'" | tr -d '\r'; }

write_inputs() {
  [ -f "$ROOT/.env" ] || { echo ".env is missing; see docs/r2-setup.md" >&2; exit 1; }
  umask 077
  mkdir -p "$OVERLAY"
  {
    echo "S3_ENDPOINT=$(dotenv S3_ENDPOINT | sed 's#/*$##')"
    echo "S3_PUBLIC_ENDPOINT=$(dotenv S3_PUBLIC_ENDPOINT | sed 's#/*$##')"
    echo "S3_BUCKET=$(dotenv S3_BUCKET)"
  } >"$OVERLAY/config.env"
  # The Postgres password is generated once and kept, since the database
  # keeps its volume across up/down.
  local pg=""
  if [ -f "$OVERLAY/secret.env" ]; then
    pg="$(sed -n 's/^POSTGRES_PASSWORD=//p' "$OVERLAY/secret.env" | head -1)"
  fi
  [ -n "$pg" ] || pg="$(openssl rand -hex 16)"
  {
    echo "POSTGRES_PASSWORD=$pg"
    echo "S3_ACCESS_KEY_ID=$(dotenv S3_ACCESS_KEY_ID)"
    echo "S3_SECRET_ACCESS_KEY=$(dotenv S3_SECRET_ACCESS_KEY)"
  } >"$OVERLAY/secret.env"
  [ -f "$OVERLAY/issuer-key.pem" ] || openssl genrsa -out "$OVERLAY/issuer-key.pem" 2048 2>/dev/null
  if [ ! -s "$OVERLAY/tunnel-token" ]; then
    echo "missing" >"$OVERLAY/tunnel-token"
    TUNNEL_READY=0
  else
    TUNNEL_READY=1
  fi
}

up() {
  echo "==> 1/4 VM"
  case "$(vm_state)" in
  Running) echo "VM $VM is running" ;;
  Stopped) limactl start "$VM" ;;
  "")
    # template://k3s is Ubuntu with k3s installed by its provisioning script;
    # k3s brings Traefik (ingress) and local-path storage.
    limactl start --name "$VM" --cpus "$CPUS" --memory "$MEMORY_GB" --disk "$DISK_GB" --tty=false template://k3s
    ;;
  *) limactl start "$VM" ;;
  esac
  mkdir -p "$(dirname "$KUBECONFIG_FILE")"
  (umask 077 && limactl shell "$VM" sudo cat /etc/rancher/k3s/k3s.yaml >"$KUBECONFIG_FILE")
  for _ in $(seq 1 60); do
    if k get --raw /readyz >/dev/null 2>&1; then break; fi
    sleep 2
  done
  k wait --for=condition=Ready node --all --timeout=180s >/dev/null

  echo "==> 2/4 manifests"
  write_inputs
  k apply -k "$OVERLAY"
  if [ "$TUNNEL_READY" -eq 0 ]; then
    k -n rigforge scale deployment/cloudflared --replicas=0 >/dev/null
    echo "no tunnel token yet: cloudflared is scaled to 0 (add $OVERLAY/tunnel-token and run up again)"
  else
    k -n rigforge scale deployment/cloudflared --replicas=1 >/dev/null
  fi

  echo "==> 3/4 rollout"
  for workload in statefulset/postgres deployment/redis deployment/issuer deployment/api deployment/worker deployment/web; do
    k -n rigforge rollout status "$workload" --timeout=600s
  done

  echo "==> 4/4 checks"
  # Reach the cluster's ingress from this machine through the API server, and
  # send the public host name so the host-based Ingress rule matches.
  k -n kube-system port-forward svc/traefik "$LOCAL_PORT:80" >/dev/null 2>&1 &
  local pf=$!
  sleep 2
  CONNECT_TO="$DOMAIN:80:127.0.0.1:$LOCAL_PORT" "$ROOT/scripts/smoke_test.sh" "http://$DOMAIN"
  kill "$pf" 2>/dev/null || true
  if [ "$TUNNEL_READY" -eq 1 ]; then
    k -n rigforge rollout status deployment/cloudflared --timeout=120s
    for _ in $(seq 1 30); do
      if curl -fsS "https://$DOMAIN/api/health" >/dev/null 2>&1; then break; fi
      sleep 2
    done
    curl -fsS "https://$DOMAIN/api/health" >/dev/null && echo "public: https://$DOMAIN is up" ||
      echo "public: https://$DOMAIN not reachable yet (DNS or tunnel still propagating; check 'status')"
  fi
  echo
  status
}

down() {
  case "$(vm_state)" in
  Running) limactl stop "$VM" && echo "VM stopped; https://$DOMAIN is offline until 'up'" ;;
  "") echo "VM $VM does not exist" ;;
  *) echo "VM $VM is already stopped" ;;
  esac
}

status() {
  echo "VM: ${VM} $(vm_state || echo absent)"
  if [ "$(vm_state)" = Running ] && [ -f "$KUBECONFIG_FILE" ]; then
    k get pods -n rigforge 2>/dev/null || true
    echo
    if curl -fsS -m 10 "https://$DOMAIN/api/health" >/dev/null 2>&1; then
      echo "public: https://$DOMAIN is up"
    else
      echo "public: https://$DOMAIN is not reachable"
    fi
  fi
}

destroy() {
  limactl delete --force "$VM"
  rm -f "$KUBECONFIG_FILE"
  echo "VM deleted"
}

case "${1:-}" in
up) up ;;
down) down ;;
status) status ;;
destroy) destroy ;;
*)
  sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
  ;;
esac
