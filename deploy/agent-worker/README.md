# nova-agent-worker

The `nova-agent-worker` is the HTTP/NDJSON bridge between NovaWorkbench's Go
backend and the `claude` CLI on a **remote Agent host**. NovaWorkbench reaches
it over an SSH `direct-tcpip` channel (so the worker binds `127.0.0.1` only),
and the worker spawns `claude -p … --output-format stream-json --verbose` and
forwards the events line-by-line unchanged.

This directory is the canonical, version-controlled copy of the worker's
deployable artifacts:

| file                       | purpose                                                        |
|----------------------------|----------------------------------------------------------------|
| `server.mjs`               | the worker (Node, single `express` dependency)                 |
| `package.json`             | worker manifest (`express`)                                    |
| `nova-agent-worker.service`| systemd **user** unit (pins `TMPDIR=/tmp`, `Restart=always`)   |
| `install.sh`               | idempotent deploy script (systemd, with a nohup fallback)      |

## Deploying

```bash
deploy/agent-worker/install.sh
```

Overrides (defaults shown):

```bash
NOVA_AGENT_WORKER_DIR=~/nova-agent-worker
NOVA_AGENT_WORKER_SERVICE=nova-agent-worker
NOVA_AGENT_WORKER_HOST=127.0.0.1
NOVA_AGENT_WORKER_PORT=7000
```

## Why the deployment used to fail

The Agent Server's "install environment dependencies" flow reported two
coupled symptoms:

1. **`systemd --user` service `active` but not listening.** The unit is
   `Type=simple`, which systemd marks active the instant `node` spawns —
   *before* the port is bound. A worker that crashed on startup left a
   "green" unit that never served. The crash cause was a macOS-shaped
   `TMPDIR` (`/var/folders/<uuid>/T`) forwarded from the dev box via SSH
   `SendEnv`: on the Linux agent host that directory doesn't exist, and
   `claude` (and, in earlier worker revisions, the worker itself) EACCES'd
   during tmpdir setup.
2. **`nohup` fallback failed with `exit=143`.** `143 = 128 + SIGTERM`: the
   install loop launched `nohup node server.mjs &`, inherited the same bad
   `TMPDIR`, and then SIGTERM'd the process as soon as the health probe timed
   out. A slow-but-healthy startup and a genuinely-dead worker were
   indistinguishable, so the loop always escalated to kill.

## The fix

- **`nova-agent-worker.service`** pins `Environment=TMPDIR=/tmp` so the worker
  process itself starts with a writable tmpdir regardless of what the SSH
  session inherited.
- **`server.mjs`** adds `resolveTmpdir()`, which walks a writable-tmpdir
  fallback chain (existing `TMPDIR` → `$HOME` → `/tmp` → cwd → bare `/tmp`)
  for the `claude` child, so the CLI never inherits a `/var/folders` path.
- **`install.sh`**
  - health-checks the real `GET /v1/health` endpoint instead of trusting
    `systemctl` `ActiveState`;
  - reuses an existing `node_modules` (offline re-deploy works even when the
    npm registry is unreachable), with an optional bundled
    `node_modules.tar.gz` fallback;
  - daemonizes the nohup fallback properly (`setsid` + `disown` + pidfile +
    log + `TMPDIR=/tmp`) and only reports failure when the process has
    actually exited — never on a transient "not yet listening".

## Verifying

```bash
curl -fsS http://127.0.0.1:7000/v1/health
# {"status":"ok","claudeVersion":"…"}
systemctl --user is-active nova-agent-worker   # active
```
