#!/usr/bin/env bash
# NovaWorkbench backend local debug launcher (PostgreSQL).
# Ensures the nova-postgres container is running, then starts the backend.
# Frontend (separate terminal): cd frontend && npm run dev
set -euo pipefail

cd "$(dirname "$0")/backend"

PORT="${NOVA_PORT:-9527}"

# Kill any stale backend still bound to $PORT. A previous `go run` child can
# survive Ctrl+C and make the next launch fail with "address already in use".
STALE_PID="$(lsof -tiTCP:"$PORT" -sTCP:LISTEN 2>/dev/null || true)"
if [[ -n "$STALE_PID" ]]; then
  echo "killing stale backend on :$PORT (pid ${STALE_PID})..."
  kill $STALE_PID 2>/dev/null || true
  sleep 1
fi

# //go:embed all:dist (web/embed.go) needs web/dist to exist, else `go run`
# fails with "pattern all:dist: no matching files found" after `make clean`.
# Keep a placeholder so the backend can run without a frontend build.
mkdir -p web/dist
[[ -f web/dist/.gitkeep ]] || touch web/dist/.gitkeep

# Start the postgres container if it isn't running, and wait for readiness.
if ! docker ps --format '{{.Names}}' | grep -qx nova-postgres; then
  echo "starting nova-postgres container..."
  docker start nova-postgres >/dev/null
fi
until docker exec nova-postgres pg_isready -U postgres >/dev/null 2>&1; do
  sleep 1
done

export NOVA_DB_DRIVER=postgres
export NOVA_DB_DSN='postgres://postgres:nova@127.0.0.1:5434/novaworkbench?sslmode=disable'

exec go run ./cmd/server
