#!/usr/bin/env bash
# Regenerate the two derived copies of the nova-agent-worker source from the
# single source of truth at agent-worker/server.mjs:
#
#   backend/internal/handler/agent_worker_files.go  (agentWorkerServerMJS const,
#       embedded into the Go binary; what production installs upload)
#   deploy/agent-worker/server.mjs                  (standalone deploy bundle)
#
# Why this script exists: workerSourceServerMJS() in agent_server.go is
# DISK-FIRST — a NovaWorkbench running from a repo checkout uploads
# agent-worker/server.mjs, while a packaged binary uploads the embedded
# const. When the two drift, the worker an operator gets depends on how
# NovaWorkbench itself was started, which is invisible from the UI and makes
# remote-execution bugs unreproducible. They had in fact drifted in both
# directions (the disk copy had the workdir-scope guard but not the
# NOVA_AGENT_WORKER_EXTRA_PATHS support; the embedded copy had the reverse).
#
# Usage:
#   scripts/sync-agent-worker.sh           # regenerate the derived copies
#   scripts/sync-agent-worker.sh --check   # verify they are in sync (exit 1 if not)
#
# Remember to bump agentWorkerVersion in agent_worker_files.go whenever
# server.mjs changes — the check flow compares it against the running
# worker's reported version to detect a stale remote process.
set -euo pipefail

cd "$(dirname "$0")/.."

SRC="agent-worker/server.mjs"
GO_FILE="backend/internal/handler/agent_worker_files.go"
DEPLOY_COPY="deploy/agent-worker/server.mjs"
MODE="${1:-write}"

[ -f "$SRC" ] || { echo "missing $SRC" >&2; exit 1; }

# The Go const is a raw string literal delimited by backticks, so the JS must
# not contain any. Fail loudly rather than emitting a Go file that won't
# compile (or, worse, one that terminates the literal early and still does).
if grep -q '`' "$SRC"; then
  echo "ERROR: $SRC contains backticks; the Go raw-string literal cannot hold them." >&2
  echo "Use single quotes + string concatenation instead. Offending lines:" >&2
  grep -n '`' "$SRC" >&2
  exit 1
fi

node --check "$SRC"

TMP_GO="$(mktemp)"
trap 'rm -f "$TMP_GO"' EXIT

# Splice server.mjs into the agentWorkerServerMJS raw-string literal, keeping
# everything before the opening backtick and after the closing one untouched.
python3 - "$SRC" "$GO_FILE" "$TMP_GO" <<'PY'
import sys
src, go_file, out_file = sys.argv[1], sys.argv[2], sys.argv[3]
js = open(src).read()
go = open(go_file).read()
marker = 'const agentWorkerServerMJS = `'
start = go.index(marker) + len(marker)
end = go.index('\n`\n', start)
open(out_file, 'w').write(go[:start] + js.rstrip('\n') + go[end:])
PY

if [ "$MODE" = "--check" ]; then
  rc=0
  cmp -s "$TMP_GO" "$GO_FILE" || { echo "OUT OF SYNC: $GO_FILE" >&2; rc=1; }
  cmp -s "$SRC" "$DEPLOY_COPY" || { echo "OUT OF SYNC: $DEPLOY_COPY" >&2; rc=1; }
  [ "$rc" -eq 0 ] && echo "agent-worker copies are in sync"
  [ "$rc" -eq 0 ] || echo "run scripts/sync-agent-worker.sh to regenerate" >&2
  exit "$rc"
fi

cp "$TMP_GO" "$GO_FILE"
cp "$SRC" "$DEPLOY_COPY"
# gofmt is a nicety, not a requirement: the splice only rewrites the contents
# of a raw-string literal, so the surrounding Go formatting is untouched.
# Skipping when the toolchain isn't on PATH keeps --check usable from a
# frontend-only shell (and keeps this script's output byte-identical either
# way, which --check depends on).
if command -v gofmt >/dev/null 2>&1; then
  gofmt -w "$GO_FILE"
fi
echo "synced -> $GO_FILE"
echo "synced -> $DEPLOY_COPY"
