# Taking Rigforge to a public URL

The repo deploys to a local kind cluster today. This is the path to a hosted
deployment at `https://rigforge.khademi.tech`, what it costs, and what is
already prepared for it.

## The on-demand option: this Mac

A hosted URL does not need a server in a data centre. `make hosted-up` creates
a k3s VM on this machine (Lima, 4 CPU / 4 GB), deploys the `tunnel` overlay
(R2, GHCR images, one worker) and checks it; Lima forwards the VM's ingress
to the host's port 80, and a [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/)
run by `cloudflared` on the host publishes that as
`https://rigforge.khademi.tech` with no open ports and no certificate to
manage. `make hosted-down` stops the VM and the URL goes dark until the next
`up`; the tunnel itself can stay up (cloudflared idles at about 30 MB) and
Cloudflare answers with its own error page while the VM is off. The VM costs
nothing while off. Measured while up and idle (Activity Monitor's figure for
the VM process, on an M4 Mac mini): 3.4 GB of memory and about a fifth of one
core, nearly all of it k3s itself; inside the guest the stack uses 1.5 GB.
The images are built for `linux/arm64` as well as `linux/amd64` by CI, which
is what makes this work on Apple Silicon.

One-time setup, with the tunnel shared by every app hosted on the machine
(one `cloudflared`, one host name per app):

1. Move `khademi.tech` to Cloudflare DNS (Add a domain, Free plan, change the
   nameservers at the registrar) and wait until the zone is active.
2. `brew install cloudflared`, then

   ```bash
   cloudflared tunnel login                      # opens the browser once
   cloudflared tunnel create mac-mini
   cloudflared tunnel route dns mac-mini rigforge.khademi.tech
   ```

   and `~/.cloudflared/config.yml`:

   ```yaml
   tunnel: <tunnel id>
   credentials-file: /Users/<you>/.cloudflared/<tunnel id>.json
   ingress:
     - hostname: rigforge.khademi.tech
       service: http://localhost:80 # the VM's ingress, forwarded by Lima
     # other apps on this machine go here, one hostname each
     - service: http_status:404
   ```

   `cloudflared tunnel run mac-mini` to try it, `sudo cloudflared service
   install` to keep it running across logins. cloudflared passes the
   original `Host` header through, which is what the host-based Ingress
   rule matches on.
3. Add `https://rigforge.khademi.tech` to the bucket's CORS origins
   (`docs/r2-cors.json`).

`scripts/hosted.sh status` reports the VM, the pods, whether the ingress
answers on the forwarded port, whether cloudflared is running and whether the
public URL responds.

The VPS path below is the always-on alternative.

## What is already there

- Images for all four services, built and pushed to GHCR by CI on every push
  to `main` (`.github/workflows/ci.yml`, job `images`).
- `deploy/k8s/overlays/production`: the base manifests plus R2 as the bucket, a
  host-based Ingress with TLS from cert-manager, and the image names on GHCR.
- A deploy-on-tag job in CI, gated on the repository variable
  `DEPLOY_ENABLED`, that applies that overlay with the tag's image versions.
- `scripts/smoke_test.sh <url>` to check a deployment from outside.

## Options

| Option | Monthly cost (approx.) | Notes |
| --- | --- | --- |
| **k3s on one VPS** (recommended) | 7-15 USD for 4 vCPU / 8 GB (Hetzner CPX31/CX32, DigitalOcean, Vultr) | Single node, Traefik ingress and local storage included with k3s. Plenty for a demo; workers need the memory (MediaPipe + ffmpeg, about 1 GB each). |
| Managed Kubernetes (DOKS, Linode LKE, Civo) | 25-40 USD (node + load balancer) | Real control plane, more parts to configure, nothing the demo needs. |
| GKE Autopilot / EKS / AKS | 70+ USD | Overkill for this. |

The rest assumes k3s on a VPS.

## One command

Once the VPS exists and DNS points at it, steps 3 to 8 below are one script:

```bash
ACME_EMAIL=you@example.com scripts/deploy_production.sh root@<ip>
```

