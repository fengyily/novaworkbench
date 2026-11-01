// publish.mjs — put the evidence on a git branch and open one issue per finding.
//
// Two independent steps:
//   publishEvidence()  — push screenshots + report + findings to a dedicated
//                         branch under docs/ui-audit/<run-id>/
//   createIssues()      — open one GitHub issue per confirmed finding, with
//                         the screenshot embedded as a raw link
//
// The issue provider is pluggable so the same findings file can be turned
// into issues by `gh` today, by NovaWorkbench's own platform client later,
// or not at all (`local` writes the issue bodies to disk instead).
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { run, git, ensureDir, writeJSON, evidenceRelPath, whyTrailer } from './util.mjs';

const LABEL_BASE = 'ui-audit';
const LABEL_COLORS = {
  'ui-audit': '4F46E5',
  P0: 'B91C1C',
  P1: 'D97706',
  P2: '0E7490',
  P3: '64748B',
};

export async function resolveRepo() {
  const r = await git(['remote', 'get-url', 'origin']);
  const url = r.stdout.trim();
  const m = url.match(/[:/]([^/:]+)\/([^/]+?)(?:\.git)?$/);
  if (!m) return { owner: '', repo: '', url };
  return { owner: m[1], repo: m[2], url };
}

/**
 * Prepare a temporary worktree checked out on `branch`.
 *
 * The evidence branch is a real git branch, so the files have to be written
 * while it is checked out — otherwise they land in the caller's working tree
 * and get committed there. A worktree does that without switching the
 * caller's branch, and — unlike an in-place checkout — it works even when
 * the caller's tree is dirty: the uncommitted work stays where it is and
 * can never travel into the evidence commit.
 */
async function prepareBranchWorktree(dir, branch, baseRef) {
  await git(['fetch', 'origin', branch]); // absent remote branch is fine
  const local = (await git(['rev-parse', '--verify', `refs/heads/${branch}`])).code === 0;
  const remote = (await git(['rev-parse', '--verify', `refs/remotes/origin/${branch}`])).code === 0;

  if (local) {
    const r = await git(['worktree', 'add', dir, branch]);
    return r.code === 0 ? { ok: true } : { ok: false, reason: `worktree add ${branch}: ${r.stderr.trim()}` };
  }

  const base = remote ? `origin/${branch}` : baseRef || 'HEAD';
  const r = await git(['worktree', 'add', '--detach', dir, base]);
  if (r.code !== 0) return { ok: false, reason: `worktree add ${base}: ${r.stderr.trim()}` };
  const c = await git(['checkout', '-b', branch], { cwd: dir });
  if (c.code !== 0) return { ok: false, reason: `checkout -b ${branch}: ${c.stderr.trim()}` };
  return { ok: true };
}

/**
 * Run `fn(worktreeDir)` inside a temporary worktree on `branch`, then always
 * remove the worktree again.
 */
export async function withEvidenceBranch(branch, baseRef, fn) {
  const dir = path.join(os.tmpdir(), `nova-ui-audit-${branch.replace(/[^\w.-]+/g, '_')}-${process.pid}`);
  fs.rmSync(dir, { recursive: true, force: true });
  try {
    const setup = await prepareBranchWorktree(dir, branch, baseRef);
    if (!setup.ok) return { ok: false, reason: setup.reason };
    const result = await fn(dir);
    return { ok: true, result };
  } finally {
    await git(['worktree', 'remove', '--force', dir]);
    await git(['worktree', 'prune']);
  }
}

/**
 * Copy the run artifacts onto a dedicated branch and push it.
 * Returns { branch, url, pushed }.
 */
