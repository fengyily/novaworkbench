package handler

// Embedded copies of nova-agent-worker source files. We keep them inline in
// the Go binary so the install flow can SFTP-write to a fresh agent host
// without needing an internet fetch (and without committing the user to a
// specific GitHub release URL that might move). Update these strings when
// shipping a new worker version; the install flow will redeploy on the next
// runInstall.
//
// DO NOT EDIT agentWorkerServerMJS BY HAND. agent-worker/server.mjs in the
// repo root is the single source of truth; run scripts/sync-agent-worker.sh
// to regenerate this const (and deploy/agent-worker/server.mjs) from it, and
// scripts/sync-agent-worker.sh --check to verify they match.
//
// This matters because workerSourceServerMJS (agent_server.go) is DISK-FIRST:
// a NovaWorkbench started from a repo checkout uploads agent-worker/server.mjs
// while a packaged binary uploads this const. When the two drift, which worker
// a remote host ends up running depends on how NovaWorkbench itself was
// launched — invisible from the UI, and a reliable way to make remote-execution
// bugs unreproducible.
//
// Note: the JS bodies below use single quotes only (no backticks) so the
// Go raw-string literals (delimited by backticks) stay well-formed. The sync
// script enforces this.

// agentWorkerVersion is the single source of truth for the nova-agent-worker
// version. The install flow stamps it into the uploaded server.mjs (replacing
// the __WORKER_VERSION__ placeholder in agent-worker/server.mjs) and records it
// on the agent_servers row; the check flow compares the running worker's
// reported workerVersion against it to detect a stale process that survived a
// restart. Bump it whenever agent-worker/server.mjs changes.
//
// 0.4.0: /v1/run accepts claudeBin + extraPaths so NovaWorkbench can pin the
// claude binary per request instead of relying on the worker's frozen PATH;
// /v1/health additionally reports the resolved PATH.
// 0.4.1: preflight no longer fast-fails on Claude Code's
// '[claude-code:unrecognized_model]' stderr line — that is a print-mode
// diagnostic emitted for every custom model on a private base URL, and the
// probe it was killing exits 0. Bumping forces a hot-upgrade of agent hosts
// still running 0.4.0, which would keep failing every run with
// 'preflight 失败（unrecognized_model）'.
const agentWorkerVersion = "0.4.1"

