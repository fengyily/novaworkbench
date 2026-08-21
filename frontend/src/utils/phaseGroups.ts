import { useEffect, useState } from 'react';
import {
  coalesceLogLines,
  isThinkingPhase,
  type LogLine,
} from './logLines';

/**
 * One tool invocation rendered under a phase.
 * - `content` is the human-readable label produced by the backend (already
 *   emoji-prefixed and Chinese-localized via toolCallLabel).
 * - `at` is the Unix ms timestamp the backend stamped when it emitted the
 *   event; `durationMs` is filled in post-hoc from the gap to the next
 *   sibling event (or to the phase's end).
 */
export interface ToolCall {
  content: string;
  at: number;
  durationMs?: number;
}

/**
 * A phase = one named step the user can see in the log:
 *   "🤖 Claude 已连接，正在思考…"
 *   "📖 正在预读项目上下文..."
 *   "⚡ 执行命令: go build"
 *   etc.
 *
 * Phases are bounded by `phase` events in the log line stream. A phase stays
 * open (isActive=true) until the next phase starts or the job finishes.
 */
export interface Phase {
  label: string;
  startedAt: number;
  finishedAt: number;
  durationMs: number;
  toolCalls: ToolCall[];
  isActive: boolean;
  // The "🤔 模型思考中… (N tokens)" heartbeat, when present, is rendered
  // as an italic sub-row under its parent phase (not as its own phase).
  thinking?: { content: string; at: number };
}

/**
 * Group a flat log line stream into named phases. Steps:
 *   1. coalesceLogLines() — collapse runs of thinking-tokens heartbeats into
 *      one entry each (the latest token count wins).
 *   2. Walk the coalesced stream: every `phase` opens a new Phase and closes
 *      the previous one at this phase's `at` (zero-gap for back-to-back phases).
 *   3. The trailing phase is marked `isActive`; its `finishedAt` is the last
 *      event's `at` (or `now` if the log is empty).
 *   4. Backfill each ToolCall's `durationMs` from the gap to the next event.
 */
export function buildPhaseGroups(
  lines: LogLine[],
  now: number = Date.now(),
): Phase[] {
  const coalesced = coalesceLogLines(lines);
  const phases: Phase[] = [];
  let current: Phase | null = null;

  const close = (endAt: number) => {
    if (!current) return;
    current.finishedAt = endAt;
    current.durationMs = Math.max(0, endAt - current.startedAt);
    current.isActive = false;
    current = null;
  };

  for (const l of coalesced) {
    if (l.type === 'phase') {
      // Close the previous phase at THIS event's timestamp. For two phases
      // emitted back-to-back this gives a zero-second gap, which matches the
      // visible experience (one finishes, the next begins in the same frame).
      close(l.at ?? now);
      const isThinking = isThinkingPhase(l);
      phases.push({
        label: isThinking ? '(thinking)' : l.content,
        startedAt: l.at ?? now,
        finishedAt: l.at ?? now,
        durationMs: 0,
        toolCalls: [],
        isActive: true,
        ...(isThinking
          ? { thinking: { content: l.content, at: l.at ?? now } }
          : {}),
      });
      current = phases[phases.length - 1];
      continue;
    }
    if (l.type === 'tool_call') {
      // Orphan tool_call before any phase — skip rather than synthesize a
      // phase for it (the backend always emits a "🤖 Claude 已连接..." phase
      // before any tool_call, so this should be unreachable in practice).
      if (!current) continue;
      current.toolCalls.push({ content: l.content, at: l.at ?? now });
    }
  }

  // Close the trailing active phase. Use the last coalesced event's `at` (or
  // `now` if the log was empty) so the duration reflects real elapsed time
  // up to this point. Mark `isActive` so the UI shows a live ticking label.
  if (current) {
    const lastAt =
      coalesced.length > 0
        ? (coalesced[coalesced.length - 1].at ?? now)
        : now;
    current.finishedAt = lastAt;
    current.durationMs = Math.max(0, lastAt - current.startedAt);
    current.isActive = true;
  }

  // Backfill per-tool-call duration. The last tool call's gap runs to the
  // phase's finishedAt (which is still ticking if the phase is active).
  for (const p of phases) {
    const endAt = p.isActive ? Date.now() : p.finishedAt;
    for (let i = 0; i < p.toolCalls.length; i++) {
      const next = p.toolCalls[i + 1];
      const nextAt = next?.at ?? endAt;
      p.toolCalls[i].durationMs = Math.max(0, nextAt - p.toolCalls[i].at);
    }
  }

  return phases;
}

/**
 * Compact human-readable duration:
 *   < 1s  → "487ms"
 *   < 1m  → "12.3s"
 *   >= 1m → "1m 23s"
 */
export function formatDuration(ms: number): string {
  if (ms < 0) ms = 0;
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const s = Math.round(ms / 100) / 10;
  if (s < 60) return `${s.toFixed(1)}s`;
  const m = Math.floor(s / 60);
  const rest = Math.round(s - m * 60);
  return `${m}m ${rest}s`;
}

/**
 * Tick counter that increments every `intervalMs` while `active` is true.
 * Components call this and include the returned number in their render so the
 * active phase's live duration re-renders without each component having to
 * wire its own setInterval. The counter value itself is unused — only the
 * re-render matters.
 */
export function useTick(active: boolean, intervalMs = 500): number {
  const [, setN] = useState(0);
  useEffect(() => {
    if (!active) return;
    const t = setInterval(() => setN((x) => x + 1), intervalMs);
    return () => clearInterval(t);
  }, [active, intervalMs]);
  return 0;
}