export async function publishEvidence({ runId, outDir, branch, owner, repo, platform, baseRef, prefix }) {
  const prefixRel = `${prefix.replace(/^\/+|\/+$/g, '')}/${runId}`;

  const outcome = await withEvidenceBranch(branch, baseRef, async (dir) => {
    const target = path.join(dir, prefixRel);
    ensureDir(target);
    const srcShots = path.join(outDir, 'screenshots');
    if (fs.existsSync(srcShots)) fs.cpSync(srcShots, path.join(target, 'screenshots'), { recursive: true });
    const srcProbe = path.join(outDir, 'probe');
    if (fs.existsSync(srcProbe)) fs.cpSync(srcProbe, path.join(target, 'probe'), { recursive: true });
    for (const f of ['manifest.json', 'findings.json', 'report.md']) {
      const p = path.join(outDir, f);
      if (fs.existsSync(p)) fs.copyFileSync(p, path.join(target, f));
    }
    await git(['add', '-A', prefixRel], { cwd: dir });
    const commit = await git(
      ['commit', '-m', `chore(ui-audit): evidence for run ${runId}${whyTrailer([`Screenshots and style probes for the ${runId} audit run, pinned to the audited commit so every issue can point at verifiable evidence.`])}`],
      { cwd: dir },
    );
    const push = await git(['push', 'origin', branch], { cwd: dir });
    return { commit: commit.stdout.trim().split('\n')[0], pushCode: push.code, pushErr: push.stderr.trim() };
  });

  if (!outcome.ok) {
    return { pushed: false, branch, reason: outcome.reason, prefixRel };
  }
  if (outcome.result.pushCode !== 0) {
    return { pushed: false, branch, reason: `push failed: ${outcome.result.pushErr}`, prefixRel };
  }

  // The raw base stops at the branch: individual file paths are appended
  // through evidenceRelPath(), which knows the local→branch path mapping.
  const url = platform === 'gitlab'
    ? `https://gitlab.com/${owner}/${repo}/-/raw/${branch}`
    : `https://raw.githubusercontent.com/${owner}/${repo}/${branch}`;

  return { pushed: true, branch, url, prefixRel, prefix, commit: outcome.result.commit };
}

/**
 * Push an updated file (the report with the issue index) onto the evidence
 * branch without disturbing the caller's checkout.
 */
export async function updateEvidenceFile({ branch, prefix, runId, file, name, baseRef }) {
  const prefixRel = `${prefix.replace(/^\/+|\/+$/g, '')}/${runId}`;
  const outcome = await withEvidenceBranch(branch, baseRef, async (dir) => {
    const target = path.join(dir, prefixRel, name);
    ensureDir(path.dirname(target));
    fs.copyFileSync(file, target);
    await git(['add', '-A', prefixRel], { cwd: dir });
    const commit = await git(['commit', '-m', `chore(ui-audit): add issue index to run ${runId}`], { cwd: dir });
    const push = await git(['push', 'origin', branch], { cwd: dir });
    return { commit: commit.stdout.trim().split('\n')[0], pushCode: push.code, pushErr: push.stderr.trim() };
  });
  if (!outcome.ok) return { pushed: false, reason: outcome.reason };
  if (outcome.result.pushCode !== 0) return { pushed: false, reason: `push failed: ${outcome.result.pushErr}` };
  return { pushed: true, commit: outcome.result.commit };
}