It installs k3s over ssh, fetches the kubeconfig to `tmp/kubeconfig-production`,
installs cert-manager, applies the overlay, waits for the certificate and runs
the smoke test against `https://rigforge.khademi.tech`. The manual steps are
kept here for reference and for debugging.

## Steps

1. **VPS.** Ubuntu 24.04, 4 vCPU, 8 GB RAM, 80 GB disk, your SSH key. Note
   the public IP.
2. **DNS.** In the `khademi.tech` zone add an `A` record `rigforge` → the VPS
   IP. If the zone is on Cloudflare, keep the record **DNS only** (grey cloud)
   until TLS works, or cert-manager's HTTP challenge will be proxied.
3. **k3s.** On the VPS:
   ```bash
   curl -sfL https://get.k3s.io | sh -
   ```
   This installs Kubernetes with Traefik as the default ingress controller and
   a local-path StorageClass, both of which the manifests expect.
4. **kubeconfig on your machine.**
   ```bash
   scp root@<ip>:/etc/rancher/k3s/k3s.yaml ~/.kube/rigforge-prod.yaml
   sed -i '' 's/127.0.0.1/<ip>/' ~/.kube/rigforge-prod.yaml
   export KUBECONFIG=~/.kube/rigforge-prod.yaml
   kubectl get nodes
   ```
5. **cert-manager** for Let's Encrypt certificates:
   ```bash
   kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
   kubectl -n cert-manager rollout status deployment/cert-manager-webhook
   kubectl apply -f deploy/k8s/overlays/production/cluster-issuer.yaml   # edit the email first
   ```
6. **Images.** Push the repo to GitHub; CI publishes
   `ghcr.io/ariankhademi/rigforge-{api,issuer,worker,web}`. Make the four
   packages public (GitHub → Packages → package → Package settings → Change
   visibility), or create an image pull secret and add it to the overlay.
7. **R2.** Follow [`docs/r2-setup.md`](r2-setup.md); add
   `https://rigforge.khademi.tech` to the bucket's CORS origins. Put the values
   in `deploy/k8s/overlays/production/config.env` and `secret.env`, and
   generate the issuer key:
   ```bash
   cd deploy/k8s/overlays/production
   cp config.env.example config.env && cp ../../base/secret.env.example secret.env
   openssl genrsa -out issuer-key.pem 2048
   ```
8. **Deploy.**
   ```bash
   kubectl apply -k deploy/k8s/overlays/production
   kubectl -n rigforge rollout status deployment/api
   scripts/smoke_test.sh https://rigforge.khademi.tech
   ```
9. **Deploy on tag from CI** (optional). Repository variable
   `DEPLOY_ENABLED=true`; secrets `KUBE_CONFIG` (the file from step 4),
   `R2_CONFIG_ENV`, `R2_SECRET_ENV` and `ISSUER_KEY_PEM` (the contents of the
   three files from step 7). Then `git tag v0.1.0 && git push --tags`.

## Before announcing the URL

- **The dev issuer gives a token to anyone.** On a public URL that means
  anyone can upload videos into your bucket and use your CPU. Cloudflare
  Access does not fit a portfolio link (it needs a list of allowed visitors
  or an email domain; strangers have neither), so the demo relies on three
  smaller limits: a Cloudflare rate-limiting rule (Security → WAF → Rate
  limiting rules, one is free: match `http.request.method eq "POST" and
  http.request.uri.path eq "/api/uploads"`, 5 requests per 10 seconds per
  IP, block for 10 seconds), the api's size cap (`UPLOAD_MAX_SIZE`, set to
  4 GiB in the hosted overlay instead of the 50 GiB default), and the VM
  being up only while someone is showing the demo. The proper fix is a real
  identity provider: point `JWT_JWKS_URL`, `JWT_ISSUER` and `JWT_AUDIENCE`
  at it and delete the issuer Deployment.
- **Quotas.** There is no per-user storage or job limit yet (listed as future
  work in the brief); the limits above bound the damage but do not meter
  anyone. R2's free tier (10 GB-month) is the backstop: it refuses writes
  rather than billing.
- **Backups.** Postgres and Redis use local-path volumes on the single node. For
  a demo that is acceptable; the source of truth for outputs is the bucket.
