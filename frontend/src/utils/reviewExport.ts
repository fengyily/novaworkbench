// Turning a Review session into a document the user can keep.
//
// Why this isn't just `result_md`: the backend SPLITS a finished report in
// two. The trailing JSON contract block is parsed out into findings_json
// (that is what the graded P0/P1/P3 list on screen is rendered from) and
// result_md keeps only the prose. Exporting result_md alone would therefore
// hand over a document missing the structured findings — the part a reviewer
// is most likely to paste into an issue tracker. So the Markdown is
// reassembled here: header → verdict → ledger → goal evidence → findings →
// the report prose → optionally the 追问 thread.
//
// Both export formats share this one builder: `.md` downloads the string as
// a Blob, PDF hands the same string to exportDesignPdf (which pre-bakes
// Mermaid into inline SVG, so the 改动概览图 survives into the PDF).
import i18next from 'i18next';
import {
  parseReviewFindings, parseGoalChecks,
  type CodeReview, type CodeReviewFollowup, type ReviewFinding, type ReviewSeverity,
} from '../api/client';

/** Display order of the severity buckets. P2 is intentionally absent. */
const SEVERITIES: ReviewSeverity[] = ['P0', 'P1', 'P3'];

export interface ReviewExportInput {
  review: CodeReview;
  /** Appended as a 追问附录 when non-empty. */
  followups?: CodeReviewFollowup[];
  /** Project name, shown in the document header. */
  projectName?: string;
  /** Include the 追问 thread. Off for the "report only" export. */
  includeFollowups?: boolean;
}

// Named `rt`, not `t`: scripts/check-i18n-keys.mjs greps for literal
// `rt('…')` calls and would read these short keys as top-level ones. The
// template below still registers the `projects.detail.` prefix with that
// checker, so the namespace itself stays verified.
const rt = (key: string, opts?: Record<string, unknown>) => i18next.t(`projects.detail.${key}`, opts) as string;

/** Human label for the review's target: `#42` for a PR, else `branch ← base`. */
export function reviewTargetLabel(rv: CodeReview): string {
  const head = rv.review_type === 'pr' && rv.pr_number > 0 ? `#${rv.pr_number}` : rv.branch;
  return rv.base_branch ? `${head} ← ${rv.base_branch}` : head;
}

/**
 * The document title, also used as the download filename stem. A
 * user-written title wins; otherwise the target reads well enough on its own
 * ("Review 报告 — feat/pagination ← main").
 */
export function reviewExportTitle(rv: CodeReview): string {
  const base = rt('reviewReportTitle');
  return rv.title.trim() ? `${base} — ${rv.title.trim()}` : `${base} — ${reviewTargetLabel(rv)}`;
}

/**
 * Reassemble a full Markdown document for one review.
 *
 * Deliberately plain Markdown — no HTML, no CSS classes — so the same string
 * works as a `.md` file, as the PDF exporter's input, and pasted into a PR
 * comment or an issue.
 */
