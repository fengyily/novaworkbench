package handler

// Embedded copies of nova-agent-worker source files. We keep them inline in
// the Go binary so the install flow can SFTP-write to a fresh agent host
// without needing an internet fetch (and without committing the user to a
// specific GitHub release URL that might move). Update these strings when
// shipping a new worker version; the install flow will redeploy on the next
// runInstall.
//
// Keep in sync with the files under agent-worker/ in the repo root. Any
// change there must be mirrored here, otherwise remote servers will drift.
//
// Note: the JS bodies below use single quotes only (no backticks) so the
// Go raw-string literals (delimited by backticks) stay well-formed.

// agentWorkerServerMJS is the body of nova-agent-worker/server.mjs that gets
// uploaded to the remote agent host during install. It's the same content
// as agent-worker/server.mjs in the repo — see that file for the protocol
// and architecture notes.
const agentWorkerServerMJS = `// nova-agent-worker — HTTP/NDJSON bridge between NovaWorkbench (Go backend)
// and the claude CLI on a remote Agent host. Replaces an earlier
// @anthropic-ai/claude-agent-sdk-based flow; spawning claude directly gives
// us the raw exit code + stderr that the SDK used to swallow, and keeps
// auth / settings handling under our control.
//
// Why this is a service (not a per-invocation script):
//   * Node startup adds ~300-500ms; doing it per coding pass is noticeable.
//   * The Go side reuses one SSH connection (direct-tcpip channel) to talk
//     here, no new ports opened to the network — bind 127.0.0.1 only.
//
// Protocol:
//   GET  /v1/health   → 200 {status, claudeVersion}
//   POST /v1/run      → application/x-ndjson of stream-json events emitted
//                       by 'claude -p ... --output-format stream-json
//                       --verbose --dangerously-skip-permissions' (or
//                       --permission-mode plan). One JSON event per line,
//                       flushed immediately. NovaWorkbench's Go-side
//                       parseStreamJSONFromReader consumes them line-by-line
//                       unchanged.
//
// Body fields for POST /v1/run map to the CLI flags we build:
//   workDir     → cmd.Dir
//   prompt      → -p <prompt>
//   model       → --model <id>
//   systemPrompt→ --system-prompt (or --append-system-prompt in plan mode)
//   sessionId   → --session-id (new) or --resume (existing)
//   resume      → --resume when true
//   fork        → --fork-session (with --resume)
//   forkSessionId → --session-id on a forked run (pre-mint the id)
//   env         → process env for the child
//   allowedTools→ --allowedTools "Tool1 Tool2 ..."
//   disallowedTools → --disallowedTools "Tool1 Tool2 ..."
//   permissionMode  → "plan" → --permission-mode plan; "" → --dangerously-skip-permissions
//   ignoreLocalSettings (default true) → --setting-sources "" (drop all
//                       settings files; the platform env is the only source
//                       of auth / base URL / model pinning).
//   overrideSettingSources (legacy) → --setting-sources project,local
//                       (drop only the user source).
//
// Reliability layers:
//   1) Preflight — before invoking claude we spawn 'claude --print ping'
//      ourselves with the same env + cwd. The CLI's own output surfaces
//      the actual cause (401 / ENOTFOUND / unrecognized model / etc.),
//      which we hand to classifyError so the Go side can show a tailored
//      fix hint instead of an opaque exit code.
//   2) classifyError — pattern-matches the captured stderr / spawn error
//      into a coarse errorCategory the Go side maps to a user-facing fix
//      hint (see backend/internal/handler/wizard.go:workerCategoryHint).
import express from 'express';
import { spawn } from 'node:child_process';
import { mkdtempSync, accessSync, constants as fsConstants } from 'node:fs';
import { join } from 'node:path';

var app = express();
app.use(express.json({ limit: '50mb' }));

// /v1/health — used by NovaWorkbench's Check flow to verify the worker is
// alive before a coding run. Returns the claude CLI version (best effort)
// so we can alert on stale installs.
app.get('/v1/health', async function (_req, res) {
  var claudeVersion = 'unknown';
  try {
    claudeVersion = await readClaudeVersion();
  } catch (e) {
    // Health must never fail on a missing CLI — the wizard's "install
    // deps" path surfaces that, not the per-run health probe. An unknown
    // version is a soft signal, not an error.
  }
  res.json({ status: 'ok', claudeVersion: claudeVersion });
});

// /v1/run — primary endpoint. Streams 'claude -p ... --output-format
// stream-json --verbose ...' events as NDJSON (one JSON object per line,
// flushed immediately) so NovaWorkbench's Go-side parseStreamJSONFromReader
// can consume them line-by-line unchanged.
//
// Why NDJSON instead of SSE: SSE wraps each event in 'data: <json>\n\n',
// but our parser does a straight json.Unmarshal per line. Keeping the wire
// format identical to the CLI's --output-format stream-json --verbose
// output means zero parser changes on the Go side.
//
// Streaming: the response stays open for the lifetime of the child process.
// One JSON event per line, flushed after each write. Errors surface as
// {type:"error", errorCategory, error, stderr?, code?, signal?} lines
// followed by response close.
app.post('/v1/run', async function (req, res) {
  var opts = buildRunRequest(req.body || {});
  res.setHeader('Content-Type', 'application/x-ndjson');
  res.setHeader('Cache-Control', 'no-cache');
  res.setHeader('Connection', 'keep-alive');
  if (res.flushHeaders) res.flushHeaders();

  // Preflight — validate the claude CLI works in this env+cwd before
  // launching the real run. Without this probe, a misconfigured environment
  // would surface as the real run emitting one 'error' event and then
  // exiting silently — indistinguishable from a successful empty run. 5s
  // budget keeps the round-trip short on the failure path; the success
  // path adds ~1-2s which is dominated by Node + claude cold-start anyway.
  // The preflight env mirrors what we hand the real run so auth + base URL
  // + model-pinning are identical.
  var mergedEnv = Object.assign({}, process.env, opts.env);
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
  var tmpdir = resolveTmpdir(mergedEnv);
  mergedEnv.TMPDIR = tmpdir;
  mergedEnv.TMP = tmpdir;
  mergedEnv.TEMP = tmpdir;
  // Also patch our own process.env so any node-side libraries (or a
  // future SDK we add) inherit the same writable location — the previous
  // version only patched the child env, which left Node's own
  // os.tmpdir() pointing at the bad path.
  process.env.TMPDIR = tmpdir;
  process.env.TMP = tmpdir;
  process.env.TEMP = tmpdir;
  // Strip model-pinning env vars from the preflight env. gateway.go's
  // BuildEnvPairs injects ANTHROPIC_DEFAULT_{HAIKU,SONNET,OPUS}_MODEL so
  // a real /v1/run call has every tier pinned to the user's model. But the
  // preflight passes no --model — the CLI then reads those env vars and
  // tries to use them, only to emit [claude-code:unrecognized_model] for
  // any custom model id (MiniMax-M3 etc.) and either fail the ping or hang
  // retrying. Locally the user sees claude --print ping work because their
  // local env does not have ANTHROPIC_DEFAULT_*_MODEL set, so the CLI falls
  // back to its built-in catalog model. We mirror that on the preflight by
  // stripping these three vars +
  // CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT (which only
  // matters when a custom model is in play). Auth + base URL + TMPDIR
  // override are preserved so the probe still validates connectivity.
  var preflightEnv = Object.assign({}, mergedEnv);
  delete preflightEnv.ANTHROPIC_DEFAULT_HAIKU_MODEL;
  delete preflightEnv.ANTHROPIC_DEFAULT_SONNET_MODEL;
  delete preflightEnv.ANTHROPIC_DEFAULT_OPUS_MODEL;
  delete preflightEnv.CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT;
  var pf = await preflight(opts.workDir, preflightEnv);
  if (!pf.ok) {
    res.write(JSON.stringify({
      type: 'error',
      errorCategory: pf.errorCategory,
      error: 'preflight 失败（' + pf.errorCategory + '）',
      stderr: pf.stderr || '',
      code: pf.code,
      preflight: true,
    }) + '\n');
    res.end();
    return;
  }

  var args = buildClaudeArgs(opts);
  var proc;
  try {
    proc = spawn('claude', args, {
      cwd: opts.workDir,
      env: mergedEnv,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
  } catch (e) {
    // spawn() can throw synchronously when the binary is not found (rare
    // on Linux but possible). Treat as cli_not_found.
    res.write(JSON.stringify({
      type: 'error',
      errorCategory: 'cli_not_found',
      error: String((e && e.message) || e),
      code: -1,
    }) + '\n');
    res.end();
    return;
  }

  var stderr = '';
  proc.stderr.on('data', function (d) { stderr += d.toString(); });
  proc.stdout.on('data', function (chunk) {
    // claude emits one JSON object per line; forward verbatim so the Go
    // parser can json.Unmarshal each line directly. Flush after every
    // chunk so the client sees events live (otherwise the kernel buffer
    // hides progress for the first few seconds of a long turn).
    res.write(chunk);
    if (typeof res.flush === 'function') res.flush();
  });
  proc.on('error', function (e) {
    // ENOENT from the event emitter means spawn() couldn't find claude
    // (asynchronous path). Treat other errors as network-shaped.
    var cat = (e && e.code === 'ENOENT')
      ? 'cli_not_found'
      : classifyError(null, String(e.message || e));
    res.write(JSON.stringify({
      type: 'error',
      errorCategory: cat,
      error: String(e.message || e),
      stderr: String(e.message || e),
      code: e.code,
    }) + '\n');
    res.end();
  });
  proc.on('close', function (code, signal) {
    if (code === 0) {
      res.end();
      return;
    }
    // Non-zero exit — emit a single error event with the captured stderr
    // so the Go parser (which already knows how to surface errorCategory
    // + stderr) can render a tailored fix hint. serializeCLIError mirrors
    // the shape classifyError expects.
    var payload = serializeCLIError({ code: code, signal: signal, stderr: stderr });
    res.write(JSON.stringify({ type: 'error', ...payload }) + '\n');
    res.end();
  });
});

// preflight spawns 'claude --print ping' with the same env+cwd as the real
// call and returns {ok, errorCategory, stderr, stdout, code}. Catches the
// actual CLI failure the Go side would otherwise see as a generic 'exit 1'.
//
// 5s timeout: 'claude --print ping' round-trips through the Anthropic API
// and normally completes in <2s on a healthy host. A timeout here is
// indistinguishable from 'API hung' — we surface it as its own category so
// the Go side can show a network-unreachable hint without conflating it
// with a 30s-deep hang the real run would otherwise expose.
function preflight(workDir, env) {
  return new Promise(function (resolve) {
    var proc;
    try {
      // --setting-sources "" drops ALL settings files (user / project /
      // local) so the preflight mirrors the real /v1/run invocation,
      // which also drops them via ignoreLocalSettings=true. Without
      // this, the preflight reads the seeded ~/.claude/settings.json
      // (placed there by the install script with a placeholder
      // ANTHROPIC_AUTH_TOKEN and ANTHROPIC_DEFAULT_*_MODEL=MiniMax-M3
      // but WITHOUT CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT).
      // The CLI's settings.json env block applies OVER the process env
      // we pass in, so the placeholder token would shadow the platform's
      // real auth, the unknown model would trigger
      // [claude-code:unrecognized_model], and the ping call would hang /
      // fail — making the preflight useless as an early-warning probe.
      proc = spawn('claude', ['--print', 'ping', '--output-format', 'text', '--setting-sources', 'project,local'], {
        cwd: workDir,
        env: env,
        stdio: ['ignore', 'pipe', 'pipe'],
      });
    } catch (e) {
      resolve({
        ok: false,
        errorCategory: 'cli_not_found',
        stderr: String((e && e.message) || e),
        code: -1,
      });
      return;
    }
    var stderr = '';
    var stdout = '';
    proc.stdout.on('data', function (d) { stdout += d.toString(); });
    proc.stderr.on('data', function (d) { stderr += d.toString(); });
    proc.on('error', function (e) {
      var cat = (e && e.code === 'ENOENT')
        ? 'cli_not_found'
        : classifyError(null, String(e.message || e));
      resolve({ ok: false, errorCategory: cat, stderr: String(e.message || e), code: e.code });
    });
    proc.on('close', function (code) {
      if (code === 0) {
        resolve({ ok: true });
      } else {
        resolve({
          ok: false,
          errorCategory: classifyError(null, stderr || stdout),
          stderr: stderr,
          stdout: stdout,
          code: code,
        });
      }
    });
    setTimeout(function () {
      try { proc.kill('SIGTERM'); } catch (e) {}
      // SIGTERM gives the CLI ~1s to flush stderr before our exit
      // resolves; the final stderr we read from above already has the
      // early output and is usually enough to classify the failure.
      resolve({
        ok: false,
        errorCategory: 'preflight_timeout',
        stderr: (stderr || '') + '\n[preflight timeout after 5s]',
        code: 143,
      });
    }, 5000);
  });
}

// resolveTmpdir returns a writable tmpdir path to inject into the child
// env. Returns null when the env already points at a usable directory.
//
// Why we override TMPDIR: on macOS, SSH-only users (e.g. the nova-agent
// SSH user provisioned by our install flow) often have $TMPDIR inherited
// from the per-user temp dir launchd creates at graphical login — but
// if the user has never logged in graphically, that dir doesn't exist.
// 'claude' then tries to mkdir '/var/folders' (root-owned) and the whole
// process exits with EACCES before --print ping can do anything.
//
// We trust an existing $TMPDIR if it points somewhere that's not the
// root-owned /var/folders symlink target. Otherwise we mkdtempSync a
// fresh subdir under $HOME, which is always writable for the SSH user.
// HOME is also a safer default than /tmp on multi-tenant hosts where
// other users' files are visible.
function resolveTmpdir(env) {
  var cur = env && env.TMPDIR;
  // Only trust the existing TMPDIR if it exists, is writable, AND is not
  // the macOS-shaped path that wouldn't exist on Linux. The previous
  // version only did a string-shape check, which accepted /var/folders
  // paths that didn't exist on the agent host and left claude to EACCES
  // on mkdir.
  if (cur && cur !== '/var/folders' && cur.indexOf('/var/folders/') !== 0) {
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
  var home = (env && env.HOME) || process.env.HOME;
  if (home) {
    try {
      var pHome = mkdtempSync(join(home, '.nova-agent-worker-'));
      console.error('[nova-agent-worker] resolved TMPDIR via $HOME: ' + pHome);
      return pHome;
    } catch (e) {
      console.error('[nova-agent-worker] mkdtempSync under $HOME=' + home + ' failed: ' + (e.code || e.message) + '; trying /tmp');
    }
  } else {
    console.error('[nova-agent-worker] $HOME not set; trying /tmp');
  }

  // 3) /tmp-based tmpdir
  try {
    var pTmp = mkdtempSync('/tmp/.nova-agent-worker-');
    console.error('[nova-agent-worker] resolved TMPDIR via /tmp: ' + pTmp);
    return pTmp;
  } catch (e) {
    console.error('[nova-agent-worker] mkdtempSync under /tmp failed: ' + (e.code || e.message) + '; trying cwd');
  }

  // 4) cwd-based tmpdir
  try {
    var pCwd = mkdtempSync(join(process.cwd(), '.nova-agent-worker-'));
    console.error('[nova-agent-worker] resolved TMPDIR via cwd: ' + pCwd);
    return pCwd;
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
// health probe shouldn't drag the user-visible badge into a 'loading' state.
function readClaudeVersion() {
  return new Promise(function (resolve, reject) {
    var proc = spawn('claude', ['--version'], { stdio: ['ignore', 'pipe', 'pipe'] });
    var out = '';
    proc.stdout.on('data', function (d) { out += d.toString(); });
    proc.on('error', reject);
    proc.on('close', function (code) {
      if (code !== 0) { reject(new Error('claude --version exit ' + code)); return; }
      var first = out.split('\n').map(function (l) { return l.trim(); }).find(Boolean);
      resolve(first || 'unknown');
    });
    setTimeout(function () {
      try { proc.kill('SIGTERM'); } catch (e) {}
      reject(new Error('claude --version timeout'));
    }, 3000);
  });
}

// classifyError maps a captured stderr (or thrown Error) into a coarse
// category. The point isn't to be exhaustive — it's to give the Go side
// enough signal to surface a tailored fix hint (e.g. 'unrecognized_model'
// → '检查设置 → Claude 配置里的 model 名' instead of the generic 'exit 1').
//
// Pattern sources:
//   - Claude Code CLI's own error tags: '[claude-code:unrecognized_model]',
//     '[claude-code:not_logged_in]' etc.
//   - Node / undici / DNS error codes: ENOTFOUND, ECONNREFUSED, etc.
//   - HTTP-status hints in stderr: 401, 403, 404, 429, 5xx.
//
// Order matters — more specific patterns come first so e.g.
// '401 Unauthorized' classifies as auth_failed instead of falling
// through to a generic 'unauthorized' bucket.
function classifyError(err, stderr) {
  var msg = (err && err.message) || '';
  var code = err && err.code;
  var text = (msg + '\n' + (stderr || '')).toLowerCase();
  if (/unrecognized_model|model.{0,4}catalog|behavesas|modelpicker/.test(text)) return 'unrecognized_model';
  if (/401|unauthorized|authentication failed|not logged in|invalid.{0,4}token|invalid.{0,4}api.{0,4}key|invalid.{0,4}auth/.test(text)) return 'auth_failed';
  if (/403|forbidden/.test(text)) return 'auth_forbidden';
  if (/429|rate.{0,4}limit|too many requests/.test(text)) return 'rate_limited';
  if (/quota|insufficient.{0,4}credit|insufficient.{0,4}balance/.test(text)) return 'quota_exceeded';
  if (/404/.test(text) && /model/.test(text)) return 'model_not_found';
  if (/session.{0,4}not.{0,4}found|no conversation found|cannot find.{0,8}session/.test(text)) return 'session_not_found';
  if (code === 'ENOENT' || /enoent/.test(text) || /command not found/.test(text) || /spawn.{0,8}enoent/.test(text)) return 'cli_not_found';
  if (/enetunreach|etimedout|eai_again/.test(text)) return 'network_unreachable';
  if (/enotfound|getaddrinfo/.test(text)) return 'dns_unresolved';
  if (/econnreset/.test(text)) return 'connection_reset';
  if (/econnrefused/.test(text)) return 'connection_refused';
  if (/eacces/.test(text)) return 'permission_denied';
  if (/max.{0,4}turns|maximum.{0,4}turns/.test(text)) return 'max_turns';
  return 'unknown';
}

// serializeCLIError shapes a {code, signal, stderr} view of a non-zero
// child-process exit into the error-payload shape the Go parser already
// understands. The Go side looks at errorCategory to pick a fix hint
// and at stderr to show the actual CLI failure line; the other fields
// are diagnostic sugar.
function serializeCLIError(info) {
  var code = info.code;
  var signal = info.signal;
  var stderr = info.stderr || '';
  var trimmed = stderr.trim();
  var tail = trimmed ? trimmed.split('\n').slice(-1)[0].slice(0, 800) : '';
  var out = {
    error: tail || ('claude 进程退出码 ' + code + (signal ? '（信号 ' + signal + '）' : '')),
    errorCategory: classifyError(null, stderr),
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
// is --setting-sources '' (no settings files at all).
function buildRunRequest(body) {
  var workDir = body.workDir;
  var prompt = body.prompt;
  var model = body.model;
  var systemPrompt = body.systemPrompt;
  var sessionId = body.sessionId;
  var resume = body.resume;
  var fork = body.fork;
  var forkSessionId = body.forkSessionId;
  var env = body.env;
  var allowedTools = body.allowedTools;
  var disallowedTools = body.disallowedTools;
  var permissionMode = body.permissionMode;
  var overrideSettingSources = body.overrideSettingSources;
  var ignoreLocalSettings = body.ignoreLocalSettings;

  if (!workDir) throw new Error('workDir is required');
  if (typeof prompt !== 'string') throw new Error('prompt is required and must be a string');

  // Coerce env values to strings to avoid [object Object] leaks when
  // callers accidentally pass non-strings (e.g. numbers from JSON).
  var envMap;
  if (env && typeof env === 'object') {
    envMap = {};
    for (var k in env) {
      if (Object.prototype.hasOwnProperty.call(env, k)) {
        envMap[k] = env[k] == null ? '' : String(env[k]);
      }
    }
  }

  // Setting-source resolution. Three modes, in order:
  //   ignoreLocalSettings === false  → load every source (CLI default)
  //   overrideSettingSources         → drop user only ('project,local')
  //   default                        → drop everything so platform env wins
  var settingSources = 'project,local';
  if (ignoreLocalSettings === false) {
    settingSources = 'user,project,local';
  } else if (overrideSettingSources) {
    settingSources = 'project,local';
  }

  return {
    workDir: workDir,
    prompt: prompt,
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
    settingSources: settingSources,
  };
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
function buildClaudeArgs(opts) {
  var args = ['-p', opts.prompt, '--output-format', 'stream-json', '--verbose'];
  // Setting-sources always present so the user can't accidentally land on
  // the CLI default (which loads ~/.claude/settings.json over process env
  // and would silently shadow the platform's auth). The default '' drops
  // all sources; callers that need a non-empty list pass an explicit value.
  args.push('--setting-sources', opts.settingSources);

  if (opts.permissionMode === 'plan') {
    args.push('--permission-mode', 'plan');
  } else {
    args.push('--dangerously-skip-permissions');
  }

  if (opts.model) {
    // Prepend --model so it lands before the -p block; gateway.go does
    // the same to keep the relative ordering stable for diff debugging.
    args.unshift('--model', opts.model);
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

var port = parseInt(process.env.NOVA_AGENT_WORKER_PORT || '7000', 10);
var host = process.env.NOVA_AGENT_WORKER_HOST || '127.0.0.1';
app.listen(port, host, function () {
  console.log('nova-agent-worker listening on http://' + host + ':' + port);
});
`

// agentWorkerPackageJSON is the package.json that goes alongside server.mjs.
// Pinned to known-good versions; update via the worker version field in
// agentWorkerManifest when bumping.
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
  </dict>
  <key>WorkingDirectory</key><string>/opt/nova-agent-worker</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/var/log/nova-agent-worker.log</string>
  <key>StandardErrorPath</key><string>/var/log/nova-agent-worker.log</string>
</dict>
</plist>
`