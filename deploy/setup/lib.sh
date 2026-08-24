#!/bin/bash
# Shared helpers for the deploy / cleanup scripts.
# Source from each script:
#   source "$(dirname "$0")/lib.sh"
#
# Provides:
#   ensure_docker_access       - add the SSH user to the docker group on
#                                demand and re-exec the current script
#                                under that group so docker compose /
#                                docker inspect just work.
#   ensure_nginx_proxy_network - create the shared 'nginx-proxy' Docker
#                                network if it doesn't already exist
#                                (init-server.sh is the canonical bootstrap
#                                but a fresh server may not have run it).

set -euo pipefail

# --- NovaWorkbench fronting model -----------------------------------------
# The host's nginx terminates TLS for *.nova.yishield.com and proxies to an
# INTERNAL nginx-proxy container (HTTP only) bound to 127.0.0.1. That container
# auto-routes by each backend's VIRTUAL_HOST env, so prod.nova + every
# req-xxx.nova preview are routed dynamically — no per-preview host vhost and
# no per-preview port allocation. A single wildcard vhost + cert covers all.
NOVA_DOMAIN="nova.yishield.com"
NOVA_CERT_DIR="/etc/nginx/ssl/${NOVA_DOMAIN}"
NOVA_NGINX_PROXY_PORT="${NOVA_NGINX_PROXY_PORT:-9580}"
NOVA_DEPLOY_DIR="${HOME}/nova/deploy"

ensure_docker_access() {
  # Already reachable → nothing to do.
  if docker info >/dev/null 2>&1; then
    return 0
  fi

  echo ">>> docker.sock not reachable — attempting to add ${USER} to the 'docker' group"

  # Try sudo first (typical cloud VMs have passwordless sudo), then bare
  # usermod (works when invoked as root).
  if ! sudo -n true 2>/dev/null && [[ "$(id -u)" -ne 0 ]]; then
    echo "!! passwordless sudo not available — please run on the server once:" >&2
    echo "   sudo usermod -aG docker ${USER} && exit && ssh back in" >&2
    exit 1
  fi

  sudo usermod -aG docker "${USER}" 2>/dev/null \
    || usermod -aG docker "${USER}" \
    || { echo "!! failed to add ${USER} to docker group" >&2; exit 1; }

  echo ">>> added ${USER} to docker group; re-execing this script under 'sg docker'"

  # `sg` spawns a new login session with the docker group active. Re-running
  # the same script under that group makes the rest of the deploy work
  # without further changes.
  exec sg docker -c "$(printf '%q ' "$0" "$@")"
}

ensure_nginx_proxy_network() {
  if docker network inspect nginx-proxy >/dev/null 2>&1; then
    return 0
  fi
  echo ">>> nginx-proxy network missing — creating"
  docker network create nginx-proxy
  echo "   (network created; nginx-proxy container + certs still need init-server.sh)"
}

# ensure_nginx_proxy_container — start the internal nginx-proxy HTTP upstream
# (bound to 127.0.0.1, behind the host nginx which does TLS). Idempotent.
ensure_nginx_proxy_container() {
  if docker ps --format '{{.Names}}' | grep -qx nginx-proxy; then
    return 0
  fi
  local f="${NOVA_DEPLOY_DIR}/docker-compose.nginx-proxy.yml"
  if [[ ! -f "${f}" ]]; then
    echo "!! ${f} missing — sync deploy/ to ${NOVA_DEPLOY_DIR} first" >&2
    exit 1
  fi
  echo ">>> starting nginx-proxy (internal HTTP upstream on 127.0.0.1:${NOVA_NGINX_PROXY_PORT})"
  docker compose -f "${f}" up -d
}

