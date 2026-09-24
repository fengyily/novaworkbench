/**
 * JobLogView — stateless SSE log renderer used by DevelopingStage.
 *
 * Historically this exact JSX block lived inside RequirementDetail.tsx's
 * "developing" branch (~lines 4212-4274) and was duplicated for every
 * sub-task card inside SubTaskPanel. After the developing-stage refactor
 * there is only ONE shared SSE log panel, so the render block is hoisted
 * here as a plain presentational component.
 *
 * Design choices:
 *   - Keeps the existing .coding-panel / .coding-line* / .coding-message-md
 *     CSS class names from RequirementDetail.css so the visual treatment
 *     does not regress. No new CSS is introduced here — see
 *     DevelopingStage.css for the surrounding layout.
 *   - Does NOT manage SSE or refresh logic — the parent (DevelopingStage)
 *     owns the stream and feeds `lines / usage / status / busy` in.
 *   - Renders a sticky header containing the ContextUsageBar so the
 *     usage bar stays visible while the user scrolls long logs.
 *   - Provides the in-panel redo affordance + fullscreen toggle. These
 *     apply only to the main task; the parent hides them for sub-task
 *     selections (sub-task Redo lives on the per-card adjust drawer).
 */

import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { useTranslation } from 'react-i18next';
import type { ReactNode } from 'react';
import type { LogLine, UsageInfo } from '../utils/logLines';
import { buildPhaseGroups, formatDuration, useTick } from '../utils/phaseGroups';
import { ContextUsageBar } from './ContextUsageBar';
import { FullscreenButton } from './FullscreenButton';
import { IconHourglass, IconRefresh } from './icons';

export type JobLogStatus = 'running' | 'done' | 'error' | 'pending' | 'idle' | 'stopped';

export interface JobLogViewProps {
  // Header text shown at the top of the panel. For the main task this is
  // the requirement title; for a sub-task it's the sub-task's title.
  title: string;
  // Cumulative log lines appended by the SSE handler. The panel does not
  // mutate this array — it just renders the current snapshot.
  lines: LogLine[];
  // Latest usage snapshot from the `usage` SSE event. undefined while the
  // first result event is still pending; the bar renders a 0% placeholder.
  usage?: UsageInfo;
  // True while the underlying SSE stream is still open / job is alive.
  // Drives the trailing phase's live-tick animation + the working-hint row.
  busy: boolean;
  // Higher-level status snapshot, used by the parent to choose which
  // affordances to show (redo / compress / etc.). The component itself
  // currently only inspects `busy` for the spinner hints; status is held
  // here for future affordances (e.g. show "completed" badge once).
  status: JobLogStatus;
  // Optional hint label shown while busy. Pass the stage name (e.g.
  // "开发实现" / "Sub-task") so the user knows which stage they're looking at.
  stepLabel?: string;
  // Optional coding-phase sentinel surfaced by the backend in plan mode
  // (one of 'planning' / 'decomposing' / ''). Used to render a more
  // specific "正在制定实施步骤…" / "正在拆分子任务…" hint instead of the
  // generic "Claude 正在工作" line. Mirrors the original
  // RequirementDetail.tsx hint block.
  codingPhase?: 'planning' | 'decomposing' | string | null;
  // Compress / show-summary controls. Mirrors the ContextUsageBar props.
  compressible?: boolean;
  compressing?: boolean;
  compressedAt?: string | null;
  onCompress?: () => void;
  onShowSummary?: () => void;
  // Redo affordance for the main task only. Hidden if not provided.
  // Sub-task Redo lives on the per-card adjust drawer instead.
  showRedo?: boolean;
  redoDisabled?: boolean;
  onRedo?: () => void;
  redoLabel?: string;
  redoTitle?: string;
  // Fullscreen toggle for the surrounding panel. The "exit" variant is a
  // floating close button rendered inside the panel while fullscreen.
  fullscreen?: { active: boolean; onToggle: () => void };
}

// PhaseBlock mirrors the same helper that lives at the top of
// RequirementDetail.tsx (~lines 326-367). We intentionally duplicate it
// here so JobLogView stays self-contained and the page-level render block
// can be retired without changing the visual treatment.
function PhaseBlock({
  group,
  working: phaseWorking,
  phaseStartIdx,
  lines,
}: {
  group: LogLine[];
  working: boolean;
  phaseStartIdx: number;
  lines: LogLine[];
}): ReactNode {
  const phases = buildPhaseGroups(group);
  const lastInOriginal = phaseStartIdx + group.length >= lines.length;
  const phase = phases[0];
  if (!phase) return null;
  const active = phaseWorking && lastInOriginal && phase.isActive;
  const displayMs = active ? Date.now() - phase.startedAt : phase.durationMs;
  return (
    <div className={`coding-line-phase-group${active ? ' active' : ''}`}>
      <div className="coding-line-phase-header">
        <span className="coding-line-phase-label">{phase.label}</span>
        <span className="coding-line-phase-time">{formatDuration(displayMs)}</span>
      </div>
      {phase.thinking && (
        <div className="coding-line coding-line-phase">{phase.thinking.content}</div>
      )}
      {phase.toolCalls.map((tc, j) => (
        <div key={j} className="coding-line coding-line-tool_call">
          <span>{tc.content}</span>
          {tc.durationMs != null && tc.durationMs > 0 && (
            <span className="coding-line-entry-time">
              {' · '}{formatDuration(tc.durationMs)}
            </span>
          )}
        </div>
      ))}
    </div>
  );
}

