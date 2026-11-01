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

import { evidenceRelPath, normalizeColor, findColorLiterals } from './util.mjs';
import { extractJSON } from './review.mjs';
import { readGlobalTokens, readDefinedClasses, readUsedClasses } from './rules/static.mjs';

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
