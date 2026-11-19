// Unit tests for the nova-agent-worker's pure helpers.
//
// Run with:  node --test agent-worker/
//
// The module binds port 7000 on import unless NOVA_AGENT_WORKER_NO_LISTEN=1
// is set, so the test sets it before importing. That keeps the tests free
// of a port race while leaving the production start path untouched.
//
// WHAT IS PINNED HERE:
//
//   1. `effort` travels from the POST body into the worker's opts — a
//      field the Go side sends and an OLDER worker silently drops. If the
//      destructuring ever loses the field again, remote runs would lose
//      their reasoning tier without any error surfacing.
//
//   2. `--effort` lands in the RIGHT PLACE in argv: after --disallowedTools
//      and BEFORE --add-dir. `--add-dir` is variadic, so a flag placed
//      after it would be swallowed as another directory value — the tier
//      would silently become a path.
//
//   3. An old body (no `effort` at all) produces byte-identical argv to
//      what it produced before the field existed. The remote protocol has
//      to stay backward compatible: a NovaWorkbench talking to an older
//      worker (or the other way round) must degrade to "no tier", never to
//      a broken run.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync, chmodSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

process.env.NOVA_AGENT_WORKER_NO_LISTEN = '1';

const { buildRunRequest, buildClaudeArgs, buildSettingsArg, normalizeEffort, cliSupportsEffort } =
  await import('./server.mjs');

/**
 * Writes a fake claude binary and returns its path.
 *
 * The probe decides on the CONTENT of `--help`, not on the exit code: a
 * build may print its usage and still exit non-zero. These fakes cover
 * that shape, plus the "no output at all" shape that must be reported as
 * unreadable.
 */
function fakeClaude(name, body) {
  const dir = mkdtempSync(join(tmpdir(), 'nova-effort-probe-'));
  const path = join(dir, name);
  writeFileSync(path, body);
  chmodSync(path, 0o755);
  return path;
}

/** The minimal body buildRunRequest accepts. */
function baseBody(extra = {}) {
  return {
    workDir: '/tmp/nova-agent/proj_1/req_1',
    prompt: 'hello',
    ...extra,
  };
}

/** Index of the first argv slot exactly equal to want, or -1. */
function indexOfArg(args, want) {
  return args.findIndex((a) => a === want);
}

test('normalizeEffort keeps only the CLI whitelist', () => {
  for (const tier of ['low', 'medium', 'high', 'xhigh', 'max']) {
    assert.equal(normalizeEffort(tier), tier);
  }
  for (const bad of ['', '   ', 'HIGH', 'High', 'ultra', 'low-medium', '1', null, undefined, 42]) {
    assert.equal(normalizeEffort(bad), '', `normalizeEffort(${JSON.stringify(bad)}) should be ""`);
  }
});

test('buildRunRequest forwards the effort tier', () => {
  const opts = buildRunRequest(baseBody({ effort: 'high' }));
  assert.equal(opts.effort, 'high');
});

test('buildRunRequest normalizes an unknown tier away', () => {
  // The CLI aborts the whole run on an unknown --effort value, so a bad
  // tier must never reach argv.
  const opts = buildRunRequest(baseBody({ effort: 'ultra' }));
  assert.equal(opts.effort, '');
});

test('buildRunRequest tolerates a body without effort (old client)', () => {
  const opts = buildRunRequest(baseBody());
  assert.equal(opts.effort, '');
});

test('buildClaudeArgs emits --effort with the tier as its value', () => {
  const opts = buildRunRequest(baseBody({ effort: 'medium' }));
  const args = buildClaudeArgs(opts, '');
  const i = indexOfArg(args, '--effort');
  assert.notEqual(i, -1, `--effort missing from ${JSON.stringify(args)}`);
  assert.equal(args[i + 1], 'medium');
});

test('buildClaudeArgs omits --effort when no tier was sent', () => {
  const opts = buildRunRequest(baseBody());
  const args = buildClaudeArgs(opts, '');
  assert.equal(indexOfArg(args, '--effort'), -1);
});

