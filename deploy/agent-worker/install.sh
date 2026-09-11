#!/usr/bin/env bash
# NovaWorkbench Agent Worker — deployment script.
#
# Installs the nova-agent-worker (the HTTP/NDJSON bridge between
# NovaWorkbench's Go backend and the `claude` CLI on a remote Agent host)
# as a systemd --user service, with a nohup fallback for hosts where the
# systemd user bus isn't actually reachable at runtime.
#
# This script encodes the fixes for two deployment failures that used to
# block the Agent Server's "install environment dependencies" flow:
#
#   1. "systemd --user service reports active but nothing is listening"
#      Root cause: `Type=simple` marks the unit active the instant the
#      process spawns, BEFORE it binds the port. A worker that crashed on
#      startup — because it inherited a macOS-shaped TMPDIR (/var/folders/…)
#      forwarded via SSH SendEnv from the dev box, or because `express`
#      was missing after a failed `npm install` — left a unit that was
#      "active" but never listening. We pin TMPDIR=/tmp in the unit
#      (belt-and-braces with server.mjs's own resolveTmpdir) and health-check
#      the actual HTTP endpoint instead of trusting systemctl's ActiveState.
#
#   2. "nohup fallback fails with exit=143"
#      143 = 128 + SIGTERM. The old fallback ran `nohup node server.mjs &`,
#      inherited the SSH session's TMPDIR, and was SIGTERM'd by the install
#      loop the moment the health probe timed out — a slow-but-healthy
#      startup and a genuinely-dead worker were indistinguishable, so the
#      loop always escalated to kill. Here we daemonize properly
#      (nohup + disown + pidfile + log redirect + TMPDIR=/tmp) and only
#      fail when the process has actually exited, never on a transient
#      "not yet listening".
#
# Idempotent: safe to re-run on every deploy. It reuses an already-installed
# node_modules (so an unreachable npm registry doesn't block re-deploying an
# already-provisioned host) and never kills a worker that is merely slow to
# start.
set -euo pipefail

# --- configuration ---------------------------------------------------------
INSTALL_DIR="${NOVA_AGENT_WORKER_DIR:-$HOME/nova-agent-worker}"
SERVICE_NAME="${NOVA_AGENT_WORKER_SERVICE:-nova-agent-worker}"
HOST="${NOVA_AGENT_WORKER_HOST:-127.0.0.1}"
PORT="${NOVA_AGENT_WORKER_PORT:-7000}"
HEALTH_URL="http://${HOST}:${PORT}/v1/health"
SYSTEMD_UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log()  { printf '%s\n' "$*"; }
fail() { printf '!! %s\n' "$*" >&2; exit 1; }

# worker_listening — probe the worker's own /v1/health endpoint rather than
# trusting systemctl's ActiveState (which, with Type=simple, only proves the
# process spawned, not that it bound the port). Returns 0 when the worker is
# up and answering.
worker_listening() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsS --max-time 3 "${HEALTH_URL}" >/dev/null 2>&1
    return $?
  fi
  HEALTH_URL="${HEALTH_URL}" node -e '
    const http = require("http");
    const req = http.get(process.env.HEALTH_URL, (res) => {
      res.resume();
      process.exit(res.statusCode === 200 ? 0 : 1);
    });
    req.on("error", () => process.exit(1));
    req.setTimeout(3000, () => { req.destroy(); process.exit(1); });
  '
}

# wait_healthy — poll the endpoint with a bounded number of attempts.
wait_healthy() {
  local attempts="${1:-15}" delay="${2:-1}" i
  for ((i = 0; i < attempts; i++)); do
    worker_listening && return 0
    sleep "${delay}"
  done
  return 1
}

# ensure_node_modules — install express (+ deps) only when absent. The
# worker has a single runtime dependency (express), so we prefer an existing
# node_modules over `npm install`; that keeps a re-deploy working even when
# the npm registry is unreachable. When a bundled tarball is shipped next to
# this script (node_modules.tar.gz), it serves as the offline fallback.
ensure_node_modules() {
  if [[ -f "${INSTALL_DIR}/node_modules/express/package.json" ]]; then
    log ">>> node_modules already present (reusing; no npm install needed)"
    return 0
  fi
  log ">>> installing worker dependencies (npm install)"
  if (cd "${INSTALL_DIR}" && npm install --omit=dev --no-audit --no-fund); then
    return 0
  fi
  if [[ -f "${SCRIPT_DIR}/node_modules.tar.gz" ]]; then
    log ">>> npm install failed (registry unreachable?) — extracting bundled node_modules"
    (cd "${INSTALL_DIR}" && tar xzf "${SCRIPT_DIR}/node_modules.tar.gz")
    [[ -f "${INSTALL_DIR}/node_modules/express/package.json" ]] \
      || fail "bundled node_modules.tar.gz is missing express"
    return 0
  fi
  fail "npm install failed and no bundled node_modules.tar.gz to fall back to — check the npm registry (npm config get registry)"
}

