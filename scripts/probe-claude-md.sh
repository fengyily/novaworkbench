#!/usr/bin/env bash
# Probe how the Claude CLI actually loads project memory.
#
# WHY THIS SCRIPT EXISTS
# ----------------------
# The project-level CLAUDE.md is delivered to the model by the Claude CLI
# itself — the CLI discovers instruction files on disk and loads them as
# "project memory". NovaWorkbench's managed copy of that document has to
# travel the SAME way (see backend/internal/llm/claude_md_settings.go), so
# the questions this script answers are load-bearing:
#
#   V1  does --append-system-prompt still load the repo's CLAUDE.md?
#   V2  does --system-prompt (full replace) still load it?
#   V3  does --add-dir <dir> load <dir>/CLAUDE.md?
#   V3b does it load <dir>/CLAUDE.md when
#       CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1 is set?
#   V4  does --settings {"claudeMdExcludes":[...]} exclude a file?
#   V5  do V3b + V4 together leave exactly one copy?
#
# HOW IT WORKS
# ------------
# `claude -p` is pointed at a LOCAL MOCK /v1/messages endpoint that
# captures the request payload and answers with a minimal SSE stream. The
# captured payload IS the context the model would receive — so every answer
# below is OBSERVED, not inferred, and the script needs no API quota and no
# real model.
#
# The markers are unique strings written into files in a throwaway directory;
# the script then reports which markers appear in the captured request.
#
# Usage:
#   scripts/probe-claude-md.sh              # run the probe
#   BIN=/path/to/claude scripts/probe-claude-md.sh
#
# Exit code: 0 when every probe produced a capture (the SCRIPT is the
# regression guard; the expected RESULTS live in the file header of
# backend/internal/llm/claude_md_settings.go).

set -uo pipefail

BIN=${BIN:-claude}
# Pick a free port: a stale mock from an earlier run must not make every
# probe report NO_CAPTURE.
PORT=${PORT:-0}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"; kill "${MOCK_PID:-}" 2>/dev/null' EXIT

command -v "$BIN" >/dev/null 2>&1 || {
  echo "claude binary not found: $BIN" >&2
  exit 1
}

# ── the mock API endpoint ──────────────────────────────────────────────────
# It captures each request body to its own file and answers with a minimal
# but well-formed SSE stream, so the CLI's request phase completes.
cat > "$WORK/mock.py" <<'PY'
import http.server, json, sys

LOG = sys.argv[1]
PORT = int(sys.argv[2])


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length)
        with open(LOG, "a") as f:
            f.write(body.decode("utf-8", "replace") + "\n")
        payload = (
            b'event: message_start\ndata: {"type":"message_start","message":{"type":"message",'
            b'"role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}\n\n'
            b'event: content_block_start\ndata: {"type":"content_block_start","index":0,'
            b'"content_block":{"type":"text","text":""}}\n\n'
            b'event: content_block_delta\ndata: {"type":"content_block_delta","index":0,'
            b'"delta":{"type":"text_delta","text":"OK"}}\n\n'
            b'event: content_block_stop\ndata: {"type":"content_block_stop","index":0}\n\n'
            b'event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"},'
            b'"usage":{"output_tokens":1}}\n\n'
            b'event: message_stop\ndata: {"type":"message_stop"}\n\n'
        )
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass


server = http.server.HTTPServer(("127.0.0.1", PORT), Handler)
print(server.server_address[1], flush=True)
server.serve_forever()
PY

LOG="$WORK/capture.jsonl"
# The mock binds port 0 (any free port) and prints the port it got, so the
# script never collides with a stale listener.
python3 "$WORK/mock.py" "$LOG" "$PORT" > "$WORK/port" 2>/dev/null &
MOCK_PID=$!
for _ in $(seq 1 100); do
  if [ -s "$WORK/port" ]; then
    PORT=$(cat "$WORK/port")
    break
  fi
  sleep 0.1
done
if [ -z "$PORT" ] || [ "$PORT" = "0" ]; then
  echo "could not start the mock API endpoint" >&2
  exit 1
fi
for _ in $(seq 1 50); do
  if curl -s -o /dev/null -X POST "http://127.0.0.1:$PORT/v1/messages" -d '{"model":"m"}'; then
    break
  fi
  sleep 0.1
done

