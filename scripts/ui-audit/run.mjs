#!/usr/bin/env node
// run.mjs — entry point for the NovaWorkbench global UI consistency audit.
//
//   node scripts/ui-audit/run.mjs all      capture → detect → review → publish
//   node scripts/ui-audit/run.mjs capture  screenshots + style probe only
//   node scripts/ui-audit/run.mjs detect   rules only (no browser needed)
//   node scripts/ui-audit/run.mjs review   re-run the model pass on findings.json
//   node scripts/ui-audit/run.mjs publish  push evidence + open the issues
//   node scripts/ui-audit/run.mjs check    CI gate: fail on NEW rule violations
//
// `check` needs no browser and no model, so it can run on every push.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { ensureDir, writeJSON, readJSON, REPO_ROOT } from './util.mjs';
import { runStaticRules, readGlobalTokens } from './rules/static.mjs';
import { runRuntimeRules } from './rules/runtime.mjs';
import { reviewFindings } from './review.mjs';
import { renderReport } from './report.mjs';
import { publishEvidence, updateEvidenceFile, createIssues, resolveRepo } from './publish.mjs';

const HERE = path.dirname(fileURLToPath(import.meta.url));

// capture.mjs pulls in playwright-core. Importing it lazily keeps `check`
// runnable with nothing but node — the CI gate must not need a browser
// driver installed, and must not fail on a machine that never ran a capture.
let captureFn = null;
async function loadCapture() {
  if (!captureFn) captureFn = (await import('./capture.mjs')).capture;
  return captureFn;
}

function parseArgs(argv) {
  const out = { _: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a.startsWith('--')) {
      const key = a.slice(2);
      const next = argv[i + 1];
      if (next === undefined || next.startsWith('--')) {
        out[key] = true;
      } else {
        out[key] = next;
        i++;
      }
    } else {
      out._.push(a);
    }
  }
  return out;
}

