#!/bin/bash
# Preview deploy — invoked by GitHub Actions via SSH.
# Required env (exported by caller):
#   GHCR_NAMESPACE, IMAGE_TAG, ANTHROPIC_AUTH_TOKEN,
#   PROJECT_NAME, VIRTUAL_HOST
set -euo pipefail

: "${GHCR_NAMESPACE:?GHCR_NAMESPACE required}"
: "${IMAGE_TAG:?IMAGE_TAG required}"
: "${ANTHROPIC_AUTH_TOKEN:?ANTHROPIC_AUTH_TOKEN required}"
: "${PROJECT_NAME:?PROJECT_NAME required}"
: "${VIRTUAL_HOST:?VIRTUAL_HOST required}"

cd /srv/nova/deploy

echo ">>> Preparing persistent dirs"
mkdir -p "/srv/nova/preview/${PROJECT_NAME}/data"
mkdir -p "/srv/nova/preview/${PROJECT_NAME}/workspace"

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