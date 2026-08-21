#!/bin/bash
# NovaWorkbench — server one-shot bootstrap.
# Run as the SSH user (e.g. ubuntu) on a fresh Ubuntu 22.04+ host that has
# Docker installed and the public IP pointed at by nova.yishield.com.
#
# All paths live under the SSH user's home (~/nova/) so no root/sudo is
# needed for the recurring deploys that GitHub Actions runs.
#
# Required env:
#   Ali_Key           - Aliyun AccessKey ID (DNS-01 challenge)
#   Ali_Secret        - Aliyun AccessKey Secret
#
# Optional:
#   ACME_EMAIL        - Let's Encrypt registration email (default: admin@yishield.com)
set -euo pipefail

ACME_EMAIL="${ACME_EMAIL:-admin@yishield.com}"
CERT_DIR="/etc/nginx/ssl/nova.yishield.com"
NOVA_HOME="${HOME}/nova"
DEPLOY_DIR="${NOVA_HOME}/deploy"

echo ">>> [1/6] Creating shared 'nginx-proxy' Docker network"
docker network create nginx-proxy 2>/dev/null || true

echo ">>> [1b] Ensuring the SSH user is in the 'docker' group"
if ! id -nG "${USER}" | grep -qw docker; then
  if [[ "$(id -u)" -eq 0 ]]; then
    usermod -aG docker "${USER}"
  elif sudo -n true 2>/dev/null; then
    sudo usermod -aG docker "${USER}"
  else
    echo "!! cannot add ${USER} to docker group (no sudo / no root)" >&2
    exit 1
  fi
  # Active for any new shell; current process keeps its old groups.
  echo "    added ${USER} to docker group — re-login (or 'newgrp docker') for it to take effect"
fi

echo ">>> [2/6] Setting up nova home + syncing deploy/ dir"
mkdir -p "${NOVA_HOME}/prod/data" "${NOVA_HOME}/prod/workspace"
mkdir -p "${NOVA_HOME}/preview"
# deploy/ files were synced to ${NOVA_HOME}/deploy by GitHub Actions; if not
# yet (first bootstrap), fall back to a manual copy from this script's dir.
if [[ ! -f "${DEPLOY_DIR}/docker-compose.prod.yml" ]]; then
  echo "!! ${DEPLOY_DIR} missing or empty; copy deploy/ to ${NOVA_HOME}/deploy manually first" >&2
  exit 1
fi

echo ">>> [3/6] Starting nginx-proxy (loads /etc/nginx/ssl/nova.yishield.com as /etc/nginx/certs)"
mkdir -p /etc/nginx/vhost.d
docker compose -f "${DEPLOY_DIR}/docker-compose.nginx-proxy.yml" up -d

echo ">>> [4/6] Installing acme.sh"
curl -s https://get.acme.sh | sh -s email="${ACME_EMAIL}"
# shellcheck disable=SC1091
source ~/.bashrc

if [[ -z "${Ali_Key:-}" || -z "${Ali_Secret:-}" ]]; then
  echo "!! Ali_Key / Ali_Secret not set — re-run with: Ali_Key=... Ali_Secret=... bash init-server.sh" >&2
  exit 1
fi

echo ">>> [5/6] Issuing wildcard cert *.nova.yishield.com via Aliyun DNS-01"
~/.acme.sh/acme.sh --issue \
  --dns dns_ali \
  -d nova.yishield.com \
  -d '*.nova.yishield.com' \
  --server letsencrypt

echo ">>> [6/6] Installing cert into nginx-proxy cert dir"
mkdir -p "${CERT_DIR}"
~/.acme.sh/acme.sh --install-cert -d nova.yishield.com \
  --cert-file      "${CERT_DIR}/nova.yishield.com.crt" \
  --key-file       "${CERT_DIR}/nova.yishield.com.key" \
  --fullchain-file "${CERT_DIR}/nova.yishield.com.crt" \
  --reloadcmd      "docker kill --signal=HUP nginx-proxy"

echo ""
echo "✅ Init complete. acme.sh cron handles auto-renewal (30 days before expiry)."
echo "   - cert: ${CERT_DIR}"
echo "   - deploy root: ${NOVA_HOME}"
echo "   - nginx-proxy: docker logs nginx-proxy"