function defaultRunId() {
  const d = new Date();
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}`;
}

const args = parseArgs(process.argv.slice(2));
const cmd = args._[0] || 'all';

const opts = {
  baseUrl: args['base-url'] || process.env.NOVA_AUDIT_BASE_URL || 'http://localhost:9527',
  runId: args['run-id'] || defaultRunId(),
  username: args.username || process.env.NOVA_AUDIT_USER || '',
  password: args.password || process.env.NOVA_AUDIT_PASS || '',
  token: args.token || process.env.NOVA_AUDIT_TOKEN || '',
  artifactsDir: path.resolve(REPO_ROOT, args['artifacts-dir'] || 'artifacts/ui-audit'),
  evidencePrefix: args['evidence-prefix'] || 'docs/ui-audit',
  branch: args.branch || '',
  provider: args.provider || 'gh',
  minSeverity: args['min-severity'] || 'P2',
  maxIssues: Number(args['max-issues'] || 30),
  viewportWidth: Number(args.width || 1440),
  viewportHeight: Number(args.height || 900),
  deviceScaleFactor: Number(args.scale || 2),
  settleMs: Number(args.settle || 900),
  timeoutMs: Number(args['review-timeout'] || 600000),
  noReview: !!args['no-review'],
  only: args.route ? (Array.isArray(args.route) ? args.route : [args.route]) : null,
  seed: !!args.seed,
  writeBaseline: !!args['write-baseline'],
  platform: args.platform || 'github',
  dryRun: !!args['dry-run'],
};

const runDir = path.join(opts.artifactsDir, opts.runId);

/**
 * A stable identity for a finding, used by the CI gate.
 *
 * Rules emit an explicit signature that identifies the finding itself — the
 * class name, the semantic role, the rule — rather than the files it happens
 * to mention. Deriving the identity from file paths made the gate report a
 * "new" violation every time an unrelated file was edited.
 */
function signatureOf(f) {
  if (f.signature) return f.signature;
  const routes = (f.evidence || []).map((e) => e.route).filter(Boolean).sort().join(',');
  if (routes) return `${f.rule}::${f.category}::${routes}`;
  const files = [...new Set((f.locations || []).map((l) => l.file))].sort().slice(0, 3).join(',');
  return `${f.rule}::${f.category}::${files}`;
}

async function cmdDetect() {
  const manifest = readJSON(path.join(runDir, 'manifest.json'));
  const { findings: staticFindings, stats } = runStaticRules();
  let runtimeFindings = [];
  if (manifest) {
    runtimeFindings = runRuntimeRules(manifest, path.join(runDir, 'probe')).findings;
  }
  const findings = [...staticFindings, ...runtimeFindings];
  ensureDir(runDir);
  writeJSON(path.join(runDir, 'findings.json'), { runId: opts.runId, findings });
  process.stdout.write(`detect: ${findings.length} findings (${staticFindings.length} static, ${runtimeFindings.length} runtime)\n`);
  return { findings, stats, manifest };
}

async function cmdReview(findings) {
  if (opts.noReview) {
    process.stdout.write('review: skipped (--no-review)\n');
    return { findings, reviewed: 0, rejected: 0 };
  }
  const tokens = readGlobalTokens();
  const tokenValues = [...tokens.entries()].map(([normalized, v]) => ({ normalized, names: v.names }));
  const res = await reviewFindings(findings, { tokenValues, timeoutMs: opts.timeoutMs });
  process.stdout.write(`review: ${res.reviewed} kept, ${res.rejected} rejected${res.unavailable ? ' (claude unavailable)' : ''}\n`);
  return res;
}

async function cmdPublish(findings, manifest) {
  const { owner, repo } = await resolveRepo();
  if (!owner || !repo) {
    process.stdout.write('publish: cannot determine origin repo — skipping\n');
    return {};
  }
  const branch = opts.branch || `audit/ui-${opts.runId}`;
  // The raw base stops at the branch; evidenceRelPath() appends the file
  // path inside the branch.
  const rawBase = {
    url: opts.platform === 'gitlab'
      ? `https://gitlab.com/${owner}/${repo}/-/raw/${branch}`
      : `https://raw.githubusercontent.com/${owner}/${repo}/${branch}`,
    prefix: opts.evidencePrefix,
    branch,
  };

  // Render before publishing so the branch carries a report even when the
  // issue pass fails — the report is part of the evidence, not a side effect
  // of opening the issues.
  const report = renderReport({
    runId: opts.runId,
    manifest,
    findings,
    rawBase,
    issueUrls: {},
  });
  fs.writeFileSync(path.join(runDir, 'report.md'), report);

  const rawInfo = await publishEvidence({
    runId: opts.runId,
    outDir: runDir,
    branch,
    owner,
    repo,
    platform: opts.platform,
    baseRef: args['base-ref'] || '',
    prefix: opts.evidencePrefix,
  });
  if (!rawInfo.pushed) {
    process.stdout.write(`publish: evidence NOT pushed — ${rawInfo.reason}\n`);
  } else {
    process.stdout.write(`publish: evidence on branch ${rawInfo.branch}\n`);
  }

  const raw = rawInfo.pushed ? rawInfo.url : '';
  const { issueUrls, failures, created, reused } = await createIssues({
    findings,
    runId: opts.runId,
    repo: `${owner}/${repo}`,
    raw,
    branch: rawInfo.pushed ? rawInfo.branch : '',
    provider: opts.dryRun ? 'local' : opts.provider,
    minSeverity: opts.minSeverity,
    maxIssues: opts.maxIssues,
    outDir: runDir,
    prefix: opts.evidencePrefix,
  });
  process.stdout.write(
    `publish: ${created} issue(s) opened, ${reused || 0} already existed, ${failures.length} failed\n`,
  );
  for (const f of failures) process.stdout.write(`  FAILED ${f.id || ''}: ${f.reason}\n`);

  // Now that the issue numbers are known, re-render the report with the
  // issue index and push the updated copy onto the evidence branch.
  const finalReport = renderReport({
    runId: opts.runId,
    manifest,
    findings,
    rawBase,
    issueUrls: issueUrls || {},
  });
  fs.writeFileSync(path.join(runDir, 'report.md'), finalReport);
  if (rawInfo.pushed) {
    const updated = await updateEvidenceFile({
      branch: rawInfo.branch,
      prefix: opts.evidencePrefix,
      runId: opts.runId,
      file: path.join(runDir, 'report.md'),
      name: 'report.md',
    });
    if (!updated.pushed) process.stdout.write(`publish: report index not pushed — ${updated.reason}\n`);
  }
  return { issueUrls, rawInfo, failures };
}

