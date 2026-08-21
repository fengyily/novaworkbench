#!/bin/bash
# Production deploy — invoked by GitHub Actions via SSH.
# Required env (exported by caller):
#   GHCR_NAMESPACE, IMAGE_TAG
# Optional env:
#   ANTHROPIC_AUTH_TOKEN  - injected into the backend container so the
#                           wizard pipeline's claude CLI can authenticate.
#                           Without it the container still runs; only
#                           AI-driven features (requirement wizard, code
#                           generation) won't work.
set -euo pipefail

source "$(dirname "$0")/lib.sh"
ensure_docker_access "$@"

: "${GHCR_NAMESPACE:?GHCR_NAMESPACE required}"
: "${IMAGE_TAG:?IMAGE_TAG required}"

# Default to empty string when the secret isn't set so docker compose env
# substitution doesn't choke. The Go backend treats an empty token as "no
# claude CLI auth" — non-AI features still work.
export ANTHROPIC_AUTH_TOKEN="${ANTHROPIC_AUTH_TOKEN:-}"

NOVA_HOME="${HOME}/nova"
DEPLOY_DIR="${NOVA_HOME}/deploy"

cd "${DEPLOY_DIR}"

echo ">>> Pulling ghcr.io/${GHCR_NAMESPACE}/nova:${IMAGE_TAG}"
docker compose \
  -f docker-compose.prod.yml \
  --project-name nova-prod \
  pull backend

echo ">>> Starting / updating nova-prod stack"
docker compose \
  -f docker-compose.prod.yml \
  --project-name nova-prod \
  up -d --remove-orphans

echo ">>> Waiting for backend to become healthy"
for i in {1..30}; do
  if docker inspect --format='{{.State.Health.Status}}' nova-prod-backend 2>/dev/null | grep -q healthy; then
    echo "✅ nova-prod-backend is healthy"
    exit 0
  fi
  sleep 2
done

echo "!! nova-prod-backend did not become healthy in 60s" >&2
docker logs --tail=100 nova-prod-backend >&2 || true
exit 1