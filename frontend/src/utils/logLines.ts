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
 * (`used`) are pre-computed server-side from input_tokens + cache_creation +
 * cache_read against `context_window`, so the UI doesn't need to know the
 * denominator — just clamp `pct` to 0..100 and pick a color band.
 */
export interface UsageInfo {
  step: 'analyst_chat' | 'architect_design' | 'coding' | 'adjust_coding' | string;
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_creation_tokens: number;
  cache_read_tokens: number;
  context_window: number;
  /** input + cache_creation + cache_read — the input-side cost of this turn. */
  used: number;
  /** used / context_window * 100, may exceed 100 if cache_read dominates. */
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

/** Clamp a raw pct into [0, 100] for bar width. Original pct (which may
 *  exceed 100 when cache_read dominates) is still shown in tooltips. */
export function clampPct(pct: number): number {
  if (!Number.isFinite(pct) || pct < 0) return 0;
  if (pct > 100) return 100;
  return pct;
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

/** A raw usage snapshot as stored in requirements.usage_snapshots (no `step`
 *  / `used` / `pct` — those are derived). Matches what the backend persists. */
export interface UsageSnapshot {
  model?: string;
  input_tokens?: number;
  output_tokens?: number;
  cache_creation_tokens?: number;
  cache_read_tokens?: number;
  context_window?: number;
}

export type SessionKey = 'analyst_chat' | 'architect_design' | 'coding';

/** Turn a raw snapshot (from the persisted blob OR a `usage` SSE payload that
 *  lacked used/pct) into a full UsageInfo by computing used + pct client-side.
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
  const used = input + cc + cr;
  if (used <= 0 && out <= 0) return undefined;
  const cw = raw.context_window || 200000;
  const pct = cw > 0 ? (used / cw) * 100 : 0;
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
