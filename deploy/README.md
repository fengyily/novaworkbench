# Deploy

GitHub Actions CI/CD for NovaWorkbench.

## Branches → URLs

| Branch              | URL                                  |
|---------------------|--------------------------------------|
| `prod`              | `https://prod.nova.yishield.com`     |
| `feat/<req-id>`     | `https://<req-id>.nova.yishield.com` |

`<req-id>` is derived from the branch name: underscores / slashes become dashes,
then truncated to 63 chars. e.g. `feat/req_af3ca270364fb2b0` →
`req-af3ca270364fb2b0.nova.yishield.com`.

## Server one-shot setup

1. Install Docker on a fresh Ubuntu 22.04+ host.
2. Add the `*.nova.yishield.com` A-record and `prod.nova` A-record in Aliyun DNS,
   both pointing at the host's public IP.
3. Copy `deploy/` to `~/nova/` on the server (the GitHub workflow does this
   for every push, but the first push has no files yet — sync manually or
   bootstrap via `git clone`).
4. Run, as a user in the `docker` group:
   ```bash
   Ali_Key=... Ali_Secret=... bash ~/nova/deploy/setup/init-server.sh
   ```
   This:
   - creates the shared `nginx-proxy` Docker network,
   - starts the `nginxproxy/nginx-proxy` container,
   - installs `acme.sh` and issues `*.nova.yishield.com` via Aliyun DNS-01,
   - installs the cert into `/etc/nginx/ssl/nova.yishield.com/` so
     nginx-proxy picks it up automatically,
   - registers a daily cron for auto-renewal.

### Cert self-heal on every deploy

Every prod **and** preview deploy runs `ensure_wildcard_cert` (in
`deploy/setup/lib.sh`) before bringing the stack up:

- **cert absent** → issues `*.nova.yishield.com` via Aliyun DNS-01 (uses
  `Ali_Key`/`Ali_Secret`, mapped from the `ALI_ACCESS_KEY_ID`/`_SECRET`
  repo secrets by `deploy.yml`). Issuance failure **aborts** the deploy —
  we never ship to a cert-less site.
- **cert within `RENEW_DAYS` (default 30) of expiry** → renews; renewal
  failure only aborts if the installed cert is already expired, so a
  transient Let's Encrypt hiccup doesn't block all deploys.
- **cert fresh** → no-op (no reload).
- Installs the cert into `/etc/nginx/ssl/nova.yishield.com/` and HUPs
  `nginx-proxy`, but **only when the cert content changed**.

So a server that lost its cert, or whose bootstrap was never run,
self-heals on the next push — no manual `init-server.sh` needed for
(re)issuance. `init-server.sh` remains the canonical first-time bootstrap
(nginx-proxy container + network); the deploy flow then owns cert health.

## GitHub repository configuration

### Secrets (Settings → Secrets and variables → Actions → Secrets)

| Name                    | Purpose                                       |
|-------------------------|-----------------------------------------------|
| `SSH_PRIVATE_KEY`       | SSH key for the deploy user on the server     |
| `ALI_ACCESS_KEY_ID`     | Aliyun DNS API — cert self-heal (DNS-01) + Terraform |
| `ALI_ACCESS_KEY_SECRET` | Aliyun DNS API — cert self-heal (DNS-01) + Terraform |
| `ANTHROPIC_AUTH_TOKEN`  | Injected into the backend container at runtime |

### Variables (Settings → Secrets and variables → Actions → Variables)

| Name                   | Purpose                                                 |
|------------------------|---------------------------------------------------------|
| `PROD_SERVER_HOST`     | Hostname / IP of the production server                  |
| `PROD_SERVER_USER`     | SSH user (typically `ubuntu`)                           |
| `PREVIEW_SERVER_HOST`  | Preview server (defaults to `PROD_SERVER_HOST`)         |
| `PREVIEW_SERVER_USER`  | Preview SSH user (defaults to `PROD_SERVER_USER`)       |
| `GHCR_NAMESPACE`       | ghcr.io namespace (defaults to repo owner)              |

### First push

Before the first `prod` push, the workflow needs a packages:write token to push
images to GHCR. `GITHUB_TOKEN` already grants this — no extra setup required.
Make sure the repo's GHCR visibility matches your intent (Settings → Packages).

## Cleanup

- Preview environments are torn down automatically when their PR is closed
  (`cleanup-preview` job in `deploy.yml`).
- Manually delete a preview: `PROJECT_NAME=req-xxx bash deploy/setup/cleanup-preview.sh`
  on the server.

## Files

```
deploy/
├── docker-compose.prod.yml        # prod stack (image + env)
├── docker-compose.preview.yml     # preview stack template (env-driven)
├── docker-compose.nginx-proxy.yml # shared nginx-proxy (one-time)
└── setup/
    ├── init-server.sh             # one-time bootstrap
    ├── deploy-prod.sh             # called by GH Actions on prod push
    ├── deploy-preview.sh          # called by GH Actions on feat/* push
    └── cleanup-preview.sh         # called when a PR is closed
```