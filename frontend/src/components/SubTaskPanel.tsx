import { useCallback, useEffect, useRef, useState } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import {
  subTasksApi,
  subTaskCliCommand,
  subTaskAdjustCommand,
  fmtNum,
  type SubTask,
  type SubTaskStatus,
  type Requirement,
} from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import { appendLogLine, type LogLine } from '../utils/logLines';
import AtMentionTextarea from './AtMentionTextarea';
import './SubTaskPanel.css';

// fmtCost / fmtNum are imported from the shared API client. TokenStrip
// was retired; the header-right quickstats block (🪙 + cost + ⏱) reads
// the persisted sub_tasks fields directly without a usageApi round-trip.

interface Props {
  requirementId: string;
  // Empty when the developer stage hasn't run yet — render the
  // pre-flight hint instead of the create form.
  codingSessionId: string;
  requirement: Requirement;
  // Optional callback fired after each successful list fetch with the
  // current item count. Lets the parent hide the requirement-level
  // "追加调整" composer the moment this panel shows at least one child,
  // without waiting for the next refetch. The parent should pass a
  // stable setter (useState's setState) so this callback reference is
  // stable across re-renders — otherwise the panel's useEffect would
  // re-fire and cause an infinite loop. The panel only re-emits when
  // the count changes, so the parent never sees a redundant call.
  onSubTasksChange?: (count: number) => void;
}

// Status vocabulary. The label stays in plain Chinese so the chip reads
// the same as the other developer-stage UI (CodingChat / AdjustCoding
// use 中文 labels). The glyph gives a glanceable cue; the chip class
// drives the platform-color treatment.
const statusMeta: Record<SubTaskStatus, { label: string; chipClass: string }> = {
  pending: { label: '排队中', chipClass: 'sub-card-status-chip sub-card-status-pending' },
  running: { label: '运行中', chipClass: 'sub-card-status-chip sub-card-status-running' },
  done:    { label: '已完成', chipClass: 'sub-card-status-chip sub-card-status-done' },
  error:   { label: '出错',   chipClass: 'sub-card-status-chip sub-card-status-error' },
};

// truncate keeps the monospace header line at a predictable width — a
// 100-chars wide terminal header is the convention this panel mimics.
function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return s.slice(0, max) + '…';
}

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
  if (s < 60) return `${s}秒前`;
  const mn = Math.floor(s / 60);
  if (mn < 60) return `${mn}分钟前`;
  const h = Math.floor(mn / 60);
  if (h < 24) return `${h}小时前`;
  return new Date(d2).toLocaleString();
}

