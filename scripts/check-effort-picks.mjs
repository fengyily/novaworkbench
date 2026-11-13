#!/usr/bin/env node
// check-effort-picks.mjs — verifies that the frontend never sends a
// RESOLVED reasoning-effort tier as an explicit pick.
//
// Usage: node scripts/check-effort-picks.mjs   (from the repo root)
// Exit 1 on any violation.
//
// WHY THIS EXISTS
//
// The backend owns the resolution chain for the claude CLI's --effort flag
// (llm.Gateway.ResolveEffort):
//
//	explicit pick > configured stage default > built-in default
//
// The frontend shows the effective tier in its picker (`pick ??
// configuredDefault`) — that is a DISPLAY value. If that display value is
// also what goes on the wire, it arrives as an explicit pick and takes
// priority over the configured stage default, so the backend's layer is
// bypassed entirely and the two implementations of the same default
// become rival sources of truth.
//
// The failure is silent and the damage is real: the frontend's copy of the
// default can be degraded (a failed GET /api/settings/effort falls back to
// the shipped values) or stale (the module-level cache is not refreshed
// across tabs), and in either case it would OVERRIDE the operator's
// actual setting — while looking perfectly configured in the UI.
//
// The rule the frontend follows is therefore:
//
//	the wire carries the RAW pick; the display carries the resolved value.
//
// `effortPick()` in frontend/src/utils/effort.ts is the one way to put a
// pick on the wire, and it omits the field when the user has not picked
// one. This script enforces that.
//
// It also allows the few places where a tier is deliberately persisted as
// CONFIGURATION rather than sent per run (the issue monitor's saved
// per-stage config, and the API client's own field declarations).

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// The repo root is derived from this script's own location, so the check
// works from any cwd (CI, an editor task, a nested shell).
const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, '..');
const src = path.join(root, 'frontend', 'src');

function walk(dir, out = []) {
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) walk(p, out);
    else if (/\.(tsx?|jsx?)$/.test(e.name)) out.push(p);
  }
  return out;
}

// `effort: <expr>` / `design_effort: <expr>` / `coding_effort: <expr>`
// written as an object LITERAL in a request body. A spread (`...effortPick(...)`)
// is the approved form and is not matched.
//
// The value must not be a "resolved" identifier — one whose name ends in
// `Value` / `EffortValue`, which by convention in this codebase is the
// display value (`pick ?? configuredDefault`).
const literalRe = /\b(effort|design_effort|coding_effort)\s*:\s*([A-Za-z_$][\w$]*)/g;

// Places where a tier is a saved CONFIGURATION, not a per-run pick:
//   - frontend/src/api/client.ts declares the request/response field types
//   - frontend/src/pages/ProjectIssueMonitor.tsx stores the monitor's own
//     per-stage config (its tier IS the monitor's saved setting)
const ALLOWED_FILES = new Set([
  path.join('frontend', 'src', 'api', 'client.ts'),
  path.join('frontend', 'src', 'pages', 'ProjectIssueMonitor.tsx'),
]);

// Identifiers that legitimately hold a RAW pick (not a resolved display
// value). A `*Value` name is by convention the resolved one.
const resolvedValueRe = /Value$/;

const problems = [];
const sourceFiles = walk(src).filter((f) => !f.includes(path.join(src, 'i18n', 'locales')));

for (const f of sourceFiles) {
  const rel = path.relative(root, f);
  if (ALLOWED_FILES.has(rel)) continue;
  const s = fs.readFileSync(f, 'utf8');
  const lines = s.split('\n');
  lines.forEach((line, i) => {
    // Skip comment lines — the prose explains the rule and mentions the
    // field names.
    const trimmed = line.trim();
    if (trimmed.startsWith('//') || trimmed.startsWith('*') || trimmed.startsWith('/*')) return;
    literalRe.lastIndex = 0;
    let m;
    while ((m = literalRe.exec(line))) {
      const [, field, value] = m;
      if (!resolvedValueRe.test(value)) continue;
      problems.push(
        `${rel}:${i + 1}  ${field}: ${value}  — a resolved display value must not go on the wire; ` +
        `use ...effortPick(<raw pick>) so an unpicked run leaves the field off`,
      );
    }
  });
}

if (problems.length) {
  console.error(`check-effort-picks: ${problems.length} problem(s)`);
  for (const line of problems) console.error('  ' + line);
  process.exit(1);
}
console.log(`check-effort-picks: OK (${sourceFiles.length} files scanned)`);
