import { useCallback, useEffect, useRef, useState } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { useTranslation } from 'react-i18next';
import i18next from 'i18next';
import {
  subTasksApi,
  subTaskCliCommand,
  subTaskAdjustCommand,
  claudeApi,
  claudeSettingsPrefix,
  DefaultModelLabel,
  type SubTask,
  type SubTaskStatus,
  type Requirement,
  type OrchestrationBatch,
} from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import { appendLogLine, computeUsage, type LogLine, type UsageInfo } from '../utils/logLines';
import { modelContextWindow } from '../utils/modelWindow';
import AtMentionTextarea from './AtMentionTextarea';
import ModelSelect from './ModelSelect';
import ContextUsageBar from './ContextUsageBar';
import { IconRobot, IconDashboard, IconSparkles, IconCopy, IconCheck } from './icons';
import './SubTaskPanel.css';

// The header-right quickstats block (cost + ⏱) reads the persisted
// sub_tasks fields directly without a usageApi round-trip. The live
// context-usage bar lives in its own row just under the header (see
// `<div className="sub-card-usage">` below) and is fed by:
//   - SSE `usage` frames (step="sub_task") during a live run,
//   - a client-side recompute from sub_tasks.*_tokens columns otherwise.

interface Props {
  requirementId: string;
  // Empty when the developer stage hasn't run yet — render the
  // pre-flight hint instead of the create form.
  codingSessionId: string;
  requirement: Requirement;
  // Optional callback fired after each successful list fetch with the
  // current item count. Lets the parent hide the requirement-level
  // "Follow-up" composer the moment this panel shows at least one child,
  // without waiting for the next refetch. The parent should pass a
  // stable setter (useState's setState) so this callback reference is
  // stable across re-renders — otherwise the panel's useEffect would
  // re-fire and cause an infinite loop. The panel only re-emits when
  // the count changes, so the parent never sees a redundant call.
  onSubTasksChange?: (count: number) => void;
  // Effective developer-stage model id (role default → active config
  // default). Shown beside the DefaultModelLabel sentinel in every
  // per-stage picker so the user sees the model that will actually be
  // dispatched when they leave the picker on the default. Required for
  // the new create / adjust / re-split pickers; the panel falls back to
  // "" if omitted (legacy callers / tests).
  developerDefaultModel?: string;
  // Current orchestration batch for this requirement (new restartable
  // orchestration flow). null when the backend has no batch row yet —
  // e.g. before StartCoding, or after a manual-only flow that never
  // went through the auto-orchestrator. Drives which summary CTAs the
  // banner surfaces (see sub-orchestrator-status block).
  batch: OrchestrationBatch | null;
  // Optional callback fired after a manual or early-summary round trip
  // completes (success or failure). The parent uses it to refresh the
  // batch snapshot so the banner's disabled state updates without
  // waiting for the next periodic list-poll cycle.
  onBatchChange?: () => void;
}

// Status vocabulary — labels hold i18n KEYS (resolved at render) so the
// chip text follows the active language. The glyph gives a glanceable
// cue; the chip class drives the platform-color treatment.
const statusLabelKeys: Record<SubTaskStatus, string> = {
  pending: 'components.subTaskCard.statusPending',
  running: 'components.subTaskCard.statusRunning',
  done:    'components.subTaskCard.statusDone',
  error:   'components.subTaskCard.statusError',
};
const statusChipClass: Record<SubTaskStatus, string> = {
  pending: 'sub-card-status-chip sub-card-status-pending',
  running: 'sub-card-status-chip sub-card-status-running',
  done:    'sub-card-status-chip sub-card-status-done',
  error:   'sub-card-status-chip sub-card-status-error',
};

// truncate keeps the monospace header line at a predictable width — a
// 100-chars wide terminal header is the convention this panel mimics.
function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return s.slice(0, max) + '…';
}

// timeAgo renders a small "N seconds ago / N minutes ago / N hours ago"
// label, falling back to a full localized timestamp for very old rows.
// Plain i18next call (not a hook) so it can be reused from non-React
// helpers; the panel re-renders on language change anyway.
function timeAgo(iso: string): string {
  if (!iso) return '';
  // Backend serializes time.Time as RFC3339Nano with a numeric tz offset
  // (e.g. "2026-08-31T15:17:01.579491+08:00"). Date.parse handles both
  // this and the "Z" form, but some legacy rows use a SQLite-flavored
  // "YYYY-MM-DD HH:MM:SS.sss" string that Date.parse chokes on — fall
  // back to a relaxed parser so an old row still shows *something*
  // rather than "NaNs ago".
  const t = new Date(iso).getTime();
  let d2 = t;
  if (Number.isNaN(d2)) {
    const m = iso.match(/^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})/);
    if (!m) return iso;
    d2 = Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +m[6]);
  }
  const delta = Math.max(0, Date.now() - d2);
  const s = Math.floor(delta / 1000);
  const t4 = (key: string, opts: Record<string, unknown>) =>
    i18next.t(key, opts) as string;
  if (s < 60) return t4('components.subTaskCard.secondsAgo', { n: s });
  const mn = Math.floor(s / 60);
  if (mn < 60) return t4('components.subTaskCard.minutesAgo', { n: mn });
  const h = Math.floor(mn / 60);
  if (h < 24) return t4('components.subTaskCard.hoursAgo', { n: h });
  return new Date(d2).toLocaleString();
}

// formatDuration renders a sub-task's wall-clock duration (seconds) the
// same way the dashboard renders token-cost durations:
// "42s / 2m 15s / 1h 3m". Pass 0 / undefined for unfinished runs so
// callers can render a live ticker instead.
function fmtDuration(seconds: number | undefined): string {
  if (!seconds || seconds <= 0) return '';
  const tt = (key: string, opts: Record<string, unknown>) =>
    i18next.t(key, opts) as string;
  if (seconds < 60) return tt('components.subTaskCard.secondsOnly', { n: seconds });
  const m = Math.floor(seconds / 60);
  const s = seconds % 60;
  if (m < 60) {
    return s > 0
      ? tt('components.subTaskCard.minutesSeconds', { m, s })
      : tt('components.subTaskCard.minutesOnly', { m });
  }
  const h = Math.floor(m / 60);
  const mm = m % 60;
  return mm > 0
    ? tt('components.subTaskCard.hoursMinutes', { h, m: mm })
    : tt('components.subTaskCard.hoursOnly', { h });
}

// A persistent clipboard utility that falls back to a textarea when
// navigator.clipboard isn't available (older browsers / insecure context).
async function writeClipboard(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch { /* fall through to textarea trick */ }
  try {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand('copy');
    document.body.removeChild(ta);
    return ok;
  } catch { return false; }
}

