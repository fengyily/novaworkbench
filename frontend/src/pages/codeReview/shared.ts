// Pieces shared by the Review tab's four panels (composer / report / chat /
// history). Kept separate from the panels themselves so none of them has to
// import another just to borrow a constant.

import type { CodeReview, ReviewSeverity } from '../../api/client';

/** Display order of the severity buckets. P2 is intentionally absent. */
export const SEVERITIES: ReviewSeverity[] = ['P0', 'P1', 'P3'];

/**
 * Headline state of a review, derived from status + counts.
 *
 *   block   — at least one P0: must not merge
 *   warn    — no P0 but some P1: merge after fixing
 *   clean   — graded, nothing blocking
 *   error   — the run itself failed
 *   pending — draft or still running, so there is no result yet
 *   plain   — the model broke the trailing-JSON contract, nothing to grade
 */
export type ReviewVerdict = 'error' | 'block' | 'warn' | 'clean' | 'pending' | 'plain';

/**
 * The one thing a reader wants before anything else: can this be merged?
 *
 * Counts come from the persisted columns rather than the decoded findings
 * array — the backend derived both from the same JSON, and the columns stay
 * meaningful (all zero) on the degraded path where findings_json is empty.
 */
export function deriveVerdict(rv: CodeReview | null): ReviewVerdict {
  if (!rv) return 'plain';
  if (rv.status === 'error') return 'error';
  // A draft picked from history has never produced a report — saying
  // "ungraded" there would read as a model failure rather than "not run".
  if (rv.status === 'draft' || rv.status === 'running') return 'pending';
  if (!rv.findings_json) return 'plain';
  if (rv.p0_count > 0) return 'block';
  if (rv.p1_count > 0) return 'warn';
  return 'clean';
}

/** TitleCase a discriminant for i18n key interpolation ('block' → 'Block'). */
export function titleCase(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/** Per-severity count off the persisted columns. */
export function severityCount(rv: CodeReview, sev: ReviewSeverity): number {
  return sev === 'P0' ? rv.p0_count : sev === 'P1' ? rv.p1_count : rv.p3_count;
}

/**
 * Collect the bodies of every ```mermaid fence in a report, in order.
 *
 * Line-based rather than regex-based because a review report is full of
 * fenced code: a regex for ```mermaid[\s\S]*?``` happily matches across a
 * Go snippet that itself contains the word, and a non-greedy match stops at
 * the first ``` inside a nested fence. Tracking fence state line by line
 * also means a ```go block mentioning mermaid is skipped outright.
 *
 * An unterminated final fence still yields its body — models truncate the
 * closing marker often enough that dropping the diagram would lose a real
 * picture (same reasoning as the backend's lastJSONFence).
 */
export function extractMermaidBlocks(md: string): string[] {
  const blocks: string[] = [];
  let current: string[] | null = null;
  let inOtherFence = false;
  for (const line of md.split('\n')) {
    const t = line.trim();
    if (current) {
      if (/^`{3,}\s*$/.test(t)) {
        if (current.join('\n').trim()) blocks.push(current.join('\n'));
        current = null;
      } else {
        current.push(line);
      }
    } else if (inOtherFence) {
      if (/^`{3,}\s*$/.test(t)) inOtherFence = false;
    } else if (/^`{3,}\s*mermaid\b/i.test(t)) {
      current = [];
    } else if (/^`{3,}/.test(t)) {
      inOtherFence = true;
    }
  }
  if (current && current.join('\n').trim()) blocks.push(current.join('\n'));
  return blocks;
}

/**
 * A draft nobody ever filled in: the auto-created row behind an abandoned
 * composer. These are a side effect of removing the manual「创建草稿」 step,
 * so the history list hides them and offers a one-click purge instead of
 * making the user delete each one.
 */
export function isBlankDraft(rv: CodeReview): boolean {
  return rv.status === 'draft'
    && !rv.result_md
    && !rv.description.trim()
    && !rv.title.trim();
}

/** One streamed SSE line as the panels render it. */
export interface StreamLine {
  type: string;
  content: string;
}
