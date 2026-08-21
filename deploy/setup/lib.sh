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

# ensure_wildcard_cert — idempotent *.nova.yishield.com issuance + renewal
# via Aliyun DNS-01. Safe to call on every deploy: issues only when absent,
# renews only within RENEW_DAYS of expiry, and only re-installs / reloads
# nginx-proxy when the cert content actually changed.
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
    echo ">>> Installing cert into nginx-proxy cert dir"
    sudo mkdir -p "${cert_dir}"
    sudo cp "${store_full}" "${installed_full}"
    sudo cp "${store_key}"  "${installed_key}"
    sudo chmod 644 "${installed_full}"
    sudo chmod 600 "${installed_key}"
    # HUP makes nginx re-read the cert files; fall back to a restart if the
    # signal can't be delivered (e.g. container freshly recreated).
    docker kill --signal=HUP nginx-proxy 2>/dev/null \
      || docker restart nginx-proxy 2>/dev/null \
      || echo "!! could not reload nginx-proxy — is the container running?" >&2
  else
    echo ">>> Wildcard cert up to date — no reload needed"
  fi
}