// Inline log renderer — a compact, terminal-styled variant. We can't
// import CodingLines (it lives inside RequirementDetail.tsx as a private
// component) so this is a stripped-down equivalent that renders the same
// {type, content} event shape the SSE pipeline emits.
function SubTaskLogView({ lines }: { lines: LogLine[] }) {
  const { t } = useTranslation();
  if (lines.length === 0) {
    return <div className="sub-log-empty">{t('components.subTaskCard.logEmpty')}</div>;
  }
  const rendered: React.ReactNode[] = [];
  let phaseBucket: LogLine[] = [];
  const flush = (k: number) => {
    if (phaseBucket.length === 0) return;
    rendered.push(
      <div key={`p-${k}`} className="sub-log-phase-block">
        {phaseBucket.map((l, i) => (
          <div key={i} className={`sub-log-row sub-log-${l.type}`}>
            <span className="sub-log-prompt">$</span>
            <span className="sub-log-text">{l.content}</span>
          </div>
        ))}
      </div>,
    );
    phaseBucket = [];
  };
  lines.forEach((line, i) => {
    if (line.type === 'phase' || line.type === 'tool_call') {
      phaseBucket.push(line);
      return;
    }
    flush(i);
    if (line.type === 'message' || line.type === 'result') {
      rendered.push(
        <div key={`m-${i}`} className="sub-log-md">
          <ReactMarkdown remarkPlugins={[remarkGfm]}>{line.content}</ReactMarkdown>
        </div>,
      );
    } else if (line.type === 'error') {
      rendered.push(
        <div key={`e-${i}`} className="sub-log-row sub-log-error"><span className="sub-log-prompt">!</span><span>{line.content}</span></div>,
      );
    } else if (line.type === 'done') {
      rendered.push(
        <div key={`d-${i}`} className="sub-log-row sub-log-done"><span className="sub-log-prompt">$</span><span>{line.content}</span></div>,
      );
    } else {
      rendered.push(
        <div key={`r-${i}`} className={`sub-log-row sub-log-${line.type}`}>
          <span className="sub-log-prompt">·</span>
          <span>{line.content}</span>
        </div>,
      );
    }
  });
  flush(lines.length);
  return <div className="sub-log">{rendered}</div>;
}

// launchSettingsRef caches the --settings prefix for the copy-paste CLI
// commands (one /active fetch per page load). Empty string = nothing to pin
// (no active config with base URL / model) → commands render without the flag.
let launchSettingsRef: string | null = null;
async function launchSettings(): Promise<string> {
  if (launchSettingsRef !== null) return launchSettingsRef;
  try {
    const active = await claudeApi.active();
    launchSettingsRef = claudeSettingsPrefix(active?.base_url, active?.default_model);
  } catch {
    launchSettingsRef = '';
  }
  return launchSettingsRef;
}

function CopyCliBlock({ st, variant }: { st: SubTask; variant: 'continue' | 'adjust' }) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  const [settings, setSettings] = useState(launchSettingsRef ?? '');
  useEffect(() => {
    let cancelled = false;
    launchSettings().then((s) => { if (!cancelled) setSettings(s); });
    return () => { cancelled = true; };
  }, []);
  const cmd = variant === 'continue'
    ? subTaskCliCommand(st, settings)
    : subTaskAdjustCommand(st, '', settings);
  const onCopy = useCallback(async () => {
    const ok = await writeClipboard(cmd);
    if (ok) {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    }
  }, [cmd]);
  return (
    <div className="sub-cli">
      <span className="sub-cli-prompt">$</span>
      <code className="sub-cli-cmd">{cmd}</code>
      <button type="button" className="sub-cli-copy" onClick={onCopy}>
        {copied ? t('components.subTaskCard.copied') : t('components.subTaskCard.copyBtn')}
      </button>
    </div>
  );
}

interface CardProps {
  st: SubTask;
  index: number;
  total: number;
  onChanged: (next: SubTask) => void;
  // Optional callback fired after a redo (or any action that adds a NEW
  // sub-task row) succeeds. The panel root passes loadList so the freshly
  // created redo row appears immediately — the periodic poll only runs while
  // a child is alive, so a redo of an otherwise-terminal list would never
  // refresh without this.
  onCreated?: () => void;
  // Panel-level model selection — applies to the next "Follow-up" turn so
  // the user picks the model once at the panel header and every card
  // uses it without owning its own copy. Distinct from the per-card
  // `redoModel` (re-runs a failed sub-task with a possibly different
  // model).
  adjustModel?: string;
  // Mirror setter so the per-card ModelSelect in the adjust drawer can
  // write back to the panel-level adjustModel state. Without this each
  // card would need its own picker copy.
  onAdjustModelChange?: (model: string) => void;
}

