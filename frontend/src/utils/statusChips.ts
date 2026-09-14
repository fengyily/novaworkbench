// statusChips — derive the combined-status badge chips shown on the
// requirements lists from a Requirement's existing fields.
//
// The legacy single-badge model (`status: string` + statusLabelKeys) was
// fine while statuses were linear. Real development is non-linear: a
// `designing` requirement that has already produced a design doc still
// needs human confirmation; a `developing` requirement on the "直接开发"
// path reads differently from one that went through the architect stage.
// This module expresses those combinations as a small ordered list of
// chips, each chip being just (code, toneClass, labelKey) so the
// presentational concern stays in the React component.
//
// Design rule: derived from the four existing fields only — no new
// state. A `designing` row that hasn't produced a design doc stays on
// "📐 方案设计中" (avoids claiming "方案完成" prematurely); once
// `design_docs` parses to a valid design (object / legacy array / raw
// markdown), the row upgrades to "方案完成 + 待确认".
//
// Mapping table (status × skip_design × hasDesign × devComplete →
// ordered chips, fixed order [来源/方案] → [进度] → [待确认]):
//
//   status      skip  hasD  devC  chips
//   ---------   ----  ----  ----  -------------------------------------
//   draft       F     -     -     draft
//   draft       T     -     -     draft, skipDesign
//   analyzing   -     -     -     analyzing
//   designing   -     F     -     designing
//   designing   -     T     -     designed, pendingConfirm
//   designed    -     -     -     designed
//   developing  F     -     F     designed, developing
//   developing  F     -     T     designed, devDone, pendingConfirm
//   developing  T     -     F     skipDesign, developing
//   developing  T     -     T     skipDesign, devDone, pendingConfirm
//   done        F     -     -     designed, devDone
//   done        T     -     -     skipDesign, devDone
//   archived    -     -     -     archived (terminal, never combined)
//   (other)     -     -     -     single fallback chip
//
// devComplete = status === 'developing' && !!dev_ended_at. dev_ended_at
// is a *time.Time the backend fills via a subquery against sub_tasks
// when every sub-task is closed; empty / null / undefined all mean "not
// done yet".
//
// hasDesignDocs replicates the three-shape parse that
// pages/RequirementDetail.tsx's parseDesign performs: legacy JSON arrays,
// {plan_markdown, overview, steps, ...} objects, and raw markdown
// (including a single outer ``` fence). The three shapes cover every
// design doc that has ever been persisted in the `requirements` table.

export type ChipCode =
  | 'skipDesign'
  | 'devDone'
  | 'pendingConfirm'
  | 'draft'
  | 'analyzing'
  | 'designing'
  | 'designed'
  | 'developing'
  | 'done'
  | 'archived';

export interface StatusChip {
  code: string;
  toneClass: string;
  labelKey: string;
}

// toneClass reuses the existing .status-badge.status-* palette in
// index.css (lines ~482-488). Reusing instead of inventing new classes
// keeps the chip visually consistent with the legacy single-badge look:
//   skipDesign     → status-draft       (slate — "绕过")
//   designed       → status-designed    (blue)
//   developing     → status-developing  (cyan)
//   devDone        → status-done        (emerald)
//   pendingConfirm → status-analyzing   (amber — reuses "需关注" warm tone)
const TONE: Record<ChipCode, string> = {
  draft: 'status-draft',
  analyzing: 'status-analyzing',
  designing: 'status-designing',
  designed: 'status-designed',
  developing: 'status-developing',
  done: 'status-done',
  archived: 'status-archived',
  skipDesign: 'status-draft',
  devDone: 'status-done',
  pendingConfirm: 'status-analyzing',
};

// ── parseDesign parity ───────────────────────────────────────────────────
// pages/RequirementDetail.tsx ships a React-bound parseDesign helper that
// branches three ways. We re-implement the same branch here so this
// utils module stays framework-free (no React import) yet agrees with
// the detail-page "方案已完成 · 待确认" treatment on every shape.

interface DesignShape {
  plan_markdown?: unknown;
  overview?: unknown;
  files?: unknown;
  steps?: unknown;
  model_changes?: unknown;
  risks?: unknown;
}

function isDesignShape(v: unknown): v is DesignShape {
  if (!v || typeof v !== 'object' || Array.isArray(v)) return false;
  const o = v as DesignShape;
  return (
    typeof o.plan_markdown === 'string' ||
    o.overview !== undefined ||
    o.files !== undefined ||
    o.steps !== undefined ||
    o.model_changes !== undefined ||
    o.risks !== undefined
  );
}

