#!/bin/bash
# Tear down a preview stack (invoked on PR close).
# Required env:
#   PROJECT_NAME
set -euo pipefail

: "${PROJECT_NAME:?PROJECT_NAME required}"

cd /srv/nova/deploy

echo ">>> Stopping + removing stack ${PROJECT_NAME}"
docker compose \
  -f docker-compose.preview.yml \
  --project-name "${PROJECT_NAME}" \
  down --remove-orphans || true

echo ">>> Removing persistent data for ${PROJECT_NAME}"
rm -rf "/srv/nova/preview/${PROJECT_NAME}" || true

echo "✅ Preview ${PROJECT_NAME} cleaned up"