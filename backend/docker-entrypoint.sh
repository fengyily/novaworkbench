#!/bin/sh
# docker-entrypoint.sh — runs as root once per container start, fixes
# ownership of bind-mounted state directories so the non-root `node` user
# (UID 1000; provided by the node base image — see Dockerfile) can read/write
# them, then drops privileges with su-exec and execs the server.
#
# Why this exists:
#   The Claude CLI refuses `--dangerously-skip-permissions` when uid==0,
#   so the server must run as a non-root user. Host bind-mounts
#   (`~/.novaworkbench`, `~/workspace`) are owned by the host user
#   (often root in side-car setups) — chown-ing them is the only way the
#   node user can write to the DB / clone repos inside the container.
#
# This script is intentionally minimal: any failure here aborts container
# start instead of silently letting the server crash on its first write.

set -eu

# Directories that must be writable by node. These mirror the volume
# mount targets in docker-compose.yml and deploy/docker-compose.prod.yml.
DATA_DIR="${NOVA_HOME:-/home/node}/.novaworkbench"
WORK_DIR="${NOVA_WORK:-/home/node}/workspace"

mkdir -p "$DATA_DIR" "$WORK_DIR"

# Backward-compat: if an older deployment wrote data to /root/... and
# /home/node/... is empty, migrate it so users don't lose their projects.
if [ -d /root/.novaworkbench ] && [ ! -f "$DATA_DIR/data/nova.db" ] && [ -f /root/.novaworkbench/data/nova.db ]; then
  echo "[entrypoint] migrating /root/.novaworkbench → $DATA_DIR"
  cp -a /root/.novaworkbench/. "$DATA_DIR/"
fi
if [ -d /root/workspace ] && [ -z "$(ls -A "$WORK_DIR" 2>/dev/null)" ]; then
  echo "[entrypoint] migrating /root/workspace → $WORK_DIR"
  cp -a /root/workspace/. "$WORK_DIR/" 2>/dev/null || true
fi

# Fix ownership of everything node will touch. -R is safe because the
# only state these dirs hold is the bind-mount content; chowning it on
# every start is a few ms and idempotent.
chown -R node:node "$DATA_DIR" "$WORK_DIR"

# Drop privileges and exec the server. su-exec (an Alpine-friendly
# gosu-equivalent) replaces the shell so the server becomes PID 1's
# child but receives signals from tini cleanly. HOME is set explicitly
# because Go's os/user defaults to /etc/passwd and an unset HOME breaks
# XDG lookups inside the node processes.
export HOME="${NOVA_HOME:-/home/node}"
exec su-exec node env "HOME=$HOME" "$@"