# ensure_systemd_unit — write the user unit (TMPDIR=/tmp + Restart=always)
# and reload/enable it. `enable` is what makes it survive logins together
# with `loginctl enable-linger`.
ensure_systemd_unit() {
  mkdir -p "${SYSTEMD_UNIT_DIR}"
  local dst="${SYSTEMD_UNIT_DIR}/${SERVICE_NAME}.service"
  # The repo template is always named nova-agent-worker.service; SERVICE_NAME
  # only controls the installed unit's name (overridable for tests / co-tenancy).
  local src="${SCRIPT_DIR}/nova-agent-worker.service"
  [[ -f "${src}" ]] || fail "missing ${src}"

  # Substitute the @INSTALL_DIR@/@HOST@/@PORT@ placeholders so the unit's
  # WorkingDirectory and listen address match the actual deploy targets (the
  # repo copy is a portable template, not a per-host file).
  local rendered
  rendered="$(sed \
    -e "s|@INSTALL_DIR@|${INSTALL_DIR}|g" \
    -e "s|@HOST@|${HOST}|g" \
    -e "s|@PORT@|${PORT}|g" \
    "${src}")"

  if [[ ! -f "${dst}" ]] || [[ "$(cat "${dst}")" != "${rendered}" ]]; then
    printf '%s\n' "${rendered}" >"${dst}"
    log ">>> installed systemd user unit ${dst}"
  fi

  # Linger keeps the user session (and its services) alive after logout, so
  # the worker isn't torn down when the SSH deploy session closes. Best
  # effort — some hosts (containers) have no loginctl.
  if command -v loginctl >/dev/null 2>&1; then
    loginctl enable-linger "${USER:-$(id -un)}" >/dev/null 2>&1 || true
  fi

  if command -v systemctl >/dev/null 2>&1 && systemctl --user daemon-reload 2>/dev/null; then
    systemctl --user enable "${SERVICE_NAME}.service" >/dev/null 2>&1 || true
  fi
}

# systemd_available — is the systemd *user* bus actually usable? Some SSH
# deployments report the unit "active" via the system scope but the user bus
# can't drive the process; we only trust it if `systemctl --user` round-trips.
systemd_available() {
  command -v systemctl >/dev/null 2>&1 \
    && systemctl --user --quiet is-system-running >/dev/null 2>&1
}

# try_systemd — start/restart under systemd and wait for the endpoint.
# Returns 0 only when the worker is *actually* listening. If systemd can't
# bring it up, we fall back to nohup rather than leaving a "green" unit that
# isn't serving.
try_systemd() {
  if ! systemd_available; then
    log ">>> systemd --user bus not usable — skipping to nohup fallback"
    return 1
  fi

  systemctl --user restart "${SERVICE_NAME}.service" >/dev/null 2>&1 || true

  if wait_healthy 15 1; then
    log ">>> worker healthy via systemd (${HEALTH_URL})"
    return 0
  fi

  # Give systemd one more beat: RestartSec=3 can hide a brief crash loop.
  if wait_healthy 5 1; then
    log ">>> worker healthy via systemd (${HEALTH_URL})"
    return 0
  fi
  log ">>> systemd unit active but not listening — falling back to nohup"
  return 1
}

# try_nohup — detached fallback. `nohup` ignores SIGHUP and `disown` stops the
# shell from re-sending it on teardown; the worker is pinned to a writable
# TMPDIR, logs to a file, and records its pid. Health is re-checked after a
# generous window; the process is only reported dead when it really exited.
try_nohup() {
  local pidfile="${INSTALL_DIR}/worker.pid"
  local logfile="${INSTALL_DIR}/worker.log"

  # Reuse an already-running fallback if it is healthy.
  if [[ -f "${pidfile}" ]] && kill -0 "$(cat "${pidfile}")" 2>/dev/null && worker_listening; then
    log ">>> nohup worker already running (pid $(cat "${pidfile}"))"
    return 0
  fi

  log ">>> starting worker via nohup fallback"
  # Deliberately NOT `setsid` here: when a background job is a process-group
  # leader, `setsid` forks a child and exits the parent, so `$!` would
  # capture the immediately-exiting parent instead of node — the pidfile
  # would then be wrong and the "is it dead?" check below would misfire.
  # `nohup env … node` execs in place (nohup → env → node), so `$!` is the
  # worker's real pid. `nohup` ignores SIGHUP on SSH teardown; `disown`
  # stops the shell from re-sending it; `</dev/null` + log redirect detach
  # stdio. TMPDIR=/tmp stops a macOS-shaped TMPDIR forwarded via SendEnv
  # from reaching the worker/claude child.
  nohup env \
    TMPDIR=/tmp TMP=/tmp TEMP=/tmp \
    NOVA_AGENT_WORKER_HOST="${HOST}" \
    NOVA_AGENT_WORKER_PORT="${PORT}" \
    node "${INSTALL_DIR}/server.mjs" >>"${logfile}" 2>&1 </dev/null &
  local pid=$!
  echo "${pid}" >"${pidfile}"
  disown "${pid}" 2>/dev/null || true

  if wait_healthy 20 1; then
    log ">>> worker healthy via nohup (pid ${pid})"
    return 0
  fi

  # Distinguish "dead" from "slow". Never SIGTERM a process that is still
  # alive just because the first health window elapsed — that was the old
  # exit=143 bug.
  if kill -0 "${pid}" 2>/dev/null; then
    fail "worker alive but not listening after 20s — check ${logfile}"
  fi
  fail "worker exited during startup — check ${logfile}"
}

# --- main ------------------------------------------------------------------
log ">>> deploying ${SERVICE_NAME} into ${INSTALL_DIR}"
mkdir -p "${INSTALL_DIR}"
cp "${SCRIPT_DIR}/server.mjs" "${INSTALL_DIR}/server.mjs"
cp "${SCRIPT_DIR}/package.json" "${INSTALL_DIR}/package.json"

ensure_node_modules
ensure_systemd_unit

if try_systemd; then
  exit 0
fi
try_nohup