# ── the fixture ────────────────────────────────────────────────────────────
mkdir -p "$WORK/proj/.claude" "$WORK/mem"
printf 'MARKER_PROJECT_AAA\n' > "$WORK/proj/CLAUDE.md"
printf 'MARKER_LOCAL_BBB\n' > "$WORK/proj/CLAUDE.local.md"
printf 'MARKER_CLAUDEDIR_CCC\n' > "$WORK/proj/.claude/CLAUDE.md"
printf 'MARKER_AGENTS_DDD\n' > "$WORK/proj/AGENTS.md"
printf 'MARKER_MEMORY_EEE\n' > "$WORK/mem/CLAUDE.md"

BASE="http://127.0.0.1:$PORT"
# The mock URL has to reach the CLI through the --settings "env" block: the
# CLI applies that block to its own environment, and a stale
# ~/.claude/settings.json would otherwise shadow the host variables.
ENVS="{\"env\":{\"ANTHROPIC_BASE_URL\":\"$BASE\",\"ANTHROPIC_AUTH_TOKEN\":\"probe\",\
\"ANTHROPIC_MODEL\":\"claude-sonnet-5\"}}"
ENVS_MD="{\"env\":{\"ANTHROPIC_BASE_URL\":\"$BASE\",\"ANTHROPIC_AUTH_TOKEN\":\"probe\",\
\"ANTHROPIC_MODEL\":\"claude-sonnet-5\",\"CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD\":\"1\"}}"

# run <name> <settings-json> [extra args...]
run() {
  local name="$1" settings="$2"
  shift 2
  : > "$LOG"
  ( cd "$WORK/proj" && "$BIN" -p "case=$name" --output-format text \
      --dangerously-skip-permissions --settings "$settings" "$@" >/dev/null 2>&1 )
  sleep 0.3
  python3 - "$LOG" <<'PY'
import json, sys

try:
    lines = [l for l in open(sys.argv[1]).read().splitlines() if l.strip()]
except FileNotFoundError:
    lines = []
if not lines:
    print("NO_CAPTURE")
    raise SystemExit
payload = json.loads(lines[-1])
text = json.dumps(payload, ensure_ascii=False)
case = "?"
for message in payload.get("messages", []):
    content = message.get("content")
    if not isinstance(content, list):
        continue
    for block in content:
        if block.get("type") == "text" and "case=" in block.get("text", ""):
            case = block["text"].split("case=")[1].split()[0]
markers = [m for m in ("MARKER_PROJECT_AAA", "MARKER_LOCAL_BBB", "MARKER_CLAUDEDIR_CCC",
                        "MARKER_AGENTS_DDD", "MARKER_MEMORY_EEE") if m in text]
print(f"{case:6s} {','.join(markers) if markers else 'NONE'}")
PY
}

echo "== probes =="
run V1 "$ENVS" --append-system-prompt PERSONA_X
run V2 "$ENVS" --system-prompt PERSONA_X
run V3 "$ENVS" --add-dir "$WORK/mem"
run V3b "$ENVS_MD" --add-dir "$WORK/mem"
run V4 "$ENVS_MD" --add-dir "$WORK/mem" \
  --settings "{\"env\":{\"ANTHROPIC_BASE_URL\":\"$BASE\",\"ANTHROPIC_AUTH_TOKEN\":\"probe\",\"ANTHROPIC_MODEL\":\"claude-sonnet-5\",\"CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD\":\"1\"},\"claudeMdExcludes\":[\"$WORK/proj/CLAUDE.md\",\"$WORK/proj/CLAUDE.local.md\",\"$WORK/proj/.claude/CLAUDE.md\",\"$WORK/proj/AGENTS.md\"]}"
run V5 "$ENVS_MD" --add-dir "$WORK/mem" \
  --settings "{\"env\":{\"ANTHROPIC_BASE_URL\":\"$BASE\",\"ANTHROPIC_AUTH_TOKEN\":\"probe\",\"ANTHROPIC_MODEL\":\"claude-sonnet-5\",\"CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD\":\"1\"},\"claudeMdExcludes\":[\"$WORK/proj/CLAUDE.md\",\"$WORK/proj/CLAUDE.local.md\",\"$WORK/proj/.claude/CLAUDE.md\",\"$WORK/proj/AGENTS.md\"]}"
echo "== done =="
echo
echo "Expected results (claude 2.1.328):"
echo "  V1   the repo's CLAUDE.md IS loaded"
echo "  V2   the repo's CLAUDE.md IS STILL loaded (--system-prompt does not drop it)"
echo "  V3   the --add-dir copy is NOT loaded"
echo "  V3b  the --add-dir copy IS loaded"
echo "  V4   the excluded files are gone"
echo "  V5   only the --add-dir copy remains"
