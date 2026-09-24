# nova-agent-worker

A small Node.js service that bridges NovaWorkbench's Go backend to the
[`claude` CLI](https://www.npmjs.com/package/@anthropic-ai/claude-code)
on a remote Agent host. It replaces the previous approach of SSH-ing into
the agent and running `claude -p ... --output-format stream-json ...`
directly, which suffered three recurring classes of bug:

- **Shell quoting of env vars.** The previous code prefixed `claude` with
  `env KEY=VAL ...`, but the env values were inherited from the SSH session
  (which on macOS includes `BASH_FUNC_*%%=() {...}` exports). Unquoted
  `()` and `{}` confuse bash, killing the run before `claude` ever started.
- **sshd argv limits.** Long shell prompts + many env pairs hit
  `MaxCommandLength` / `ARG_MAX` on some sshd configs and got silently
  truncated, producing the same "stream interrupted, 0 events" symptom.
- **env-as-argument confusion.** Single-quoting env pairs (a tempting fix)
  makes bash treat each as a literal command-name argument, so it tries to
  execute `MANPATH=...` as a command.

## Architecture

```
[Frontend] ──HTTPS SSE──▶ [Go Backend (NovaWorkbench)]
                                │
                                │ SSH direct-tcpip channel
                                ▼
                       [Node.js Worker (this)]   ← 127.0.0.1:7000
                                │
                                │ spawn('claude', [...args])
                                ▼
                          [claude CLI]
                                │
                                ▼
                         [api.anthropic.com]
```

The worker binds **only** `127.0.0.1:7000` by design — NovaWorkbench reaches
it via SSH direct-tcpip channel (see `backend/internal/ssh/client.go`'s
`HTTPTransport`), so no new port is exposed on the network.

## Endpoints

### `GET /v1/health`

```json
{ "status": "ok", "claudeVersion": "1.x.y", "workerVersion": "0.4.0", "path": "/usr/bin:/bin" }
```

Used by the `Check` flow and by every remote run to verify the worker is
alive and able to work before starting.

`claudeVersion` is produced by running `claude --version` through the
worker's *own* spawn path, so it is the only signal that reflects whether
this process — whose `PATH` was frozen when it started — can actually launch
the CLI. `"unknown"` means it could not. An SSH shell that finds `claude`
proves nothing here: it sources `nvm.sh` and widens `PATH`, the worker does
not. `path` echoes the worker's resolved `PATH` so that split is diagnosable
without reading `/proc/<pid>/environ`.

`workerVersion` is stamped into `server.mjs` at install time from the Go
binary's `agentWorkerVersion`. NovaWorkbench compares it on every Check and
before every remote run; a mismatch means the deployed `server.mjs` predates
the running backend and will silently ignore request fields it has never
heard of (`claudeBin` being the load-bearing one), so both flows hot-swap
`server.mjs` and restart the worker rather than waiting for a manual
「安装依赖」.

### `POST /v1/run`

Request body:

| field | type | required | notes |
|---|---|---|---|
| `workDir` | string | yes | absolute path on the worker host |
| `prompt` | string | yes | the `-p` payload |
| `model` | string | no | CLI model id (e.g. `MiniMax-M3`) |
| `systemPrompt` | string | no | full system prompt; replaced in plan mode with `--append-system-prompt` |
| `sessionId` | string | no | for `--resume` / `--session-id` |
| `resume` | bool | no | `true` → `--resume` |
| `fork` | bool | no | `true` + `resume` → `--fork-session` |
| `forkSessionId` | string | no | pre-mint the forked session id (`--session-id` on a fork) |
| `env` | object | no | extra env vars (`KEY: string`) |
| `allowedTools` | string[] | no | `--allowedTools` (space-separated) |
| `disallowedTools` | string[] | no | `--disallowedTools` (space-separated) |
| `permissionMode` | string | no | `"plan"` → `--permission-mode plan`; empty → `--dangerously-skip-permissions` |
| `overrideSettingSources` | bool | no | `true` → `--setting-sources project,local` (drop user) |
| `ignoreLocalSettings` | bool | no | default `true` → `--setting-sources ""` (drop ALL settings files) |
| `claudeBin` | string | no | absolute path to the `claude` CLI; spawned directly, bypassing `PATH` |
| `extraPaths` | string | no | `:`-separated dirs prepended to the child's `PATH` when `claudeBin` is unusable |

`claudeBin` / `extraPaths` mirror `agent_servers.claude_bin` /
`.extra_paths`, which every Check refreshes. They exist because the worker's
own `PATH` is frozen at process start, so a `claude` installed somewhere that
`PATH` doesn't cover (classically `~/.nvm/versions/node/<ver>/bin`, visible
only to a shell that sourced `nvm.sh`) makes every run die with
`spawn claude ENOENT` while the settings UI happily shows the detected
runtime paths. `claudeBin` is used only if it passes an `X_OK` check, so an
nvm node upgrade degrades to `PATH` lookup rather than hard-failing a host
that would otherwise work.

Response: `application/x-ndjson`. One JSON object per line (the same shape
the CLI's `--output-format stream-json --verbose` emits). NovaWorkbench's
existing `parseStreamJSONFromReader` reads this line-by-line unchanged.

```jsonc
{ "type": "system",       "subtype": "init", "session_id": "..." }
{ "type": "user",         "content": [...] }
{ "type": "assistant",    "content": [...] }
{ "type": "tool_use",     "name": "Read", "input": {...} }
{ "type": "tool_result",  "content": "..." }
{ "type": "result",       "subtype": "success", "result": "..." }
```

Errors come through as `{type:"error", errorCategory, error, stderr?, code?, signal?}`
frames, then the response closes. A `cli_not_found` additionally carries
`resolvedClaudeBin` / `resolvedPath` — what the worker actually tried to
spawn and with which `PATH` — which is what distinguishes "no path was sent"
from "the sent path was wrong" from "this worker is too old to read the
field".

## Why not `@anthropic-ai/claude-agent-sdk`?

An earlier revision routed every call through the SDK's `query()` API, but
the SDK was opaque about child-process failures — a thrown Error's
`.message` collapsed every root cause into the same generic
"Claude Code process exited with code 1", losing the actual CLI stderr
that names the real cause (401 / ENOTFOUND / unrecognized model / etc.).
Spawning `claude` directly gives us the raw exit code, the captured stderr,
and the live stream-json events without an extra layer of state in
between. It also keeps auth / settings handling under our direct control:
the platform's `claude_configs` row is the only source of
`ANTHROPIC_AUTH_TOKEN` / `ANTHROPIC_BASE_URL` / model pinning, with
`--setting-sources ""` ensuring no on-host `~/.claude/settings.json` can
shadow it.

The wire protocol is unchanged from the SDK-era version: NovaWorkbench's
Go-side `parseStreamJSONFromReader` still consumes the NDJSON line-by-line
without modification.

## Install (manual)

```bash
cd /opt/nova-agent-worker
npm install   # installs express (only runtime dep)
node server.mjs &
```

## Install (systemd)

NovaWorkbench's `agent-server install` flow handles this automatically: it
SFTPs `server.mjs` + `package.json` to `/opt/nova-agent-worker/`, runs
`npm install --omit=dev`, writes the systemd unit, enables and starts it.

The unit file lives at `systemd/nova-agent-worker.service` in this directory.
