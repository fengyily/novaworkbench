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
}

const THINKING_PREFIX = '🤔 模型思考中';

/** True for the heartbeat "🤔 模型思考中… (N tokens)" phase lines. */
export function isThinkingPhase(line: LogLine | undefined): boolean {
  return !!line && line.type === 'phase' && line.content.startsWith(THINKING_PREFIX);
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
