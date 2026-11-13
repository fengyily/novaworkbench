// report.mjs — render the audit result as a human-readable Markdown report.
import path from 'node:path';
import { rel, evidenceRelPath } from './util.mjs';

const SEVERITY_ORDER = { P0: 0, P1: 1, P2: 2, P3: 3 };

/** Absolute raw URL of a screenshot on the evidence branch. */
function rawUrl(rawBase, screenshot, runId) {
  if (!rawBase?.url) return '';
  return `${rawBase.url}/${evidenceRelPath(screenshot, runId, rawBase.prefix || 'docs/ui-audit')}`;
}

export function sortFindings(findings) {
  return [...findings].sort((a, b) => {
    const sa = SEVERITY_ORDER[a.severity] ?? 9;
    const sb = SEVERITY_ORDER[b.severity] ?? 9;
    if (sa !== sb) return sa - sb;
    return String(a.rule).localeCompare(String(b.rule));
  });
}

export function renderReport({ runId, manifest, findings, stats, rawBase, issueUrls }) {
  const live = findings.filter((f) => f.status !== 'rejected');
  const rejected = findings.filter((f) => f.status === 'rejected');
  const bySeverity = {};
  for (const f of live) bySeverity[f.severity] = (bySeverity[f.severity] || 0) + 1;
  const byRule = {};
  for (const f of live) byRule[f.rule] = (byRule[f.rule] || 0) + 1;

  const out = [];
  out.push(`# NovaWorkbench 全局 UI 一致性审计报告`);
  out.push('');
  out.push(`| | |`);
  out.push(`|---|---|`);
  out.push(`| run id | \`${runId}\` |`);
  out.push(`| 被审 commit | \`${manifest.gitCommit}\` |`);
  out.push(`| 被审实例 | ${manifest.baseUrl} |`);
  out.push(`| 采集时间 | ${manifest.capturedAt} |`);
  out.push(`| 视口 | ${manifest.viewport.width}×${manifest.viewport.height} @${manifest.viewport.deviceScaleFactor}x |`);
  out.push(`| 浏览器 | ${manifest.browser} |`);
  out.push(`| 覆盖路由 | ${manifest.routes.length} |`);
  out.push(`| 跳过路由 | ${manifest.skipped.length} |`);
  out.push(`| 证据分支 | \`${rawBase?.branch || '(未发布)'}\` |`);
  out.push(`| 证据根路径 | \`${rawBase?.prefix || 'docs/ui-audit'}/${runId}\` |`);
  out.push('');

  out.push('## 严重度分布');
  out.push('');
  out.push('| 严重度 | 数量 |');
  out.push('|---|---|');
  for (const s of ['P0', 'P1', 'P2', 'P3']) out.push(`| ${s} | ${bySeverity[s] || 0} |`);
  out.push('');

  out.push('## 规则命中分布');
  out.push('');
  out.push('| 规则 | 数量 | 说明 |');
  out.push('|---|---|---|');
  const RULE_DESC = {
    R0: '审计范围与 App.tsx 路由不一致',
    R1: '调色板外的硬编码颜色',
    R2: '令牌值被写成字面量而非 var(--token)',
    R3: '同一颜色存在多种写法',
    R4: '使用了没有任何样式表定义的类名',
    R5: '类名存在前缀式/后缀式两套写法',
    R6: '同类容器存在多套类名',
    R7: '同类区块标题存在多套类名',
    R8: '状态色存在并行词汇表或旁路着色',
    R9: '内联样式密度过高，压过共享类',
    R10: 'Markdown 渲染点未挂 .md-prose',
    R11: '同一控件在不同页面的计算样式不一致',
    R12: '占位页',
  };
  for (const r of Object.keys(byRule).sort()) {
    out.push(`| ${r} | ${byRule[r]} | ${RULE_DESC[r] || ''} |`);
  }
  out.push('');

  out.push('## 索引');
  out.push('');
  out.push('| # | 严重度 | 规则 | 标题 | 证据 | Issue |');
  out.push('|---|---|---|---|---|---|');
  sortFindings(live).forEach((f, i) => {
    const shots = (f.evidence || []).filter((e) => e.screenshot);
    const shot = shots[0];
    const url = shot ? rawUrl(rawBase, shot.screenshot, runId) : '';
    // One thumbnail per row is noise when every runtime finding would show
    // the same first screenshot. The number of routes involved is what tells
    // the findings apart; the details section lists every screenshot.
    const routeCount = new Set(shots.map((e) => e.route).filter(Boolean)).size;
    const cell = url
      ? `${routeCount || 1} 个路由 · [首张](${url})`
      : routeCount
        ? `${routeCount} 个路由`
        : '—';
    const issueUrl = issueUrls?.[f.id];
    const issueNo = issueUrl ? String(issueUrl).match(/(\d+)\s*$/)?.[1] : '';
    const issue = issueUrl ? `[#${issueNo || '?'}](${issueUrl})` : '—';
    out.push(`| ${i + 1} | ${f.severity} | ${f.rule} | ${f.title} | ${cell} | ${issue} |`);
  });
  out.push('');

  out.push('## 详情');
  out.push('');
  for (const [i, f] of sortFindings(live).entries()) {
    out.push(`### ${i + 1}. ${f.title}`);
    out.push('');
    out.push(`- **规则**：${f.rule}${RULE_DESC[f.rule] ? ` — ${RULE_DESC[f.rule]}` : ''}`);
    out.push(`- **严重度**：${f.severity}`);
    out.push(`- **编号**：\`${f.id}\``);
    out.push(`- **复核**：${f.reviewed ? '已由模型读图复核' : '未经模型复核（沿用规则文案）'}`);
    out.push('');
    out.push(f.detail);
    out.push('');
    if (f.diff?.length) {
      out.push('差异明细：');
      out.push('');
      for (const d of f.diff) out.push(d);
      out.push('');
    }
    if ((f.evidence || []).length) {
      out.push('证据：');
      out.push('');
      for (const e of f.evidence.slice(0, 8)) {
        const url = rawUrl(rawBase, e.screenshot, runId);
        const line = `- \`${e.route || ''}\`${url ? ` — [截图](${url})` : ''}${e.sha256 ? ` · sha256 \`${e.sha256.slice(0, 16)}…\`` : ''}${e.value ? ` · \`${e.value}\`` : ''}`;
        out.push(line);
      }
      out.push('');
    }
    if ((f.locations || []).length) {
      out.push('涉及位置：');
      out.push('');
      for (const l of f.locations.slice(0, 12)) {
        out.push(`- \`${l.file}${l.line ? ':' + l.line : ''}\`${l.snippet ? ` — ${l.snippet}` : ''}`);
      }
      out.push('');
    }
    if (f.table?.length) {
      out.push(`| ${(f.tableHeader || []).join(' | ')} |`);
      out.push(`|${(f.tableHeader || []).map(() => '---').join('|')}|`);
      for (const row of f.table.slice(0, 40)) out.push(`| ${row} |`);
      if (f.table.length > 40) out.push(`| … 另有 ${f.table.length - 40} 行 | | |`);
      out.push('');
    }
    if (f.suggestion) {
      out.push(`**建议**：${f.suggestion}`);
      out.push('');
    }
  }

  if (rejected.length) {
    out.push('## 被判定为误报（未建单）');
    out.push('');
    for (const f of rejected) {
      out.push(`- \`${f.id}\` ${f.title} — ${f.reviewNote || ''}`);
    }
    out.push('');
  }

  if (manifest.skipped.length) {
    out.push('## 跳过的路由');
    out.push('');
    for (const s of manifest.skipped) out.push(`- \`${s.path}\` — ${s.reason}`);
    out.push('');
  }
  if (manifest.failures?.length) {
    out.push('## 采集失败');
    out.push('');
    for (const s of manifest.failures) out.push(`- \`${s.path}\` — ${s.reason}`);
    out.push('');
  }

  out.push('## 复现方式');
  out.push('');
  out.push('```bash');
  out.push(`node scripts/ui-audit/run.mjs all --base-url ${manifest.baseUrl} --run-id ${runId}`);
  out.push('```');
  out.push('');
  out.push('每张截图的 sha256 记录在 `manifest.json` 中，可用于校验证据未被替换。');
  out.push('');

  return out.join('\n');
}