/**
 * Returns true when `designDocs` contains a real design — either a
 * parsed {plan_markdown, overview, steps, ...} object, a non-empty
 * legacy JSON array, or raw markdown with at least one non-whitespace
 * character. Mirrors the truthiness gate
 * `design.overview || design.steps.length || design.plan_markdown`
 * that pages/RequirementDetail.tsx uses to decide whether to render the
 * design panel.
 */
export function hasDesignDocs(designDocs?: string | null): boolean {
  if (!designDocs) return false;
  const trimmed = designDocs.trim();
  if (!trimmed) return false;
  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch {
    // Not JSON → treat as raw markdown. Non-whitespace content counts.
    return true;
  }
  if (Array.isArray(parsed)) {
    // Legacy shape: ["# 方案\n## 详情", ...]. parseDesign picks the
    // first non-empty string and returns it as plan_markdown; if that
    // first string is non-empty, the detail page's hasDesign is true.
    return parsed.some((v) => typeof v === 'string' && v.trim() !== '');
  }
  if (isDesignShape(parsed)) {
    // Mirror the detail-page gate exactly: plan_markdown, overview, or
    // any steps entry wins. files / model_changes / risks are
    // intentionally NOT counted — the detail page ignores them, and a
    // doc with only those empty arrays would render as an empty design
    // panel that we shouldn't claim is "已完成".
    if (typeof parsed.plan_markdown === 'string' && parsed.plan_markdown.trim() !== '') return true;
    if (typeof parsed.overview === 'string' && parsed.overview.trim() !== '') return true;
    if (Array.isArray(parsed.steps) && parsed.steps.length > 0) return true;
    return false;
  }
  // JSON parsed but shape is something exotic (e.g. just a string, or a
  // number). parseDesign on the detail page wraps it as
  // { plan_markdown: stripOuterFence(raw) }; if raw was non-empty,
  // hasDesign would have been true. We mirror that here.
  return trimmed.length > 0;
}

export interface StatusChipsInput {
  status: string;
  skip_design?: boolean;
  design_docs?: string | null;
  dev_ended_at?: string | null;
}

function make(code: ChipCode, labelKey: string): StatusChip {
  return { code, toneClass: TONE[code], labelKey };
}

/**
 * Returns the ordered list of status chips for `req`.
 *
 * - Order is fixed as [来源/方案 chip] → [进度 chip] → [待确认 chip] so
 *   a reader's eye follows the same path on every row.
 * - Unknown `status` values collapse to a single fallback chip whose
 *   labelKey points at `status.req.<code>` — i18n's tLabel falls back
 *   to the raw code when the key isn't registered, so the chip never
 *   renders `undefined`.
 * - Empty `status` returns an empty array (no chip rendered).
 * - `archived` is terminal and never combined.
 */
export function statusChips(req: StatusChipsInput): StatusChip[] {
  const status = (req.status || '').toLowerCase();
  const skip = !!req.skip_design;
  const devComplete = status === 'developing' && !!req.dev_ended_at;

  switch (status) {
    case 'draft':
      return skip
        ? [make('draft', 'status.req.draft'), make('skipDesign', 'status.reqChip.skipDesign')]
        : [make('draft', 'status.req.draft')];

    case 'analyzing':
      return [make('analyzing', 'status.req.analyzing')];

    case 'designing':
      return hasDesignDocs(req.design_docs)
        ? [make('designed', 'status.req.designed'), make('pendingConfirm', 'status.reqChip.pendingConfirm')]
        : [make('designing', 'status.req.designing')];

    case 'designed':
      return [make('designed', 'status.req.designed')];

    case 'developing':
      if (skip) {
        return devComplete
          ? [
              make('skipDesign', 'status.reqChip.skipDesign'),
              make('devDone', 'status.reqChip.devDone'),
              make('pendingConfirm', 'status.reqChip.pendingConfirm'),
            ]
          : [make('skipDesign', 'status.reqChip.skipDesign'), make('developing', 'status.req.developing')];
      }
      return devComplete
        ? [
            make('designed', 'status.req.designed'),
            make('devDone', 'status.reqChip.devDone'),
            make('pendingConfirm', 'status.reqChip.pendingConfirm'),
          ]
        : [make('designed', 'status.req.designed'), make('developing', 'status.req.developing')];

    case 'done':
      return skip
        ? [make('skipDesign', 'status.reqChip.skipDesign'), make('devDone', 'status.reqChip.devDone')]
        : [make('designed', 'status.req.designed'), make('devDone', 'status.reqChip.devDone')];

    case 'archived':
      return [make('archived', 'status.req.archived')];

    default: {
      const raw = req.status || '';
      if (!raw) return [];
      return [{ code: raw, toneClass: 'status-draft', labelKey: `status.req.${raw}` }];
    }
  }
}