test('buildClaudeArgs places --effort BEFORE --add-dir', () => {
  // --add-dir is variadic: anything after it is swallowed as another
  // directory value, so the tier would silently become a path.
  const opts = buildRunRequest(baseBody({ effort: 'high', addDirs: ['/abs/one'] }));
  const args = buildClaudeArgs(opts, '');
  const effortIdx = indexOfArg(args, '--effort');
  const addDirIdx = indexOfArg(args, '--add-dir');
  assert.notEqual(effortIdx, -1, '--effort missing');
  assert.notEqual(addDirIdx, -1, '--add-dir missing');
  assert.ok(effortIdx < addDirIdx, `--effort at ${effortIdx}, --add-dir at ${addDirIdx}`);
  // The tier must be the VALUE of --effort, and the directory must still
  // be the value of --add-dir.
  assert.equal(args[effortIdx + 1], 'high');
  assert.equal(args[addDirIdx + 1], '/abs/one');
});

test('buildClaudeArgs leaves an old body byte-identical', () => {
  // The remote protocol must degrade to "no tier" against an older
  // worker, never to a broken run. Compare the argv of a body without
  // `effort` against the same body with an explicitly empty one.
  const withoutEffort = buildClaudeArgs(buildRunRequest(baseBody()), '');
  const withEmptyEffort = buildClaudeArgs(buildRunRequest(baseBody({ effort: '' })), '');
  assert.deepEqual(withoutEffort, withEmptyEffort);
});

// ── the capability probe ───────────────────────────────────────────────────
//
// The probe exists to avoid handing `--effort` to a CLI that does not know
// it (such a CLI aborts the whole run). So the decision has to be made on
// the CONTENT of `--help` — never on the exit code, and never on a guess.

test('cliSupportsEffort reads the flag out of the help text', () => {
  const bin = fakeClaude('modern', "#!/bin/sh\necho '  --effort <level>  Effort level'\n");
  assert.equal(cliSupportsEffort(bin, process.env), true);
});

test('cliSupportsEffort trusts the help text even on a non-zero exit', () => {
  // The build prints its usage and exits 1 — the usage text is the
  // evidence, the exit code is noise.
  const bin = fakeClaude('modern-exit1', "#!/bin/sh\necho '  --effort <level>  Effort level'\nexit 1\n");
  assert.equal(cliSupportsEffort(bin, process.env), true);
});

test('cliSupportsEffort reports no support when the flag is absent', () => {
  const bin = fakeClaude('legacy', "#!/bin/sh\necho '  --model <model>  Model'\n");
  assert.equal(cliSupportsEffort(bin, process.env), false);
});

test('cliSupportsEffort reports no support when the flag is absent and the exit is non-zero', () => {
  const bin = fakeClaude('legacy-exit1', "#!/bin/sh\necho 'some other output'\nexit 3\n");
  assert.equal(cliSupportsEffort(bin, process.env), false);
});

test('cliSupportsEffort refuses to guess when the binary cannot be read', () => {
  // No output at all → we could not probe the binary. Emitting --effort on
  // that guess is exactly the "unknown option '--effort'" run-killer the
  // probe exists to prevent, so the flag is dropped.
  const bin = fakeClaude('silent', "#!/bin/sh\nexit 1\n");
  assert.equal(cliSupportsEffort(bin, process.env), false);
});

test('cliSupportsEffort refuses to guess when the binary is missing', () => {
  const bin = fakeClaude('gone', "#!/bin/sh\nexit 0\n");
  assert.equal(cliSupportsEffort(bin + '.missing', process.env), false);
});

test('cliSupportsEffort never emits for an empty command', () => {
  assert.equal(cliSupportsEffort('', process.env), false);
  assert.equal(cliSupportsEffort(null, process.env), false);
});

