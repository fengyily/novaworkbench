#!/bin/bash
# NovaWorkbench — server one-shot bootstrap.
# Run as the SSH user (e.g. ubuntu) on a fresh Ubuntu 22.04+ host that has
# Docker installed and the public IP pointed at by nova.yishield.com.
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

echo ">>> [1/5] Creating shared 'nginx-proxy' Docker network"
docker network create nginx-proxy 2>/dev/null || true

echo ">>> [2/5] Starting nginx-proxy (loads /etc/nginx/ssl/nova.yishield.com as /etc/nginx/certs)"
mkdir -p /etc/nginx/vhost.d
docker compose -f /srv/nova/deploy/docker-compose.nginx-proxy.yml up -d

echo ">>> [3/5] Installing acme.sh"
curl -s https://get.acme.sh | sh -s email="${ACME_EMAIL}"
# shellcheck disable=SC1091
source ~/.bashrc

if [[ -z "${Ali_Key:-}" || -z "${Ali_Secret:-}" ]]; then
  echo "!! Ali_Key / Ali_Secret not set — exporting and retrying in caller shell" >&2
  echo "!! Re-run with: Ali_Key=... Ali_Secret=... bash init-server.sh" >&2
  exit 1
fi

echo ">>> [4/5] Issuing wildcard cert *.nova.yishield.com via Aliyun DNS-01"
~/.acme.sh/acme.sh --issue \
  --dns dns_ali \
  -d nova.yishield.com \
  -d '*.nova.yishield.com' \
  --server letsencrypt

echo ">>> [5/5] Installing cert into nginx-proxy cert dir"
mkdir -p "${CERT_DIR}"
~/.acme.sh/acme.sh --install-cert -d nova.yishield.com \
  --cert-file      "${CERT_DIR}/nova.yishield.com.crt" \
  --key-file       "${CERT_DIR}/nova.yishield.com.key" \
  --fullchain-file "${CERT_DIR}/nova.yishield.com.crt" \
  --reloadcmd      "docker kill --signal=HUP nginx-proxy"

# Persistent data roots
mkdir -p /srv/nova/prod/data /srv/nova/prod/workspace
mkdir -p /srv/nova/preview

echo ""
echo "✅ Init complete. acme.sh cron handles auto-renewal (30 days before expiry)."
echo "   - cert: ${CERT_DIR}"
echo "   - nginx-proxy: docker logs nginx-proxy"