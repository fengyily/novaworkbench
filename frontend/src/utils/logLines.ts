// Coalescing for the "🤔 模型思考中… (N tokens)" heartbeat phase lines.
//
// The backend (wizard.go runClaudeStream) emits a `phase` LogLine every ~50
// thinking tokens so the user sees live activity from proxy models that batch
// their text instead of streaming token-by-token. Each emission is a distinct
// LogLine, and the JobStore ring buffer is append-only — so both the live SSE
// stream and the full-history replay on reconnect would stack a wall of
// near-duplicate "模型思考中… (N tokens)" rows.
//
// These helpers collapse consecutive thinking phase lines into a single
// updatable line: only the latest token count is kept. Use appendLogLine() in
// streaming onmessage handlers and coalesceLogLines() when restoring a full
// log snapshot (job replay on reconnect / refresh).

export interface LogLine {
  type: string;
  content: string;
  at?: number; // Unix ms; 后端自动注入；老数据/直连 SSE 兜底缺省
  // Usage snapshot emitted by wizard.go runClaudeStream's `usage` event.
  // The Content field carries the same JSON as a string for backwards
  // compatibility with callers that only read `type + content`; the parsed
  // form is exposed here for components that want to render a live usage bar
  // without re-parsing on every frame. Either may be absent for non-usage
  // events.
  usage?: UsageInfo;
}

/**
 * Live token-usage snapshot for one wizard turn, emitted by the backend via
 * the `usage` SSE event. The percentage (`pct`) and absolute used-token count
 * (`used`) are pre-computed server-side from input_tokens + cache_creation
 * against `context_window`, so the UI doesn't need to know the denominator —
 * just clamp `pct` to 0..100 and pick a color band.
 *
 * Why cache_read_tokens is NOT part of `used`:
 *   `cache_read_tokens` represents tokens the model READ from an existing
 *   prompt cache — i.e. content that was already counted as `cache_creation`
 *   in a previous turn. Anthropic charges for it (cheap reuse) but it does
 *   NOT add to the context window's fresh fill. Including it in `used`
 *   produces pct > 100% in two ways:
 *     (a) per-turn: a prompt with multiple large cache breakpoints can
 *         report cr alone approaching the window, pushing cc + cr past it;
 *     (b) cumulative: SubTaskService.Finish SUMs cache_read across turns,
 *         so a sub-task with N cache-hit turns double-counts the same
 *         cached prefix N times. The SubTaskPanel persistent fallback
 *         would render pct ≫ 100% on any multi-turn sub-task that hit cache.
 *   Hence `used = input + cache_creation`. cache_read is still surfaced in
 *   the breakdown tooltip for transparency.
 */
export interface UsageInfo {
  step: 'analyst_chat' | 'architect_design' | 'coding' | 'adjust_coding' | string;
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_creation_tokens: number;
  cache_read_tokens: number;
  context_window: number;
  /** input + cache_creation — the net new context fill (cache read excluded,
   *  see interface docstring). */
  used: number;
  /** used / context_window * 100. Mathematically ≤ 100 in normal use; capped
   *  at 99% by the display layer when raw pct reaches/exceeds 100 to avoid
   *  showing the user "150%" (which would read as a bug). The raw pct is
   *  still exposed in tooltips. */
  pct: number;
}

const THINKING_PREFIX = '🤔 模型思考中';

/** True for the heartbeat "🤔 模型思考中… (N tokens)" phase lines. */
export function isThinkingPhase(line: LogLine | undefined): boolean {
  return !!line && line.type === 'phase' && line.content.startsWith(THINKING_PREFIX);
}

// ── Shared usage-bar helpers ────────────────────────────────────────────
// Used by ContextUsageBar (in-panel bars) and the SessionContextStrip (top
// always-on strip) so both render the same color thresholds / number format.

/** Clamp a raw pct into [0, 100] for bar width. With the new computeUsage
 *  formula (cache_read excluded from used) the raw pct should never reach
 *  100 in normal operation; the clamp is a defensive belt-and-braces for
 *  any legacy persisted blob or upstream caller that still produces pct>100.
 *  Original pct (which could still exceed 100 on legacy data) is shown in
 *  the breakdown tooltip. */
export function clampPct(pct: number): number {
  if (!Number.isFinite(pct) || pct < 0) return 0;
  if (pct > 100) return 100;
  return pct;
}

/** True when the raw pct has reached/exceeded the context window. The bar
 *  display layer uses this to render "99%+" instead of e.g. "108%" so the
 *  user never sees a percentage above 100% — anything beyond means the
 *  prompt itself overflowed the window, which Anthropic reports as an error
 *  anyway. Defensive guard for legacy persisted blobs only. */
export function isPctOverLimit(pct: number): boolean {
  return Number.isFinite(pct) && pct >= 100;
}

/** Format a compact, human-readable breakdown string for the usage bar
 *  tooltip — input / cache_creation / cache_read / window. The caller
 *  passes an i18n template so we keep i18n ownership in the components layer. */
export interface UsageBreakdown {
  input: number;
  cache_creation: number;
  cache_read: number;
  used: number;
  window: number;
  pct: number;
}
export function usageBreakdown(u: UsageInfo | undefined): UsageBreakdown | undefined {
  if (!u) return undefined;
  return {
    input: u.input_tokens,
    cache_creation: u.cache_creation_tokens,
    cache_read: u.cache_read_tokens,
    used: u.used,
    window: u.context_window,
    pct: u.pct,
  };
}

