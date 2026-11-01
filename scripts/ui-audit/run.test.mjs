// run.test.mjs — unit tests for the audit tool's pure logic.
//
//   node --test scripts/ui-audit/
//
// These cover the parts where a silent mistake would corrupt the whole
// report: path mapping between the local artifacts and the evidence branch,
// JSON extraction from a model reply, colour normalisation, and the class
// name extraction that decides which findings exist at all.
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import { evidenceRelPath, normalizeColor, findColorLiterals } from './util.mjs';
import { extractJSON } from './review.mjs';
import { readGlobalTokens, readDefinedClasses, readUsedClasses } from './rules/static.mjs';
import { runRuntimeRules } from './rules/runtime.mjs';

test('evidenceRelPath maps artifact paths onto the evidence branch', () => {
  const runId = '20261025-2015';
  assert.equal(
    evidenceRelPath(`artifacts/ui-audit/${runId}/screenshots/root.png`, runId),
    `docs/ui-audit/${runId}/screenshots/root.png`,
  );
  assert.equal(
    evidenceRelPath(`artifacts/ui-audit/${runId}/screenshots/root.png`, runId, 'evidence/ui'),
    `evidence/ui/${runId}/screenshots/root.png`,
  );
});

test('evidenceRelPath never emits a path that escapes the run directory', () => {
  const runId = '20261025-2015';
  const out = evidenceRelPath('artifacts/ui-audit/other/screenshots/x.png', runId);
  assert.ok(out.startsWith(`docs/ui-audit/${runId}/`));
  assert.ok(!out.includes('..'));
});

test('normalizeColor folds the spellings the codebase actually uses', () => {
  assert.equal(normalizeColor('#FFF'), '#ffffff');
  assert.equal(normalizeColor('#ffffff'), '#ffffff');
  assert.equal(normalizeColor('#64748B'), '#64748b');
  assert.equal(normalizeColor('rgb(30, 41, 59)'), '#1e293b');
  assert.equal(normalizeColor('rgba(30, 41, 59, 1)'), '#1e293b');
  assert.notEqual(normalizeColor('rgba(30, 41, 59, 0.5)'), '#1e293b');
});

test('findColorLiterals does not treat ids or hex-like words as colours', () => {
  const found = findColorLiterals('color: #fff; border: 1px solid #e2e8f0; /* #123456 */');
  const values = found.map((f) => f.normalized);
  assert.ok(values.includes('#ffffff'));
  assert.ok(values.includes('#e2e8f0'));
  // The one inside the comment is still found by the raw scanner; the rules
  // layer strips comments before counting, so this only pins the scanner.
  assert.equal(found.length, 3);
});

test('extractJSON prefers the last fenced json block', () => {
  const text = ['```json', '[{"id":"a"}]', '```', 'noise', '```json', '[{"id":"b"}]', '```'].join('\n');
  assert.deepEqual(extractJSON(text), [{ id: 'b' }]);
});

test('extractJSON falls back to a bare array', () => {
  assert.deepEqual(extractJSON('here you go: [{"id":"x","verdict":"keep"}] done'), [
    { id: 'x', verdict: 'keep' },
  ]);
});

test('extractJSON survives braces inside strings', () => {
  const text = '[{"id":"a","detail":"use {var} here"}]';
  assert.deepEqual(extractJSON(text), [{ id: 'a', detail: 'use {var} here' }]);
});

test('extractJSON returns null rather than throwing on garbage', () => {
  assert.equal(extractJSON('no json at all'), null);
  assert.equal(extractJSON('```json\n[{"broken":\n```'), null);
});

test('the global palette is read from index.css', () => {
  const tokens = readGlobalTokens();
  assert.ok(tokens.size > 15, `expected a populated palette, got ${tokens.size}`);
  assert.ok(tokens.has('#4f46e5'), 'primary colour should be a token');
  assert.ok(tokens.get('#4f46e5').names.includes('--color-primary'));
});

test('class names used in JSX are extracted from every quoting style', () => {
  const used = readUsedClasses();
  assert.ok(used.has('btn'), 'plain className="btn" should register');
  assert.ok(used.has('btn-primary'), 'plain className should register');
  // A template literal contributes its static text …
  assert.ok(used.has('req-row'), 'template literal static text should register');
  // … but not the tokens that live inside the interpolation.
  assert.ok(!used.has('r'), 'identifiers inside a hole must not become classes');
  assert.ok(!used.has('==='), 'operators inside a hole must not become classes');
  // A dynamic prefix is not a class on its own.
  assert.ok(!used.has('status-'), 'dynamic class prefixes must be skipped');
});

test('class names are defined in the stylesheets', () => {
  const defined = readDefinedClasses();
  assert.ok(defined.has('btn-primary'), 'index.css defines .btn-primary');
  assert.ok(defined.has('status-badge'), 'index.css defines .status-badge');
});

test('a runtime finding is identified by the rule that produced it', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'uia-runtime-'));
  try {
    const probeDir = path.join(dir, 'probe');
    fs.mkdirSync(probeDir);
    // Two routes that paint the same role differently — the exact shape the
    // R11 rule exists to catch.
    const mk = (height) => ({
      roles: {
        'btn-primary': {
          [`h=${height}`]: {
            fingerprint: { fontSize: '13px', height, padding: '8px 16px' },
            count: 2,
            samples: ['button.btn-primary "Go"'],
          },
        },
      },
    });
    fs.writeFileSync(path.join(probeDir, 'a.json'), JSON.stringify(mk('36px')));
    fs.writeFileSync(path.join(probeDir, 'b.json'), JSON.stringify(mk('54px')));
    const manifest = {
      routes: [
        { slug: 'a', path: '/a', group: 'core', screenshot: 'artifacts/ui-audit/x/screenshots/a.png', sha256: 'aa' },
        { slug: 'b', path: '/b', group: 'core', screenshot: 'artifacts/ui-audit/x/screenshots/b.png', sha256: 'bb' },
      ],
    };
    const { findings } = runRuntimeRules(manifest, probeDir);
    assert.ok(findings.length >= 1, 'the drift between two routes is reported');
    for (const f of findings) {
      assert.ok(f.id.startsWith(f.rule), `${f.id} must name the rule ${f.rule} that produced it`);
    }
    // The stub-page finding lives in the same file but is a different rule,
    // so its id must not claim to be an R11.
    const stub = findings.find((f) => f.rule === 'R12');
    if (stub) assert.ok(stub.id.startsWith('R12'));
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
