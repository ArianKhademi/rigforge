# Taking Rigforge to a public URL

The repo deploys to a local kind cluster today. This is the path to a hosted
deployment at `https://rigforge.khademi.tech`, what it costs, and what is
already prepared for it.

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
  anyone can upload videos into your bucket and use your CPU. The cheapest
  fix for a portfolio demo is to put the whole site behind Cloudflare Access
  (Zero Trust, free for up to 50 users): a one-time code to an allowed email
  before anything, including `/api` and `/issuer`, is reachable. The proper fix
  is a real identity provider: point `JWT_JWKS_URL`, `JWT_ISSUER` and
  `JWT_AUDIENCE` at it and delete the issuer Deployment.
- **Quotas.** There is no per-user storage or job limit yet (listed as future
  work in the brief). With Access in front this is manageable; without it, add
  a cap before going public.
- **Backups.** Postgres and Redis use local-path volumes on the single node. For
  a demo that is acceptable; the source of truth for outputs is the bucket.