function SubTaskCard({ st, index, total, onChanged, onCreated, adjustModel = '', onAdjustModelChange }: CardProps) {
  const { t } = useTranslation();
  // The card uses a layout that mirrors an issue tracker detail view:
  //   ┌─ terminal-style header line ────────────────────────────────┐
  //   │  ▶ $ sub-task [01/03] · claude-sonnet · 12s ago         ⌄  │
  //   └─────────────────────────────────────────────────────────────┘
  //   ┌─ body (when expanded): live log OR artifact md ─────────────┐
  //   │  ─ running: phase + tool_call + message stream              │
  //   │  ─ done:    copy CLI block + markdown artifact              │
  //   └─────────────────────────────────────────────────────────────┘
  // Default expansion rule: only ACTIVE sub-tasks (running/pending) open
  // automatically — finished cards (done/error) stay collapsed so a long
  // history doesn't take over the page. The user can still click any
  // header to expand / collapse; the rule just sets the initial state.
  const [expanded, setExpanded] = useState<boolean>(
    st.status === 'running' || st.status === 'pending',
  );
  const [streaming, setStreaming] = useState<boolean>(st.status === 'running' || st.status === 'pending');
  const [lines, setLines] = useState<LogLine[]>([]);
  const [artifact, setArtifact] = useState<string>(st.artifact);
  // Mirror the parent's st.artifact into local state on every prop
  // change. useState alone only captures the mount-time value, so a
  // list-poll that refreshes the row (e.g. after SSE job_done causes
  // the parent to re-fetch /sub-tasks) wouldn't update the rendered
  // artifact unless the SSE-driven subTasksApi.get also completes.
  // This effect closes that gap: any newer artifact coming through
  // props wins, and we deliberately skip the SSE-derived setArtifact
  // callback that fights with this — see the job_done branch below
  // (it still sets local state when SSE arrives faster than the next
  // list poll, which is the common path).
  useEffect(() => {
    setArtifact(st.artifact);
  }, [st.artifact]);
  const [adjusting, setAdjusting] = useState(false);
  const [adjustInput, setAdjustInput] = useState('');
  const [adjustBusy, setAdjustBusy] = useState(false);
  const [adjustError, setAdjustError] = useState<string | null>(null);
  // Redo (🔄 Redo): re-run a failed sub-task with the original prompt. The
  // user may switch the model before re-dispatching. Defaults to the failed
  // run's model so a plain "Redo" re-runs with the same model.
  const [redoing, setRedoing] = useState(false);
  const [redoModel, setRedoModel] = useState<string>(st.model);
  const [redoBusy, setRedoBusy] = useState(false);
  const [redoError, setRedoError] = useState<string | null>(null);
  // Live ticker for the header-right "⏱ 0:42" badge: starts when the card
  // mounts in "running" status and stops on terminal status. Replaced by
  // the persisted duration_seconds once the row finishes, so the badge
  // stays stable across a page refresh.
  const [liveSeconds, setLiveSeconds] = useState<number>(0);
  useEffect(() => {
    if (st.status !== 'running' && st.status !== 'pending') return;
    setLiveSeconds(0);
    const t = setInterval(() => setLiveSeconds((v) => v + 1), 1000);
    return () => clearInterval(t);
  }, [st.status]);
  // Live usage snapshot — driven by SSE `usage` frames (step="sub_task")
  // OR computed client-side from the persisted sub_tasks.*_tokens columns
  // when the card has finished and SSE has gone quiet. We display
  // `live ?? fallback` so refresh-after-finish still shows the same bar.
  const [usage, setUsage] = useState<UsageInfo | undefined>(undefined);
  const esRef = useRef<EventStream | null>(null);
  const chipLabel = t(statusLabelKeys[st.status]);

  // Open / close the SSE stream. Re-subscribes on each status flip; the
  // createEventStream handle is kept in a ref so we can close on unmount
  // and on premature drop.
  useEffect(() => {
    if (!streaming || !st.job_id) return;
    esRef.current = createEventStream(
      `/api/wizard/jobs/${st.job_id}/stream`,
      (evt) => {
        if (!evt || typeof evt !== 'object') return;
        const t = evt.type as string;
        if (t === 'usage') {
          try {
            const raw = typeof evt.content === 'string' ? JSON.parse(evt.content) : null;
            if (raw) setUsage(computeUsage(raw, 'sub_task'));
          } catch { /* malformed payload — ignore */ }
          // usage frames do NOT go into lines[] — SubTaskLogView treats
          // unknown types as plain log rows, which would render the JSON
          // payload as terminal scrollback and confuse the user.
          return;
        }
        if (t === 'job_done') {
          setStreaming(false);
          subTasksApi.get(st.requirement_id, st.id)
            .then((next) => { setArtifact(next.artifact); onChanged(next); })
            .catch(() => { /* keep last-known state */ });
          return;
        }
        setLines((prev) => appendLogLine(prev, {
          type: t,
          content: typeof evt.content === 'string' ? evt.content : (evt.content ? JSON.stringify(evt.content) : ''),
          at: typeof evt.at === 'number' ? evt.at : Date.now(),
        }));
      },
      () => {
        setStreaming(false);
        subTasksApi.get(st.requirement_id, st.id)
          .then((next) => { setArtifact(next.artifact); onChanged(next); })
          .catch(() => {});
      },
    );
    return () => { esRef.current?.close(); esRef.current = null; };
  // We deliberately use the minimal dep set — parent re-renders shouldn't
  // re-subscribe. (See ESLint exhaustive-deps guidance in the codebase.)
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [streaming, st.job_id, st.id, st.requirement_id]);

  const submitAdjust = useCallback(async () => {
    const p = adjustInput.trim();
    if (!p || adjustBusy) return;
    setAdjustBusy(true);
    setAdjustError(null);
    try {
      // The panel-level adjustModel flows through here so the user can
      // change model between adjust rounds without re-picking on every
      // card. Empty selection means "let the backend pick the developer
      // role's effective model".
      const resp = await subTasksApi.adjust(st.requirement_id, st.id, {
        prompt: p,
        ...(adjustModel ? { model: adjustModel } : {}),
      });
      // Replace this card with a brand-new one driven by the new
      // sub_task_id via the parent's onChanged; for now, drop the
      // adjustment composer and let the next list-poll show the new row.
      setAdjusting(false);
      setAdjustInput('');
      // Fire-and-forget: parent will refresh via list poll, then this
      // card's matching row keeps showing this card's old id (now stale).
      // The cleaner fix — reloading the whole list — is already handled
      // by the periodic refresh in the panel root.
      void resp;
    } catch (e: any) {
      setAdjustError(e?.message || t('components.subTaskCard.errAdjust'));
    } finally {
      setAdjustBusy(false);
    }
  }, [adjustInput, adjustBusy, adjustModel, st.id, st.requirement_id, t]);

  const submitRedo = useCallback(async () => {
    if (redoBusy) return;
    setRedoBusy(true);
    setRedoError(null);
    try {
      // Normalize the DefaultModelLabel sentinel so it never reaches the
      // backend as an explicit model id — the backend then falls back to
      // the role default.
      const model = redoModel && redoModel !== DefaultModelLabel ? redoModel : undefined;
      await subTasksApi.redo(st.requirement_id, st.id, { model });
      setRedoing(false);
      setRedoError(null);
      // The redo creates a brand-new sub-task row. Fire the parent's refresh
      // callback so it appears immediately; the periodic poll only runs while
      // a child is alive, so a redo of an otherwise-terminal list would never
      // surface without this.
      onCreated?.();
    } catch (e: any) {
      setRedoError(e?.message || t('components.subTaskCard.errRedo'));
    } finally {
      setRedoBusy(false);
    }
  }, [redoBusy, redoModel, st.id, st.requirement_id, onCreated, t]);

  // The header-right summary block surfaces two quick-glance signals the
  // user always wants at a glance without expanding the card:
  // cost · ⏱ duration · ⏳ relative-time. Each is independently null-safe —
  // the badge only renders the cells that have a value, so a pre-finish
  // row shows only ⏱ (live ticker) + ⏳, a finished row shows ⏱ (persisted)
  // + ⏳. The detailed token breakdown now lives on its own line right
  // below the header — see `<div className="sub-card-usage">` below.
  const costCell = st.cost_cents > 0
    ? (st.cost_cents >= 100 ? `$${(st.cost_cents / 100).toFixed(2)}` : `$${(st.cost_cents / 100).toFixed(3)}`)
    : '';
  const durationCell = (st.status === 'running' || st.status === 'pending')
    ? t('components.subTaskCard.liveTicker', { duration: fmtDuration(liveSeconds) })
    : (st.duration_seconds > 0 ? t('components.subTaskCard.liveTicker', { duration: fmtDuration(st.duration_seconds) }) : '');

  return (
    <article className={`sub-card sub-card-${st.status}`}>
      <header
        className="sub-card-header"
        onClick={() => setExpanded((v) => !v)}
        role="button"
        tabIndex={0}
        onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); setExpanded(v => !v); } }}
      >
        {/* Meta line: status chip + source badge + counter + model + time.
            Sits ABOVE the title so a long user-supplied title never
            competes for horizontal space with the metadata row. */}
        <div className="sub-card-meta">
          <span className={statusChipClass[st.status]}>{chipLabel}</span>
          {/* Source badge: 🪄 Auto (auto-orchestrated) vs 👤 Manual (manual).
              Drives off `source` first; falls back to the legacy `batch_id`
              heuristic so older backends (no source column) still render. */}
          {(st.source === 'auto' || (!st.source && st.batch_id)) && (
            <span className="sub-card-source sub-card-source-auto" title={t('components.subTaskCard.sourceAutoTitle')}>
              🪄 {t('components.subTaskCard.sourceAuto')}
            </span>
          )}
          {(st.source === 'manual' || (!st.source && !st.batch_id)) && (
            <span className="sub-card-source sub-card-source-manual" title={t('components.subTaskCard.sourceManualTitle')}>
              👤 {t('components.subTaskCard.sourceManual')}
            </span>
          )}
          <span className="sub-card-counter">{String(index + 1).padStart(2, '0')} / {String(total).padStart(2, '0')}</span>
          {st.model && st.model !== DefaultModelLabel && (
            <span className="sub-card-model">{st.model}</span>
          )}
          {st.created_at && (
            <span className="sub-card-time">{timeAgo(st.created_at)}</span>
          )}
          {/* Header-right quick-glance summary: cost / duration. The
              detailed context-usage bar lives in its own row below the
              header (`.sub-card-usage`); the header stays compact so a
              stack of concurrent cards doesn't push the body off-screen. */}
          <span className="sub-card-quickstats">
            {durationCell && <span className="sub-card-stat sub-card-stat-time">{durationCell}</span>}
            {costCell && (
              <span className="sub-card-stat sub-card-stat-cost" title={t('components.subTaskCard.costTitle')}>{costCell}</span>
            )}
          </span>
          <span className="sub-card-toggle" aria-hidden="true">{expanded ? '▾' : '▸'}</span>
        </div>
        {/* Title line: the user-supplied sub-task title in display weight.
            Wraps freely so long titles stay fully visible. */}
        <h4 className="sub-card-title">{st.title || t('components.subTaskCard.noTitle')}</h4>
      </header>

      {/* Per-card ContextUsageBar — mirrors the bar shown on the parent
          CodingChat. Sits between the header and the collapsible body so
          it's visible whether the card is expanded or collapsed. Drives
          off (live SSE `usage` frames) ?? (client-side recompute from
          the persisted sub_tasks.*_tokens columns). compressible={false}
          hides the "compress context" button — sub-tasks don't expose
          compression to the user. */}
      <div className="sub-card-usage">
        <ContextUsageBar
          usage={usage ?? computeUsage({
            input_tokens: st.input_tokens,
            output_tokens: st.output_tokens,
            cache_creation_tokens: st.cache_creation_tokens,
            cache_read_tokens: st.cache_read_tokens,
            model: st.model,
            context_window: modelContextWindow(st.model),
          }, 'sub_task')}
          onCompress={() => {}}
          compressible={false}
          stepLabel={t('components.subTaskCard.usageLabel')}
        />
      </div>

      {expanded && (
        <div className="sub-card-body">
          {/* Live log (streaming) — full-width terminal scrollback. */}
          {streaming && <SubTaskLogView lines={lines} />}

          {/* Artifact (finished) — full-width Markdown report. */}
          {!streaming && (st.status === 'done' || st.status === 'error') && artifact && (
            <div className="sub-card-artifact">
              <ReactMarkdown remarkPlugins={[remarkGfm]}>{artifact}</ReactMarkdown>
            </div>
          )}

          {/* Only show "no artifact" once the row is actually terminal —
              between the SSE job_done frame and the next /sub-tasks list
              poll, the local `artifact` state is still the empty string
              captured at mount (before claude finished) while `streaming`
              has already flipped to false. Showing "无产物" in that
              window is misleading: the row IS done, we just haven't
              fetched the artifact Markdown yet. The guard below matches
              the one above so the two branches stay symmetric. */}
          {!streaming && (st.status === 'done' || st.status === 'error') && !artifact && (
            <div className="sub-card-empty">{t('components.subTaskCard.noArtifact')}</div>
          )}

          {/* 🪙 Token + cost strip — moved to the header-right quickstats
              (sub-card-quickstats) so the user always sees the totals without
              expanding the card. The expanded body still has SubTaskLogView
              (live log) and the artifact Markdown for full detail. */}

          {/* Terminal CLI copy: visible only when the sub-task has a session id,
              or can suggest a fork-session variant from the source. */}
          {!streaming && st.session_id && (
            <div className="sub-card-cli">
              <div className="sub-card-cli-label">{t('components.subTaskCard.copyCliContinue')}</div>
              <CopyCliBlock st={st} variant="continue" />
            </div>
          )}

          {!streaming && !st.session_id && st.source_session_id && (
            <div className="sub-card-cli">
              <div className="sub-card-cli-label">{t('components.subTaskCard.copyCliAdjust')}</div>
              <CopyCliBlock st={st} variant="adjust" />
            </div>
          )}

          {/* Append-adjustment: only available on a finished sub-task
              (done or error). Adjusts resume the parent's session via
              --fork-session so the child inherits prior edits. */}
          {!streaming && (st.status === 'done' || st.status === 'error') && st.session_id && (
            <div className="sub-card-adjust">
              {/* Toggle row: both actions collapse to a single row when no
                  pane is open. Redo is only offered on a FAILED sub-task. */}
              {!adjusting && !redoing && (
                <div className="sub-adjust-actions">
                  <button
                    type="button"
                    className="sub-adjust-toggle"
                    onClick={() => setAdjusting(true)}
                  >{t('components.subTaskCard.adjustToggle')}</button>
                  {st.status === 'error' && (
                    <button
                      type="button"
                      className="sub-adjust-toggle"
                      onClick={() => setRedoing(true)}
                    >{t('components.subTaskCard.redoToggle')}</button>
                  )}
                </div>
              )}

              {adjusting && (
                <div className="sub-adjust-pane">
                  <AtMentionTextarea
                    value={adjustInput}
                    onChange={setAdjustInput}
                    rows={3}
                    placeholder={t('components.subTaskCard.adjustPlaceholder')}
                    disabled={adjustBusy}
                    className="sub-adjust-textarea"
                  />
                  <ModelSelect
                    value={adjustModel}
                    onChange={onAdjustModelChange || (() => {})}
                    label={t('components.subTaskCard.adjustModelLabel')}
                    defaultModelName={adjustModel}
                    disabled={adjustBusy}
                  />
                  <div className="sub-adjust-toolbar">
                    <span className="sub-adjust-hint">{t('components.subTaskCard.adjustSubmitHint')}</span>
                    {adjustError && <span className="sub-adjust-err">{adjustError}</span>}
                    <button type="button" className="btn" onClick={() => { setAdjusting(false); setAdjustInput(''); setAdjustError(null); }} disabled={adjustBusy}>{t('components.subTaskCard.cancel')}</button>
                    <button type="button" className="btn btn-primary" onClick={submitAdjust} disabled={!adjustInput.trim() || adjustBusy}>
                      {adjustBusy ? t('components.subTaskCard.adjustBusy') : t('components.subTaskCard.adjustSubmit')}
                    </button>
                  </div>
                </div>
              )}

              {/* Redo: re-run the SAME prompt with an optional model switch.
                  No textarea — the original prompt is reused verbatim. */}
              {!adjusting && redoing && (
                <div className="sub-adjust-pane">
                  <div className="sub-adjust-hint">{t('components.subTaskCard.redoHint')}</div>
                  <ModelSelect
                    value={redoModel}
                    onChange={setRedoModel}
                    label={t('components.subTaskCard.redoModelLabel')}
                    defaultModelName={st.model && st.model !== DefaultModelLabel ? st.model : ''}
                    disabled={redoBusy}
                  />
                  <div className="sub-adjust-toolbar">
                    {redoError && <span className="sub-adjust-err">{redoError}</span>}
                    <button type="button" className="btn" onClick={() => { setRedoing(false); setRedoError(null); }} disabled={redoBusy}>{t('components.subTaskCard.cancel')}</button>
                    <button type="button" className="btn btn-primary" onClick={submitRedo} disabled={redoBusy}>
                      {redoBusy ? t('components.subTaskCard.adjustBusy') : t('components.subTaskCard.redoSubmit')}
                    </button>
                  </div>
                </div>
              )}
            </div>
          )}
        </div>
      )}
    </article>
  );
}

