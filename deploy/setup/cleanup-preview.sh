#!/bin/bash
# Tear down a preview stack (invoked on PR close).
# Required env:
#   PROJECT_NAME
set -euo pipefail

source "$(dirname "$0")/lib.sh"
ensure_docker_access "$@"

: "${PROJECT_NAME:?PROJECT_NAME required}"

DEPLOY_DIR="${HOME}/nova/deploy"
NOVA_HOME="${HOME}/nova"

cd "${DEPLOY_DIR}"

echo ">>> Stopping + removing stack ${PROJECT_NAME}"
docker compose \
  -f docker-compose.preview.yml \
  --project-name "${PROJECT_NAME}" \
  down --remove-orphans || true

echo ">>> Removing persistent data for ${PROJECT_NAME}"
rm -rf "${NOVA_HOME}/preview/${PROJECT_NAME}" || true

echo "✅ Preview ${PROJECT_NAME} cleaned up"