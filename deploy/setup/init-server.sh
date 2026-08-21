#!/bin/bash
# NovaWorkbench — server one-shot bootstrap (host-nginx model).
# Run as a user in the 'docker' group with passwordless sudo (the host nginx
# config + cert install need root).
#
# This sets up the shared fronting layer only:
#   - the shared 'nginx-proxy' Docker network,
#   - the internal nginx-proxy HTTP upstream (127.0.0.1:9580),
#   - the *.nova.yishield.com wildcard cert (Aliyun DNS-01, installed into the
#     host nginx cert dir),
#   - the host nginx *.nova.yishield.com server block (sites-enabled).
#
# Nova backend containers (prod + previews) are started by the per-deploy
# scripts; nginx-proxy discovers them by their VIRTUAL_HOST automatically.
#
# Required env:
#   Ali_Key / Ali_Secret — Aliyun AK/SK for the DNS-01 challenge
# Optional:
#   ACME_EMAIL        — Let's Encrypt registration email (default: admin@yishield.com)
#   NOVA_NGINX_PROXY_PORT — internal nginx-proxy port (default: 9580)
set -euo pipefail

source "$(dirname "$0")/lib.sh"

# Make sure the docker group is active for this session before we start
# containers (the per-deploy scripts re-use this helper too).
ensure_docker_access "$@"

ensure_nginx_proxy_network
ensure_nginx_proxy_container
# Order matters: the cert must be on disk before ensure_nova_nginx_vhost runs
# `nginx -t` against the ssl_certificate directives it writes.
ensure_wildcard_cert
ensure_nova_nginx_vhost

echo ""
echo "✅ Init complete."
echo "   - internal nginx-proxy: docker logs nginx-proxy (127.0.0.1:${NOVA_NGINX_PROXY_PORT})"
echo "   - host nginx vhost:      /etc/nginx/sites-enabled/nova"
echo "   - cert:                  ${NOVA_CERT_DIR}  (acme.sh cron renews; deploys also self-heal)"
echo "   - deploy root:           ${HOME}/nova"
