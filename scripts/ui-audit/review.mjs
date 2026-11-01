// review.mjs — let Claude look at the screenshots and judge the findings.
//
// The deterministic rules decide *what* is inconsistent; this stage decides
// *whether it matters* and writes the wording that goes into the issue.
// Claude's Read tool can open the PNGs, so the visual check is real, not a
// guess.
//
// Every failure mode degrades to "keep the rule-generated wording, mark it
// unreviewed" — the audit must never fail because the model was unavailable.
import path from 'node:path';
import { run, REPO_ROOT } from './util.mjs';

const CLAUDE_BIN = process.env.CLAUDE_BIN || 'claude';
const BATCH_SIZE = 5;

/** Pull the last JSON array/object out of a model reply. */
export function extractJSON(text) {
  const fence = [...text.matchAll(/```json\s*([\s\S]*?)```/g)].pop();
  const candidates = [];
  if (fence) candidates.push(fence[1]);
  candidates.push(text);

  for (const candidate of candidates) {
    const trimmed = candidate.trim();
    if (!trimmed) continue;
    // Prefer a top-level array.
    const arrStart = trimmed.indexOf('[');
    const objStart = trimmed.indexOf('{');
    const start = arrStart >= 0 && (objStart < 0 || arrStart < objStart) ? arrStart : objStart;
    if (start < 0) continue;
    const open = trimmed[start];
    const close = open === '[' ? ']' : '}';
    let depth = 0;
    let inStr = false;
    let esc = false;
    for (let i = start; i < trimmed.length; i++) {
      const ch = trimmed[i];
      if (esc) { esc = false; continue; }
      if (ch === '\\') { esc = true; continue; }
      if (ch === '"') { inStr = !inStr; continue; }
      if (inStr) continue;
      if (ch === open) depth++;
      else if (ch === close) {
        depth--;
        if (depth === 0) {
          try {
            return JSON.parse(trimmed.slice(start, i + 1));
          } catch {
            break;
          }
        }
      }
    }
  }
  return null;
}

function buildPrompt(batch, tokenSet) {
  const items = batch.map((f) => {
    const shots = (f.evidence || [])
      .filter((e) => e.screenshot)
      .map((e) => `- ${e.route || ''} → ${path.resolve(REPO_ROOT, e.screenshot)}`)
      .join('\n');
    const locs = (f.locations || []).slice(0, 6).map((l) => `- ${l.file}:${l.line}${l.snippet ? ` (${l.snippet})` : ''}`).join('\n');
    return `### ${f.id} [${f.rule}] ${f.title}
严重度(初判): ${f.severity}
说明: ${f.detail}
涉及位置:
${locs || '- (无)'}
截图:
${shots || '- (无)'}`;
  });

  return `你正在为 NovaWorkbench（一个 React 19 + TypeScript 的本地 AI 工作台）做一次全局 UI 一致性审计。
下面是自动化规则发现的候选不一致点，每条都附带截图文件的绝对路径。

请用 Read 工具**实际打开**每条对应的截图（PNG），肉眼确认它描述的现象在图上是否成立，
然后判断这条是不是「用户在界面上真的能感知到的不统一」。

判断准则：
1. 保留(verdict="keep")：用户能看出的不统一——同一控件在不同页面长得不一样、
   颜色/圆角/间距明显不一致、同一类元素用了两套视觉语言、用了没有定义的类名导致外观随机。
2. 丢弃(verdict="reject")：纯内部实现差异、外部代码/第三方类名导致的误报、
   或者该页面本来就是独立场景（登录页、占位页）不需要与全站一致。
3. 视觉上确实有问题的，请把 title 改写成用户视角的描述（不要出现"规则 R11"这类内部编号）。
4. detail 写「用户看到什么、为什么算不统一」，2-3 句，不要复述规则名。
5. suggestion 写具体的改法（改哪个文件/加哪个令牌/换成哪个类名），1-2 句。
6. severity 可在 P0/P1/P2/P3 之间调整：P0=明显破坏可用性，P1=用户能感知的跨页不一致，
   P2=命名/写法层面的不统一，P3=提示级。

调色板参考（index.css 的 :root 令牌，共 ${tokenSet.size} 个色值）：
${[...tokenSet].slice(0, 40).join(', ')}

请只输出一个 JSON 数组，每个元素形如：
{"id":"<原始 id>","verdict":"keep|reject","title":"...","detail":"...","suggestion":"...","severity":"P1"}
不要输出数组以外的任何内容。

待审条目：
${items.join('\n\n')}`;
}

export async function reviewFindings(findings, opts) {
  // Every finding is worth a verdict: the runtime ones carry a screenshot the
  // model can open, and the static ones carry file locations and tables it
  // can reason about. The model only ever rewrites wording and marks a
  // finding as a false positive — it never decides what exists.
  const reviewable = findings;
  if (!reviewable.length) return { findings, reviewed: 0, rejected: 0, unavailable: false };

  const probe = await run(CLAUDE_BIN, ['--version'], { timeout: 20000 });
  if (probe.code !== 0) {
    return { findings: reviewable.map((f) => ({ ...f, reviewed: false })), reviewed: 0, rejected: 0, unavailable: true };
  }

  const tokenSet = new Set(
    (opts.tokenValues || []).map((t) => t.normalized),
  );

  const batches = [];
  for (let i = 0; i < reviewable.length; i += BATCH_SIZE) batches.push(reviewable.slice(i, i + BATCH_SIZE));

  const byId = new Map(reviewable.map((f) => [f.id, f]));
  let reviewed = 0;
  let rejected = 0;

  for (const [bi, batch] of batches.entries()) {
    const prompt = buildPrompt(batch, tokenSet);
    const res = await run(
      CLAUDE_BIN,
      ['-p', prompt, '--output-format', 'text', '--dangerously-skip-permissions'],
      { timeout: opts.timeoutMs || 600000, cwd: REPO_ROOT, maxBuffer: 64 * 1024 * 1024 },
    );
    if (res.code !== 0) {
      process.stdout.write(`  review batch ${bi + 1}/${batches.length}: claude exited ${res.code} — keeping rule wording\n`);
      continue;
    }
    const parsed = extractJSON(res.stdout);
    if (!parsed || !Array.isArray(parsed)) {
      process.stdout.write(`  review batch ${bi + 1}/${batches.length}: no JSON array in reply — keeping rule wording\n`);
      continue;
    }
    for (const item of parsed) {
      const target = byId.get(item.id);
      if (!target) continue;
      if (item.verdict === 'reject') {
        target.status = 'rejected';
        target.reviewNote = item.detail || 'model judged this a false positive';
        rejected++;
        continue;
      }
      if (item.title) target.title = String(item.title);
      if (item.detail) target.detail = String(item.detail);
      if (item.suggestion) target.suggestion = String(item.suggestion);
      if (item.severity) target.severity = String(item.severity);
      target.reviewed = true;
      reviewed++;
    }
    process.stdout.write(`  review batch ${bi + 1}/${batches.length}: ${parsed.length} verdicts\n`);
  }

  return { findings: reviewable, reviewed, rejected, unavailable: false };
}