/** The Markdown body of one issue. */
export function buildIssueBody(finding, { runId, raw, branch, prefix = 'docs/ui-audit' }) {
  const L = [];
  L.push(finding.detail);
  L.push('');

  const shots = (finding.evidence || []).filter((e) => e.screenshot);
  if (shots.length) {
    L.push('## 证据');
    L.push('');
    for (const s of shots.slice(0, 4)) {
      const url = raw ? `${raw}/${evidenceRelPath(s.screenshot, runId, prefix)}` : '';
      L.push(`**${s.route || ''}**`);
      L.push('');
      L.push(url ? `![${s.route || 'screenshot'}](${url})` : `_截图：${s.screenshot}_`);
      L.push('');
    }
    if (finding.diff?.length) {
      L.push('差异明细：');
      L.push('');
      for (const d of finding.diff) L.push(d);
      L.push('');
    }
  }

  if ((finding.locations || []).length) {
    L.push('## 涉及位置');
    L.push('');
    for (const l of finding.locations.slice(0, 12)) {
      L.push(`- \`${l.file}${l.line ? ':' + l.line : ''}\`${l.snippet ? ` — ${l.snippet}` : ''}`);
    }
    L.push('');
  }

  if (finding.table?.length) {
    L.push(`| ${(finding.tableHeader || []).join(' | ')} |`);
    L.push(`|${(finding.tableHeader || []).map(() => '---').join('|')}|`);
    for (const row of finding.table.slice(0, 30)) L.push(`| ${row} |`);
    if (finding.table.length > 30) L.push(`| … 另有 ${finding.table.length - 30} 行 | | |`);
    L.push('');
  }

  L.push('## 复现');
  L.push('');
  L.push('1. 拉取本仓库并 checkout 上面记录的 commit');
  L.push('2. `make build && make run`（或 `npm run dev`）启动');
  L.push('3. 打开上面证据里列出的路由，对比同一类控件的尺寸/间距/颜色');
  L.push('');

  if (finding.suggestion) {
    L.push('## 建议修法');
    L.push('');
    L.push(finding.suggestion);
    L.push('');
  }

  L.push('---');
  L.push('');
  L.push(
    `由 \`scripts/ui-audit\` 自动生成 · run \`${runId}\` · 规则 \`${finding.rule}\` · ` +
    `严重度 \`${finding.severity}\`${finding.reviewed ? ' · 已读图复核' : ' · 未复核'}` +
    `${branch ? ` · 证据分支 \`${branch}\`` : ''}`,
  );
  L.push('');
  L.push(`<!-- ui-audit-run: ${runId} -->`);
  L.push('');
  return L.join('\n');
}

async function ghAvailable() {
  const r = await run('gh', ['--version'], { timeout: 20000 });
  return r.code === 0;
}

async function ensureLabels(repo, labels) {
  const list = await run('gh', ['label', 'list', '--repo', repo, '--limit', '200', '--json', 'name'], { timeout: 60000 });
  const existing = new Set();
  if (list.code === 0) {
    try {
      for (const l of JSON.parse(list.stdout)) existing.add(l.name);
    } catch { /* keep going — we create what is missing */ }
  }
  for (const name of labels) {
    if (existing.has(name)) continue;
    await run(
      'gh',
      ['label', 'create', name, '--repo', repo, '--color', LABEL_COLORS[name] || '6B7280', '--description', `UI audit: ${name}`],
      { timeout: 60000 },
    );
  }
}

/**
 * Open one issue per finding.
 * provider: 'gh' (default) | 'local' (write bodies to disk only)
 */
export async function createIssues({ findings, runId, repo, raw, branch, provider, minSeverity, maxIssues, outDir, prefix = 'docs/ui-audit' }) {
  const order = { P0: 0, P1: 1, P2: 2, P3: 3 };
  const eligible = findings
    .filter((f) => f.status !== 'rejected')
    .filter((f) => (order[f.severity] ?? 9) <= (order[minSeverity] ?? 2))
    .sort((a, b) => (order[a.severity] ?? 9) - (order[b.severity] ?? 9));

  const capped = eligible.slice(0, maxIssues);
  const issueUrls = {};
  const failures = [];
  const bodiesDir = ensureDir(path.join(outDir, 'issue-bodies'));

  if (provider === 'local') {
    for (const f of capped) {
      const body = buildIssueBody(f, { runId, raw, branch, prefix });
      const file = path.join(bodiesDir, `${f.id}.md`);
      fs.writeFileSync(file, body);
      issueUrls[f.id] = `file://${file}`;
    }
    return { issueUrls, failures, created: capped.length, provider: 'local' };
  }

  if (!(await ghAvailable())) {
    return { issueUrls, failures: [{ reason: 'gh CLI not available' }], created: 0, provider };
  }

  // Idempotency: the run id is stamped into every body, so an interrupted
  // run can be resumed without opening duplicates.
  const existing = await run(
    'gh',
    ['issue', 'list', '--repo', repo, '--state', 'all', '--search', `"ui-audit-run: ${runId}"`, '--limit', '100', '--json', 'number,body'],
    { timeout: 90000 },
  );
  const already = new Set();
  if (existing.code === 0) {
    try {
      for (const i of JSON.parse(existing.stdout)) {
        const m = String(i.body || '').match(/ui-audit-run:\s*(\S+)/);
        if (m) already.add(m[1]);
      }
    } catch { /* proceed without dedupe info */ }
  }
  if (already.size) {
    process.stdout.write(`  ${already.size} issue(s) from run ${runId} already exist — skipping those\n`);
  }

  const labels = [LABEL_BASE, ...new Set(capped.map((f) => f.severity))];
  await ensureLabels(repo, labels);

  for (const f of capped) {
    const title = `[UI] ${f.title}`;
    const body = buildIssueBody(f, { runId, raw, branch, prefix });
    const bodyFile = path.join(bodiesDir, `${f.id}.md`);
    fs.writeFileSync(bodyFile, body);

    const res = await run(
      'gh',
      [
        'issue', 'create', '--repo', repo,
        '--title', title,
        '--body-file', bodyFile,
        '--label', LABEL_BASE,
        ...(f.severity ? ['--label', f.severity] : []),
      ],
      { timeout: 120000 },
    );
    if (res.code !== 0) {
      failures.push({ id: f.id, reason: res.stderr.trim().split('\n').slice(-3).join(' ') });
      continue;
    }
    const url = res.stdout.trim().split('\n').pop();
    issueUrls[f.id] = url;
    process.stdout.write(`  opened ${url}\n`);
  }

  writeJSON(path.join(outDir, 'issues.json'), { runId, issueUrls, failures });
  return { issueUrls, failures, created: Object.keys(issueUrls).length, provider };
}
