#!/usr/bin/env bash
# NovaWorkbench backend local debug launcher (PostgreSQL).
# Ensures the nova-postgres container is running, then starts the backend.
# Frontend (separate terminal): cd frontend && npm run dev
set -euo pipefail

cd "$(dirname "$0")/backend"

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