// Kept outside the component so it isn't recreated on every render.
// Threshold mirrors RequirementDetail.tsx:isLongDesignDoc (lines > 12 || chars > 1200)
// so the two long-doc surfaces fold at the same density.
function isLongSummary(raw: string | undefined): boolean {
  if (!raw || !raw.trim()) return false;
  const lines = raw.split('\n').length;
  const chars = raw.length;
  return lines > 12 || chars > 1200;
}

export default function SubTaskPanel({ requirementId, codingSessionId, requirement, onSubTasksChange, developerDefaultModel = '', batch, onBatchChange }: Props) {
  const { t } = useTranslation();
  const [items, setItems] = useState<SubTask[] | null>(null);
  const [prompt, setPrompt] = useState('');
  // Title input was removed: opening a sub-task now only needs a description.
  // The backend auto-derives a card-header title from the prompt (first 40
  // chars via truncateForTitle) when the caller leaves the title blank, so
  // downstream rendering still has something to show in the card header.
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Per-stage model selection for the sub-task entry points. Empty
  // string means "leave it to the backend's role default" — the same
  // convention the main-task path uses (RequirementDetail.doStartCoding
  // spreads model only when truthy). Naming deliberately avoids the
  // per-card `redoModel` so each entry point owns its own picker without
  // coupling.
  //
  // `createModel` is the SINGLE composer picker shared by BOTH the
  // "Start sub-task" and "Re-split" buttons — they live in the same
  // toolbar so one selection covers both actions. (An earlier iteration
  // had a second picker beside Re-split, which read as duplicate UI.)
  // `adjustModel` is panel-level (shared across all cards' "Follow-up"
  // composers) — picking once applies to the next adjustment round,
  // mirroring how a user thinks about model choice on the main
  // requirement.
  const [createModel, setCreateModel] = useState<string>('');
  const [adjustModel, setAdjustModel] = useState<string>('');
  // Track an auto-orchestrate batch (the new "one-click orchestrate =
  // main agent auto-dispatches" path in StartCoding). Children may still
  // be running so the panel shows the "auto-orchestrate in flight" status.
  const [activeBatch, setActiveBatch] = useState<{ childIds: string[]; startedAt: number } | null>(null);
  // Remember the count we last reported to the parent so loadList (which
  // re-runs on periodic poll + after every create / adjust) doesn't fire
  // onSubTasksChange on every tick. Only emit on actual transitions.
  const lastReportedCountRef = useRef<number>(-1);
  // Manual / early-summary round-trip state. Distinct from the per-card
  // `adjustBusy` so the composer submit lock doesn't accidentally disable
  // the orchestrator banner's summary CTA. `summaryToast` mirrors the
  // toast pattern from WorktreePathHint — reuses the global
  // `.merge-hint-toast` style for a 1.8s auto-dismiss.
  const [summaryBusy, setSummaryBusy] = useState(false);
  const [summaryToast, setSummaryToast] = useState<{ kind: 'ok' | 'err'; text: string } | null>(null);
  const summaryToastTimerRef = useRef<number | null>(null);
  const showSummaryToast = useCallback((kind: 'ok' | 'err', text: string) => {
    setSummaryToast({ kind, text });
    if (summaryToastTimerRef.current) window.clearTimeout(summaryToastTimerRef.current);
    summaryToastTimerRef.current = window.setTimeout(() => {
      setSummaryToast(null);
      summaryToastTimerRef.current = null;
    }, 1800);
  }, []);
  useEffect(() => () => {
    if (summaryToastTimerRef.current) window.clearTimeout(summaryToastTimerRef.current);
  }, []);

  // The orchestration summary Markdown lives on the requirement row
  // (`requirements.coding_plan`, written by the developer's main agent at
  // the end of every orchestrated batch via RequirementService.UpdateCodingPlan).
  // Computed BEFORE the summary-related hooks below so handleSummaryCopy
  // and the [requirement?.id, summaryReport] effect can reference it without
  // hitting a TDZ. Type-safe now that Requirement.coding_plan is declared
  // in the API client (see client.ts:coding_plan).
  const summaryReport = requirement?.coding_plan;
  const hasSummary = typeof summaryReport === 'string' && summaryReport.trim() !== '';

  // Summary markdown expand/collapse + copy state. Long reports
  // (per isLongSummary, mirrors isLongDesignDoc) collapse by default with a
  // fade-out mask; the toggle button un/folds. `summaryCopied` is the
  // transient 1.5s "✓ Copied" feedback on the header copy button.
  const [isLongSummaryState, setIsLongSummaryState] = useState(false);
  const [summaryExpanded, setSummaryExpanded] = useState(false);
  const [summaryCopied, setSummaryCopied] = useState(false);
  const summaryCopyTimerRef = useRef<number | null>(null);

  const handleSummaryCopy = () => {
    if (!summaryReport) return;
    void writeClipboard(summaryReport);
    setSummaryCopied(true);
    if (summaryCopyTimerRef.current) window.clearTimeout(summaryCopyTimerRef.current);
    summaryCopyTimerRef.current = window.setTimeout(() => setSummaryCopied(false), 1500);
  };

  // Unmount cleanup for the copy timer — mirrors the summaryToast cleanup
  // above so a stale timer can't fire setState after the component is gone.
  useEffect(() => () => {
    if (summaryCopyTimerRef.current) window.clearTimeout(summaryCopyTimerRef.current);
  }, []);

  // Reset the collapse state when the underlying requirement / coding_plan
  // changes. Mirrors RequirementDetail.tsx:897-902 (design doc surface):
  // switching requirements or the main agent regenerating the summary
  // should not leave the UI stranded in a stale "expanded" state.
  useEffect(() => {
    setIsLongSummaryState(isLongSummary(summaryReport));
    setSummaryExpanded(false);
  }, [requirement?.id, summaryReport]);

  // Sort children newest-first. The requirement explicitly asks for
  // "按创建时间排倒序" (newest first). This replaces the previous
  // batch_seq ASC sort so that a freshly-created manual card immediately
  // floats to the top. The id DESC tie-breaker keeps rows with identical
  // created_at timestamps in a stable order.
  const sortedItems = (() => {
    if (!items) return items;
    return [...items].sort((a, b) => {
      const aTime = a.created_at ? new Date(a.created_at).getTime() : 0;
      const bTime = b.created_at ? new Date(b.created_at).getTime() : 0;
      if (aTime !== bTime) return bTime - aTime;
      return (b.id || '').localeCompare(a.id || '');
    });
  })();

  // Decide which summary CTA (if any) the banner should show. Kept as
  // a pure derivation so the JSX below stays declarative and easy to
  // review against the plan's three-state rule (early / manual / progress).
  const summaryCta: { mode: 'early' | 'manual' | 'progress' | null } = (() => {
    if (batch?.status === 'summarizing') return { mode: 'progress' };
    if (batch?.status === 'dispatching') return { mode: 'early' };
    if (batch && (batch.status === 'completed' || batch.status === 'errored')) return { mode: null };
    if (batch) return { mode: null }; // unknown status — no CTA
    // batch === null — manual flow. Offer the CTA only when there's at
    // least one terminal sub-task; a fully-empty list shows the empty
    // state instead and a still-running list shows the "wait for completion"
    // pattern implicitly (no CTA → user can't fire prematurely).
    if (!items || items.length === 0) return { mode: null };
    const allTerminal = items.every((s) => s.status === 'done' || s.status === 'error');
    return allTerminal ? { mode: 'manual' } : { mode: null };
  })();

  const onGenerateSummary = useCallback(async (mode: 'early' | 'manual') => {
    if (summaryBusy) return;
    if (mode === 'early') {
      const ok = window.confirm(t('components.subTaskPanel.summaryConfirmEarly'));
      if (!ok) return;
    }
    setSummaryBusy(true);
    try {
      await subTasksApi.generateSummary(requirementId, {});
      showSummaryToast('ok', t('components.subTaskPanel.summaryToastOk'));
      onBatchChange?.();
    } catch (e: any) {
      const msg = e?.message || t('components.subTaskPanel.summaryToastErrFallback');
      showSummaryToast('err', `${t('components.subTaskPanel.summaryToastErrPrefix')} ${msg}`);
    } finally {
      setSummaryBusy(false);
    }
  }, [summaryBusy, requirementId, onBatchChange, showSummaryToast, t]);

  const loadList = useCallback(async () => {
    try {
      const list = await subTasksApi.list(requirementId);
      setItems(list);
      // Forward the new count to the parent so the page can flip
      // hasSubTasks and hide the requirement-level "Follow-up" composer.
      // The ref guard avoids redundant parent re-renders — the panel
      // polls every 5s while children are alive and we don't want a
      // fresh onChange call each tick.
      if (onSubTasksChange && list.length !== lastReportedCountRef.current) {
        lastReportedCountRef.current = list.length;
        onSubTasksChange(list.length);
      }
      // Detect a brand-new auto-orchestrate batch: any "running" child
      // whose created_at is within the last 10 minutes AND that we don't
      // yet have a local activeBatch marker for gets folded into the
      // tracked set. This lets StartCoding's auto-dispatch show a
      // "main agent dispatching children..." banner without any explicit
      // orchestrate click.
      const running = list.filter((s) => s.status === 'running' || s.status === 'pending');
      if (running.length > 0) {
        const recent = running.filter((s) => {
          const ageMs = Date.now() - new Date(s.created_at).getTime();
          return ageMs < 10 * 60_000; // 10 minutes
        });
        if (recent.length > 0) {
          const known = new Set(activeBatch?.childIds ?? []);
          const fresh = recent.filter((s) => !known.has(s.id));
          if (fresh.length > 0) {
            setActiveBatch({
              childIds: [
                ...(activeBatch?.childIds ?? []),
                ...fresh.map((s) => s.id),
              ],
              startedAt: activeBatch?.startedAt ?? Date.now(),
            });
          }
        }
      }
    } catch (e: any) {
      setError(e?.message || t('components.subTaskPanel.errLoad'));
    }
  }, [requirementId, activeBatch, onSubTasksChange, t]);

  useEffect(() => {
    loadList();
  }, [loadList]);

  // Startup poll: tryAutoOrchestrate runs in a goroutine after job_done, so
  // the panel may mount before sub-tasks are written. Poll every 2s for up to
  // 60s when the list is still empty, then give up.
  const startupDeadlineRef = useRef<number>(Date.now() + 60_000);
  useEffect(() => {
    if (items === null) return; // not yet loaded
    if (items.length > 0) return; // already have data, nothing to do
    if (Date.now() > startupDeadlineRef.current) return;
    const t = setInterval(async () => {
      if (Date.now() > startupDeadlineRef.current) { clearInterval(t); return; }
      const list = await subTasksApi.list(requirementId).catch(() => null);
      if (list && list.length > 0) { clearInterval(t); loadList(); }
    }, 2000);
    return () => clearInterval(t);
  }, [items, requirementId, loadList]);

  // Periodic refresh while any child is alive — catches JobStore eviction
  // and the eventual artifact write without having to thread job_done
  // from each card up to the panel root.
  useEffect(() => {
    if (!items || !items.some((s) => s.status === 'running' || s.status === 'pending')) return;
    const t = setInterval(loadList, 5000);
    return () => clearInterval(t);
  }, [items, loadList]);

  // Auto-clear the active-batch badge when all children reach a terminal
  // state — otherwise the badge would linger after the summary has landed.
  useEffect(() => {
    if (!activeBatch || !items) return;
    const stillRunning = activeBatch.childIds.some((id) => {
      const st = items.find((s) => s.id === id);
      return st && (st.status === 'running' || st.status === 'pending');
    });
    if (!stillRunning) setActiveBatch(null);
  }, [activeBatch, items]);

  const onItemChanged = useCallback((next: SubTask) => {
    setItems((prev) => prev ? prev.map((p) => p.id === next.id ? next : p) : prev);
  }, []);

  const onCreate = useCallback(async () => {
    const p = prompt.trim();
    if (!p || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      // No title field on the composer — the backend derives a card-header
      // title from the prompt's first 40 chars when title is omitted, so
      // the sub-task row still has a human-readable header downstream.
      // `model` is optional; empty selection lets the backend fall back to
      // the developer-role effective model. Sending the literal DefaultModelLabel
      // sentinel would never happen here — ModelSelect normalises it to "".
      await subTasksApi.create(requirementId, {
        prompt: p,
        ...(createModel ? { model: createModel } : {}),
      });
      setPrompt('');
      await loadList();
    } catch (e: any) {
      setError(e?.message || t('components.subTaskPanel.errCreate'));
    } finally {
      setSubmitting(false);
    }
  }, [prompt, submitting, createModel, requirementId, loadList, t]);

  // --- Manual re-split (🔄 Re-split) ------------------------------------
  // Escape hatch for when StartCoding's auto-orchestration produced no
  // children (main agent didn't decompose) or the user wants a fresh split.
  // POSTs re-orchestrate, then streams the main agent's progress from the
  // returned job so the user sees "Re-splitting…" instead of a dead click.
  const [reSplitBusy, setReSplitBusy] = useState(false);
  const [reSplitLines, setReSplitLines] = useState<LogLine[]>([]);
  const reSplitEsRef = useRef<EventStream | null>(null);

  const anyAlive = !!items?.some((s) => s.status === 'running' || s.status === 'pending');

  const onReSplit = useCallback(async () => {
    if (reSplitBusy) return;
    setReSplitBusy(true);
    setReSplitLines([]);
    setError(null);
    try {
      // model is optional — empty selection lets the backend fall back to
      // the developer-role effective model. Shares the composer's
      // createModel picker (there's only ONE model picker in the panel,
      // beside the textarea) so the user doesn't pick the model twice.
      const { job_id } = await subTasksApi.reOrchestrate(
        requirementId,
        { ...(createModel ? { model: createModel } : {}) },
      );
      reSplitEsRef.current?.close();
      reSplitEsRef.current = createEventStream(
        `/api/wizard/jobs/${job_id}/stream`,
        (evt) => {
          if (!evt || typeof evt !== 'object') return;
          const t = evt.type as string;
          if (t === 'job_done') {
            setReSplitBusy(false);
            reSplitEsRef.current?.close();
            reSplitEsRef.current = null;
            loadList();
            return;
          }
          setReSplitLines((prev) => appendLogLine(prev, {
            type: t,
            content: typeof evt.content === 'string' ? evt.content : (evt.content ? JSON.stringify(evt.content) : ''),
            at: typeof evt.at === 'number' ? evt.at : Date.now(),
          }));
        },
        () => {
          setReSplitBusy(false);
          reSplitEsRef.current = null;
          loadList();
        },
      );
    } catch (e: any) {
      setError(e?.message || t('components.subTaskPanel.errReSplit'));
      setReSplitBusy(false);
    }
  }, [reSplitBusy, createModel, requirementId, loadList, t]);

  // Close the re-split stream on unmount.
  useEffect(() => () => { reSplitEsRef.current?.close(); reSplitEsRef.current = null; }, []);

  if (!codingSessionId) {
    return (
      <section className="sub-panel" aria-labelledby="sub-panel-title">
        <header className="sub-panel-header">
          <h3 id="sub-panel-title" className="sub-panel-title">
            <span className="sub-panel-title-icon" aria-hidden="true"><IconRobot size={16} /></span>
            <span>{t('components.subTaskPanel.title')}</span>
          </h3>
        </header>
        <div className="sub-panel-hint">
          {t('components.subTaskPanel.preFlightHint')}
          {requirement?.title && <em>{t('components.subTaskPanel.preFlightCurrent', { title: requirement.title })}</em>}
        </div>
      </section>
    );
  }

  // summaryReport / hasSummary are declared earlier (above the summary
  // hooks block) so handleSummaryCopy and the [requirement?.id,
  // summaryReport] effect can read them. See the declaration near the top
  // of the component body for the JSDoc explaining the data source.
  const activeChildCount = activeBatch?.childIds.length ?? 0;

  return (
    <section className="sub-panel" aria-labelledby="sub-panel-title">
      <header className="sub-panel-header">
        <h3 id="sub-panel-title" className="sub-panel-title">
          <span className="sub-panel-title-icon" aria-hidden="true"><IconRobot size={16} /></span>
          <span>{t('components.subTaskPanel.title')}</span>
        </h3>
        <span className="sub-panel-meta">
          {t('components.subTaskPanel.shareSession')} <code>{truncate(codingSessionId, 12)}</code>
          {' · '}
          <span className="sub-panel-count">{items?.length ?? 0}</span> {t('components.subTaskPanel.countSuffix')}
        </span>
      </header>

      {/* Auto-orchestrate: ask the main agent to decompose + dispatch + summarize.
          Distinct from the manual composer below — orchestrate is a SINGLE click
          that creates N children AND a summary report, while the composer is for
          ad-hoc one-off children. */}
      {/* Auto-orchestrate status: the manual "Start execution" button was
          removed — StartCoding's main agent now does the decomposition +
          dispatch automatically when the user kicks off development.
          What remains is the in-flight badge (so the user knows the
          main agent is dispatching children) and the summary report
          surface (each completed batch refreshes requirements.coding_plan).
          The CTA cluster on the right drives the manual / early-summary
          round-trips against /api/requirements/{id}/sub-tasks/summary
          (creating summarizing batches → OrchestrationQueue tick handoff). */}
      {(activeChildCount > 0 || summaryCta.mode !== null || (batch && (batch.status === 'summarizing' || batch.status === 'dispatching'))) && (
        <div className="sub-orchestrator-status sub-orchestrator-status--with-cta">
          <div className="sub-orchestrator-status-row">
            <span className="sub-orchestrator-status-text">
              {activeChildCount > 0
                ? t('components.subTaskPanel.autoOrchestrateRunning', { n: activeChildCount })
                : batch?.status === 'summarizing'
                  ? t('components.subTaskPanel.bannerSummarizing')
                  : batch?.status === 'dispatching'
                    ? t('components.subTaskPanel.bannerDispatchingStatus')
                    : t('components.subTaskPanel.bannerAllDoneManual')}
            </span>
            <span className="sub-orchestrator-status-actions">
              {summaryCta.mode === 'early' && (
                <button
                  type="button"
                  className="btn btn-sm sub-orchestrator-cta"
                  onClick={() => onGenerateSummary('early')}
                  disabled={summaryBusy}
                  title={t('components.subTaskPanel.summaryCtaEarlyTitle')}
                >
                  {summaryBusy ? t('components.subTaskPanel.summarySending') : t('components.subTaskPanel.summaryCtaEarlyBtn')}
                </button>
              )}
              {summaryCta.mode === 'manual' && (
                <button
                  type="button"
                  className="btn btn-sm btn-primary sub-orchestrator-cta"
                  onClick={() => onGenerateSummary('manual')}
                  disabled={summaryBusy}
                  title={t('components.subTaskPanel.summaryCtaManualTitle')}
                >
                  {summaryBusy ? t('components.subTaskPanel.summarySending') : t('components.subTaskPanel.summaryCtaManualBtn')}
                </button>
              )}
              {summaryCta.mode === 'progress' && (
                <button
                  type="button"
                  className="btn btn-sm sub-orchestrator-cta"
                  disabled
                  title={t('components.subTaskPanel.summaryCtaProgressTitle')}
                >
                  {t('components.subTaskPanel.summaryCtaProgressBtn')}
                </button>
              )}
              {summaryToast && (
                <span
                  className="merge-hint-toast sub-orchestrator-toast"
                  role="status"
                  data-kind={summaryToast.kind}
                >
                  {summaryToast.text}
                </span>
              )}
            </span>
          </div>
        </div>
      )}

      {/* Manual re-split progress: streams the main agent's re-decomposition
          turn so a click never looks dead. */}
      {(reSplitBusy || reSplitLines.length > 0) && (
        <div className="sub-orchestrator-status sub-resplit-status">
          {reSplitBusy ? t('components.subTaskPanel.reSplitRunning') : t('components.subTaskPanel.reSplitDone')}
          {reSplitLines.length > 0 && (
            <div className="sub-resplit-log">
              {reSplitLines.slice(-4).map((l, i) => (
                <div key={i} className={`sub-resplit-line sub-resplit-line-${l.type}`}>{l.content}</div>
              ))}
            </div>
          )}
        </div>
      )}

      {/* Summary report: shown ABOVE children when an orchestrated batch
          has produced one. Only renders when requirement.coding_plan is
          non-empty (manual-only flows leave it blank). */}
      {hasSummary && (
        <div className="sub-summary">
          <header className="sub-summary-header">
            <span className="sub-summary-icon" aria-hidden="true"><IconDashboard size={14} /></span>
            <span className="sub-summary-title">{t('components.subTaskPanel.summaryTitle')}</span>
            <span className="sub-summary-actions">
              <button
                type="button"
                className="sub-summary-action-btn"
                onClick={handleSummaryCopy}
                title={t('components.subTaskPanel.summaryCopyBtn')}
                aria-label={t('components.subTaskPanel.summaryCopyBtn')}
                data-copied={summaryCopied ? 'true' : 'false'}
              >
                {summaryCopied ? <IconCheck size={13} /> : <IconCopy size={13} />}
              </button>
            </span>
          </header>
          <div
            className={
              'sub-summary-body-wrap' +
              (isLongSummaryState && !summaryExpanded ? ' is-collapsed' : '')
            }
          >
            <div className="sub-summary-body">
              <ReactMarkdown remarkPlugins={[remarkGfm]}>{summaryReport!}</ReactMarkdown>
            </div>
          </div>
          {isLongSummaryState && (
            <button
              type="button"
              className="sub-summary-toggle-btn"
              onClick={() => setSummaryExpanded(v => !v)}
              aria-expanded={summaryExpanded}
            >
              {summaryExpanded
                ? t('components.subTaskPanel.summaryExpandCollapse')
                : t('components.subTaskPanel.summaryExpandShow')}
            </button>
          )}
        </div>
      )}

      <div className="sub-composer">
        {/* Composer textarea — title field removed; opening a sub-task
            only needs a description. The backend auto-derives a card-header
            title from the prompt's first 40 chars when title is omitted. */}
        <AtMentionTextarea
          value={prompt}
          onChange={setPrompt}
          placeholder={t('components.subTaskPanel.composerPlaceholder')}
          rows={4}
          disabled={submitting}
          className="sub-composer-textarea"
        />
        {/* Sub-task model picker — the SINGLE picker for the panel's
            composer row. It applies to BOTH the "Start sub-task" and
            "Re-split" buttons (they share the same claude_configs
            list, and dispatching a re-split with a different model
            would just create a confusing mixed batch). Per-stage
            (developer) so the dropdown shows the same model list as the
            main "Start coding" picker on RequirementDetail. Empty selection
            = let the backend fall back to the developer-role effective
            model; "Default model (X)" shows what that fallback actually is. */}
        <ModelSelect
          value={createModel}
          onChange={setCreateModel}
          label={t('components.subTaskPanel.modelLabel')}
          stage="developer"
          defaultModelName={developerDefaultModel}
          disabled={submitting || reSplitBusy}
          working={submitting || reSplitBusy}
        />
        {!createModel && !developerDefaultModel && (
          <div className="sub-model-warning" role="note">
            {t('components.subTaskPanel.modelEmptyWarning')}
          </div>
        )}
        <div className="sub-composer-toolbar">
          <span className="sub-composer-hint">
            {t('components.subTaskPanel.composerHint')}
          </span>
          {error && <span className="sub-composer-err">{error}</span>}
          <button
            type="button"
            className="btn btn-secondary sub-composer-resplit"
            onClick={onReSplit}
            disabled={reSplitBusy || submitting || anyAlive}
            title={anyAlive ? t('components.subTaskPanel.reSplitTitleBusy') : t('components.subTaskPanel.reSplitTitle')}
          >
            {reSplitBusy ? t('components.subTaskPanel.reSplitBusy') : t('components.subTaskPanel.reSplitBtn')}
          </button>
          <button
            type="button"
            className="btn btn-primary sub-composer-submit"
            onClick={onCreate}
            disabled={submitting || reSplitBusy || !prompt.trim()}
          >
            {submitting ? t('components.subTaskPanel.submitBusy') : t('components.subTaskPanel.submitBtn')}
          </button>
        </div>
      </div>

      <div className="sub-list">
        {items === null && <div className="sub-list-loading">{t('components.subTaskPanel.loading')}</div>}
        {items && items.length === 0 && (
          <div className="sub-list-empty">
            <div className="sub-list-empty-icon" aria-hidden="true"><IconSparkles size={28} /></div>
            <div>{t('components.subTaskPanel.empty')}</div>
          </div>
        )}
        {sortedItems && sortedItems.length > 0 && sortedItems.map((st, i) => (
          <SubTaskCard
            key={st.id}
            st={st}
            index={i}
            total={sortedItems.length}
            onChanged={onItemChanged}
            onCreated={loadList}
            adjustModel={adjustModel}
            onAdjustModelChange={setAdjustModel}
          />
        ))}
      </div>
    </section>
  );
}