// agentWorkerServerMJS is the body of nova-agent-worker/server.mjs that gets
// uploaded to the remote agent host during install. It's the same content
// as agent-worker/server.mjs in the repo — see that file for the protocol
// and architecture notes.
const agentWorkerServerMJS = `// nova-agent-worker — HTTP/NDJSON bridge between NovaWorkbench (Go backend)
// and the 'claude' CLI on a remote Agent host.
//
// Why this is a service (not a per-invocation script):
//   * Node startup adds ~300-500ms; doing it per coding pass is noticeable.
//   * The Go side reuses one SSH connection (direct-tcpip channel) to talk
//     here, no new ports opened to the network — bind 127.0.0.1 only.
//
// Earlier revisions routed calls through '@anthropic-ai/claude-agent-sdk',
// but the SDK's 'query()' API is opaque about child-process failures (the
// thrown Error often collapses to "Claude Code process exited with code 1",
// losing the real CLI stderr). Spawing 'claude' directly gives us the raw
// exit code, stderr, and stream-json events without an extra abstraction
// layer, and avoids the SDK's own auth / settings-state machinery leaking
// into the wizard flow.
//
// Protocol:
//   GET  /v1/health   → 200 {status, claudeVersion}
//   POST /v1/run      → application/x-ndjson of stream-json events emitted
//                       by 'claude -p ... --output-format stream-json
//                       --verbose --dangerously-skip-permissions'. One JSON
//                       event per line, flushed immediately. NovaWorkbench's
//                       Go-side parseStreamJSONFromReader consumes them
//                       line-by-line unchanged.
//
// Body fields for POST /v1/run map directly to the CLI flags we build:
//   workDir     → cmd.Dir
//   prompt      → -p <prompt>
//   model       → ANTHROPIC_MODEL inside --settings '{"env":{...}}'
//   systemPrompt→ --system-prompt (or --append-system-prompt in plan mode)
//   sessionId   → --session-id (new) or --resume (existing)
//   resume      → --resume when true
//   fork        → --fork-session (with --resume)
//   forkSessionId → --session-id on a forked run (pre-mint the id)
//   env         → merged into --settings '{"env":{...}}' (auth / base URL /
//                 tier-model pins); NOT the child process env
//   allowedTools→ --allowedTools "Tool1 Tool2 ..."
//   disallowedTools → --disallowedTools "Tool1 Tool2 ..."
//   permissionMode  → "plan" → --permission-mode plan; "" → --dangerously-skip-permissions
//   ignoreLocalSettings (default true) → --setting-sources "" (drop all
//                       settings files; the --settings env block is the only
//                       source of auth / base URL / model pinning).
//   overrideSettingSources (legacy) → --setting-sources project,local
//                       (drop only the user source).
//
// Two reliability layers:
//   1) Preflight — before invoking claude we spawn 'claude --print ping'
//      ourselves with the same env + cwd. The CLI's own output surfaces the
//      actual cause (401 / ENOTFOUND / unrecognized model / etc.), which we
//      hand to classifyError so the Go side can show a tailored fix hint
//      instead of an opaque exit code.
//   2) classifyError — pattern-matches the captured stderr / stdout / spawn
//      error into a coarse errorCategory the Go side maps to a user-facing
//      fix hint (see backend/internal/handler/wizard.go:workerCategoryHint).
import express from 'express';
import { spawn, execFileSync } from 'node:child_process';
import { mkdtempSync, accessSync, realpathSync, constants as fsConstants } from 'node:fs';
// pathSep ('/') splits a filesystem path into segments; pathDelim (':')
// splits $PATH into directories. Mixing them up silently corrupts PATH —
// splitting '/usr/bin:/bin' on '/' and rejoining yields garbage — so they are
// imported under names that cannot be confused for one another.
import { join, sep as pathSep, delimiter as pathDelim } from 'node:path';

// Extend PATH on the agent host so the spawned 'claude' child can be
// located by name alone — regardless of how the worker process itself was
// launched. systemd user units start with the bare PATH
// (/usr/local/sbin:/usr/local/bin:/usr/bin:/usr/sbin:/sbin:/bin), so an
// 'npm i -g @anthropic-ai/claude-code' install into ~/.local/bin (Linux
// default) or /opt/homebrew/bin (macOS) becomes invisible to the child
// even though SSH login shells see it via ~/.bashrc. The result is a
// spawned child whose first action is 'spawn claude ENOENT', classified
// as 'cli_not_found' and surfaced in the wizard job panel as
// "Claude CLI 未找到" — even though NovaWorkbench's Check flow (which
// uses SSH RunCommand and therefore inherits the login-shell PATH)
// reports '✓ claude <version>'.
//
// We don't replace PATH — we prepend a candidate list, dedup, and keep
// the existing entries. Detection of 'npm root -g' is wrapped so a
// missing/broken npm install doesn't take down /v1/health.
function resolveExtendedPath() {
  const home = process.env.HOME || process.env.USERPROFILE || '';
  const candidates = [
    home && join(home, '.local', 'bin'),
    home && join(home, '.npm-global', 'bin'),
    home && join(home, 'bin'),
    '/opt/homebrew/bin',
    '/usr/local/bin',
  ].filter(Boolean);
  // npm root -g on its own can hang for ~1s on a cold cache; cap it.
  // CAUTION: this returns the GLOBAL PREFIX for whichever user the worker
  // runs as, which is NOT necessarily the user that ran 'npm install -g
  // @anthropic-ai/claude-code' during install. On the install → worker
  // hand-off the SSH re-dials as nova/ubuntu, so 'npm root -g' resolves to
  // nova's prefix (often empty /usr/lib/node_modules) and misses the
  // root-installed claude entirely. The NOVA_AGENT_WORKER_EXTRA_PATHS env
  // below is the cross-user override: installNodeWorker writes the
  // actual path of the installed claude CLI there so this branch picks it
  // up regardless of which user the worker is running under.
  try {
    const npmGlobalBin = execFileSync('npm', ['root', '-g'], {
      timeout: 1500,
      stdio: ['ignore', 'pipe', 'ignore'],
    }).toString().trim();
    if (npmGlobalBin) candidates.push(join(npmGlobalBin, 'bin'));
  } catch {
    // npm not installed / slow / broken — skip silently.
  }
  // Install-supplied extra paths. Forwarded by the install flow in
  // agent_server.go installNodeWorker (systemd Environment=PATH=..., launchd
  // EnvironmentVariables.PATH=..., and the nohup env line). Always treat
  // these as authoritative: they were computed on the SSH session that
  // actually ran the install, before any user re-dial, so they reflect
  // where 'claude' was actually written to disk.
  const extra = (process.env.NOVA_AGENT_WORKER_EXTRA_PATHS || '')
    .split(pathDelim)
    .map((s) => s.trim())
    .filter(Boolean);
  for (const d of extra) candidates.push(d);
  const existing = (process.env.PATH || '').split(pathDelim).filter(Boolean);
  const seen = new Set(existing);
  const merged = [...existing];
  for (const d of candidates) {
    if (!seen.has(d)) { seen.add(d); merged.push(d); }
  }
  const finalPath = merged.join(pathDelim);
  // Surface the resolved PATH to the worker log so an operator diagnosing
  // 'spawn claude ENOENT' can confirm whether the install-supplied bin dir
  // actually landed on PATH. Cheap (one line per process start), and the
  // only way to know whether NOVA_AGENT_WORKER_EXTRA_PATHS reached us.
  console.error('[nova-agent-worker] resolved PATH: ' + finalPath);
  return finalPath;
}
process.env.PATH = resolveExtendedPath();

// resolveClaudeCommand decides WHAT to spawn for a claude invocation.
// Priority: caller-supplied absolute path > extraPaths-widened PATH > bare
// 'claude'.
//
// Why this exists: the worker process's own PATH is frozen at process start
// (systemd Environment=PATH=..., launchd EnvironmentVariables.PATH=..., or
// the nohup env line — all computed by the install flow). Nothing that
// happens afterwards can change it. But NovaWorkbench re-resolves the real
// claude location on every Check and stores it in agent_servers.claude_bin,
// so a host whose worker was launched with a stale PATH (the classic case:
// claude installed under ~/.nvm/versions/node/<ver>/bin, which only exists
// on a PATH that sourced nvm.sh) shows "runtime paths detected" in the UI
// while every run dies with 'spawn claude ENOENT'. Passing the absolute
// path per-request closes that gap without requiring a worker restart.
//
// The fallback chain is deliberate: an nvm node upgrade invalidates the
// stored absolute path, and in that case we must degrade to PATH lookup
// rather than hard-fail a host that would otherwise work. X_OK (not F_OK)
// because a path that exists but isn't executable is just as fatal to
// spawn() as a missing one.
function resolveClaudeCommand(claudeBin, extraPaths, baseEnv) {
  const env = { ...baseEnv };
  if (claudeBin) {
    try {
      accessSync(claudeBin, fsConstants.X_OK);
      return { cmd: claudeBin, env };
    } catch (e) {
      console.error('[nova-agent-worker] claudeBin 不可执行，回退 PATH 解析: '
        + claudeBin + ' (' + (e && (e.code || e.message)) + ')');
    }
  }
  const extra = String(extraPaths || '').split(pathDelim).map((s) => s.trim()).filter(Boolean);
  if (extra.length) {
    const cur = (env.PATH || '').split(pathDelim).filter(Boolean);
    const seen = new Set(extra);
    env.PATH = [...extra, ...cur.filter((d) => !seen.has(d))].join(pathDelim);
  }
  return { cmd: 'claude', env };
}

// WORKER_VERSION is a placeholder that NovaWorkbench's install flow stamps
// with the binary's own agentWorkerVersion before uploading this file to a
// remote Agent host (see backend/internal/handler/agent_worker_files.go).
// Running the worker directly via 'npm start' in dev reports the raw
// placeholder, which is harmless — no dev flow depends on this value.
const WORKER_VERSION = '__WORKER_VERSION__';

const app = express();
// 50MB cap: the prompt can carry pre-read project context (~40KB) plus
// resume session transcripts. Tight caps here would surface as 413 mid-stream,
// which is harder to diagnose than a generous limit.
app.use(express.json({ limit: '50mb' }));

// /v1/health — used by NovaWorkbench's Check flow to verify the worker is
// alive before starting a coding run. Returns the claude CLI version (best
// effort) so we can alert on stale installs.
//
// claudeVersion is the ONLY signal that reflects whether this worker
// process — with its frozen PATH — can actually spawn claude. The Go-side
// Check probe compares it against the SSH-shell probe: SSH sees a
// login-ish PATH (and sources nvm.sh), the worker does not, so "SSH finds
// claude" never implies "the worker can spawn it". 'path' is echoed back
// so an operator diagnosing that split can see the worker's actual PATH
// without SSH-ing in and reading /proc/<pid>/environ.
app.get('/v1/health', async (_req, res) => {
  let claudeVersion = 'unknown';
  try {
    claudeVersion = await readClaudeVersion();
  } catch {
    // Health must never fail on a missing CLI — the wizard's "install
    // deps" path is the one that surfaces that, not the per-run health
    // probe. An unknown version is a soft signal, not an error.
  }
  res.json({
    status: 'ok',
    claudeVersion,
    workerVersion: WORKER_VERSION,
    path: process.env.PATH || '',
  });
});

// /v1/run — primary endpoint. Streams 'claude -p ... --output-format
// stream-json --verbose ...' events as NDJSON (one JSON object per line,
// flushed immediately) so NovaWorkbench's Go-side parseStreamJSONFromReader
// can consume them line-by-line unchanged.
//
// Why NDJSON instead of SSE: SSE wraps each event in 'data: <json>\n\n',
// but our parser does a straight 'json.Unmarshal' per line. Keeping the wire
// format identical to 'claude --output-format stream-json --verbose' output
// means zero parser changes on the Go side.
//
// Streaming: the response stays open for the lifetime of the child process.
// One JSON event per line, flushed after each 'write'. Errors surface as
// '{type:"error", errorCategory, error, stderr?, code?, cause?}' lines
// followed by response close — Go parser handles 'type:"error"' and
// surfaces the 'errorCategory' for a tailored fix-hint.
app.post('/v1/run', async (req, res) => {
  const opts = buildRunRequest(req.body ?? {});

  res.setHeader('Content-Type', 'application/x-ndjson');
  res.setHeader('Cache-Control', 'no-cache');
  res.setHeader('Connection', 'keep-alive');
  res.flushHeaders?.();

  // Defense-in-depth: verify workDir is strictly inside
  // /tmp/nova-agent/<projectID>/<reqID> before any spawn. NovaWorkbench's
  // runRemoteCoding already does this on the Go side via
  // RequireRemoteDevWorkDir; this worker-side check is the last gate against
  // any path escaping the per-requirement isolation. Fail fast with an
  // explicit 4xx so a misconfigured request doesn't spend 5s on preflight
  // before being rejected. Wire format matches the rest of /v1/run: a
  // single NDJSON error event then res.end() — Go-side parseStreamJSONFromReader
  // already handles 'type:"error"' and surfaces 'error' to the user.
  const reqBody = req.body ?? {};
  const workDirReqId = (typeof reqBody.reqId === 'string' && reqBody.reqId)
    || (typeof opts.workDir === 'string' ? opts.workDir.split('/').filter(Boolean).pop() : '');
  try {
    assertWorkDirInScope(opts.workDir, workDirReqId);
  } catch (e) {
    const status = e.status || 500;
    res.status(status);
    res.write(JSON.stringify({
      type: 'error',
      errorCategory: 'workdir_out_of_scope',
      error: e.message,
      code: status,
    }) + '\n');
    res.end();
    return;
  }

  // Early bailout: if the worker itself is running as root (uid 0), the
  // Claude CLI refuses --dangerously-skip-permissions with a hard error
  // before doing any work. We don't want to wait through the 5s preflight
  // + the real-run timeout to surface that — the Go side already maps the
  // 'running_as_root' category to a tailored fix hint telling the operator
  // to provision a non-root SSH user. The check is 'typeof process.getuid
  // === 'function'' so platforms without a uid (Windows) silently skip —
  // Windows doesn't have the root/sudo concept the CLI is rejecting here.
  if (typeof process.getuid === 'function' && process.getuid() === 0) {
    res.write(JSON.stringify({
      type: 'error',
      errorCategory: 'running_as_root',
      error: 'Agent 服务器以 root 身份运行，Claude CLI 不允许 root/sudo 使用 --dangerously-skip-permissions',
      stderr: '--dangerously-skip-permissions cannot be used with root/sudo privileges for security reasons',
      code: -1,
    }) + '\n');
    res.end();
    return;
  }

  // Preflight — validate the claude CLI works in this env+cwd before
  // launching the real run. Without this probe, a misconfigured environment
  // would surface as the real run emitting one "error" event and then
  // exiting silently — indistinguishable from a successful empty run. 5s
  // budget keeps the round-trip short on the failure path; the success
  // path adds ~1-2s which is dominated by Node + claude cold-start anyway.
  // The child process env is the agent host's own environment only. The
  // platform-pinned keys (auth token / base URL / model / tier pins) are NOT
  // merged here — they're delivered via --settings '{"env":{...}}' (see
  // buildSettingsArg) so they land at the top of the CLI's settings stack and
  // can't be shadowed by a stale ~/.claude/settings.json or an inherited
  // ANTHROPIC_API_KEY.
  // process.env.PATH was already rewritten at module load (see
  // resolveExtendedPath above) so the spawn child can resolve 'claude' even
  // when systemd launched us with a stripped PATH.
  const childEnv = { ...process.env };
  // Ensure TMPDIR (and the TMP/TEMP aliases) point at a writable location
  // before we hand the env to claude. On macOS, agent users provisioned
  // only via SSH often inherit $TMPDIR=/var/folders/<random>/T from the
  // per-user temp dir provisioned at graphical login — but if the SSH
  // session's per-user dir was never created (no graphical login, fresh
  // account, or a stripped launchd env), the CLI's internal tmpdir setup
  // tries to mkdir '/var/folders' itself and fails with EACCES before
  // --print ping even starts. We walk a fallback chain (existing TMPDIR →
  // $HOME → /tmp → cwd → bare /tmp) so the CLI always has a writable
  // scratch dir regardless of how the SSH user was provisioned.
  const tmpdir = resolveTmpdir(childEnv);
  childEnv.TMPDIR = tmpdir;
  childEnv.TMP = tmpdir;
  childEnv.TEMP = tmpdir;
  // Also export the resolved value back into this worker's own env so any
  // node-side libraries (e.g. any future fs.mkdtemp call inside the worker
  // itself, or a downstream SDK that we don't currently use) inherit the
  // same writable location — the previous version only patched the child
  // env, which left Node's own os.tmpdir() pointing at the bad path.
  process.env.TMPDIR = tmpdir;
  process.env.TMP = tmpdir;
  process.env.TEMP = tmpdir;
  // Build the --settings block once and hand the SAME string to both the
  // preflight probe and the real run, so the ping validates byte-for-byte
  // the auth / base URL / model the real run will use (instead of the agent
  // host's CLI defaults, which a third-party endpoint may not serve).
  const settingsArg = buildSettingsArg(opts);

  // Log BOTH the preflight command and the planned real-run command up front
  // — before any spawn. The previous shape only logged after preflight
  // succeeded, so a preflight failure (timeout / unrecognized_model /
  // network unreachable) left the journal and the wizard job panel with
  // nothing but 'preflight 失败' — operators couldn't tell whether the
  // settings JSON was wrong, the model name was wrong, or the auth token was
  // missing. We now write three lines to stderr + the NDJSON stream so a
  // failure shows the exact arg list the CLI was invoked with.
  const realArgs = buildClaudeArgs(opts, settingsArg);
  const realRendered = renderCommand(realArgs);
  // The preflight uses its own (smaller) pingArgs list — render it the same
  // way so the logged command is copy-pasteable, then append the same
  // --settings JSON so the two logs diff cleanly on auth / base URL / model.
  const pingArgs = ['--print', 'ping', '--output-format', 'text', '--setting-sources', 'project,local'];
  if (settingsArg) pingArgs.push('--settings', settingsArg);
  const pingRendered = renderCommand(pingArgs);
  console.error('[nova-agent-worker] exec preflight: ' + pingRendered);
  console.error('[nova-agent-worker] exec planned:  ' + realRendered);
  res.write(JSON.stringify({ type: 'log', content: '准备 preflight: ' + pingRendered }) + '\n');
  res.write(JSON.stringify({ type: 'log', content: '准备执行主命令: ' + realRendered }) + '\n');
  if (typeof res.flush === 'function') res.flush();

  // Resolve the claude command ONCE and use the same {cmd, env} for both
  // the preflight probe and the real run — otherwise a preflight that
  // passed via the absolute path could be followed by a real run that
  // fell back to PATH (or vice versa), and the probe would stop being
  // predictive of the thing it's supposed to gate.
  const claudeCmd = resolveClaudeCommand(opts.claudeBin, opts.extraPaths, childEnv);
  if (claudeCmd.cmd !== 'claude') {
    res.write(JSON.stringify({ type: 'log', content: '使用下发的 claude 绝对路径: ' + claudeCmd.cmd }) + '\n');
  }

  const pf = await preflight(opts.workDir, claudeCmd, settingsArg);
  if (pf.ok && pf.warning) {
    // Non-fatal preflight noise (today: the model-catalog diagnostic). Surfaced
    // once per run so an operator reading the job panel knows the line in the
    // journal is expected for custom models, instead of hunting a failure that
    // isn't there.
    console.error('[nova-agent-worker] ' + pf.warning);
    res.write(JSON.stringify({ type: 'log', content: '⚠ ' + pf.warning }) + '\n');
    if (typeof res.flush === 'function') res.flush();
  }
  if (!pf.ok) {
    res.write(JSON.stringify({
      type: 'error',
      errorCategory: pf.errorCategory,
      error: 'preflight 失败（' + pf.errorCategory + '）',
      stderr: pf.stderr || '',
      code: pf.code,
      preflight: true,
      // Echo what we actually tried. Without these two fields a
      // cli_not_found is indistinguishable between "no path was sent",
      // "the sent path was wrong" and "PATH lookup missed" — which is
      // exactly the ambiguity that made this class of bug hard to pin down.
      resolvedClaudeBin: claudeCmd.cmd,
      resolvedPath: claudeCmd.env.PATH || '',
    }) + '\n');
    res.end();
    return;
  }

  const args = realArgs;
  // Real-run entry log — preflight already passed, just record the boundary.
  console.error('[nova-agent-worker] exec: ' + realRendered);
  res.write(JSON.stringify({ type: 'log', content: '执行命令: ' + realRendered }) + '\n');
  if (typeof res.flush === 'function') res.flush();
  let proc;
  try {
    proc = spawn(claudeCmd.cmd, args, {
      cwd: opts.workDir,
      env: claudeCmd.env,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
  } catch (e) {
    // spawn() can throw synchronously when the binary is not found (rare
    // on Linux but possible). Treat as cli_not_found.
    res.write(JSON.stringify({
      type: 'error',
      errorCategory: 'cli_not_found',
      error: String(e && e.message || e),
      code: -1,
      resolvedClaudeBin: claudeCmd.cmd,
      resolvedPath: claudeCmd.env.PATH || '',
    }) + '\n');
    res.end();
    return;
  }

  let stderr = '';
  proc.stderr.on('data', (d) => { stderr += d.toString(); });
  proc.stdout.on('data', (chunk) => {
    // claude emits one JSON object per line; forward verbatim so the Go
    // parser can json.Unmarshal each line directly. Flush after every
    // chunk so the client sees events live (otherwise the kernel buffer
    // hides progress for the first few seconds of a long turn).
    res.write(chunk);
    if (typeof res.flush === 'function') res.flush();
  });
  proc.on('error', (e) => {
    // ENOENT from the event emitter means spawn() couldn't find claude
    // (asynchronous path). Treat other errors as network-shaped.
    const cat = (e && e.code === 'ENOENT') ? 'cli_not_found' : classifyError(null, String(e.message || e));
    res.write(JSON.stringify({
      type: 'error',
      errorCategory: cat,
      error: String(e.message || e),
      stderr: String(e.message || e),
      code: e.code,
      resolvedClaudeBin: claudeCmd.cmd,
      resolvedPath: claudeCmd.env.PATH || '',
    }) + '\n');
    res.end();
  });
  proc.on('close', (code, signal) => {
    if (code === 0) {
      res.end();
      return;
    }
    // Non-zero exit — emit a single error event with the captured stderr so
    // the Go parser (which already knows how to surface errorCategory +
    // stderr) can render a tailored fix hint. 'serializeError' mirrors the
    // shape classifyError expects.
    const payload = serializeCLIError({ code, signal, stderr });
    res.write(JSON.stringify({ type: 'error', ...payload }) + '\n');
    res.end();
  });
});

// preflight spawns 'claude --print ping' with the same env+cwd as the real
// call and returns {ok, errorCategory, stderr, stdout, code}. Catches the
// actual CLI failure the Go side would otherwise see as a generic "exit 1".
//
// 15s timeout: 'claude --print ping' round-trips through the Anthropic API
// and normally completes in <2s on a healthy host, but custom base URLs
// (e.g. minimax, deepseek proxies) can run the CLI's TLS handshake +
// first-call model catalog lookup in 5-8s on a cold cache, and a
// freshly-restarted systemd --user worker adds another second of node
// startup before the spawn even happens. 5s (the previous value) was
// empirically too aggressive — even a healthy host with MiniMax-M3 on a
// private base URL would surface as 'preflight_timeout' because the ping
// takes ~6s end-to-end, masking the real CLI behaviour behind an opaque
// timeout. 15s still distinguishes "API hung" from "slow first call"
// without dragging the failure path into the 30s-deep territory a real
// run exposes. Keep this in sync with the [preflight timeout after Xs]
// string injected into the stderr below.
//
// Fast-fail on classified stderr: a well-known failure pattern (see
// classifyError) is emitted to stderr as soon as the CLI rejects a config
// (auth token, base URL), but the CLI then hangs waiting on something else
// (catalog refresh, retry) and never exits on its own. Without fast-fail,
// those errors would surface as 'preflight_timeout' after 15s, hiding the
// real reason behind a misleading "Agent 服务器无法访问 API". classifyError
// runs on every stderr chunk; any non-'unknown' category resolves the
// promise immediately and SIGTERMs the hung child. The Go side already has
// tailored fix hints for those categories (workerCategoryHint), so a
// sub-second failure is also a much more actionable error message.
//
// What fast-fail must NOT do is kill a healthy run: Claude Code writes
// '[claude-code:unrecognized_model] {"model":"...","query_source":"sdk"}' to
// stderr whenever a request goes out for a model id that is not in its local
// catalog — which is EVERY request for a custom model on a private base URL
// (MiniMax-M3, DeepSeek, …). That line is a print-mode DIAGNOSTIC, not a
// rejection: the request still goes out, the answer still comes back, and
// 'claude --print ping' still exits 0. Fast-failing on it SIGTERM'd a
// working preflight and reported 'preflight 失败（unrecognized_model）' with
// a null exit code. The fast-fail path therefore classifies through
// classifyFatalError, which strips those diagnostic lines, so they can never
// resolve the probe early; if the probe really does fail with nothing else
// on stderr, the close handler still classifies it as 'unrecognized_model'
// via classifyError's end-of-function fallback and the tailored hint is
// preserved.
//
// 'claudeCmd' is the {cmd, env} pair returned by resolveClaudeCommand — the
// caller resolves it once and passes the SAME pair here and to the real
// run, so the probe exercises byte-for-byte the binary and PATH the real
// spawn will use.
const PREFLIGHT_TIMEOUT_MS = 15000;
function preflight(workDir, claudeCmd, settingsArg) {
  return new Promise((resolve) => {
    let proc;
    let settled = false;
    const settle = (result) => {
      if (settled) return;
      settled = true;
      try { if (proc) proc.kill('SIGTERM'); } catch {}
      resolve(result);
    };
    try {
      // The --settings block (built by buildSettingsArg, the same string the
      // real run uses) is the PRIMARY source of auth / base URL / model: it
      // sits at the top of the CLI's settings stack, above the seeded
      // ~/.claude/settings.json the install script places on the agent host
      // (placeholder token + MiniMax-M3 model pins), so the ping round-trips
      // the platform's real config instead of the host's stale defaults.
      //
      // --setting-sources "project,local" is kept as defense-in-depth: it
      // drops the user source so other stray user-settings keys (permissions,
      // hooks, a top-level 'model') can't leak into the probe. CLI quirk:
      // '--setting-sources' only accepts combinations of {user, project,
      // local} — no "drop ALL" value — so project+local is the best we can do;
      // a fresh agent host only seeds the user source, so this is equivalent
      // to "drop everything" in practice.
      const pingArgs = ['--print', 'ping', '--output-format', 'text', '--setting-sources', 'project,local'];
      if (settingsArg) {
        pingArgs.push('--settings', settingsArg);
      }
      proc = spawn(claudeCmd.cmd, pingArgs, {
        cwd: workDir,
        env: claudeCmd.env,
        stdio: ['ignore', 'pipe', 'pipe'],
      });
    } catch (e) {
      settle({
        ok: false,
        errorCategory: 'cli_not_found',
        stderr: String(e && e.message || e),
        code: -1,
      });
      return;
    }
    let stderr = '';
    let stdout = '';
    let sawModelDiagnostic = false;
    proc.stdout.on('data', (d) => { stdout += d.toString(); });
    proc.stderr.on('data', (d) => {
      stderr += d.toString();
      if (hasClaudeModelDiagnostic(stderr)) sawModelDiagnostic = true;
      // Fast-fail on classifier-detected errors. The CLI emits a tagged
      // error line on stderr the moment it rejects the config, then keeps
      // the process alive doing internal bookkeeping — the next chunk won't
      // come for seconds. classifyError's regexes match the partial stderr,
      // so a single tagged line is enough. 'unknown' is intentionally
      // skipped: a CLI that just happens to print something category-shaped
      // shouldn't poison an otherwise-healthy run. classifyFatalError drops
      // the diagnostic-only lines (the model-catalog notice) entirely, so a
      // chunk carrying nothing else reads as 'unknown' and the probe runs to
      // completion — the process is still alive and may well exit 0.
      const cat = classifyFatalError(null, stderr);
      if (cat !== 'unknown') {
        settle({
          ok: false,
          errorCategory: cat,
          stderr,
          stdout,
          code: null,
        });
      }
    });
    proc.on('error', (e) => {
      const cat = (e && e.code === 'ENOENT') ? 'cli_not_found' : classifyError(null, String(e.message || e));
      settle({ ok: false, errorCategory: cat, stderr: String(e.message || e), stdout, code: e.code });
    });
    proc.on('close', (code) => {
      if (code === 0) {
        settle({
          ok: true,
          warning: sawModelDiagnostic
            ? 'Claude Code 不认识该 model id（自定义 model 走私有 base URL 时正常现象），stderr 里的 [claude-code:unrecognized_model] 仅为诊断提示；preflight 已通过，执行不受影响。'
            : '',
        });
      } else {
        settle({
          ok: false,
          errorCategory: classifyError(null, stderr || stdout),
          stderr,
          stdout,
          code,
        });
      }
    });
    setTimeout(() => {
      // SIGTERM gives the CLI ~1s to flush stderr before our exit
      // resolves; the final stderr we read from above already has the
      // early output and is usually enough to classify the failure.
      settle({
        ok: false,
        errorCategory: 'preflight_timeout',
        stderr: (stderr || '') + '\n[preflight timeout after ' + (PREFLIGHT_TIMEOUT_MS / 1000) + 's]',
        stdout,
        code: 143,
      });
    }, PREFLIGHT_TIMEOUT_MS);
  });
}

// resolveTmpdir returns a writable tmpdir path to inject into the child
// env. Always returns a path — even on total failure we fall back to bare
// '/tmp', which is world-writable on every Linux distro (and where the
// only failure mode is a full disk, which is its own problem).
//
// Why we override TMPDIR: on macOS, SSH-only users (e.g. the nova-agent
// SSH user provisioned by our install flow) often have $TMPDIR inherited
// from the per-user temp dir launchd creates at graphical login — but
// if the user has never logged in graphically, that dir doesn't exist.
// 'claude' then tries to mkdir '/var/folders' (root-owned) and the whole
// process exits with EACCES before --print ping can do anything. The
// same shape hits an SSH session forwarded by 'SendEnv TMPDIR' from a
// macOS dev box — the SSH user on a Linux agent never has a
// /var/folders/<uuid>/T/ tree.
//
// Fallback chain (first success wins):
//   1. Existing $TMPDIR — but ONLY if it's not the macOS-only
//      /var/folders/<uuid>/T path AND we can stat + write to it.
//   2. $HOME/.nova-agent-worker-XXXX — the SSH user's home is always
//      writable for the user itself, and survives a 'chmod 0700 $HOME'
//      that some hardening guides apply (the per-process mkdtempSync
//      runs as the user, so it inherits the user's own write perms).
//   3. /tmp/.nova-agent-worker-XXXX — Linux always grants world-write
//      on /tmp (sticky bit stops cross-user stomping). Used when $HOME
//      is somehow not writable (e.g. NFS-mounted home, container with
//      a read-only mount).
//   4. process.cwd() — last resort before the bare /tmp fallback; only
//      fails if cwd was deleted out from under us mid-flight.
//   5. Bare '/tmp' — never fails on Linux. Not ideal (multi-tenant
//      visibility) but always functional, and the CLI only uses it for
//      scratch during one turn, so the noise is bounded.
//
// Each attempt's outcome is logged via console.error so the worker's
// own log (~/nova-agent-worker/worker.log) shows why we picked the path
// we did — without that signal, "still EACCES" with a /var/folders
// stderr was indistinguishable from "override silently failed because
// HOME was read-only" and operators couldn't tell which knob to turn.
function resolveTmpdir(env) {
  const cur = env && env.TMPDIR;
  // Only trust the existing TMPDIR if it exists, is writable, AND is not
  // the macOS-shaped path that wouldn't exist on Linux. We do an actual
  // access check (W_OK + X_OK) — a path-shaped check alone was too eager
  // in the previous version and accepted /var/folders paths that didn't
  // exist on the agent host, leaving claude to EACCES on mkdir.
  if (cur && cur !== '/var/folders' && !cur.startsWith('/var/folders/')) {
    try {
      accessSync(cur, fsConstants.W_OK | fsConstants.X_OK);
      return cur;
    } catch (e) {
      console.error('[nova-agent-worker] existing TMPDIR=' + cur + ' not usable: ' + (e.code || e.message) + '; falling back');
    }
  }
  if (cur) {
    console.error('[nova-agent-worker] ignoring macOS-shaped TMPDIR=' + cur + ' (would EACCES on Linux)');
  }

  // 2) HOME-based tmpdir
  const home = (env && env.HOME) || process.env.HOME;
  if (home) {
    try {
      const p = mkdtempSync(join(home, '.nova-agent-worker-'));
      console.error('[nova-agent-worker] resolved TMPDIR via $HOME: ' + p);
      return p;
    } catch (e) {
      console.error('[nova-agent-worker] mkdtempSync under $HOME=' + home + ' failed: ' + (e.code || e.message) + '; trying /tmp');
    }
  } else {
    console.error('[nova-agent-worker] $HOME not set; trying /tmp');
  }

  // 3) /tmp-based tmpdir
  try {
    const p = mkdtempSync('/tmp/.nova-agent-worker-');
    console.error('[nova-agent-worker] resolved TMPDIR via /tmp: ' + p);
    return p;
  } catch (e) {
    console.error('[nova-agent-worker] mkdtempSync under /tmp failed: ' + (e.code || e.message) + '; trying cwd');
  }

  // 4) cwd-based tmpdir
  try {
    const p = mkdtempSync(join(process.cwd(), '.nova-agent-worker-'));
    console.error('[nova-agent-worker] resolved TMPDIR via cwd: ' + p);
    return p;
  } catch (e) {
    console.error('[nova-agent-worker] mkdtempSync under cwd=' + process.cwd() + ' failed: ' + (e.code || e.message) + '; using bare /tmp');
  }

  // 5) Last resort — bare /tmp. Always exists on Linux. The CLI will
  // use this as its scratch dir for the lifetime of one turn; sub-temp
  // dirs inside it (e.g. /tmp/.claude-<pid>) are still per-process and
  // don't collide between concurrent runs.
  return '/tmp';
}

// readClaudeVersion shells out to 'claude --version' and returns the first
// non-empty line. Used by /v1/health to surface the installed CLI version
// to the Go side (so an operator can tell at a glance whether a remote
// agent server is on a stale claude). Bounded 3s — if the CLI hangs the
// health probe shouldn't drag the user-visible badge into a "loading" state.
function readClaudeVersion() {
  return new Promise((resolve, reject) => {
    // Deliberately NO claudeBin override here: /v1/health must report what
    // this worker resolves on its OWN frozen PATH, because that is exactly
    // the property the Go-side Check needs to test. Resolving via a
    // caller-supplied path would make health report 'ok' for a worker that
    // still can't run anything the moment a request arrives without one.
    const proc = spawn('claude', ['--version'], {
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let out = '';
    proc.stdout.on('data', (d) => { out += d.toString(); });
    proc.on('error', reject);
    proc.on('close', (code) => {
      if (code !== 0) {
        reject(new Error('claude --version exit ' + code));
        return;
      }
      const first = out.split('\n').map(l => l.trim()).find(Boolean);
      resolve(first || 'unknown');
    });
    setTimeout(() => {
      try { proc.kill('SIGTERM'); } catch {}
      reject(new Error('claude --version timeout'));
    }, 3000);
  });
}

// classifyError maps a captured stderr (or thrown Error) into a coarse
// category. The point isn't to be exhaustive — it's to give the Go side
// enough signal to surface a tailored fix hint (e.g. "unrecognized_model"
// → "检查设置 → Claude 配置里的 model 名" instead of the generic "exit 1").
//
// Pattern sources:
//   - Claude Code CLI's own error tags: '[claude-code:not_logged_in]' etc.
//     ('[claude-code:unrecognized_model]' is NOT one of them — it is a
//     diagnostic the CLI prints on successful runs too; see
//     stripClaudeDiagnostics below.)
//   - Node / undici / DNS error codes: ENOTFOUND, ECONNREFUSED, etc.
//   - HTTP-status hints in stderr: 401, 403, 404, 429, 5xx.
//
// Order matters — more specific patterns come first so e.g.
// "401 Unauthorized" classifies as 'auth_failed' instead of falling
// through to a generic "unauthorized" bucket.
function classifyError(err, stderr) {
  const msg = (err && err.message) || '';
  const code = err && err.code;
  const raw = (msg + '\n' + (stderr || '')).toLowerCase();
  // Classify on the stderr MINUS Claude Code's diagnostic-only lines. The
  // model-catalog notice rides along with every request for a custom model,
  // including successful ones, so leaving it in would (a) fast-fail healthy
  // preflights and (b) shadow the real category when a run fails for an
  // unrelated reason (a 401 preceded by the notice used to classify as
  // 'unrecognized_model' and produce the wrong fix hint).
  const text = stripClaudeDiagnostics(raw);

  // Model catalog issues — Claude Code ships a hardcoded model list and
  // refuses to run when the selected model is neither in it nor served by
  // the configured endpoint.
  if (/model.{0,4}catalog|behavesas|modelpicker|issue with the selected model/.test(text)) return 'unrecognized_model';

  // Auth — 401 / not-logged-in / token-shaped rejections. We accept both
  // "Authentication failed" and "not logged in" because Claude Code
  // changes wording between versions.
  if (/401|unauthorized|authentication failed|not logged in|invalid.{0,4}token|invalid.{0,4}api.{0,4}key|invalid.{0,4}auth/.test(text)) return 'auth_failed';

  // Running as root — Claude CLI rejects --dangerously-skip-permissions
  // with a hard error before any tool/API call when the effective uid is
  // 0 or sudo is in effect. The exact stderr is "cannot be used with
  // root/sudo privileges for security reasons"; we also accept the
  // "must not be run as root" / "running with root" wordings some
  // versions use, so a CLI wording change doesn't silently fall through
  // to the 'unknown' bucket. Keep this BEFORE the generic permission
  // categories so it wins over a coincidental "permission" match in the
  // same stderr.
  if (/cannot be used with root|root\s*\/\s*sudo privileges|running with root|must not be run as root|running as root/.test(text)) return 'running_as_root';

  // Forbidden — token valid but lacks scope / region. Different fix from
  // auth_failed (token is right; permission is wrong).
  if (/403|forbidden/.test(text)) return 'auth_forbidden';

  // Rate limit / quota — worth its own category so the UI can suggest
  // "等几分钟重试" instead of "检查配置".
  if (/429|rate.{0,4}limit|too many requests/.test(text)) return 'rate_limited';
  if (/quota|insufficient.{0,4}credit|insufficient.{0,4}balance/.test(text)) return 'quota_exceeded';

  // Model-not-found on the API side — different from the catalog check
  // (this is the API saying it doesn't know the model). Pattern-matched
  // only when "model" appears nearby to keep this from firing on every
  // "404 not found" message.
  if (/404/.test(text) && /model/.test(text)) return 'model_not_found';

  // Session-not-found — the resume target session doesn't exist on disk.
  // Common when SFTP sync race or session was wiped.
  if (/session.{0,4}not.{0,4}found|no conversation found|cannot find.{0,8}session/.test(text)) return 'session_not_found';

  // CLI binary missing or not executable.
  if (code === 'ENOENT' || /enoent/.test(text) || /command not found/.test(text) || /spawn.{0,8}enoent/.test(text)) return 'cli_not_found';

  // Network — order matters: ENETUNREACH / ETIMEDOUT / ENOTFOUND / EAI_AGAIN
  // are the host-cannot-reach-API family. ECONNRESET and ECONNREFUSED are
  // mid-stream or local-only.
  if (/enetunreach|etimedout|eai_again/.test(text)) return 'network_unreachable';
  if (/enotfound|getaddrinfo/.test(text)) return 'dns_unresolved';
  if (/econnreset/.test(text)) return 'connection_reset';
  if (/econnrefused/.test(text)) return 'connection_refused';

  // Permission on local file ops — different from auth: filesystem EACCES.
  // When the EACCES path is under /var/folders the real cause is a
  // macOS-only TMPDIR mis-provisioning on SSH-only users, not the
  // worktree. Keep the same category so the wizard's Chinese fix hint
  // renders, but the hint itself narrows the advice (see
  // wizard.go:workerCategoryHint) when it sees the /var/folders
  // signature in stderr.
  if (/eacces/.test(text)) return 'permission_denied';

  // SDK-specific surfaces (no longer relevant here, but kept so an older
  // Go side that still pattern-matches max_turns continues to work).
  if (/max.{0,4}turns|maximum.{0,4}turns/.test(text)) return 'max_turns';

  // Last resort: nothing above matched, but the run did emit the model
  // diagnostic. On a FAILED run (this function is only consulted for one)
  // that's the only signal we have, so keep the old category — the Go side's
  // hint for it covers the "custom model on a private base URL" case. On a
  // successful run classifyError is never called, so this cannot turn a
  // healthy preflight into a failure.
  if (hasClaudeModelDiagnostic(raw)) return 'unrecognized_model';

  return 'unknown';
}

// CLAUDE_MODEL_DIAGNOSTIC_RE matches Claude Code's print-mode model notice:
//   [claude-code:unrecognized_model] {"model":"MiniMax-M3","query_source":"sdk"}
// Written to stderr whenever a request goes out for a model id that is not in
// the CLI's local catalog. Per the CLI's own release notes it is a diagnostic
// ("map it with modelOverrides to silence"), not a rejection — verified
// against a stub Anthropic endpoint: the line is printed, the answer still
// streams back and 'claude --print ping' exits 0.
const CLAUDE_MODEL_DIAGNOSTIC_RE = /^.*\[claude-code:unrecognized_model\].*$/gim;

// hasClaudeModelDiagnostic reports whether the text carries that notice.
// Uses .test on a fresh regex because CLAUDE_MODEL_DIAGNOSTIC_RE is /g and
// therefore stateful across calls.
function hasClaudeModelDiagnostic(text) {
  return /\[claude-code:unrecognized_model\]/i.test(String(text || ''));
}

// stripClaudeDiagnostics removes the diagnostic-only lines so the remaining
// text is the failure evidence and nothing else.
function stripClaudeDiagnostics(text) {
  return String(text || '').replace(CLAUDE_MODEL_DIAGNOSTIC_RE, '');
}

// classifyFatalError is classifyError for callers that classify a STILL-RUNNING
// process from a partial stderr chunk (the preflight fast-fail). Those callers
// must never act on the model diagnostic: the process that printed it is alive
// and usually on its way to exiting 0, so the diagnostic is dropped before
// classification instead of falling through to classifyError's last-resort
// 'unrecognized_model'. Post-mortem callers (non-zero exit) keep using
// classifyError, where that fallback is the correct read.
function classifyFatalError(err, stderr) {
  return classifyError(err, stripClaudeDiagnostics(stderr));
}

// serializeCLIError shapes a {code, signal, stderr} view of a non-zero
// child-process exit into the error-payload shape the Go parser already
// understands. The Go side looks at 'errorCategory' to pick a fix hint
// and at 'stderr' to show the actual CLI failure line; the other fields
// are diagnostic sugar.
function serializeCLIError({ code, signal, stderr }) {
  const trimmed = (stderr || '').trim();
  // The one-line 'error' summary is the last MEANINGFUL stderr line: the
  // model diagnostic is dropped first so a failure whose output happens to
  // end with it still names the actual failure. 'stderr' below keeps the
  // full text (diagnostic included) for the job panel.
  const meaningful = stripClaudeDiagnostics(trimmed).split('\n').map((l) => l.trim()).filter(Boolean);
  const out = {
    error: meaningful.length
      ? meaningful[meaningful.length - 1].slice(0, 800)
      : (trimmed
        ? trimmed.split('\n').slice(-1)[0].slice(0, 800)
        : 'claude 进程退出码 ' + code + (signal ? '（信号 ' + signal + '）' : '')),
    errorCategory: classifyError(null, stderr || ''),
  };
  if (code != null) out.code = code;
  if (signal) out.signal = signal;
  if (trimmed) {
    out.stderr = trimmed.length > 8192
      ? trimmed.slice(0, 8192) + '\n…[truncated]'
      : trimmed;
  }
  return out;
}

// buildRunRequest maps the NovaWorkbench-shaped POST body to the worker's
// internal opts. The mapping is intentionally explicit (rather than passing
// req.body straight through) so:
//   1. Unknown fields don't accidentally reach the CLI and silently change
//      behavior on a bump.
//   2. We can validate required fields here and return clean errors.
//
// Defaults match the wizard's remote-coding call site: the platform's
// active claude_configs row is the only source of auth / base URL /
// model pinning, so ignoreLocalSettings defaults true. The previous
// SDK-based worker mapped this to settingSources:[]; the CLI equivalent
// is --setting-sources "" (empty string = no settings files at all).
function buildRunRequest(body) {
  const {
    workDir,
    prompt,
    model,
    systemPrompt,
    sessionId,
    resume,
    fork,
    forkSessionId,
    env,
    allowedTools,
    disallowedTools,
    permissionMode,
    overrideSettingSources,
    ignoreLocalSettings,
    claudeConfigId,
    claudeBin,
    extraPaths,
  } = body;

  if (!workDir) {
    throw new Error('workDir is required');
  }
  if (typeof prompt !== 'string') {
    throw new Error('prompt is required and must be a string');
  }

  // Coerce env values to strings to avoid [object Object] leaks when
  // callers accidentally pass non-strings (e.g. numbers from JSON).
  let envMap;
  if (env && typeof env === 'object') {
    envMap = {};
    for (const [k, v] of Object.entries(env)) {
      envMap[k] = v == null ? '' : String(v);
    }
  }

  // Setting-source resolution. Three modes, in order:
  //   ignoreLocalSettings === false  → load every source (CLI default)
  //   overrideSettingSources         → drop user only ("project,local")
  //   default                        → drop user only ("project,local") so
  //                                   the platform's env (auth / base URL /
  //                                   model pinning) wins over the seeded
  //                                   ~/.claude/settings.json the install
  //                                   script places on the agent host.
  //
  // CLI quirk: '--setting-sources' only accepts combinations of
  // {user, project, local} — there is no "drop ALL" value, so the
  // semantic of "drop everything" maps to dropping just the user
  // source. Project + local are typically empty on a fresh agent
  // host (the install only seeds ~/.claude/settings.json), so this
  // gives us the platform-env-wins behavior the wizard wants.
  let settingSources = 'project,local';
  if (ignoreLocalSettings === false) {
    settingSources = 'user,project,local';
  } else if (overrideSettingSources) {
    settingSources = 'project,local';
  }

  return {
    workDir,
    prompt,
    model: model || '',
    systemPrompt: systemPrompt || '',
    sessionId: sessionId || '',
    resume: !!resume,
    fork: !!fork,
    forkSessionId: forkSessionId || '',
    env: envMap,
    allowedTools: Array.isArray(allowedTools) ? allowedTools : [],
    disallowedTools: Array.isArray(disallowedTools) ? disallowedTools : [],
    permissionMode: permissionMode === 'plan' ? 'plan' : '',
    // The Go side passes the resolved claude_config_id so future tweaks
    // (e.g. surfacing the active gateway in the SSE log) can read it
    // without a second round-trip. Today it's informational.
    claudeConfigId: typeof claudeConfigId === 'string' ? claudeConfigId : '',
    // Runtime location of the claude CLI, resolved by NovaWorkbench's
    // Check/Install and mirrored from agent_servers.claude_bin /
    // .extra_paths. Both optional — an older NovaWorkbench omits them and
    // resolveClaudeCommand falls back to bare PATH lookup, which is the
    // pre-existing behaviour. extraPaths is ':'-separated (the Go side
    // joins the newline-separated DB column before sending).
    claudeBin: typeof claudeBin === 'string' ? claudeBin : '',
    extraPaths: typeof extraPaths === 'string' ? extraPaths : '',
    settingSources,
  };
}

// buildSettingsArg renders the inline JSON for 'claude --settings'. Claude Code
// accepts either a settings file path OR an inline JSON string; the inline form
// lands at the top of the non-managed settings stack, so its 'env' block
// overrides both the process environment and any ~/.claude/settings.json the
// install script seeded on the agent host. That's where we now deliver the
// platform-pinned keys (auth token / base URL / model / tier pins) instead of
// leaking them into the child's process env or relying on --setting-sources to
// suppress a stale user settings file.
//
// The 'env' block mirrors what BuildRemoteEnvPairs sends (ANTHROPIC_AUTH_TOKEN,
// ANTHROPIC_BASE_URL, ANTHROPIC_DEFAULT_{HAIKU,SONNET,OPUS}_MODEL,
// CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT); we additionally fold
// opts.model in as ANTHROPIC_MODEL — the CLI's documented env var for the
// session model, equivalent to the old --model flag but settable here so the
// model travels with the rest of the platform config.
//
// Returns null when there's nothing to pin (no env keys and no model) so the
// caller can omit --settings entirely and leave the CLI defaults untouched.
function buildSettingsArg(opts) {
  const envBlock = { ...(opts.env || {}) };
  if (opts.model) {
    envBlock.ANTHROPIC_MODEL = opts.model;
  }
  if (Object.keys(envBlock).length === 0) {
    return null;
  }
  return JSON.stringify({ env: envBlock });
}

// renderCommand joins a spawn argv into one shell-style line for the worker log
// and the coding job panel. Two flags are special-cased so the line stays useful
// without leaking secrets or flooding the log:
//   --settings <json> → the value is re-serialized with the auth token
//                        (ANTHROPIC_AUTH_TOKEN / ANTHROPIC_API_KEY) blanked; the
//                        rest (base URL / model / tier pins) is shown in full.
//   -p <prompt>       → truncated to a short prefix (the prompt can carry ~40KB
//                        of pre-read project context).
// Everything else is single-quoted so the line can be copy-pasted to a shell.
function renderCommand(args) {
  const parts = [];
  for (let i = 0; i < args.length; i++) {
    const a = args[i];
    if (a === '--settings' && i + 1 < args.length) {
      parts.push('--settings ' + shellQuote(redactSettings(args[i + 1])));
      i++;
    } else if (a === '-p' && i + 1 < args.length) {
      parts.push('-p ' + shellQuote(truncate(args[i + 1], 200)));
      i++;
    } else {
      parts.push(shellQuote(a));
    }
  }
  return parts.join(' ');
}

// redactSettings blanks the auth token inside a --settings JSON string so
// secrets never reach a log. Parses + re-serializes; on parse failure returns
// the raw string unchanged (better to log the raw JSON than hide the whole line).
function redactSettings(jsonStr) {
  try {
    const obj = JSON.parse(jsonStr);
    if (obj && obj.env && typeof obj.env === 'object') {
      if (obj.env.ANTHROPIC_AUTH_TOKEN) obj.env.ANTHROPIC_AUTH_TOKEN = '***';
      if (obj.env.ANTHROPIC_API_KEY) obj.env.ANTHROPIC_API_KEY = '***';
    }
    return JSON.stringify(obj);
  } catch {
    return jsonStr;
  }
}

// shellQuote single-quotes a string so a logged command can be copy-pasted.
function shellQuote(s) {
  return "'" + String(s).replace(/'/g, "'\\''") + "'";
}

// truncate clips a string to n chars and appends a length note when clipped.
function truncate(s, n) {
  s = String(s);
  return s.length <= n ? s : s.slice(0, n) + '…(+' + (s.length - n) + ' chars)';
}

// buildClaudeArgs assembles the 'claude -p ... --output-format stream-json
// --verbose ...' flag list from buildRunRequest output. The mapping mirrors
// backend/internal/llm/gateway.go:streamArgs so the local and remote paths
// produce the same CLI invocation; differences are spelled out below.
//
// Order note: --output-format stream-json --verbose must come before
// --permission-mode / --dangerously-skip-permissions per the CLI's flag
// parser. We follow the same ordering as gateway.go so a regression in one
// doesn't slip past the other.
function buildClaudeArgs(opts, settingsArg) {
  const args = ['-p', opts.prompt, '--output-format', 'stream-json', '--verbose'];
  // Setting-sources always present so the user can't accidentally land on
  // the CLI default (which loads ~/.claude/settings.json over process env
  // and would silently shadow the platform's auth). The default "" drops
  // all sources; callers that need a non-empty list pass an explicit value.
  args.push('--setting-sources', opts.settingSources);

  if (opts.permissionMode === 'plan') {
    args.push('--permission-mode', 'plan');
  } else {
    args.push('--dangerously-skip-permissions');
  }

  if (settingsArg) {
    // Prepend --settings so it lands before the -p block (mirrors how --model
    // used to be prepended; gateway.go does the same to keep the relative
    // ordering stable for diff debugging). The model + auth + base URL travel
    // inside this JSON 'env' block instead of a --model flag + process env.
    args.unshift('--settings', settingsArg);
  }

  if (opts.systemPrompt) {
    // In plan mode the CLI's default system prompt carries the plan-mode
    // instructions (explore read-only, write plan to ~/.claude/plans/,
    // call ExitPlanMode). --system-prompt would REPLACE those and break
    // the plan workflow, so we APPEND the role persona instead.
    if (opts.permissionMode === 'plan') {
      args.push('--append-system-prompt', opts.systemPrompt);
    } else {
      args.push('--system-prompt', opts.systemPrompt);
    }
  }

  // Session threading mirrors gateway.go: a non-resume call passes
  // --session-id <uuid> (starts a new conversation with a known id); a
  // resume call passes --resume <session-id> (continues that conversation).
  // When fork is true (only meaningful with resume), --fork-session is
  // added so the CLI mints a NEW session id that inherits the resumed
  // conversation's full history. When forkSessionId is supplied on a
  // fork, it is passed as --session-id so the caller pre-assigns the
  // forked session's id rather than reading it back from the stream.
  if (opts.sessionId) {
    if (opts.resume) {
      args.push('--resume', opts.sessionId);
      if (opts.fork) {
        args.push('--fork-session');
        if (opts.forkSessionId) {
          args.push('--session-id', opts.forkSessionId);
        }
      }
    } else {
      args.push('--session-id', opts.sessionId);
    }
  }

  if (opts.allowedTools.length > 0) {
    // CLI's variadic form: comma- or space-separated tool names.
    args.push('--allowedTools', opts.allowedTools.join(' '));
  }
  if (opts.disallowedTools.length > 0) {
    args.push('--disallowedTools', opts.disallowedTools.join(' '));
  }

  return args;
}

// assertWorkDirInScope: NovaWorkbench 后端的 runRemoteCoding 严格把 wtPath 锁在
// /tmp/nova-agent/<projectID>/<reqID> 下；本 worker 端做 defense-in-depth 校验，
// 防止任何路径错误在远端被放大。即便 Go 侧所有校验都被绕过，worker 也会拒绝。
//
// 校验三层：
//   1. workDir 必须是非空字符串
//   2. realpathSync 必须成功（路径必须真实存在，否则抛 ENOENT → 400）
//   3. 解析后的真实路径必须在 /tmp/nova-agent/ 下，且 reqId 必须出现在路径段中
//
// 任何一层失败抛出带 status 的 Error，handler 转成对应 4xx + JSON {error}。
function assertWorkDirInScope(workDir, reqId) {
  if (!workDir || typeof workDir !== 'string') {
    throw httpError(400, 'workDir is required');
  }
  // 路径必须存在；realpathSync 不存在则抛 ENOENT
  let real;
  try {
    real = realpathSync(workDir);
  } catch (e) {
    throw httpError(400, 'workDir ' + workDir + ' cannot be resolved: ' + e.message);
  }
  // reqId 必须出现在路径中（防止跨需求路径串台）
  const segs = real.split(pathSep);
  if (!reqId || !segs.includes(reqId)) {
    throw httpError(403, 'workDir ' + real + ' does not contain reqId ' + reqId);
  }
  // 强约束：必须在 /tmp/nova-agent/ 下
  if (!real.startsWith('/tmp/nova-agent/')) {
    throw httpError(403, 'workDir ' + real + ' is outside /tmp/nova-agent/');
  }
  return real;
}

// httpError attaches an HTTP status to an Error so the /v1/run handler can
// emit the right status code when an assertWorkDirInScope check fails.
function httpError(status, message) {
  const err = new Error(message);
  err.status = status;
  return err;
}

// Bind 127.0.0.1 only — the worker is reached via SSH direct-tcpip channel
// from NovaWorkbench, never directly from the network. Override via env if
// a deployment really needs another bind (e.g. inside a container with port
// mapped), but the safe default is loopback.
const port = parseInt(process.env.NOVA_AGENT_WORKER_PORT ?? '7000', 10);
const host = process.env.NOVA_AGENT_WORKER_HOST ?? '127.0.0.1';
app.listen(port, host, () => {
  console.log('nova-agent-worker listening on http://' + host + ':' + port);
});
`

