/**
 * DevelopingStage — orchestrator for the "开发实现" stage on
 * RequirementDetail.
 *
 * Why this exists
 *   Before this refactor the page rendered ONE SSE panel for the main
 *   coding job (`RequirementDetail.tsx:4212-4274`) and ANOTHER SSE panel
 *   inside each SubTaskCard (`SubTaskPanel.tsx:558-613`). For a
 *   requirement with N sub-tasks the user saw N+1 stacked log panels and
 *   had to scroll past all of them to read the next sub-task's progress.
 *
 *   This stage owns a SINGLE SSE log panel (rendered via JobLogView) and a
 *   floating task-list sidebar (DevelopingTaskList). The selected task
 *   drives the panel's content; SSE streams stay attached in the
 *   background for every task so switching is instant.
 *
 * Responsibilities
 *   - Subscribe to the main coding job's SSE stream (was
 *     `streamJob` in RequirementDetail.tsx).
 *   - Subscribe to each sub-task's SSE stream (was per-card in
 *     SubTaskPanel.tsx). Re-subscribes when a sub-task gets a new job_id
 *     (Redo / Continue).
 *   - Render the DevelopingTaskList + JobLogView based on the selected
 *     task. The task list forwards selection to the parent so the
 *     SubTaskPanel can mirror the highlight.
 *   - Persist the collapsed state to localStorage so it survives a
 *     requirement reload.
 *
 * Non-responsibilities
 *   - The page-level status / merge / knowledge / agent server bits
 *     stay in RequirementDetail.tsx. This stage deliberately does NOT
 *     pull in `useFullscreen` for itself; the parent owns that hook and
 *     threads the controller through, mirroring the existing pattern.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { createEventStream, type EventStream } from '../api/stream';
import {
  subTasksApi,
  wizardApi,
  type Requirement,
  type SubTask,
} from '../api/client';
import { appendLogLine, coalesceLogLines, type LogLine, type UsageInfo } from '../utils/logLines';
import { DevelopingTaskList, type TaskKey } from './DevelopingTaskList';
import { JobLogView, type JobLogStatus } from './JobLogView';
import './DevelopingStage.css';

// localStorage key for the collapsed state. Versioned (":v1") so a future
// layout rework can introduce :v2 without trampling existing user prefs.
const COLLAPSED_LS_KEY = 'dev-panel.collapsed:v1';

// Per-task SSE state — the bus entry for one stream. `lines` is the
// append-only log array; `usage` is the latest snapshot; `busy` flips
// while the stream is open; `status` is the higher-level status, derived
// from the job_done frame when available.
interface TaskBus {
  jobId: string;
  lines: LogLine[];
  usage?: UsageInfo;
  busy: boolean;
  status: JobLogStatus;
}

// statusFromJob maps the backend's job_done status / exit_code into the
// JobLogStatus vocabulary.
function statusFromJob(evt: { status?: string; exit_code?: number }): JobLogStatus {
  if (evt.status === 'done' || evt.exit_code === 0) return 'done';
  if (evt.status === 'error' || (evt.exit_code != null && evt.exit_code !== 0)) return 'error';
  if (evt.status === 'stopped') return 'stopped';
  return 'running';
}

// initialStatusForKey derives the initial status from the requirement row
// (main) or the sub-task row (sub-task). For sub-tasks the bus starts in
// the row's persisted status so a page refresh paints the correct badge
// immediately, even before the SSE reconnects.
function initialStatusFor(
  kind: 'main' | 'sub-task',
  row?: { status: string },
): JobLogStatus {
  if (!row) return kind === 'main' ? 'idle' : 'pending';
  const s = row.status;
  if (s === 'done') return 'done';
  if (s === 'error') return 'error';
  if (s === 'stopped') return 'stopped';
  if (s === 'pending') return 'pending';
  if (s === 'running') return 'running';
  return kind === 'main' ? 'idle' : 'pending';
}

export interface DevelopingStageProps {
  requirement: Requirement;
  // Resolved coding job id (last_coding_job_id). null when the dev stage
  // has never been started.
  codingJobId: string | null;
  // Current sub-task list (parent owns the polling). The stage does NOT
  // re-fetch — it just subscribes to whatever job_id each row carries.
  subTasks: SubTask[];
  // Fullscreen controller owned by the parent (RequirementDetail). The
  // stage only forwards the toggle to JobLogView. We accept the actual
  // shape returned by utils/useFullscreen so the caller doesn't have to
  // remap fields.
  fullscreen: { isFullscreen: boolean; toggle: () => void; enter: () => void; exit: () => void };
  // Stage label shown beside the usage bar — typically the developer
  // stage label from i18n (e.g. "开发实现" / "Development").
  stepLabel: string;
  // Coding-phase sentinel from the requirement row, used to render the
  // "正在制定实施步骤…" / "正在拆分子任务…" hint. Optional — defaults to
  // the generic working hint.
  codingPhase?: 'planning' | 'decomposing' | string | null;
  // Compress controls for the main task only. Optional — sub-task
  // selection hides the compress button via `compressible=false`.
  compressing?: boolean;
  compressedAt?: string | null;
  onCompress?: () => void;
  onShowSummary?: () => void;
  // Redo affordance (only rendered on main selection).
  onRedo?: () => void;
  redoLabel?: string;
  redoTitle?: string;
  redoDisabled?: boolean;
  // Optional sub-task selection propagation — the parent SubTaskPanel
  // uses this to keep its per-card "is-selected" highlight in sync.
  onSubTaskSelect?: (key: TaskKey) => void;
}

export function DevelopingStage({
  requirement,
  codingJobId,
  subTasks,
  fullscreen,
  stepLabel,
  codingPhase,
  compressing,
  compressedAt,
  onCompress,
  onShowSummary,
  onRedo,
  redoLabel,
  redoTitle,
  redoDisabled,
  onSubTaskSelect,
}: DevelopingStageProps) {
  const { t } = useTranslation();
  // ── Selection state ────────────────────────────────────────────────
  const [selectedKey, setSelectedKeyRaw] = useState<TaskKey>('main');
  // When the selection changes, propagate it so the SubTaskPanel can keep
  // its per-card highlight in sync. Wrapped so the spread below doesn't
  // tear on every render.
  const setSelectedKey = useCallback((k: TaskKey) => {
    setSelectedKeyRaw(k);
    onSubTaskSelect?.(k);
  }, [onSubTaskSelect]);
  // ── Collapsed state (persisted) ────────────────────────────────────
  const [collapsed, setCollapsed] = useState<boolean>(() => {
    if (typeof window === 'undefined') return false;
    return window.localStorage.getItem(COLLAPSED_LS_KEY) === '1';
  });
  useEffect(() => {
    if (typeof window === 'undefined') return;
    window.localStorage.setItem(COLLAPSED_LS_KEY, collapsed ? '1' : '0');
  }, [collapsed]);
  const toggleCollapsed = useCallback(() => setCollapsed((v) => !v), []);
  // ── SSE bus ─────────────────────────────────────────────────────────
  // `bus` is the source of truth for all rendered logs. Keys: 'main' or
  // a sub-task id. The map is intentionally NOT memoized — we mutate it
  // via functional updates and the parent re-renders with the new state.
  const [bus, setBus] = useState<Record<string, TaskBus>>({});
  // event-stream handles, one per active stream. We keep these in a ref
  // so an SSE callback can mutate them without re-running the effect.
  const streamsRef = useRef<Map<string, EventStream>>(new Map());

  // Helper: writes one bus entry using the previous state.
  const writeBus = useCallback((key: string, patch: Partial<TaskBus>) => {
    setBus((prev) => {
      const cur = prev[key];
      const next: TaskBus = {
        jobId: cur?.jobId ?? '',
        lines: cur?.lines ?? [],
        usage: cur?.usage,
        busy: cur?.busy ?? false,
        status: cur?.status ?? (key === 'main' ? 'idle' : 'pending'),
        ...patch,
      };
      return { ...prev, [key]: next };
    });
  }, []);

  // ── Stream subscriber ──────────────────────────────────────────────
  // Subscribes (or re-subscribes) a key to its current job_id. Idempotent
  // when (key, jobId) hasn't changed. Closes the prior handle first to
  // prevent zombie connections.
  const subscribe = useCallback((key: TaskKey, jobId: string, opts?: { keepDone?: boolean }) => {
    if (!jobId) return;
    const existing = streamsRef.current.get(key);
    if (existing) existing.close();
    writeBus(key, { jobId, busy: true });
    // skipFirst is computed lazily below from the current line count — we
    // don't yet know how many lines the bus already has for this key, so
    // we read it from the next snapshot through a ref.
    let skipped = false;
    let skipRemaining = 0;
    const stream = createEventStream(
      `/api/wizard/jobs/${jobId}/stream`,
      (evt) => {
        const evtType = evt.type;
        if (evtType === 'usage') {
          try {
            const raw = typeof evt.content === 'string' ? JSON.parse(evt.content) : evt.content;
            if (raw && typeof raw === 'object') {
              const parsed = raw as UsageInfo;
              const used = parsed.input_tokens + parsed.cache_creation_tokens + parsed.cache_read_tokens;
              const cw = parsed.context_window || 200000;
              const pct = cw > 0 ? (used / cw) * 100 : 0;
              writeBus(key, { usage: { ...parsed, used, pct } });
            }
          } catch { /* malformed — ignore */ }
          return;
        }
        if (evtType === 'job_done') {
          streamsRef.current.delete(key);
          writeBus(key, {
            busy: false,
            status: statusFromJob(evt),
          });
          return;
        }
        // Skip replay: the first batch of SSE frames after a fresh subscribe
        // duplicates the rows we already hydrated from the snapshot — drop
        // them so the user does not see the log double-render. We compute
        // the count by snapshotting the bus at subscribe time and reading
        // it on the first frame. This matches RequirementDetail.streamJob's
        // skipFirst behavior (lines 1880-1899).
        if (!skipped) {
          skipRemaining = currentBusRef.current[key]?.lines.length ?? 0;
          skipped = true;
        }
        if (skipRemaining > 0) {
          skipRemaining--;
          return;
        }
        if (evtType === 'knowledge' || evtType === 'knowledge_result') {
          // Don't append knowledge events to the log — they are
          // surfaced as a separate list at the top of the page. Same
          // convention as RequirementDetail.streamJob.
          return;
        }
        const at = typeof evt.at === 'number' ? evt.at : Date.now();
        const content = typeof evt.content === 'string'
          ? evt.content
          : evt.content ? JSON.stringify(evt.content) : '';
        setBus((prev) => {
          const cur = prev[key];
          if (!cur) return prev;
          return {
            ...prev,
            [key]: {
              ...cur,
              lines: appendLogLine(cur.lines, { type: evtType, content, at }),
            },
          };
        });
      },
      () => {
        streamsRef.current.delete(key);
        writeBus(key, { busy: false });
      },
    );
    streamsRef.current.set(key, stream);
    // opts is captured above; we just keep it for parity with the original
    // streamJob contract (keepDone reserved for future use).
    void opts;
  }, [writeBus]);

  // currentBusRef mirrors the bus state for read inside the SSE callback
  // — the callback runs against a stale closure otherwise.
  const currentBusRef = useRef(bus);
  useEffect(() => { currentBusRef.current = bus; }, [bus]);

  // ── Subscribe on mount / job_id change ─────────────────────────────
  // Main task — re-subscribes whenever codingJobId changes (StartCoding
  // → new job id; Continue → new job id; etc).
  useEffect(() => {
    if (!codingJobId) {
      writeBus('main', { busy: false, status: 'idle', jobId: '' });
      return;
    }
    // Hydrate from the durable job_logs snapshot so the user sees the
    // existing log immediately after a refresh, before the SSE replay
    // catches up. Mirrors RequirementDetail's restore-on-mount effect
    // (~lines 2402-2430).
    let cancelled = false;
    wizardApi.getJob(codingJobId)
      .then((snap) => {
        if (cancelled) return;
        const lines = (snap.log ?? []).map((l) => ({
          type: l.type as LogLine['type'],
          content: l.content,
          at: 0,
        }));
        const filtered = coalesceLogLines(
          lines.filter((l) => l.type !== 'knowledge' && l.type !== 'knowledge_result'),
        );
        writeBus('main', {
          jobId: codingJobId,
          lines: filtered,
          status: snap.status === 'running' ? 'running' : (snap.status === 'error' ? 'error' : 'done'),
          busy: snap.status === 'running',
        });
        subscribe('main', codingJobId);
      })
      .catch(() => {
        // Snapshot fetch failed — still subscribe so we at least get the
        // SSE frames; the user may see a brief empty state.
        subscribe('main', codingJobId);
      });
    return () => { cancelled = true; };
  }, [codingJobId, subscribe, writeBus]);

  // Sub-tasks — re-subscribes per child when (job_id, id) changes.
  // We deliberately use job_id as the dep so a Redo / Continue that mints
  // a new job_id reopens the stream, while a status flip (running → done)
  // without a new job_id does not.
  //
  // We mirror the main-task effect above: hydrate the bus from the
  // durable job_logs snapshot BEFORE opening the SSE stream. Without this
  // step a finished sub-task whose JobStore ring-buffer slot has already
  // been evicted (cap 50) makes the SSE endpoint return 404, leaving the
  // right pane blank — the user could click the sub-task in the floating
  // list and see nothing. The snapshot endpoint falls back to the durable
  // jobLogSvc.Get path server-side, so the lines surface even across a
  // backend restart.
  useEffect(() => {
    let cancelled = false;
    const desired = new Map<string, string>();
    for (const st of subTasks) {
      if (st.job_id) desired.set(st.id, st.job_id);
    }
    // Close streams whose job is no longer present (e.g. sub-task removed).
    for (const [key, stream] of streamsRef.current.entries()) {
      if (key === 'main') continue;
      if (!desired.has(key)) {
        stream.close();
        streamsRef.current.delete(key);
      }
    }
    // Seed bus entries for sub-tasks we haven't seen yet, and hydrate
    // from the durable log snapshot so a finished sub-task renders its
    // lines immediately when the user clicks it in the floating list.
    for (const st of subTasks) {
      if (!bus[st.id]) {
        writeBus(st.id, {
          jobId: st.job_id,
          status: initialStatusFor('sub-task', st),
          busy: st.status === 'running' || st.status === 'pending',
        });
      } else {
        // Update the status field when the row changes — keeps the chip
        // in sync between SSE-driven writes.
        writeBus(st.id, { status: initialStatusFor('sub-task', st) });
      }
      if (!st.job_id) continue;
      // Hydrate from snapshot first, then subscribe for live updates.
      // The subscription writes `skipRemaining = currentBusRef.current[key]?.lines.length`
      // on the first frame, so any lines we just pushed via the snapshot
      // are dropped from the SSE replay — preventing a double-render when
      // the ring buffer still holds the job.
      const stId = st.id;
      const stJobId = st.job_id;
      wizardApi.getJob(stJobId)
        .then((snap) => {
          if (cancelled) return;
          if (snap.log && snap.log.length > 0) {
            const lines = (snap.log ?? []).map((l) => ({
              type: l.type as LogLine['type'],
              content: l.content,
              at: 0,
            }));
            const filtered = coalesceLogLines(
              lines.filter((l) => l.type !== 'knowledge' && l.type !== 'knowledge_result'),
            );
            setBus((prev) => {
              const cur = prev[stId];
              if (!cur) return prev;
              // Don't overwrite a bus entry that already accumulated live
              // SSE frames since the snapshot was requested — the live
              // frames are newer than the snapshot, so they'd be lost.
              if (cur.lines.length >= filtered.length) return prev;
              return { ...prev, [stId]: { ...cur, lines: filtered } };
            });
          }
          subscribe(stId, stJobId);
        })
        .catch(() => {
          // Snapshot fetch failed — still subscribe so we at least get
          // the SSE frames; the user may see a brief empty state.
          subscribe(stId, stJobId);
        });
    }
    // We deliberately exclude `bus` from the dep array — it would loop
    // every render. The inner `bus[st.id]` read is fine because we never
    // mutate the bus shape, only patch entries via writeBus/setBus.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    return () => { cancelled = true; };
  }, [subTasks, subscribe, writeBus]);

  // Cleanup on unmount — close every active stream.
  useEffect(() => () => {
    for (const stream of streamsRef.current.values()) stream.close();
    streamsRef.current.clear();
  }, []);

  // ── Resolve selection ──────────────────────────────────────────────
  // If the previously-selected sub-task disappears from the list, fall
  // back to 'main'. Keeps the panel from rendering with a stale key.
  useEffect(() => {
    if (selectedKey !== 'main' && !subTasks.some((st) => st.id === selectedKey)) {
      setSelectedKeyRaw('main');
    }
  }, [subTasks, selectedKey]);

  // ── Selected entry → render inputs ─────────────────────────────────
  const selectedEntry: TaskBus = useMemo(() => {
    if (selectedKey === 'main') {
      return bus.main ?? { jobId: '', lines: [], status: 'idle', busy: false };
    }
    return bus[selectedKey] ?? { jobId: '', lines: [], status: 'pending', busy: false };
  }, [bus, selectedKey]);

  const selectedSubTask = useMemo(
    () => (selectedKey === 'main' ? null : subTasks.find((st) => st.id === selectedKey) ?? null),
    [subTasks, selectedKey],
  );
  const selectedTitle = selectedKey === 'main'
    ? (requirement.title || t('components.devStage.mainTask'))
    : (selectedSubTask?.title || t('components.devStage.mainTask'));
  const isMain = selectedKey === 'main';

  // Main-task select always exposes compress + redo affordances. Sub-task
  // selections run compressible=false (the per-card adjust composer covers
  // model-switch / Redo / Continue) and hide the redo button.
  const showCompress = isMain && !!onCompress;
  const showRedo = isMain && !!onRedo;
  // When viewing a sub-task, the busy hint pulls from its row status
  // instead of the SSE handler's `busy` flag — sub-tasks are alive even
  // when their own SSE stream hasn't re-attached yet (post-refresh).
  const busy = isMain
    ? selectedEntry.busy
    : (selectedEntry.busy || selectedSubTask?.status === 'running' || selectedSubTask?.status === 'pending');

  return (
    <div className={`dev-stage${collapsed ? ' is-collapsed' : ''}`}>
      <DevelopingTaskList
        main={{
          jobId: codingJobId,
          status: isMain && selectedEntry.busy ? 'running' : (selectedEntry.status === 'idle' ? 'idle' : selectedEntry.status),
          title: requirement.title || '',
        }}
        items={subTasks}
        selectedKey={selectedKey}
        onSelect={setSelectedKey}
        collapsed={collapsed}
        onToggleCollapsed={toggleCollapsed}
      />
      <div className="dev-stage-main">
        <JobLogView
          title={selectedTitle}
          lines={selectedEntry.lines}
          usage={isMain ? selectedEntry.usage : undefined}
          busy={busy}
          status={selectedEntry.status}
          stepLabel={isMain ? stepLabel : t('components.devStage.mainTask')}
          codingPhase={isMain ? codingPhase : null}
          compressible={showCompress}
          compressing={compressing}
          compressedAt={isMain ? compressedAt : undefined}
          onCompress={onCompress}
          onShowSummary={isMain ? onShowSummary : undefined}
          showRedo={showRedo}
          redoLabel={redoLabel}
          redoTitle={redoTitle}
          redoDisabled={redoDisabled}
          onRedo={onRedo}
          fullscreen={{ active: fullscreen.isFullscreen, onToggle: fullscreen.toggle }}
        />
      </div>
    </div>
  );
}

// Silence unused-import lint when subTasksApi isn't referenced — kept for
// future inline controls (e.g. a "Stop all" button).
void subTasksApi;

export default DevelopingStage;