# ensure_nova_nginx_vhost — write the static *.nova.yishield.com server block
# into the HOST nginx (sites-enabled), idempotent. One wildcard server covers
# prod.nova + every req-xxx.nova preview; the internal nginx-proxy routes by
# Host to the matching backend. MUST run after ensure_wildcard_cert so the cert
# files exist before `nginx -t` validates the ssl_certificate directives.
ensure_nova_nginx_vhost() {
  local avail="/etc/nginx/sites-available/nova"
  local enabled="/etc/nginx/sites-enabled/nova"
  sudo mkdir -p /etc/nginx/sites-available /etc/nginx/sites-enabled /etc/nginx/vhost.d

  local tmp
  tmp=$(mktemp)
  cat > "${tmp}" <<EOF
# Managed by NovaWorkbench deploy (ensure_nova_nginx_vhost) — do not edit by hand.
# Host nginx terminates TLS for *.nova.yishield.com, then forwards to the
# internal nginx-proxy which routes by Host to the matching backend container.
#
# HTTP/2 is intentionally DISABLED (`listen 443 ssl` instead of
# `listen 443 ssl http2`) because the upstream (nginx-proxy → Go backend)
# serves SSE over HTTP/1.1 chunked transfer encoding. When this vhost
# terminates HTTP/2 on the client side, nginx has to repackage every
# upstream chunked SSE frame as an HTTP/2 DATA frame; long-lived streams
# then trigger `net::ERR_HTTP2_PROTOCOL_ERROR 200 (OK)` in the browser
# (flow control / trailing-headers edge cases), which discards the entire
# SSE response and leaves the UI with no output. Reverting to plain
# HTTPS/HTTP/1.1 gives SSE a stable end-to-end path. Re-enable http2 once
# the upstream chain supports native HTTP/2.
server {
    listen 443 ssl;
    server_name *.nova.yishield.com nova.yishield.com;

    ssl_certificate     ${NOVA_CERT_DIR}/${NOVA_DOMAIN}.crt;
    ssl_certificate_key ${NOVA_CERT_DIR}/${NOVA_DOMAIN}.key;

    location / {
        proxy_pass http://127.0.0.1:${NOVA_NGINX_PROXY_PORT};
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
        # Force HTTP/1.1 to nginx-proxy (already HTTP-only upstream) and
        # disable all buffering so each SSE frame is flushed to the browser
        # immediately. SSE backend responses carry X-Accel-Buffering: no;
        # this is the belt-and-braces version for any non-SSE streaming path.
        proxy_http_version 1.1;
        proxy_buffering off;
        proxy_cache off;
        proxy_set_header Connection "";
        chunked_transfer_encoding on;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}

server {
    listen 80;
    server_name *.nova.yishield.com nova.yishield.com;
    return 301 https://\$host\$request_uri;
}
EOF

  local changed=0
  if [[ ! -f "${avail}" ]] || ! sudo cmp -s "${tmp}" "${avail}"; then
    sudo cp "${tmp}" "${avail}"
    changed=1
  fi
  rm -f "${tmp}"
  if [[ ! -L "${enabled}" ]]; then
    sudo ln -sf "${avail}" "${enabled}"
    changed=1
  fi
  if [[ "${changed}" -eq 1 ]]; then
    echo ">>> (re)installed host nginx vhost for *.nova.yishield.com; reloading nginx"
    sudo nginx -t && sudo nginx -s reload
  fi
}

# ensure_nginx_proxy_sse_tuning — write a vhost.d/default override into the
# INTERNAL nginx-proxy container so its proxy→backend leg matches the host
# nginx's SSE tuning (proxy_buffering off, proxy_read_timeout 3600s). The host
# nginx leg is already tuned by ensure_nova_nginx_vhost; the internal
# nginx-proxy image defaults to proxy_read_timeout 60s + proxy_buffering on,
# which tears down long-lived SSE streams (claude design/coding jobs) during
# claude's silent thinking gaps. nginx-proxy includes /etc/nginx/vhost.d/default
# in every vhost server block when no host-specific override exists, so one
# file covers prod.nova + every req-xxx.nova preview.
#
# Idempotent: rewrites + regenerates only on content change. The backend also
# emits a 15s SSE comment heartbeat (sse.go), so this is belt-and-braces.
# Safe: the file holds only proxy_* directives (valid in http/server/location),
# so it cannot fail `nginx -t`; if regeneration ever leaves the container down,
# the override is removed and the container restarted to restore routing.
ensure_nginx_proxy_sse_tuning() {
  local dir="/etc/nginx/vhost.d"
  local f="${dir}/default"
  sudo mkdir -p "${dir}"

  local tmp
  tmp=$(mktemp)
  cat > "${tmp}" <<'EOF'
# Managed by NovaWorkbench deploy (ensure_nginx_proxy_sse_tuning) — do not edit.
# SSE / long-poll tuning for the internal nginx-proxy → backend leg. The host
# nginx already disables buffering and raises proxy_read_timeout to 3600s; this
# mirrors it here so the internal proxy's 60s default doesn't tear down
# long-lived SSE streams (claude design/coding jobs) during silent thinking
# gaps. (The backend also emits a 15s SSE comment heartbeat, so this is
# belt-and-braces.)
proxy_read_timeout 3600s;
proxy_send_timeout 3600s;
proxy_buffering off;
proxy_cache off;
chunked_transfer_encoding on;
EOF

  local changed=0
  if [[ ! -f "${f}" ]] || ! sudo cmp -s "${tmp}" "${f}"; then
    sudo cp "${tmp}" "${f}"
    changed=1
  fi
  rm -f "${tmp}"
  if [[ "${changed}" -ne 1 ]]; then
    return 0
  fi

  echo ">>> (re)installed nginx-proxy vhost.d/default SSE tuning; regenerating nginx-proxy config"
  # Restart (not reload) so docker-gen regenerates the config and the
  # vhost.d/default include takes effect; a plain `nginx -s reload` would
  # reuse the previously generated config that lacks the include.
  docker restart nginx-proxy >/dev/null

  # Safety: if the regenerated config is somehow invalid and the container
  # fails to come back up, remove the override and restart to restore routing.
  local up=0
  for _ in {1..10}; do
    if docker inspect --format='{{.State.Running}}' nginx-proxy 2>/dev/null | grep -q true; then
      up=1
      break
    fi
    sleep 1
  done
  if [[ "${up}" -ne 1 ]]; then
    echo "!! nginx-proxy failed to restart after SSE tuning — reverting vhost.d/default" >&2
    sudo rm -f "${f}"
    docker restart nginx-proxy >/dev/null
  fi
}

# ensure_wildcard_cert — idempotent *.nova.yishield.com issuance + renewal
# via Aliyun DNS-01. Safe to call on every deploy: issues only when absent,
# renews only within RENEW_DAYS of expiry, and only re-installs / reloads the
# host nginx when the cert content actually changed.
#
# Credentials: the acme dns_ali plugin reads Ali_Key / Ali_Secret from the
# env. deploy.yml maps them from the ALI_ACCESS_KEY_ID / _SECRET repo secrets.
#
# Exit policy ("A"): never ship to a cert-less site, but don't let a transient
# renewal hiccup block all deploys.
#   - cert MISSING and we can't issue (no creds / issuance fails) -> exit 1
#   - cert present but renewal fails, installed cert still valid       -> warn + continue
#   - cert present but renewal fails, installed cert missing/expired    -> exit 1
ensure_wildcard_cert() {
  local domain="nova.yishield.com"
  local cert_dir="/etc/nginx/ssl/${domain}"
  local installed_full="${cert_dir}/${domain}.crt"
  local installed_key="${cert_dir}/${domain}.key"
  local renew_days="${RENEW_DAYS:-30}"
  local acme="${HOME}/.acme.sh/acme.sh"

  # Locate the acme.sh store dir for this domain (ECC by default, RSA as fallback).
  local store_dir=""
  local d
  for d in "${HOME}/.acme.sh/${domain}_ecc" "${HOME}/.acme.sh/${domain}"; do
    if [[ -f "${d}/fullchain.cer" ]]; then
      store_dir="${d}"
      break
    fi
  done
  local store_full="${store_dir:+${store_dir}/fullchain.cer}"
  local store_key="${store_dir:+${store_dir}/${domain}.key}"

  local have_creds=0
  if [[ -n "${Ali_Key:-}" && -n "${Ali_Secret:-}" ]]; then
    have_creds=1
  fi

  # Is the currently-installed cert still valid (not past expiry)?
  installed_valid() {
    [[ -f "${installed_full}" ]] \
      && openssl x509 -in "${installed_full}" -noout -checkend 0 >/dev/null 2>&1
  }

  if [[ "${have_creds}" -eq 0 ]]; then
    if installed_valid; then
      echo "!! Ali_Key / Ali_Secret not set — cannot renew the wildcard cert, but the installed one is still valid. Continuing." >&2
      return 0
    fi
    echo "!! Wildcard cert missing or expired and no Ali_Key / Ali_Secret to (re)issue — aborting deploy." >&2
    echo "   Add ALI_ACCESS_KEY_ID / ALI_ACCESS_KEY_SECRET repo secrets and pass them via deploy.yml." >&2
    exit 1
  fi

  # acme.sh is normally installed by init-server.sh; bootstrap on demand so a
  # fresh server self-heals even before the one-shot bootstrap ran.
  if [[ ! -x "${acme}" ]]; then
    echo ">>> acme.sh not found — installing"
    curl -s https://get.acme.sh | sh -s email="${ACME_EMAIL:-admin@yishield.com}"
  fi

  if [[ -z "${store_dir}" ]]; then
    echo ">>> No wildcard cert for ${domain} — issuing *.${domain} via Aliyun DNS-01"
    if ! ~/.acme.sh/acme.sh --issue \
        --dns dns_ali \
        -d "${domain}" \
        -d "*.${domain}" \
        --server letsencrypt; then
      echo "!! Issuance failed — aborting deploy (site would have no valid cert)." >&2
      exit 1
    fi
    # Refresh store paths after issuance (store dir is now populated).
    for d in "${HOME}/.acme.sh/${domain}_ecc" "${HOME}/.acme.sh/${domain}"; do
      if [[ -f "${d}/fullchain.cer" ]]; then
        store_dir="${d}"
        break
      fi
    done
    store_full="${store_dir}/fullchain.cer"
    store_key="${store_dir}/${domain}.key"
  else
    # Renew only when within renew_days of expiry; acme.sh skips otherwise.
    if ! ~/.acme.sh/acme.sh --renew -d "${domain}" --days "${renew_days}"; then
      if installed_valid; then
        echo "!! Renewal failed but installed cert is still valid — continuing; will retry next deploy." >&2
      else
        echo "!! Renewal failed and installed cert is missing/expired — aborting deploy." >&2
        exit 1
      fi
    fi
  fi

  # Re-install to nginx-proxy only when content changed (or first install).
  local need_install=0
  if [[ ! -f "${installed_full}" ]] || [[ ! -f "${installed_key}" ]]; then
    need_install=1
  elif ! diff -q "${store_full}" "${installed_full}" >/dev/null 2>&1; then
    need_install=1
  fi

  if [[ "${need_install}" -eq 1 ]]; then
    echo ">>> Installing cert into host nginx cert dir"
    sudo mkdir -p "${cert_dir}"
    sudo cp "${store_full}" "${installed_full}"
    sudo cp "${store_key}"  "${installed_key}"
    sudo chmod 644 "${installed_full}"
    sudo chmod 600 "${installed_key}"
    # The host nginx terminates TLS with these files; reload it to pick up
    # the new cert. (The internal nginx-proxy is HTTP-only, no cert reload
    # needed there.) ensure_nova_nginx_vhost runs after this to write/reload
    # the vhost that references these files.
    sudo nginx -t && sudo nginx -s reload
  else
    echo ">>> Wildcard cert up to date — no reload needed"
  fi
}