// agentWorkerPackageJSON is the package.json that goes alongside server.mjs.
// The worker version is tracked by agentWorkerVersion above (stamped into
// server.mjs at install); the package.json "version" field is vestigial and
// not read by the worker.
const agentWorkerPackageJSON = `{
  "name": "nova-agent-worker",
  "version": "0.1.0",
  "description": "HTTP/NDJSON bridge between NovaWorkbench and the claude CLI on a remote Agent host.",
  "private": true,
  "type": "module",
  "main": "server.mjs",
  "scripts": { "start": "node server.mjs" },
  "engines": { "node": ">=20" },
  "dependencies": {
    "express": "^4.19.0"
  }
}
`

// agentWorkerSystemdUnit is the systemd unit for Linux. It binds the worker
// to loopback (NOVA_AGENT_WORKER_HOST=127.0.0.1) so the service is reachable
// only via SSH direct-tcpip from NovaWorkbench.
//
// NOTE: this is a USER unit (written to ~/.config/systemd/user/), so the
// [Install] section uses `default.target` — multi-user.target is a system
// unit target and referencing it from a user unit produces
// "added as a dependency to a non-existent unit multi-user.target"
// at enable time, after which the auto-start silently does nothing. With
// `default.target` the worker starts when the user logs in (i.e. whenever
// the operator is interacting with NovaWorkbench).
const agentWorkerSystemdUnit = `[Unit]
Description=NovaWorkbench Agent Worker
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment=NOVA_AGENT_WORKER_HOST=127.0.0.1
Environment=NOVA_AGENT_WORKER_PORT=7000
# Pin TMPDIR=/tmp so the worker process itself starts with a writable
# tmpdir even when the SSH user inherited a macOS-shaped TMPDIR
# (e.g. /var/folders/...) via SendEnv from the dev box. The worker
# also overrides TMPDIR again per-spawn for the claude child (see
# server.mjs:resolveTmpdir), but this belt-and-braces ensures Node's
# own os.tmpdir() is sane before any user code runs.
Environment=TMPDIR=/tmp
# PATH is set explicitly because the systemd --user manager inherits
# a stripped PATH (/usr/local/sbin:/usr/local/bin:/usr/bin:/usr/sbin:/sbin:/bin)
# that does NOT include nvm / Homebrew / npm-global bin dirs. Without this
# override the worker's spawn('claude', ...) fails with ENOENT even when the
# install script reports 'claude <ver>' (the Check flow runs in an SSH
# shell with the augmented PATH - see agent_server.go runCheck:228). The
# install flow replaces the __WORKER_PATH__ placeholder with the actual
# PATH (SSH session PATH + ~/.novaworkbench/extra-paths) computed on the
# remote host.
Environment=PATH=__WORKER_PATH__
WorkingDirectory=/opt/nova-agent-worker
ExecStart=/usr/bin/env node server.mjs
Restart=always
RestartSec=3
StartLimitBurst=5
StartLimitIntervalSec=60

StandardOutput=journal
StandardError=journal

[Install]
WantedBy=default.target
`