/** Compact token-count format: 123456 → "123K" / "1.2M". Keeps the bar header
 *  from being pushed around by 6-digit numbers. */
export function formatTokens(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0';
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(0)}K`;
  return String(n);
}

/** Color-band class for a clamped pct. Thresholds 80 / 95 — a haptic nudge,
 *  never a block. */
export function bandClass(pct: number): string {
  if (pct >= 95) return 'usage-bar-band usage-bar-band-critical';
  if (pct >= 80) return 'usage-bar-band usage-bar-band-warn';
  return 'usage-bar-band usage-bar-band-ok';
}

/** A raw usage snapshot as stored in requirements.usage_snapshots, OR as
 *  received from the backend's `usage` SSE payload. The backend (handler/
 *  wizard_stream.go::usagePayload) is now the single source of truth for
 *  the "cache_read excluded from used" semantics, so it pre-fills `used`
 *  and `pct` on the wire. Legacy blobs persisted before that change (and
 *  the sub_tasks-column fallback path, which doesn't go through the SSE
 *  pipeline) lack these fields — computeUsage() will recompute them
 *  client-side using the same formula as the backend.
 *
 *  Mirroring rule: when present, `used` and `pct` MUST equal
 *      used = input + cache_creation
 *      pct  = used / context_window * 100
 *  Any divergence here vs. backend's usagePayload is a bug — keep them in
 *  sync when the formula evolves. */
export interface UsageSnapshot {
  model?: string;
  input_tokens?: number;
  output_tokens?: number;
  cache_creation_tokens?: number;
  cache_read_tokens?: number;
  context_window?: number;
  /** Server-computed (preferred when present). Falls back to client-side
   *  recomputation when undefined. */
  used?: number;
  /** Server-computed percentage (preferred when present). */
  pct?: number;
}

export type SessionKey = 'analyst_chat' | 'architect_design' | 'coding';

/** Turn a raw snapshot (from the persisted blob OR a `usage` SSE payload)
 *  into a full UsageInfo. When the backend has pre-filled `used` and `pct`
 *  (the live SSE path) we trust those values verbatim — they're the single
 *  source of truth for "cache_read excluded from used". When the raw input
 *  is a legacy blob or a sub_tasks-column fallback (no `used`/`pct`), we
 *  recompute using the same formula as the backend.
 *
 *  Returns undefined when the snapshot carries no token counts. */
export function computeUsage(
  raw: UsageSnapshot | Partial<UsageInfo> | undefined,
  step: SessionKey | string,
): UsageInfo | undefined {
  if (!raw) return undefined;
  const input = raw.input_tokens ?? 0;
  const cc = raw.cache_creation_tokens ?? 0;
  const cr = raw.cache_read_tokens ?? 0;
  const out = raw.output_tokens ?? 0;
  const cw = raw.context_window || 200000;
  // Prefer server-computed values when present — the backend has already
  // applied the "cache_read excluded" rule and is the single source of
  // truth for the percentage formula. Recomputing client-side would risk
  // drift if the formula evolves.
  const used =
    typeof raw.used === 'number'
      ? raw.used
      : input + cc; // see UsageSnapshot docstring — matches usagePayload()
  const pct =
    typeof raw.pct === 'number'
      ? raw.pct
      : cw > 0 ? (used / cw) * 100 : 0;
  if (used <= 0 && out <= 0) return undefined;
  return {
    step: step as UsageInfo['step'],
    model: raw.model ?? '',
    input_tokens: input,
    output_tokens: out,
    cache_creation_tokens: cc,
    cache_read_tokens: cr,
    context_window: cw,
    used,
    pct,
  };
}

/** Parse the requirements.usage_snapshots JSON blob into a per-session map of
 *  UsageInfo. Tolerant of empty / malformed / legacy blobs (returns {}). Each
 *  entry's `used` + `pct` are computed here so the bars can render without a
 *  server round-trip. */
export function parseUsageSnapshots(
  json: string | undefined | null,
): Partial<Record<SessionKey, UsageInfo>> {
  if (!json) return {};
  let parsed: Record<string, UsageSnapshot>;
  try {
    parsed = JSON.parse(json);
  } catch {
    return {};
  }
  if (!parsed || typeof parsed !== 'object') return {};
  const out: Partial<Record<SessionKey, UsageInfo>> = {};
  (['analyst_chat', 'architect_design', 'coding'] as SessionKey[]).forEach((key) => {
    const u = computeUsage(parsed[key], key);
    if (u) out[key] = u;
  });
  return out;
}

/**
 * Append a log line, coalescing consecutive thinking-tokens phase lines into a
 * single updatable line — the feed shows one heartbeat that ticks up instead
 * of a stack of duplicate rows.
 */
export function appendLogLine<T extends LogLine>(lines: T[], line: T): T[] {
  if (isThinkingPhase(line) && isThinkingPhase(lines[lines.length - 1])) {
    return [...lines.slice(0, -1), line];
  }
  return [...lines, line];
}

/**
 * Collapse runs of thinking-tokens phase lines to just the last of each run.
 * Used when restoring a full log snapshot (job replay on reconnect) so stale
 * heartbeat lines don't restack.
 */
export function coalesceLogLines<T extends LogLine>(lines: T[]): T[] {
  const out: T[] = [];
  for (const l of lines) {
    if (isThinkingPhase(l) && isThinkingPhase(out[out.length - 1])) {
      out[out.length - 1] = l;
    } else {
      out.push(l);
    }
  }
  return out;
}