// formatDuration renders a sub-task's wall-clock duration (seconds) the
// same way the dashboard renders token-cost durations: "42秒" / "2分15秒"
// / "1小时03分". Pass 0 / undefined for unfinished runs so callers can
// render a live ticker instead.
function fmtDuration(seconds: number | undefined): string {
  if (!seconds || seconds <= 0) return '';
  if (seconds < 60) return `${seconds}秒`;
  const m = Math.floor(seconds / 60);
  const s = seconds % 60;
  if (m < 60) return s > 0 ? `${m}分${s}秒` : `${m}分`;
  const h = Math.floor(m / 60);
  const mm = m % 60;
  return mm > 0 ? `${h}小时${mm}分` : `${h}小时`;
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
  if (lines.length === 0) {
    return <div className="sub-log-empty">⏳ 等待 Claude 输出…</div>;
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

function CopyCliBlock({ st, variant }: { st: SubTask; variant: 'continue' | 'adjust' }) {
  const [copied, setCopied] = useState(false);
  const cmd = variant === 'continue'
    ? subTaskCliCommand(st)
    : subTaskAdjustCommand(st, '');
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
        {copied ? '✓ 已复制' : '复制'}
      </button>
    </div>
  );
}

interface CardProps {
  st: SubTask;
  index: number;
  total: number;
  onChanged: (next: SubTask) => void;
}

function SubTaskCard({ st, index, total, onChanged }: CardProps) {
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
  const [adjusting, setAdjusting] = useState(false);
  const [adjustInput, setAdjustInput] = useState('');
  const [adjustBusy, setAdjustBusy] = useState(false);
  const [adjustError, setAdjustError] = useState<string | null>(null);
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
  const esRef = useRef<EventStream | null>(null);
  const meta = statusMeta[st.status];

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
      const resp = await subTasksApi.adjust(st.requirement_id, st.id, { prompt: p });
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
      setAdjustError(e?.message || '追加调整失败');
    } finally {
      setAdjustBusy(false);
    }
  }, [adjustInput, adjustBusy, st.id, st.requirement_id]);

  // The header-right summary block surfaces the four quick-glance signals
  // the user always wants at a glance without expanding the card:
  // 🪙 token + cost · ⏱ duration · ⏳ relative-time. Each is independently
  // null-safe — the badge only renders the cells that have a value, so a
  // pre-finish row shows only ⏱ (live ticker) + ⏳, a finished row shows
  // 🪙 + ⏱ (persisted) + ⏳.
  const tokenCell = (st.input_tokens || st.output_tokens || st.cache_creation_tokens || st.cache_read_tokens)
    ? `${fmtNum((st.input_tokens || 0) + (st.cache_creation_tokens || 0) + (st.cache_read_tokens || 0))}↓ / ${fmtNum(st.output_tokens)}↑`
    : '';
  const costCell = st.cost_cents > 0
    ? (st.cost_cents >= 100 ? `$${(st.cost_cents / 100).toFixed(2)}` : `$${(st.cost_cents / 100).toFixed(3)}`)
    : '';
  const durationCell = (st.status === 'running' || st.status === 'pending')
    ? `⏱ ${fmtDuration(liveSeconds)}`
    : (st.duration_seconds > 0 ? `⏱ ${fmtDuration(st.duration_seconds)}` : '');

  return (
    <article className={`sub-card sub-card-${st.status}`}>
      <header
        className="sub-card-header"
        onClick={() => setExpanded((v) => !v)}
        role="button"
        tabIndex={0}
        onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); setExpanded(v => !v); } }}
      >
        {/* Meta line: status chip + counter + model + time. Sits ABOVE the
            title so a long user-supplied title never competes for horizontal
            space with the metadata row. */}
        <div className="sub-card-meta">
          <span className={meta.chipClass}>{meta.label}</span>
          <span className="sub-card-counter">{String(index + 1).padStart(2, '0')} / {String(total).padStart(2, '0')}</span>
          {st.model && st.model !== '默认模型' && (
            <span className="sub-card-model">{st.model}</span>
          )}
          {st.created_at && (
            <span className="sub-card-time">{timeAgo(st.created_at)}</span>
          )}
          {/* Header-right quick-glance summary: token / cost / duration.
              TokenStrip (in the body) carries the same data + cache details
              when the card is expanded; the header badge is the always-
              visible variant. */}
          <span className="sub-card-quickstats">
            {durationCell && <span className="sub-card-stat sub-card-stat-time">{durationCell}</span>}
            {tokenCell && (
              <span className="sub-card-stat sub-card-stat-tokens" title="输入 / 输出 tokens（含缓存）">
                🪙 {tokenCell}
              </span>
            )}
            {costCell && (
              <span className="sub-card-stat sub-card-stat-cost" title="本次子任务费用">{costCell}</span>
            )}
          </span>
          <span className="sub-card-toggle" aria-hidden="true">{expanded ? '▾' : '▸'}</span>
        </div>
        {/* Title line: the user-supplied sub-task title in display weight.
            Wraps freely so long titles stay fully visible. */}
        <h4 className="sub-card-title">{st.title || '(无标题)'}</h4>
      </header>

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

          {!streaming && !artifact && (
            <div className="sub-card-empty">无产物。</div>
          )}

          {/* 🪙 Token + cost strip — moved to the header-right quickstats
              (sub-card-quickstats) so the user always sees the totals without
              expanding the card. The expanded body still has SubTaskLogView
              (live log) and the artifact Markdown for full detail. */}

          {/* Terminal CLI copy: visible only when the sub-task has a session id,
              or can suggest a fork-session variant from the source. */}
          {!streaming && st.session_id && (
            <div className="sub-card-cli">
              <div className="sub-card-cli-label">复制到终端继续：</div>
              <CopyCliBlock st={st} variant="continue" />
            </div>
          )}

          {!streaming && !st.session_id && st.source_session_id && (
            <div className="sub-card-cli">
              <div className="sub-card-cli-label">从源会话 fork（仅在子任务未启动时）：</div>
              <CopyCliBlock st={st} variant="adjust" />
            </div>
          )}

          {/* Append-adjustment: only available on a finished sub-task
              (done or error). Adjusts resume the parent's session via
              --fork-session so the child inherits prior edits. */}
          {!streaming && (st.status === 'done' || st.status === 'error') && st.session_id && (
            <div className="sub-card-adjust">
              {!adjusting ? (
                <button
                  type="button"
                  className="sub-adjust-toggle"
                  onClick={() => setAdjusting(true)}
                >+ 追加调整</button>
              ) : (
                <div className="sub-adjust-pane">
                  <AtMentionTextarea
                    value={adjustInput}
                    onChange={setAdjustInput}
                    rows={3}
                    placeholder="追加的指令…输入 @ 引用 Skill"
                    disabled={adjustBusy}
                    className="sub-adjust-textarea"
                  />
                  <div className="sub-adjust-toolbar">
                    <span className="sub-adjust-hint">Enter 发送 · Shift+Enter 换行</span>
                    {adjustError && <span className="sub-adjust-err">{adjustError}</span>}
                    <button type="button" className="btn" onClick={() => { setAdjusting(false); setAdjustInput(''); setAdjustError(null); }} disabled={adjustBusy}>取消</button>
                    <button type="button" className="btn btn-primary" onClick={submitAdjust} disabled={!adjustInput.trim() || adjustBusy}>
                      {adjustBusy ? '启动中…' : '🚀 追加调整'}
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

export default function SubTaskPanel({ requirementId, codingSessionId, requirement, onSubTasksChange }: Props) {
  const [items, setItems] = useState<SubTask[] | null>(null);
  const [prompt, setPrompt] = useState('');
  const [title, setTitle] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Track an auto-orchestrate batch (the new "一键编排 = 主 Agent 自动派发"
  // path in StartCoding). Children may still be running so the panel shows
  // "auto-orchestrate in flight" status.
  const [activeBatch, setActiveBatch] = useState<{ childIds: string[]; startedAt: number } | null>(null);
  // Remember the count we last reported to the parent so loadList (which
  // re-runs on periodic poll + after every create / adjust) doesn't fire
  // onSubTasksChange on every tick. Only emit on actual transitions.
  const lastReportedCountRef = useRef<number>(-1);

  const loadList = useCallback(async () => {
    try {
      const list = await subTasksApi.list(requirementId);
      setItems(list);
      // Forward the new count to the parent so the page can flip
      // hasSubTasks and hide the requirement-level "追加调整" composer.
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
      // tracked set. This lets StartCoding's auto-dispatch show a "主 Agent
      // 正在派生子任务..." banner without any explicit orchestrate click.
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
      setError(e?.message || '加载子任务失败');
    }
  }, [requirementId, activeBatch, onSubTasksChange]);

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
      await subTasksApi.create(requirementId, { prompt: p, title: title.trim() || undefined });
      setPrompt('');
      setTitle('');
      await loadList();
    } catch (e: any) {
      setError(e?.message || '启动子任务失败');
    } finally {
      setSubmitting(false);
    }
  }, [prompt, title, submitting, requirementId, loadList]);

  // --- Manual re-split (🔄 重新拆分) -------------------------------------
  // Escape hatch for when StartCoding's auto-orchestration produced no
  // children (main agent didn't decompose) or the user wants a fresh split.
  // POSTs re-orchestrate, then streams the main agent's progress from the
  // returned job so the user sees "重新拆分中…" instead of a dead click.
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
      const { job_id } = await subTasksApi.reOrchestrate(requirementId);
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
      setError(e?.message || '重新拆分失败');
      setReSplitBusy(false);
    }
  }, [reSplitBusy, requirementId, loadList]);

  // Close the re-split stream on unmount.
  useEffect(() => () => { reSplitEsRef.current?.close(); reSplitEsRef.current = null; }, []);

  if (!codingSessionId) {
    return (
      <section className="sub-panel" aria-labelledby="sub-panel-title">
        <header className="sub-panel-header">
          <h3 id="sub-panel-title" className="sub-panel-title">
            <span className="sub-panel-title-icon" aria-hidden="true">🤖</span>
            <span>子任务协作</span>
          </h3>
        </header>
        <div className="sub-panel-hint">
          请先在主 Agent 中执行「开始开发」，主 Agent 完成会自动派发第一批子任务。
          {requirement?.title && <em>（当前需求：{requirement.title}）</em>}
        </div>
      </section>
    );
  }

  // summaryReport renders the Markdown the main agent produced after the
  // last orchestrated batch finished. Lives ABOVE the children list so
  // the user reads the high-level picture first, then drills into any
  // child whose artifact they want to verify.
  const summaryReport = (requirement as any).coding_plan as string | undefined;
  const hasSummary = typeof summaryReport === 'string' && summaryReport.trim() !== '';
  const activeChildCount = activeBatch?.childIds.length ?? 0;

  return (
    <section className="sub-panel" aria-labelledby="sub-panel-title">
      <header className="sub-panel-header">
        <h3 id="sub-panel-title" className="sub-panel-title">
          <span className="sub-panel-title-icon" aria-hidden="true">🤖</span>
          <span>子任务协作</span>
        </h3>
        <span className="sub-panel-meta">
          共享主 Agent 会话 <code>{truncate(codingSessionId, 12)}</code>
          {' · '}
          <span className="sub-panel-count">{items?.length ?? 0}</span> 个子任务
        </span>
      </header>

      {/* Auto-orchestrate: ask the main agent to decompose + dispatch + summarize.
          Distinct from the manual composer below — orchestrate is a SINGLE click
          that creates N children AND a summary report, while the composer is for
          ad-hoc one-off children. */}
      {/* Auto-orchestrate status: the manual "🎯 开始执行" button was
          removed — StartCoding's main agent now does the decomposition +
          dispatch automatically when the user kicks off development.
          What remains is the in-flight badge (so the user knows the
          main agent is dispatching children) and the summary report
          surface (each completed batch refreshes requirements.coding_plan). */}
      {activeChildCount > 0 && (
        <div className="sub-orchestrator-status">
          🪄 主 Agent 自动派发了 {activeChildCount} 个子任务，等待执行完成并生成汇总报告…
        </div>
      )}

      {/* Manual re-split progress: streams the main agent's re-decomposition
          turn so a click never looks dead. */}
      {(reSplitBusy || reSplitLines.length > 0) && (
        <div className="sub-orchestrator-status sub-resplit-status">
          {reSplitBusy ? '🔄 主 Agent 重新拆分任务中…' : '重新拆分结束'}
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
            <span className="sub-summary-icon" aria-hidden="true">📊</span>
            <span className="sub-summary-title">主 Agent 汇总报告</span>
          </header>
          <div className="sub-summary-body">
            <ReactMarkdown remarkPlugins={[remarkGfm]}>{summaryReport!}</ReactMarkdown>
          </div>
        </div>
      )}

      <div className="sub-composer">
        <label className="sub-composer-title-row">
          <span className="sub-composer-label">标题（可选）</span>
          <input
            type="text"
            className="sub-composer-input"
            placeholder="给这个子任务起个名字，方便事后回看"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            disabled={submitting}
            maxLength={80}
          />
        </label>
        <AtMentionTextarea
          value={prompt}
          onChange={setPrompt}
          placeholder="描述这个子任务要做什么…输入 @ 引用 Skill"
          rows={4}
          disabled={submitting}
          className="sub-composer-textarea"
        />
        <div className="sub-composer-toolbar">
          <span className="sub-composer-hint">
            启动后子 Agent 将 fork 主会话上下文，所有子任务共享同一项目认知
          </span>
          {error && <span className="sub-composer-err">{error}</span>}
          <button
            type="button"
            className="btn btn-secondary sub-composer-resplit"
            onClick={onReSplit}
            disabled={reSplitBusy || submitting || anyAlive}
            title={anyAlive ? '有子任务正在执行，完成后才能重新拆分' : '让主 Agent 重新进行任务拆分并自动派发子任务'}
          >
            {reSplitBusy ? '拆分中…' : '🔄 重新拆分'}
          </button>
          <button
            type="button"
            className="btn btn-primary sub-composer-submit"
            onClick={onCreate}
            disabled={submitting || reSplitBusy || !prompt.trim()}
          >
            {submitting ? '启动中…' : '🚀 启动子任务'}
          </button>
        </div>
      </div>

      <div className="sub-list">
        {items === null && <div className="sub-list-loading">加载中…</div>}
        {items && items.length === 0 && (
          <div className="sub-list-empty">
            <div className="sub-list-empty-icon" aria-hidden="true">✨</div>
            <div>暂无子任务。可点击「🔄 重新拆分」让主 Agent 拆分并自动派发，或在上方手动创建。</div>
          </div>
        )}
        {items && items.length > 0 && items.map((st, i) => (
          <SubTaskCard
            key={st.id}
            st={st}
            index={i}
            total={items.length}
            onChanged={onItemChanged}
          />
        ))}
      </div>
    </section>
  );
}