export function buildReviewMarkdown(input: ReviewExportInput): string {
  const { review, projectName, followups = [], includeFollowups = true } = input;
  const findings = parseReviewFindings(review);
  const checks = parseGoalChecks(review);
  const parts: string[] = [];

  parts.push(`# ${reviewExportTitle(review)}`);

  // Metadata block — enough for the document to stand on its own once it has
  // left the app (which branch, which model, when, which version).
  const meta: string[] = [];
  if (projectName) meta.push(`- ${rt('reviewExportMetaProject')}: ${projectName}`);
  meta.push(`- ${rt('reviewExportMetaTarget')}: \`${reviewTargetLabel(review)}\``);
  meta.push(`- ${rt('reviewExportMetaType')}: ${review.review_type === 'pr' ? rt('reviewModePR') : rt('reviewModeBranch')}`);
  if (review.model) meta.push(`- ${rt('reviewExportMetaModel')}: ${review.model}`);
  if (review.revision_no > 0) meta.push(`- ${rt('reviewExportMetaRevision')}: v${review.revision_no}`);
  meta.push(`- ${rt('reviewExportMetaTime')}: ${formatStamp(review.finished_at || review.created_at)}`);
  parts.push(meta.join('\n'));

  if (review.description.trim()) {
    parts.push(`## ${rt('reviewDescLabel')}\n\n${review.description.trim()}`);
  }

  // The grading, restated as a table: the counts are the first thing a
  // reader of the exported file looks for, and they live in columns the
  // prose doesn't carry.
  if (review.findings_json) {
    parts.push([
      `## ${rt('reviewExportSummary')}`,
      '',
      `| ${rt('reviewExportColSeverity')} | ${rt('reviewExportColCount')} |`,
      '| --- | --- |',
      ...SEVERITIES.map(sev => `| ${sev} — ${rt(`reviewSevShort${sev}`)} | ${severityCount(review, sev)} |`),
      '',
      `${rt('reviewGoalLabel')}: ${goalLabel(review.goal_met)}`,
    ].join('\n'));
  }

  if (checks.length > 0) {
    parts.push([
      `## ${rt('reviewGoalChecksTitle')}`,
      '',
      `| ${rt('reviewGoalChecksColCriterion')} | ${rt('reviewGoalChecksColStatus')} | ${rt('reviewGoalChecksColEvidence')} |`,
      '| --- | --- | --- |',
      ...checks.map(c => `| ${cell(c.criterion)} | ${goalLabel(c.status)} | ${cell(c.evidence)} |`),
    ].join('\n'));
  }

  if (findings.length > 0) {
    parts.push(`## ${rt('reviewFindingsTitle')}`);
    for (const sev of SEVERITIES) {
      const items = findings.filter(f => f.severity === sev);
      if (items.length === 0) continue;
      parts.push(`### ${rt(`reviewSev${sev}`)}`);
      parts.push(items.map((f, i) => renderFinding(f, `${sev}-${i + 1}`)).join('\n\n'));
    }
  }

  if (review.result_md.trim()) {
    parts.push(`## ${rt('reviewFullReport')}\n\n${review.result_md.trim()}`);
  }

  const answered = followups.filter(fu => fu.status === 'done' && fu.answer.trim());
  if (includeFollowups && answered.length > 0) {
    parts.push(`## ${rt('reviewExportFollowups')}`);
    parts.push(answered.map((fu, i) =>
      `### ${i + 1}. ${fu.question.trim()}\n\n${fu.answer.trim()}`).join('\n\n'));
  }

  return parts.join('\n\n') + '\n';
}

/** One finding as a Markdown block: heading line, detail, suggestion. */
function renderFinding(f: ReviewFinding, index: string): string {
  const loc = f.file ? ` — \`${f.file}${f.line ? `:${f.line}` : ''}\`` : '';
  const lines = [`**${index} ${f.title}**${loc}`];
  if (f.detail.trim()) lines.push('', f.detail.trim());
  if (f.suggestion.trim()) lines.push('', `> ${rt('reviewFindingSuggestion')}: ${f.suggestion.trim()}`);
  return lines.join('\n');
}

function severityCount(rv: CodeReview, sev: ReviewSeverity): number {
  return sev === 'P0' ? rv.p0_count : sev === 'P1' ? rv.p1_count : rv.p3_count;
}

function goalLabel(status: string): string {
  switch (status) {
    case 'yes': return rt('reviewGoalYes');
    case 'partial': return rt('reviewGoalPartial');
    case 'no': return rt('reviewGoalNo');
    default: return rt('reviewGoalUnknown');
  }
}

/**
 * Make a free-text value safe inside a Markdown table cell: a literal `|`
 * would end the column, and a newline would end the row.
 */
function cell(s: string): string {
  return s.replace(/\|/g, '\\|').replace(/\r?\n/g, ' ').trim() || '—';
}

function formatStamp(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString(i18next.language || 'zh-CN', { hour12: false });
}

/**
 * Strip the characters a filesystem (or a browser's download handler) won't
 * take, and cap the length. Mirrors exportDesignPdf's own sanitiser so the
 * `.md` and `.pdf` of one report land next to each other alphabetically.
 */
export function reviewExportFilename(rv: CodeReview, ext: string): string {
  const stem = reviewExportTitle(rv)
    .replace(/[\\/:*?"<>|]/g, '-')
    .replace(/\s+/g, ' ')
    .trim()
    .slice(0, 80) || rt('reviewReportTitle');
  return `${stem}.${ext}`;
}

/** Download a built Markdown document as a `.md` file. */
export function downloadReviewMarkdown(input: ReviewExportInput): void {
  const markdown = buildReviewMarkdown(input);
  const blob = new Blob([markdown], { type: 'text/markdown;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = reviewExportFilename(input.review, 'md');
  document.body.appendChild(a);
  a.click();
  a.remove();
  // Revoking immediately can race the download in Safari; one tick is
  // enough and the object is small either way.
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
