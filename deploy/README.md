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

## Fronting model

The server runs a **host nginx** as the public front (it also serves other
apps on `yishield.com`). NovaWorkbench plugs into it rather than running its
own TLS terminator:

```
client ──https──> host nginx :443  (TLS, *.nova.yishield.com cert)
                       │  proxy_pass http://127.0.0.1:9580  (Host preserved)
                       ▼
              nginx-proxy container  (HTTP only, internal)
                       │  routes by VIRTUAL_HOST → backend container
                       ▼
        nova backend (prod.nova / req-xxx.nova) on the 'nginx-proxy' network
```

- **One wildcard vhost + one cert** (`*.nova.yishield.com`) covers
  `prod.nova.yishield.com` and every `req-xxx.nova.yishield.com` preview.
- **Per-host routing is automatic**: each nova backend sets `VIRTUAL_HOST` +
  `VIRTUAL_PORT=9527` and joins the `nginx-proxy` network; nginx-proxy discovers
  it via the docker socket and routes by `Host`. No per-preview host vhost and
  no per-preview port allocation.
- nginx-proxy is **HTTP-only behind host nginx** (bound to `127.0.0.1:9580`); it
  does not own 80/443, so it does not collide with the host nginx or other apps.

## Server one-shot setup

1. Install Docker on an Ubuntu 22.04+ host whose **host nginx** already serves
   `:80`/`:443` (e.g. it already runs the appshield apps). The deploy user
   needs passwordless `sudo` (host nginx config + cert install need root).
2. Add the `*.nova.yishield.com` and `prod.nova` A-records in Aliyun DNS (or via
   the Terraform workflow), both pointing at the host's public IP.
3. Copy `deploy/` to `~/nova/` on the server (the GitHub workflow does this
   for every push; bootstrap the first copy via `git clone` or manual sync).
4. Run, as a user in the `docker` group:
   ```bash
   Ali_Key=... Ali_Secret=... bash ~/nova/deploy/setup/init-server.sh
   ```
   This:
   - creates the shared `nginx-proxy` Docker network,
   - starts the internal `nginx-proxy` HTTP upstream (`127.0.0.1:9580`),
   - installs `acme.sh` and issues `*.nova.yishield.com` via Aliyun DNS-01,
   - installs the cert into `/etc/nginx/ssl/nova.yishield.com/`,
   - writes the host-nginx `*.nova.yishield.com` server block into
     `/etc/nginx/sites-enabled/nova` and reloads host nginx,
   - registers a daily cron for auto-renewal.

### Cert self-heal on every deploy

Every prod **and** preview deploy runs `ensure_wildcard_cert` +
`ensure_nova_nginx_vhost` (in `deploy/setup/lib.sh`) before bringing the
backend up:

- **cert absent** → issues `*.nova.yishield.com` via Aliyun DNS-01 (uses
  `Ali_Key`/`Ali_Secret`, mapped from the `ALI_ACCESS_KEY_ID`/`_SECRET`
  repo secrets by `deploy.yml`). Issuance failure **aborts** the deploy —
  we never ship to a cert-less site.
- **cert within `RENEW_DAYS` (default 30) of expiry** → renews; renewal
  failure only aborts if the installed cert is already expired, so a
  transient Let's Encrypt hiccup doesn't block all deploys.
- **cert fresh** → no-op (no reload).
- Re-installs the cert into `/etc/nginx/ssl/nova.yishield.com/` and reloads
  **host nginx**, but **only when the cert content changed**.
- Also (re)writes the `*.nova.yishield.com` host-nginx vhost if it changed,
  running **after** the cert so `nginx -t` sees the cert files. The internal
  nginx-proxy container is started too if missing.

So a server that lost its cert, or whose bootstrap was never run,
self-heals on the next push — no manual `init-server.sh` needed for
(re)issuance or routing. `init-server.sh` remains the canonical first-time
bootstrap (network + nginx-proxy + cert + vhost); the deploy flow then owns
cert + vhost health.

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
├── docker-compose.nginx-proxy.yml # internal HTTP upstream behind host nginx (one-time)
└── setup/
    ├── init-server.sh             # one-time bootstrap
    ├── deploy-prod.sh             # called by GH Actions on prod push
    ├── deploy-preview.sh          # called by GH Actions on feat/* push
    └── cleanup-preview.sh         # called when a PR is closed
```