/**
 * SessionContextStrip — a compact, always-visible row showing each wizard
 * session's context-window fill (analyst / design / coding) at the top of
 * the requirement detail page.
 *
 * Why this exists separately from the in-panel ContextUsageBar:
 *   - ContextUsageBar lives inside each chat / process panel, which COLLAPSES
 *     on stage completion (design panel) or empties on refresh (coding panel).
 *     So "分析完就不展示了" — the bar vanished the moment the user finished
 *     a stage. The strip is rendered in the detail header, outside any panel,
 *     so it stays put across stage transitions, panel collapse, and refresh.
 *
 * Data model — context usage is a SESSION attribute, not a UI-mount attribute:
 *   - On load, RequirementDetail seeds the three values from the persisted
 *     requirements.usage_snapshots blob, so the strip shows the real last-
 *     known fill immediately instead of 0%.
 *   - During a turn, each `usage` SSE event lifts the live value up to
 *     RequirementDetail (the parent owns the state), which feeds it back in
 *     here — so the strip ticks live, in lockstep with the in-panel bar.
 *
 * Visibility per segment: shown when the stage has ever had activity — a live
 * usage snapshot, a non-empty session_id, OR a compressed_at timestamp.
 * "📦 已压缩" replaces the bar when the session was compressed (the session
 * id is cleared on compress, so there's no fill to show — just the badge).
 */
import type { Requirement } from '../api/client';
import type { UsageInfo } from '../utils/logLines';
import { clampPct, bandClass, formatTokens } from '../utils/logLines';

export interface SessionContextStripProps {
  analyst?: UsageInfo;
  design?: UsageInfo;
  coding?: UsageInfo;
  /** The requirement row — used for session_id / compressed_at visibility. */
  req?: Requirement | null;
}

interface Segment {
  key: string;
  label: string;
  usage?: UsageInfo;
  compressedAt?: string | null;
  sessionId?: string;
}

export function SessionContextStrip({ analyst, design, coding, req }: SessionContextStripProps) {
  const segments: Segment[] = [
    { key: 'analyst', label: '分析师', usage: analyst, compressedAt: req?.analyst_compressed_at, sessionId: req?.analysis_session_id },
    { key: 'design', label: '方案', usage: design, compressedAt: req?.design_compressed_at, sessionId: req?.design_session_id },
    { key: 'coding', label: '开发', usage: coding, compressedAt: req?.coding_compressed_at, sessionId: req?.coding_session_id },
  ];

  // Only render segments that have ever seen activity. A stage with no
  // snapshot, no session id, and no compression record is one the user
  // hasn't reached yet — showing it as "—" would be noise.
  const visible = segments.filter(
    s => s.usage || s.compressedAt || s.sessionId,
  );
  if (visible.length === 0) return null;

  return (
    <div className="session-context-strip" role="group" aria-label="会话上下文使用量">
      {visible.map(seg => {
        // Compressed → badge only, no bar (the session was cleared, so the
        // fill is meaningless; the badge points the user at the summary).
        if (seg.compressedAt && !seg.usage) {
          return (
            <div key={seg.key} className="session-context-seg session-context-compressed" title={`${seg.label}阶段已压缩`}>
              <span className="session-context-label">{seg.label}</span>
              <span className="session-context-badge">📦 已压缩</span>
            </div>
          );
        }
        const u = seg.usage;
        const used = u?.used ?? 0;
        const window = u?.context_window ?? 0;
        const pct = u?.pct ?? 0;
        const widthPct = clampPct(pct);
        return (
          <div key={seg.key} className="session-context-seg" title={`${seg.label}: ${used} / ${window} tokens(原始 ${pct.toFixed(1)}%)`}>
            <span className="session-context-label">{seg.label}</span>
            <span className="session-context-track" aria-hidden="true">
              <span className={bandClass(pct)} style={{ width: `${widthPct}%` }} />
            </span>
            <span className={`session-context-pct ${pct >= 95 ? 'session-context-pct-critical' : pct >= 80 ? 'session-context-pct-warn' : ''}`}>
              {pct > 0 ? `${pct.toFixed(0)}%` : '—'}
            </span>
            <span className="session-context-used">{formatTokens(used)}/{window ? formatTokens(window) : '?'}</span>
          </div>
        );
      })}
    </div>
  );
}

export default SessionContextStrip;