// ── the project CLAUDE.md native channel ───────────────────────────────────
//
// The managed project-level CLAUDE.md reaches the remote model through the
// Claude CLI's OWN project-memory channel rather than through the prompt
// text. Two things make that work, and both are easy to lose:
//
//   1. The directory holding the materialized copy is granted with --add-dir.
//      --add-dir is VARIADIC, so it has to stay LAST in argv.
//
//   2. CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1 has to be set: without
//      it --add-dir only grants READ access and the CLI never loads
//      <dir>/CLAUDE.md. (Measured against claude 2.1.328 — the full probe
//      results live in backend/internal/llm/claude_md_settings.go.)
//
// The exclusion list rides along in the --settings JSON as a TOP-LEVEL
// "claudeMdExcludes" key (a sibling of "env"), which is where the CLI reads
// it from.

test('buildRunRequest carries the CLAUDE.md native channel fields', () => {
  const opts = buildRunRequest(baseBody({
    claudeMdDir: '/tmp/nova-agent/proj_1/memory',
    claudeMdExcludes: ['/tmp/nova-agent/proj_1/req_1/CLAUDE.md'],
  }));
  assert.equal(opts.claudeMdDir, '/tmp/nova-agent/proj_1/memory');
  assert.deepEqual(opts.claudeMdExcludes, ['/tmp/nova-agent/proj_1/req_1/CLAUDE.md']);
});

test('buildRunRequest tolerates a body without the CLAUDE.md fields', () => {
  // An OLD NovaWorkbench talking to this worker must not break the run.
  const opts = buildRunRequest(baseBody());
  assert.equal(opts.claudeMdDir, '');
  assert.deepEqual(opts.claudeMdExcludes, []);
});

test('buildClaudeArgs grants the CLAUDE.md directory through --add-dir', () => {
  const opts = buildRunRequest(baseBody({
    claudeMdDir: '/tmp/nova-agent/proj_1/memory',
    addDirs: ['/tmp/nova-agent/proj_1/req_1/attachments'],
  }));
  const args = buildClaudeArgs(opts, buildSettingsArg(opts));
  assert.notEqual(indexOfArg(args, '--add-dir'), -1);
  // --add-dir is variadic: the memory directory has to be one of its values.
  assert.ok(args.includes('/tmp/nova-agent/proj_1/memory'));
  // And it has to stay last in argv, or a later flag would be swallowed.
  assert.equal(args[args.length - 1], '/tmp/nova-agent/proj_1/memory');
});

test('buildSettingsArg enables the CLAUDE.md dirs env var', () => {
  const opts = buildRunRequest(baseBody({ claudeMdDir: '/tmp/nova-agent/proj_1/memory' }));
  const parsed = JSON.parse(buildSettingsArg(opts));
  assert.equal(parsed.env.CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD, '1');
});

test('buildSettingsArg omits the env var without a memory directory', () => {
  // The env var is process-wide: it makes EVERY granted directory a
  // "CLAUDE.md dir". Without a managed copy to deliver there is no reason to
  // turn it on — and turning it on would pull unrelated CLAUDE.md files into
  // the context.
  const opts = buildRunRequest(baseBody({ addDirs: ['/tmp/nova-agent/proj_1/req_1/attachments'] }));
  // Nothing to pin at all → the worker omits --settings entirely, so the
  // CLI keeps its defaults (and never sees the env var).
  const settings = buildSettingsArg(opts);
  if (settings !== null) {
    const parsed = JSON.parse(settings);
    assert.equal(parsed.env.CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD, undefined);
  }
});

test('buildSettingsArg puts claudeMdExcludes at the top level', () => {
  // The CLI reads the exclusion list from a TOP-LEVEL settings key, not from
  // the env block.
  const opts = buildRunRequest(baseBody({
    claudeMdDir: '/tmp/nova-agent/proj_1/memory',
    claudeMdExcludes: ['/tmp/a/CLAUDE.md', '/tmp/a/AGENTS.md'],
  }));
  const parsed = JSON.parse(buildSettingsArg(opts));
  assert.deepEqual(parsed.claudeMdExcludes, ['/tmp/a/CLAUDE.md', '/tmp/a/AGENTS.md']);
  assert.equal(parsed.env.claudeMdExcludes, undefined);
});