// agentWorkerLaunchdPlist is the launchd unit for macOS (where systemd is
// not standard). Loaded into /Library/LaunchDaemons so it runs at boot for
// any user. KeepAlive=true: survives logout.
const agentWorkerLaunchdPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.novaworkbench.agent-worker</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/bin/env</string>
    <string>node</string>
    <string>/opt/nova-agent-worker/server.mjs</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>NOVA_AGENT_WORKER_HOST</key><string>127.0.0.1</string>
    <key>NOVA_AGENT_WORKER_PORT</key><string>7000</string>
    <key>TMPDIR</key><string>/tmp</string>
    <!-- PATH placeholder — replaced at install time with the resolved
         launchd PATH (SSH session PATH + extra-paths) so the worker's
         spawn('claude', …) can find the CLI even when launchd started
         the agent with a stripped PATH that misses /opt/homebrew/bin
         or ~/.npm-global/bin. launchd honors EnvironmentVariables.PATH. -->
    <key>PATH</key><string>__WORKER_PATH__</string>
  </dict>
  <key>WorkingDirectory</key><string>/opt/nova-agent-worker</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/var/log/nova-agent-worker.log</string>
  <key>StandardErrorPath</key><string>/var/log/nova-agent-worker.log</string>
</dict>
</plist>
`
