#!/bin/bash
# Shared helpers for the deploy / cleanup scripts.
# Source from each script:
#   source "$(dirname "$0")/lib.sh"
#
# Provides:
#   ensure_docker_access   - add the SSH user to the docker group on demand
#                            and re-exec the current script under that group
#                            so docker compose / docker inspect just work.

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