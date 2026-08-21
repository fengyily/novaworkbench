#!/bin/bash
# Preview deploy — invoked by GitHub Actions via SSH.
# Required env (exported by caller):
#   GHCR_NAMESPACE, IMAGE_TAG, PROJECT_NAME, VIRTUAL_HOST
# Optional env:
#   ANTHROPIC_AUTH_TOKEN  - injected into the backend container so the
#                           wizard pipeline's claude CLI can authenticate.
#                           Without it the container still runs; only
#                           AI-driven features (requirement wizard, code
#                           generation) won't work.
set -euo pipefail

source "$(dirname "$0")/lib.sh"
ensure_docker_access "$@"
ensure_nginx_proxy_network
ensure_nginx_proxy_container
ensure_wildcard_cert
ensure_nova_nginx_vhost

: "${GHCR_NAMESPACE:?GHCR_NAMESPACE required}"
: "${IMAGE_TAG:?IMAGE_TAG required}"
: "${PROJECT_NAME:?PROJECT_NAME required}"
: "${VIRTUAL_HOST:?VIRTUAL_HOST required}"

# Default to empty string when the secret isn't set so docker compose env
# substitution doesn't choke. The Go backend treats an empty token as "no
# claude CLI auth" — non-AI features still work.
export ANTHROPIC_AUTH_TOKEN="${ANTHROPIC_AUTH_TOKEN:-}"

NOVA_HOME="${HOME}/nova"
DEPLOY_DIR="${NOVA_HOME}/deploy"

cd "${DEPLOY_DIR}"

echo ">>> Preparing persistent dirs"
# Shared across all preview environments (one DB for every req-xxx).
mkdir -p "${NOVA_HOME}/preview/data"
# Per-project workspace (different branches clone different repos).
mkdir -p "${NOVA_HOME}/preview/${PROJECT_NAME}/workspace"

echo ">>> Pulling ghcr.io/${GHCR_NAMESPACE}/nova:${IMAGE_TAG}"
docker compose \
  -f docker-compose.preview.yml \
  --project-name "${PROJECT_NAME}" \
  pull backend

echo ">>> Starting / updating ${PROJECT_NAME} stack (VIRTUAL_HOST=${VIRTUAL_HOST})"
docker compose \
  -f docker-compose.preview.yml \
  --project-name "${PROJECT_NAME}" \
  up -d --remove-orphans

echo ">>> Waiting for backend to become healthy"
CONTAINER="${PROJECT_NAME}-backend-1"
for i in {1..30}; do
  if docker inspect --format='{{.State.Health.Status}}' "${CONTAINER}" 2>/dev/null | grep -q healthy; then
    echo "✅ ${CONTAINER} is healthy — https://${VIRTUAL_HOST}"
    exit 0
  fi
  sleep 2
done

echo "!! ${CONTAINER} did not become healthy in 60s" >&2
docker logs --tail=100 "${CONTAINER}" >&2 || true
exit 1