async function cmdAll() {
  process.stdout.write(`ui-audit: run ${opts.runId}\n`);
  const capture = await loadCapture();
  const { manifest } = await capture(opts);
  process.stdout.write(`capture: ${manifest.routes.length} route(s) captured, ${manifest.failures.length} failed\n`);

  const { findings } = await cmdDetect();
  const reviewed = await cmdReview(findings);
  writeJSON(path.join(runDir, 'findings.json'), { runId: opts.runId, findings: reviewed.findings });

  const pub = await cmdPublish(reviewed.findings, manifest);
  return { findings: reviewed.findings, manifest, pub };
}

async function cmdCheck() {
  const { findings } = runStaticRules();
  const baselineFile = path.join(HERE, 'baseline.json');
  const baseline = readJSON(baselineFile, { signatures: [] });
  const known = new Set(baseline.signatures || []);
  const newOnes = findings.filter((f) => !known.has(signatureOf(f)));

  if (opts.writeBaseline) {
    writeJSON(baselineFile, {
      generatedAt: new Date().toISOString(),
      note: 'Known UI-convention violations at the time of writing. `run.mjs check` fails only on violations NOT listed here.',
      signatures: [...new Set(findings.map(signatureOf))].sort(),
    });
    process.stdout.write(`check: baseline written with ${new Set(findings.map(signatureOf)).size} signature(s)\n`);
    return;
  }

  if (newOnes.length) {
    process.stdout.write(`check: ${newOnes.length} NEW UI convention violation(s)\n`);
    for (const f of newOnes) {
      process.stdout.write(`  [${f.severity}] ${f.rule} ${f.title}\n`);
      for (const l of (f.locations || []).slice(0, 3)) process.stdout.write(`      ${l.file}:${l.line}\n`);
    }
    process.exitCode = 1;
    return;
  }
  process.stdout.write(`check: no new violations (${findings.length} known, all baselined)\n`);
}

async function main() {
  switch (cmd) {
    case 'all':
      await cmdAll();
      break;
    case 'capture': {
      const capture = await loadCapture();
      const { manifest } = await capture(opts);
      process.stdout.write(`capture: ${manifest.routes.length} route(s)\n`);
      break;
    }
    case 'detect':
      await cmdDetect();
      break;
    case 'review': {
      const existing = readJSON(path.join(runDir, 'findings.json'), { findings: [] });
      const res = await cmdReview(existing.findings);
      writeJSON(path.join(runDir, 'findings.json'), { runId: opts.runId, findings: res.findings });
      break;
    }
    case 'publish': {
      const existing = readJSON(path.join(runDir, 'findings.json'), { findings: [] });
      const manifest = readJSON(path.join(runDir, 'manifest.json'));
      await cmdPublish(existing.findings, manifest);
      break;
    }
    case 'check':
      await cmdCheck();
      break;
    default:
      process.stderr.write(`unknown command: ${cmd}\nusage: run.mjs [all|capture|detect|review|publish|check]\n`);
      process.exitCode = 2;
  }
}

main().catch((e) => {
  process.stderr.write(`ui-audit failed: ${e?.stack || e}\n`);
  process.exitCode = 1;
});
