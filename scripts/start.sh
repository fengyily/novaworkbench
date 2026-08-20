#!/usr/bin/env bash
# Cross-platform launcher for the NovaWorkbench single-binary release.
# Picks the binary next to this script, ensures ~/.novaworkbench exists,
# forwards NOVA_PORT and other env, and exec's into the server.
#
# Usage: ./start.sh                # default port 9527
#        NOVA_PORT=9000 ./start.sh # override port
#        CLAUDE_BIN=/x/claude ./start.sh
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ "$(uname -s)" == "MINGW"* || "$(uname -s)" == "CYGWIN"* || "$(uname -s)" == "MSYS"* ]]; then
  echo "On Windows, use start.ps1 instead." >&2
  exit 1
fi

# Pick a binary matching this host's arch; fall back to amd64.
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
esac

BIN="dist/novaworkbench-${OS}-${ARCH}"
if [[ ! -x "$BIN" ]]; then
  BIN="dist/novaworkbench-${OS}-amd64"
fi
if [[ ! -x "$BIN" ]]; then
  echo "No binary found at $BIN. Build first: scripts/build-all.sh" >&2
  exit 1
fi

mkdir -p "$HOME/.novaworkbench/data"

PORT="${NOVA_PORT:-9527}"
echo "NovaWorkbench starting on http://localhost:${PORT}"
echo "Data dir: $HOME/.novaworkbench"
exec "$BIN"