// CodingLines — extracted from RequirementDetail.tsx (lines 259-321). Kept
// here so JobLogView can render a fully-saved session after a page refresh
// without depending on the page-level component. Pure presentational; the
// parent is expected to coalesce consecutive phase/tool_call rows before
// passing them in (we still walk the array defensively, mirroring the
// upstream behavior).
function CodingLines({ lines, working }: { lines: LogLine[]; working?: boolean }) {
  useTick(!!working);
  const nodes: ReactNode[] = [];
  let key = 0;
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (line.type === 'phase') {
      const phaseStart = i;
      const group: LogLine[] = [];
      while (i < lines.length && lines[i].type === 'phase') {
        group.push(lines[i]);
        i++;
      }
      while (i < lines.length && lines[i].type === 'tool_call') {
        group.push(lines[i]);
        i++;
      }
      nodes.push(
        <PhaseBlock
          key={key++}
          group={group}
          working={!!working}
          phaseStartIdx={phaseStart}
          lines={lines}
        />,
      );
      continue;
    }
    if (line.type === 'message') {
      const group: string[] = [];
      while (i < lines.length && lines[i].type === 'message') {
        group.push(lines[i].content);
        i++;
      }
      nodes.push(
        <div key={key++} className="coding-line coding-line-message coding-message-md">
          <ReactMarkdown remarkPlugins={[remarkGfm]}>{group.join('')}</ReactMarkdown>
        </div>,
      );
      continue;
    }
    if (line.type === 'result') {
      nodes.push(
        <div key={key++} className="coding-line coding-line-message coding-message-md">
          <ReactMarkdown remarkPlugins={[remarkGfm]}>{line.content}</ReactMarkdown>
        </div>,
      );
      i++;
      continue;
    }
    nodes.push(
      <div key={key++} className={`coding-line coding-line-${line.type}`}>{line.content}</div>,
    );
    i++;
  }
  return <>{nodes}</>;
}

export function JobLogView({
  lines,
  usage,
  busy,
  status,
  stepLabel,
  codingPhase,
  compressible = true,
  compressing,
  compressedAt,
  onCompress,
  onShowSummary,
  showRedo,
  redoDisabled,
  onRedo,
  redoLabel,
  redoTitle,
  fullscreen,
}: JobLogViewProps) {
  const { t } = useTranslation();
  const fsActive = fullscreen?.active ?? false;
  // The two hints below mirror the rendering that used to live at
  // RequirementDetail.tsx:4238-4247 (working hint + coding-phase sentinel)
  // and the Recovery Redo button at 4261-4272.
  const showWorkingHint = busy || (!!codingPhase && codingPhase !== '');
  return (
    <div className={`coding-panel ${fsActive ? 'is-fullscreen' : ''}`}>
      {fsActive && fullscreen && (
        <FullscreenButton isFullscreen onClick={fullscreen.onToggle} variant="floating" />
      )}
      {stepLabel && (
        <ContextUsageBar
          usage={usage}
          onCompress={onCompress ?? (() => {})}
          compressible={compressible}
          compressing={compressing}
          stepLabel={stepLabel}
          compressedAt={compressedAt}
          onShowSummary={onShowSummary}
        />
      )}
      <CodingLines lines={lines} working={busy} />
      {showWorkingHint && (
        <div className="coding-line coding-line-tool_call">
          <IconHourglass size={12} className="icon-mr" />
          {codingPhase === 'planning'
            ? t('requirements.detail2.codingPhasePlanning')
            : codingPhase === 'decomposing'
              ? t('requirements.detail2.codingPhaseDecomposing')
              : t('requirements.detail2.codingWorkingHint')}
        </div>
      )}
      {showRedo && !busy && status === 'error' && (
        <div style={{ display: 'flex', gap: 8, marginTop: 12, flexWrap: 'wrap' }}>
          <button
            className="btn btn-primary"
            title={redoTitle}
            onClick={onRedo}
            disabled={!!redoDisabled}
          >
            <IconRefresh size={13} className="btn-icon" />{redoLabel ?? t('requirements.detail2.codingRedoBtn')}
          </button>
        </div>
      )}
    </div>
  );
}

export default JobLogView;