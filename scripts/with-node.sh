#!/usr/bin/env bash
# Run a command with a Vite-compatible Node.js (20.19+ / 22.12+) on PATH.
#
#   scripts/with-node.sh npm ci
#   scripts/with-node.sh npm run build
#
# The frontend build entry points go through this wrapper so a machine whose
# default `node` is too old (Debian/Ubuntu apt ships 18.x) still builds when
# a newer Node exists under nvm / fnm / volta / asdf / homebrew. Resolution
# order and the version rule live in scripts/node-env.sh.
#
# Set NOVA_NODE_BIN=/path/to/node/bin to pin a specific installation.
set -euo pipefail

# shellcheck source=scripts/node-env.sh
source "$(dirname "${BASH_SOURCE[0]}")/node-env.sh"

if [[ $# -eq 0 ]]; then
  echo "usage: scripts/with-node.sh <command> [args...]" >&2
  exit 2
fi

current="$(nova_node_binary_version "$(command -v node 2>/dev/null || true)")"

if ! nova_activate_node; then
  echo "✗ 找不到满足要求的 Node.js（$(nova_node_requirement)）" >&2
  [[ -n "$current" ]] && echo "  当前 PATH 上的 node 为 v${current}，版本过低。" >&2
  nova_node_install_hint >&2
  exit 1
fi

resolved="$(nova_node_binary_version "$(command -v node)")"
if [[ -n "$current" && "$current" != "$resolved" ]]; then
  echo ">> node v${current} 不满足要求（$(nova_node_requirement)），改用 $(command -v node) (v${resolved})"
fi

exec "$@"
