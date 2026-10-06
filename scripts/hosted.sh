#!/usr/bin/env bash
# The on-demand hosted deployment on this Mac: a k3s VM (Lima) whose ingress
# Lima forwards to the host's port 80, published as https://rigforge.khademi.tech
# by the cloudflared that runs on the host (docs/production.md).
#
#   scripts/hosted.sh up        create or start the VM, deploy the newest images, verify
#   scripts/hosted.sh down      stop the VM (state is kept; "up" is fast)
#   scripts/hosted.sh status    VM, pods and tunnel
#   scripts/hosted.sh destroy   delete the VM and its data
#
# Needs: lima (brew install lima), kubectl, and .env with the R2 values
# (docs/r2-setup.md). The public URL additionally needs cloudflared on this
# machine with a route for the host name (docs/production.md); without it
# everything still deploys and is checked from this machine.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VM="rigforge"
# The host sees the whole allocation as used once the guest's page cache has
# filled it, so this is the VM's footprint on the Mac. 4 GB fits k3s (about
# 0.7 GB), the stack idle (about 1 GB) and one worker on a job (about 1 GB
# more); the worker's 3 GiB limit is a cap, not a reservation.
CPUS=4
MEMORY_GB=4
DISK_GB=40
DOMAIN="rigforge.khademi.tech"
OVERLAY="$ROOT/deploy/k8s/overlays/tunnel"
KUBECONFIG_FILE="$ROOT/tmp/kubeconfig-hosted"
# Lima forwards the VM's listening ports to the host, so the cluster's
# ingress (Traefik on 80) answers here. cloudflared sends the public traffic
# to the same address, so the local check below covers the same path.
INGRESS="127.0.0.1:80"

export KUBECONFIG="$KUBECONFIG_FILE"
k() { kubectl "$@"; }

vm_state() { limactl list --format '{{.Name}} {{.Status}}' 2>/dev/null | awk -v vm="$VM" '$1 == vm {print $2}'; }

# Read one KEY from .env (quotes stripped). Empty if absent.
dotenv() { sed -n "s/^$1=//p" "$ROOT/.env" 2>/dev/null | head -1 | tr -d '"'"'" | tr -d '\r'; }

# Both services the smoke test talks to first, through the ingress: the api
# directly and the issuer through the web proxy.
ingress_up() {
  curl -fsS -m 5 -o /dev/null -H "Host: $DOMAIN" "http://$INGRESS/api/health" 2>/dev/null &&
    curl -fsS -m 5 -o /dev/null -H "Host: $DOMAIN" "http://$INGRESS/issuer/.well-known/jwks.json" 2>/dev/null
}
# Resolved through a public resolver and pinned, so a local resolver that
# lags a DNS change (a VPN's, say) cannot make a live URL look down.
public_up() {
  local ip
  ip="$(dig +short "$DOMAIN" @1.1.1.1 2>/dev/null | grep -m1 -E '^[0-9.]+$')"
  [ -n "$ip" ] || return 1
  curl -fsS -m 10 -o /dev/null --resolve "$DOMAIN:443:$ip" "https://$DOMAIN/api/health" 2>/dev/null
}

write_inputs() {
  [ -f "$ROOT/.env" ] || { echo ".env is missing; see docs/r2-setup.md" >&2; exit 1; }
  umask 077
  mkdir -p "$OVERLAY"
  {
    echo "S3_ENDPOINT=$(dotenv S3_ENDPOINT | sed 's#/*$##')"
    echo "S3_PUBLIC_ENDPOINT=$(dotenv S3_PUBLIC_ENDPOINT | sed 's#/*$##')"
    echo "S3_BUCKET=$(dotenv S3_BUCKET)"
    # The dev issuer lets anyone in, so the public instance caps uploads at
    # 4 GiB (the default is 50 GiB); see docs/production.md.
    echo "UPLOAD_MAX_SIZE=4294967296"
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
  # The manifests name the images by the moving tag "latest", so applying
  # them changes nothing once they are in place. Restarting the app
  # Deployments makes every "up" pull whatever CI has published since (the
  # pods pull on every start), one replica at a time; Postgres and Redis
  # keep running.
  k -n rigforge rollout restart deployment/api deployment/issuer deployment/web deployment/worker >/dev/null

  echo "==> 3/4 rollout"
  for workload in statefulset/postgres deployment/redis deployment/issuer deployment/api deployment/worker deployment/web; do
    k -n rigforge rollout status "$workload" --timeout=600s
  done

  echo "==> 4/4 checks"
  for _ in $(seq 1 30); do
    if ingress_up; then break; fi
    sleep 2
  done
  if ! ingress_up; then
    echo "the cluster's ingress does not answer on http://$INGRESS; something else may hold port 80 on this machine (lsof -nP -iTCP:80 -sTCP:LISTEN)" >&2
    exit 1
  fi
  # The public host name, resolved to the forwarded port, so the host-based
  # Ingress rule matches and the request takes the path cloudflared uses.
  CONNECT_TO="$DOMAIN:80:$INGRESS" "$ROOT/scripts/smoke_test.sh" "http://$DOMAIN"
  if pgrep -x cloudflared >/dev/null; then
    for _ in $(seq 1 15); do
      if public_up; then break; fi
      sleep 2
    done
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
    if ingress_up; then echo "ingress: http://$INGRESS answers for $DOMAIN"; else echo "ingress: http://$INGRESS does not answer"; fi
  fi
  if pgrep -x cloudflared >/dev/null; then echo "cloudflared: running on this machine"; else echo "cloudflared: not running on this machine (docs/production.md)"; fi
  if public_up; then echo "public: https://$DOMAIN is up"; else echo "public: https://$DOMAIN is not reachable"; fi
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
  sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
  ;;
esac
