// RequirementSplitModal — the 方案拆分 (requirement split) preview.
//
// The flow mirrors the wizard's three role-gated stages: the AI produces
// CANDIDATES, the human edits / deletes / reorders / wires up the
// prerequisites, and only then does anything hit the database. The model
// never gets to declare the split "done" on its own.
//
// What the user can change before confirming:
//   · the title / description / acceptance of each candidate
//   · the order (which is also the ordinal the dependency edges refer to)
//   · which candidates a candidate depends on (only EARLIER ones — a forward
//     reference would be a cycle)
//   · which candidates reference each other (context sharing, NOT a gate)
//
// The dependency ("前置任务") and the reference ("互相参考") are two
// different things and the UI keeps them apart:
//   · dependency = an execution-order gate. A dependent cannot start coding
//     until its prerequisite is done.
//   · reference  = context sharing. It only enriches the design prompt and
//     blocks nothing.
//
// The candidate list lives in memory (the backend's job result is
// in-process state), so a page refresh loses it — the modal says so rather
// than pretending otherwise.

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  ApiError,
  requirementsApi,
  type SplitReference,
  type SplitRunState,
  type SplitTaskCandidate,
} from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import { fmtDateTime } from '../utils/intl';
import { appendLogLine, type LogLine } from '../utils/logLines';
import { BranchPicker } from './BranchPicker';
import { ExecEnvSelect, type ExecEnvServer } from './ExecEnvSelect';
import { IconClose } from './icons';
import ModelSelect from './ModelSelect';
import './RequirementSplitModal.css';

/**
 * How long the confirm request may run before the UI gives up on it.
 *
 * The confirm prepares the stack-bottom branch and PUSHES it to origin, so
 * it is the one call in this flow that talks to the network. The backend
 * bounds its own git work, but the client must not sit on "创建中…" forever
 * either — after this budget the request is aborted and the user is told to
 * retry. 150s is comfortably above a legitimate slow push on a big repo.
 */
const SPLIT_CONFIRM_TIMEOUT_MS = 150_000;

/**
 * The modal runs in four stages:
 *
 *   loading → the modal just opened and is asking the backend whether this
 *             requirement has a split run to restore.
 *   config  → the user picks the model / execution environment / source
 *             branch. NOTHING is sent to the backend here.
 *   running → the AI is producing the candidates (SSE progress).
 *   review  → the candidate table; the user edits and confirms.
 *
 * The config stage exists because the split is the one entry point that used
 * to fire the AI the instant the dialog opened — the user never got a say in
 * which model ran, on which machine, or on top of which branch.
 *
 * The loading stage exists because the modal is **conditionally rendered** by
 * the parent (`{splitOpen && <RequirementSplitModal/>}`) — closing it unmounts
 * the component and throws away every piece of local state. The only way the
 * reopened modal can show the previous run is by asking the backend for it
 * (GET /split/state), which is what the loading stage waits for.
 */
type SplitStage = 'loading' | 'config' | 'running' | 'review';

export interface RequirementSplitModalProps {
  open: boolean;
  onClose: () => void;
  requirementId: string;
  requirementTitle: string;
  /** The master's CURRENT base — the default value of the source-branch
   *  picker, and what the stack-bottom branch is created from. */
  baseBranch: string;
  /** The master's own dev branch (feat/req_<master>) — the stack bottom every
   *  child develops on. */
  masterBranch: string;
  /** Branch candidates for the source-branch picker. */
  branches: string[];
  /** Ready Agent Servers (the parent already filters by status === 'ready'). */
  agentServers: ExecEnvServer[];
  /** Initial model / Claude config / execution environment, taken from the
   *  page's design toolbar. */
  initialModel?: string;
  initialConfigId?: string;
  initialAgentServerId?: string;
  /** The real model name shown next to the empty (default model) option. */
  initialModelName?: string;
  /** Fired after the candidates were persisted. */
  onConfirmed: () => void;
}

/** A candidate as the modal holds it: the API shape plus the editable bits. */
interface Row {
  title: string;
  description: string;
  scope: string[];
  acceptance: string[];
  boundary: string;
  /** 1-based ordinal within the batch — the dependency edges refer to it. */
  order: number;
  /** Ordinals (1-based) this candidate depends on. */
  dependsOn: number[];
}

/**
 * One "互相参考" pair as the modal holds it.
 *
 * `a` / `b` are 1-based ordinals into the CURRENT candidate list. The
 * model's judgement is kept separately from the user's selection so the UI
 * can show "AI 建议" (with the reason) next to the checkbox — the user can
 * disagree with one pair without losing the rest of the judgement.
 */
interface RefPair {
  a: number;
  b: number;
  /** The model's justification — empty when the model gave none. */
  reason: string;
}

/** The backend's candidate shape (the `result.tasks` payload on the SSE frame). */
interface RawCandidate {
  title?: string;
  description?: string;
  scope?: string[];
  acceptance?: string[];
  boundary?: string;
  order?: number;
  depends_on?: number[];
}

function toRow(raw: RawCandidate, index: number): Row {
  return {
    title: (raw.title ?? '').trim(),
    description: (raw.description ?? '').trim(),
    scope: Array.isArray(raw.scope) ? raw.scope : [],
    acceptance: Array.isArray(raw.acceptance) ? raw.acceptance : [],
    boundary: (raw.boundary ?? '').trim(),
    order: raw.order && raw.order > 0 ? raw.order : index + 1,
    dependsOn: Array.isArray(raw.depends_on) ? raw.depends_on : [],
  };
}

/** Splits a textarea's content into a trimmed, non-empty list. */
function lines(text: string): string[] {
  return text
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l !== '');
}

/**
 * Pulls the candidates out of a terminal frame's `result`.
 *
 * Three shapes are accepted on purpose:
 *   ① `{tasks: [...]}`  — the backend's current contract
 *   ② `[...]`           — the bare array the backend used to send
 *   ③ `{candidates: []}`— a shape a future backend might pick
 *
 * The backend and the frontend shipped out of sync for a while (the backend
 * sent a bare array, the frontend read `result.tasks`), and the split UI was
 * therefore unusable — every run ended in "拆分结果已失效". Accepting all three
 * means neither side can break the other again.
 */
function extractCandidates(result: unknown): RawCandidate[] {
  if (Array.isArray(result)) return result as RawCandidate[];
  if (result && typeof result === 'object') {
    const obj = result as Record<string, unknown>;
    if (Array.isArray(obj.tasks)) return obj.tasks as RawCandidate[];
    if (Array.isArray(obj.candidates)) return obj.candidates as RawCandidate[];
  }
  return [];
}

/**
 * Pulls the model's "互相参考" suggestions out of a terminal frame's
 * `result`.
 *
 * Same tolerance as extractCandidates: the field is optional (a backend that
 * does not produce references yet must not break the modal), and the pairs
 * may live next to the candidates or on their own.
 */
function extractReferences(result: unknown): SplitReference[] {
  if (!result || typeof result !== 'object' || Array.isArray(result)) return [];
  const obj = result as Record<string, unknown>;
  if (!Array.isArray(obj.references)) return [];
  return (obj.references as SplitReference[]).filter(
    (r) => r && Number.isFinite(r.from) && Number.isFinite(r.to) && r.from !== r.to,
  );
}

/** Normalizes a pair into ascending order — the reference is undirected. */
function normPair(a: number, b: number): [number, number] {
  return a < b ? [a, b] : [b, a];
}

/** True when the pair (a,b) is currently selected. */
function pairSelected(refs: [number, number][], a: number, b: number): boolean {
  const [x, y] = normPair(a, b);
  return refs.some((p) => p[0] === x && p[1] === y);
}

/**
 * Rewrites the reference pairs after the candidate list was reordered or a
 * candidate was deleted.
 *
 * Ordinals are POSITIONAL, so deleting candidate 3 shifts every pair that
 * pointed at candidates 4+ down by one, and a pair that pointed at the
 * deleted candidate itself is dropped (the map simply has no entry for it).
 *
 * The old implementation threw the references away on every edit, which
 * silently discarded the model's architectural judgement — the whole point
 * of asking for it.
 */
function remapPairs(
  refs: [number, number][],
  oldToNew: Map<number, number>,
): [number, number][] {
  const out: [number, number][] = [];
  const seen = new Set<string>();
  for (const [a, b] of refs) {
    const na = oldToNew.get(a);
    const nb = oldToNew.get(b);
    if (na === undefined || nb === undefined) continue;
    const [x, y] = normPair(na, nb);
    const key = `${x}-${y}`;
    if (seen.has(key)) continue;
    seen.add(key);
    out.push([x, y]);
  }
  return out;
}

/** The same remapping for the model's suggestions (they carry a reason). */
function remapRefPairs(refs: RefPair[], oldToNew: Map<number, number>): RefPair[] {
  const out: RefPair[] = [];
  const seen = new Set<string>();
  for (const r of refs) {
    const na = oldToNew.get(r.a);
    const nb = oldToNew.get(r.b);
    if (na === undefined || nb === undefined) continue;
    const [x, y] = normPair(na, nb);
    const key = `${x}-${y}`;
    if (seen.has(key)) continue;
    seen.add(key);
    out.push({ a: x, b: y, reason: r.reason });
  }
  return out;
}

export function RequirementSplitModal({
  open,
  onClose,
  requirementId,
  requirementTitle,
  baseBranch,
  masterBranch,
  branches,
  agentServers,
  initialModel,
  initialConfigId,
  initialAgentServerId,
  initialModelName,
  onConfirmed,
}: RequirementSplitModalProps) {
  const { t } = useTranslation();

  const [rows, setRows] = useState<Row[]>([]);
  const [stage, setStage] = useState<SplitStage>('config');
  /** The visible SSE stream — every phase / tool_call / message line. */
  const [streamLines, setStreamLines] = useState<LogLine[]>([]);
  /** Whether the stream panel is expanded. */
  const [streamOpen, setStreamOpen] = useState(true);
  /** The most recent split run of this requirement — the status strip's data. */
  const [runState, setRunState] = useState<SplitRunState | null>(null);
  /** This session restored a previous run rather than starting a new one. */
  const [restored, setRestored] = useState(false);
  /** The SSE dropped and is being re-attached. */
  const [reconnecting, setReconnecting] = useState(false);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  /** Seconds the confirm request has been running — shown on the button so a
   *  slow push reads as "working" rather than "stuck". */
  const [confirmElapsed, setConfirmElapsed] = useState(0);
  /** Candidate pairs the user ticked for mutual reference: [a, b] ordinals. */
  const [references, setReferences] = useState<[number, number][]>([]);
  /** The model's own "互相参考" judgement (with reasons) — rendered as the
   *  recommended list; the user may disagree with a pair. */
  const [aiRefs, setAiRefs] = useState<RefPair[]>([]);
  /** Whether the full N×N matrix is expanded (it is long — 11 candidates
   *  make 55 pairs — so it stays hidden until asked for). */
  const [refsExpanded, setRefsExpanded] = useState(false);
  /** The branch the master's own dev branch is created from. */
  const [sourceBranch, setSourceBranch] = useState<string>(baseBranch || '');
  /** The model this split run uses (empty = the architect persona default). */
  const [model, setModel] = useState<string>(initialModel || '');
  /** The claude_configs row this split run uses (empty = the active config). */
  const [configId, setConfigId] = useState<string>(initialConfigId || '');
  /** The execution environment (empty = this machine). */
  const [agentServerId, setAgentServerId] = useState<string>(() => {
    // Guard against an AgentServer id that has dropped off the ready list
    // since the page last fetched — same fallback DesignCodingImmediateModal
    // uses. A stale id would send the split to a target that no longer
    // exists.
    if (!initialAgentServerId) return '';
    return agentServers.some((s) => s.id === initialAgentServerId)
      ? initialAgentServerId
      : '';
  });

  const streamRef = useRef<EventStream | null>(null);
  /** True once the modal unmounted — guards the async start() against
   *  touching state that no longer exists. */
  const cancelledRef = useRef(false);
  /** Attach generation — every (re)attach bumps it, so late frames from a
   *  previous stream are dropped instead of interleaving with the new ones. */
  const genRef = useRef(0);
  /** The reconnect backoff step (1s → 2s → 4s → 8s → 10s). */
  const attemptRef = useRef(0);
  /** The pending reconnect timer / state-poll interval. */
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);
  /** A terminal frame was received — the stream ending afterwards is normal
   *  shutdown, not a failure. */
  const terminalRef = useRef(false);
  /** The stream panel's scroll container. */
  const streamBodyRef = useRef<HTMLDivElement | null>(null);

  // attach / applyRunState / startPollingState call each other. Routing the
  // recursion through refs breaks the useCallback dependency cycle (React
  // would otherwise rebuild them on every render and the reconnect timer
  // would keep a stale closure).
  const attachRef = useRef<(jobId: string) => void>(() => {});
  const applyStateRef = useRef<(st: SplitRunState) => void>(() => {});
  const pollFnRef = useRef<() => void>(() => {});

  const close = useCallback(() => {
    streamRef.current?.close();
    streamRef.current = null;
    if (timerRef.current) {
      clearTimeout(timerRef.current);
      timerRef.current = null;
    }
    if (pollRef.current) {
      clearInterval(pollRef.current);
      pollRef.current = null;
    }
    genRef.current++; // invalidate any late frame from the old stream
    setRows([]);
    setStage('config');
    setError('');
    setReferences([]);
    setAiRefs([]);
    setRefsExpanded(false);
    setConfirmElapsed(0);
    setStreamLines([]);
    setRunState(null);
    setRestored(false);
    setReconnecting(false);
    onClose();
  }, [onClose]);

  // Seed the source-branch picker from the master's current base whenever
  // the modal opens (the parent's base can change between two splits).
  useEffect(() => {
    if (!open) return;
    setSourceBranch(baseBranch || '');
  }, [open, baseBranch]);

  // The split no longer starts on its own: the user configures the run
  // (model / execution environment / source branch) first and then presses
  // the start button. This effect only tears the SSE stream down on unmount.
  useEffect(() => {
    cancelledRef.current = false;
    return () => {
      cancelledRef.current = true;
      streamRef.current?.close();
      streamRef.current = null;
      if (timerRef.current) {
        clearTimeout(timerRef.current);
        timerRef.current = null;
      }
      if (pollRef.current) {
        clearInterval(pollRef.current);
        pollRef.current = null;
      }
      // Bump the generation so any late frame from the old stream is
      // discarded. The ref is captured in a local first — the cleanup must
      // not read whatever .current happens to hold by the time it runs.
      const gen = genRef;
      gen.current++;
    };
  }, []);

  // pushLine appends a frame to the visible stream, keeping the last 200.
  const pushLine = useCallback((line: LogLine) => {
    setStreamLines((prev) => {
      const next = appendLogLine(prev, line);
      return next.length > 200 ? next.slice(next.length - 200) : next;
    });
  }, []);

  // attach subscribes to a split job's SSE and owns the whole frame
  // lifecycle: live frames, the terminal frame, and reconnection.
  const attach = useCallback(
    (jobId: string) => {
      // Close the previous stream and clear the lines first — a reconnect
      // replays the job's whole history, so stale lines would double up.
      streamRef.current?.close();
      streamRef.current = null;
      setStreamLines([]);
      terminalRef.current = false;
      attemptRef.current = 0;

      const gen = ++genRef.current;
      const stream = createEventStream(
        requirementsApi.splitStreamUrl(requirementId, jobId),
        (data: any) => {
          if (gen !== genRef.current) return; // a stale stream's frame
          if (cancelledRef.current) return;
          const type = String(data?.type ?? '');
          if (type === 'job_done') {
            terminalRef.current = true;
            setReconnecting(false);
            // Close it ourselves: createEventStream fires onError when the
            // stream ends, and that would overwrite the terminal state with
            // a bogus "拆分失败".
            stream.close();
            if (data.status === 'error') {
              const msg = String(data.error_message || data.error_kind || '');
              setError(msg);
              setRunState((prev) =>
                prev
                  ? {
                      ...prev,
                      status: 'error',
                      running: false,
                      error_message: msg,
                      finished_at: Number(data.finished_at) || prev.finished_at,
                    }
                  : prev,
              );
              setStage('config');
              return;
            }
            const tasks = extractCandidates(data?.result);
            if (tasks.length === 0) {
              // The candidate list is in-process state — a backend restart
              // (or an eviction from the registry) loses it.
              setError(t('requirements.split.errExpired'));
              setStage('config');
              return;
            }
            const next = tasks.map(toRow).sort((a, b) => a.order - b.order);
            const ordered = next.map((r, i) => ({ ...r, order: i + 1 }));
            setRows(ordered);
            // The model's "互相参考" judgement comes with the candidates: the
            // user cannot work this layer out on their own, so the model
            // proposes the pairs (with a reason) and the user confirms.
            const refs = extractReferences(data?.result);
            const pairs = refs
              .filter((r) => r.from >= 1 && r.to >= 1 && r.from <= ordered.length && r.to <= ordered.length)
              .map((r) => {
                const [a, b] = normPair(r.from, r.to);
                return { a, b, reason: (r.reason || '').trim() };
              });
            setAiRefs(pairs);
            setReferences(pairs.map((p) => [p.a, p.b] as [number, number]));
            // Keep the status strip in sync — it counted the candidates from
            // the state payload, which was still the "running" snapshot.
            setRunState((prev) =>
              prev
                ? {
                    ...prev,
                    status: 'done',
                    running: false,
                    // The terminal frame carries the completion time — without
                    // it the strip's "generated N candidates · <time>" would
                    // have no time for a run that finished in this session.
                    finished_at: Number(data.finished_at) || prev.finished_at,
                    candidates: ordered.map((r) => ({
                      title: r.title,
                      description: r.description,
                      scope: r.scope,
                      acceptance: r.acceptance,
                      boundary: r.boundary,
                      order: r.order,
                      depends_on: r.dependsOn,
                    })),
                    references: pairs.map((p) => ({ from: p.a, to: p.b, reason: p.reason })),
                  }
                : prev,
            );
            setStage('review');
            return;
          }
          if (type === 'error') {
            // The backend writes the user-visible reason into an "error"
            // line — it used to be dropped, leaving the user with a bare
            // "拆分失败：" and no cause.
            const msg = String(data.content ?? '');
            if (msg) setError(msg);
            pushLine({ type: 'error', content: msg, at: data.at });
            return;
          }
          if (
            type === 'phase' ||
            type === 'message' ||
            type === 'tool_call' ||
            type === 'tool_result'
          ) {
            setReconnecting(false); // a frame arrived — we're connected again
            pushLine({ type, content: String(data.content ?? ''), at: data.at });
          }
        },
        () => {
          if (gen !== genRef.current) return;
          if (cancelledRef.current) return;
          if (terminalRef.current) return; // normal shutdown after the terminal frame
          setReconnecting(true);
          // Back off: 1s → 2s → 4s → 8s → 10s (capped).
          const attempt = attemptRef.current++;
          if (attempt >= 4) {
            // Reconnecting repeatedly failed → fall back to polling the state
            // endpoint. The registry lives outside the JobStore ring buffer,
            // so the status and the candidates survive even when the job was
            // evicted.
            pollFnRef.current();
            return;
          }
          const delay = Math.min(1000 * Math.pow(2, attempt), 10000);
          timerRef.current = setTimeout(() => {
            if (gen !== genRef.current) return;
            attachRef.current(jobId);
          }, delay);
        },
      );
      streamRef.current = stream;
    },
    [requirementId, t, pushLine],
  );

  // applyRunState folds a state payload into the modal: which stage to show,
  // whether to (re)attach the stream, and what the candidate table holds.
  const applyRunState = useCallback(
    (st: SplitRunState) => {
      setRunState(st);
      if (st.status === 'running' && st.job_id) {
        setStage('running');
        setError('');
        setRestored(true); // this is a previous run, not a fresh one
        attachRef.current(st.job_id); // reattach to the split still running
        return;
      }
      if (st.status === 'done' && st.candidates && st.candidates.length > 0) {
        const next = st.candidates.map(toRow).sort((a, b) => a.order - b.order);
        const ordered = next.map((r, i) => ({ ...r, order: i + 1 }));
        setRows(ordered);
        // The restored run carries the model's reference judgement too —
        // closing and reopening the modal must not lose it.
        const pairs = (st.references || [])
          .filter((r) => r.from >= 1 && r.to >= 1 && r.from <= ordered.length && r.to <= ordered.length)
          .map((r) => {
            const [a, b] = normPair(r.from, r.to);
            return { a, b, reason: (r.reason || '').trim() };
          });
        setAiRefs(pairs);
        setReferences(pairs.map((p) => [p.a, p.b] as [number, number]));
        setStage('review'); // show the previous candidates straight away
        setError('');
        setRestored(true);
        return;
      }
      if (st.status === 'error') {
        setError(st.error_message || t('requirements.split.errGeneric', { msg: '' }));
        setStage('config');
        return;
      }
      // never ran, or the candidates are gone → back to the config stage
      setStage('config');
    },
    [t],
  );

  // startPollingState is the fallback for when the SSE cannot be reattached.
  // It polls the state endpoint rather than the job: the registry lives
  // outside the JobStore ring buffer, so the status and the candidates
  // survive even when the job itself was evicted.
  const startPollingState = useCallback(
    () => {
      if (pollRef.current) clearInterval(pollRef.current);
      pollRef.current = setInterval(() => {
        void (async () => {
          try {
            const st = await requirementsApi.splitState(requirementId);
            if (cancelledRef.current) return;
            if (st.status === 'running') return; // still going, keep waiting
            if (pollRef.current) {
              clearInterval(pollRef.current);
              pollRef.current = null;
            }
            applyStateRef.current(st);
          } catch {
            /* network blip — retry on the next tick */
          }
        })();
      }, 3000);
    },
    [requirementId],
  );

  // Keep the refs pointing at the latest implementations.
  attachRef.current = attach;
  applyStateRef.current = applyRunState;
  pollFnRef.current = startPollingState;

  // Ask the backend what this requirement's last split looks like as soon as
  // the modal opens. The parent renders the modal conditionally, so closing
  // it destroys every piece of local state — this is the only way the
  // reopened modal can pick the previous run back up.
  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setStage('loading');
    void (async () => {
      try {
        const st = await requirementsApi.splitState(requirementId);
        if (cancelled) return;
        applyStateRef.current(st);
      } catch {
        if (cancelled) return;
        setStage('config'); // can't tell → behave as if it never ran
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [open, requirementId]);

  // Keep the stream panel pinned to the newest line.
  useEffect(() => {
    const el = streamBodyRef.current;
    if (!el) return;
    el.scrollTop = el.scrollHeight;
  }, [streamLines]);

  // start kicks off one split run with the configuration the user picked.
  const start = useCallback(async () => {
    setRows([]);
    setStreamLines([]);
    setError('');
    setReferences([]);
    setAiRefs([]);
    setStage('running');
    setRestored(false); // this one is a fresh run
    setReconnecting(false);
    try {
      const { job_id } = await requirementsApi.split(requirementId, {
        model: model || undefined,
        claude_config_id: configId || undefined,
        agent_server_id: agentServerId || undefined,
      });
      if (cancelledRef.current) return;
      // Publish the fresh run into the status strip right away — the strip is
      // meant to be there for the whole life of a split, not only after the
      // modal was reopened.
      setRunState({
        status: 'running',
        job_id,
        running: true,
        candidates: [],
      });
      attachRef.current(job_id);
    } catch (err) {
      if (cancelledRef.current) return;
      // Another tab is already splitting this requirement. Don't surface an
      // error — take over the run that is already going.
      if (err instanceof ApiError && err.code === 'SPLIT_IN_PROGRESS') {
        try {
          const st = await requirementsApi.splitState(requirementId);
          if (!cancelledRef.current) applyStateRef.current(st);
        } catch {
          /* ignore */
        }
        return;
      }
      setError(err instanceof ApiError ? err.message : t('requirements.split.errGeneric', { msg: String(err) }));
      setStage('config');
    }
  }, [requirementId, model, configId, agentServerId, t]);

  const update = (index: number, patch: Partial<Row>) => {
    setRows((prev) => prev.map((r, i) => (i === index ? { ...r, ...patch } : r)));
  };

  const remove = (index: number) => {
    const next = rows.filter((_, i) => i !== index);
    // Re-number and drop the dangling dependency ordinals — the ordinals
    // are positional, so removing a row shifts everything below it.
    const remap = new Map<number, number>();
    next.forEach((r, i) => remap.set(r.order, i + 1));
    setRows(
      next.map((r, i) => ({
        ...r,
        order: i + 1,
        dependsOn: r.dependsOn
          .map((d) => remap.get(d))
          .filter((d): d is number => d !== undefined && d < i + 1),
      })),
    );
    // The references follow the renumbering instead of being thrown away —
    // the model's judgement is worth keeping across an edit.
    setReferences((prev) => remapPairs(prev, remap));
    setAiRefs((prev) => remapRefPairs(prev, remap));
  };

  const move = (index: number, delta: number) => {
    const target = index + delta;
    if (target < 0 || target >= rows.length) return;
    const next = [...rows];
    const [moved] = next.splice(index, 1);
    next.splice(target, 0, moved);
    // Ordinals are positional — renumber and fix the edges.
    const remap = new Map<number, number>();
    next.forEach((r, i) => remap.set(r.order, i + 1));
    setRows(
      next.map((r, i) => ({
        ...r,
        order: i + 1,
        dependsOn: r.dependsOn
          .map((d) => remap.get(d))
          .filter((d): d is number => d !== undefined && d < i + 1),
      })),
    );
    setReferences((prev) => remapPairs(prev, remap));
    setAiRefs((prev) => remapRefPairs(prev, remap));
  };

  const toggleDependency = (index: number, dep: number) => {
    setRows((prev) =>
      prev.map((r, i) => {
        if (i !== index) return r;
        const has = r.dependsOn.includes(dep);
        return {
          ...r,
          dependsOn: has ? r.dependsOn.filter((d) => d !== dep) : [...r.dependsOn, dep].sort((a, b) => a - b),
        };
      }),
    );
  };

  const toggleReference = (a: number, b: number) => {
    setReferences((prev) => {
      const exists = prev.some((p) => p[0] === a && p[1] === b);
      if (exists) return prev.filter((p) => !(p[0] === a && p[1] === b));
      // A reference is undirected — store it once, in order.
      const key: [number, number] = a < b ? [a, b] : [b, a];
      return [...prev, key];
    });
  };

  const confirm = async () => {
    setBusy(true);
    setError('');
    setConfirmElapsed(0);
    // The confirm pushes the stack-bottom branch to origin — the one call
    // here that talks to the network. Show the elapsed seconds so a slow
    // push reads as "working", and abort it if it outstays its welcome:
    // the user must never be stuck on "创建中…" with no way out.
    const ticker = setInterval(() => setConfirmElapsed((n) => n + 1), 1000);
    const controller = new AbortController();
    const abortTimer = setTimeout(() => controller.abort(), SPLIT_CONFIRM_TIMEOUT_MS);
    try {
      const tasks: SplitTaskCandidate[] = rows.map((r, i) => ({
        title: r.title.trim(),
        description: r.description.trim(),
        scope: r.scope,
        acceptance: r.acceptance,
        boundary: r.boundary,
        order: i + 1,
        depends_on: r.dependsOn,
      }));
      const res = await requirementsApi.confirmSplit(
        requirementId,
        {
          tasks,
          references,
          source_branch: sourceBranch.trim() || undefined,
        },
        controller.signal,
      );
      const warn = res.warnings && res.warnings.length > 0 ? res.warnings.join('；') : '';
      close();
      if (warn) {
        // Surface the soft failure — the children landed, only the references
        // did not.
        setError(warn);
      }
      onConfirmed();
    } catch (err) {
      if (controller.signal.aborted) {
        setError(t('requirements.split.confirmTimeout'));
      } else {
        setError(err instanceof ApiError ? err.message : t('requirements.split.errGeneric', { msg: String(err) }));
      }
    } finally {
      clearInterval(ticker);
      clearTimeout(abortTimer);
      setBusy(false);
    }
  };

  // The candidates a row may depend on: only the EARLIER ones. A forward
  // reference would be a cycle.
  const possibleDeps = useMemo(
    () => (index: number) => rows.slice(0, index).map((_, i) => i + 1),
    [rows],
  );

  const referencePairs = useMemo(() => {
    const pairs: [number, number][] = [];
    for (let a = 1; a <= rows.length; a++) {
      for (let b = a + 1; b <= rows.length; b++) pairs.push([a, b]);
    }
    return pairs;
  }, [rows.length]);

  if (!open) return null;

  const canConfirm = !busy && stage === 'review' && rows.length > 0 && rows.every((r) => r.title.trim() !== '');

  // The finished-at timestamp, rendered in the app's locale. 0 means "not
  // finished yet" — the zero time's UnixMilli would otherwise show 1969.
  const finishedAt = runState?.finished_at && runState.finished_at > 0
    ? fmtDateTime(runState.finished_at)
    : '';

  return (
    <div className="modal-overlay" onClick={() => !busy && close()}>
      <div className="modal-box req-split-modal" onClick={(e) => e.stopPropagation()}>
        <div className="modal-header">
          <h3>
            {t('requirements.split.modalTitle')}
            {requirementTitle && <span className="req-split-modal-subject"> · {requirementTitle}</span>}
          </h3>
          <button
            type="button"
            className="btn btn-sm"
            onClick={close}
            disabled={busy}
            aria-label={t('requirements.split.cancel')}
            title={t('requirements.split.cancel')}
          >
            <IconClose size={12} />
          </button>
        </div>

        <div className="req-split-body">
          {/* ── 状态条（常驻）──
              "关掉弹窗再打开能看到上次拆分的状态" 的主载体。它跟着最近一次
              拆分 run 走，与当前处于哪个阶段无关。 */}
          {runState && runState.status !== 'none' && (
            <div className={`req-split-status req-split-status--${runState.status}`}>
              <span className="req-split-status-dot" aria-hidden />
              <span className="req-split-status-text">
                {runState.status === 'running' && t('requirements.split.statusRunning')}
                {runState.status === 'done' && t('requirements.split.statusDone')}
                {runState.status === 'error' && t('requirements.split.statusError')}
              </span>
              <span className="req-split-status-meta">
                {runState.status === 'running' && t('requirements.split.statusMetaRunning')}
                {runState.status === 'done' &&
                  t('requirements.split.statusMetaDone', {
                    count: runState.candidates?.length ?? 0,
                    time: finishedAt,
                  })}
                {runState.status === 'error' &&
                  (runState.error_message || t('requirements.split.statusMetaError'))}
              </span>
            </div>
          )}
          {restored && (
            <div className="req-split-hint req-split-hint-muted">
              {t('requirements.split.resumed')}
            </div>
          )}
          {reconnecting && (
            <div className="req-split-hint req-split-hint-muted">
              {t('requirements.split.reconnecting')}
            </div>
          )}

          {/* ── Config stage ──
              The model, the execution environment and the source branch are
              picked BEFORE the AI is asked for candidates. Nothing is sent
              to the backend from this block. */}
          {stage === 'config' && (
            <div className="req-split-config">
              <div className="req-split-config-title">{t('requirements.split.configTitle')}</div>

              <div className="req-split-config-field">
                <span className="req-split-config-label">{t('requirements.split.modelLabel')}</span>
                <ModelSelect
                  value={model}
                  onChange={setModel}
                  stage="architect"
                  defaultModelName={initialModelName || undefined}
                  configId={configId || undefined}
                  onConfigChange={setConfigId}
                />
              </div>

              <div className="req-split-config-field">
                <span className="req-split-config-label">{t('requirements.split.execEnvLabel')}</span>
                <div className="preflight-field-card preflight-field-card--exec">
                  <span className="preflight-field-chip" aria-hidden="true">ENV</span>
                  <ExecEnvSelect
                    className="preflight-field-input"
                    servers={agentServers}
                    value={agentServerId}
                    onChange={setAgentServerId}
                    localOptionLabel={t('requirements.detail2.preflightLocalExec')}
                  />
                </div>
                {agentServers.length === 0 && (
                  <small className="req-split-hint req-split-hint-muted">
                    {t('requirements.split.execEnvEmptyHint')}
                  </small>
                )}
                {agentServerId !== '' && (
                  <small className="req-split-hint req-split-hint-muted">
                    {t('requirements.split.execEnvRemoteHint')}
                  </small>
                )}
              </div>

              <div className="req-split-hint req-split-hint-muted">
                {t('requirements.split.configScopeHint')}
              </div>
            </div>
          )}

          {/* The source branch: the master's own dev branch is created from
              it (or aligned to it) and pushed to the remote. Every child
              then develops on that branch — stacked PRs. */}
          <div className="req-split-source">
            <BranchPicker
              inputId="split-source-branch-input"
              datalistId="split-source-branch-suggestions"
              value={sourceBranch}
              onChange={setSourceBranch}
              disabled={busy}
              placeholder={baseBranch || 'main'}
              options={branches}
              chipLabel="BASE 主需求"
              defaultName={baseBranch || 'main'}
              emptyWarnText={t('requirements.detail2.designBranchPickerEmpty')}
            />
            <div className="req-split-hint req-split-hint-muted">
              {t('requirements.split.sourceBranchHint')}
            </div>
          </div>
          <div className="req-split-hint">
            {t('requirements.split.baseHint', { base: masterBranch || baseBranch || 'main' })}
          </div>
          <div className="req-split-hint req-split-hint-muted">{t('requirements.split.parallelHint')}</div>

          {stage === 'running' && (
            <>
              <div className="req-split-progress">
                <span className="req-spinner" aria-hidden />
                {streamLines.length > 0
                  ? streamLines[streamLines.length - 1].content
                  : t('requirements.split.generating')}
              </div>

              {/* ── 执行过程 ──
                  The live SSE stream, one row per frame. Before this the
                  running stage showed a single line that every new frame
                  overwrote — the user could not see what the AI was doing. */}
              <div className="req-split-stream">
                <button
                  type="button"
                  className="req-split-stream-title"
                  onClick={() => setStreamOpen((v) => !v)}
                  aria-expanded={streamOpen}
                >
                  <span>{t('requirements.split.streamTitle')}</span>
                  <span className="req-split-stream-toggle">
                    {streamOpen
                      ? t('requirements.split.streamCollapse')
                      : t('requirements.split.streamExpand')}
                  </span>
                </button>
                {streamOpen && (
                  <div className="req-split-stream-body" ref={streamBodyRef}>
                    {streamLines.length === 0 ? (
                      <div className="req-split-stream-line req-split-stream-line--muted">
                        {t('requirements.split.streamEmpty')}
                      </div>
                    ) : (
                      streamLines.map((l, i) => (
                        <div
                          key={i}
                          className={`req-split-stream-line req-split-stream-line--${l.type}`}
                        >
                          {l.content}
                        </div>
                      ))
                    )}
                  </div>
                )}
              </div>
            </>
          )}

          {busy && (
            <div className="req-split-hint req-split-hint-muted">
              {t('requirements.split.confirmingHint')}
            </div>
          )}
          {error && <div className="req-split-error">{error}</div>}

          {stage === 'review' && rows.length > 0 && (
            <>
              <div className="req-split-table">
                <div className="req-split-row req-split-row-head">
                  <span className="req-split-col-idx">#</span>
                  <span className="req-split-col-title">{t('requirements.split.colTitle')}</span>
                  <span className="req-split-col-desc">{t('requirements.split.colDesc')}</span>
                  <span className="req-split-col-dep">{t('requirements.split.colDepends')}</span>
                  <span className="req-split-col-act">{t('requirements.split.colActions')}</span>
                </div>
                {rows.map((r, i) => (
                  <div className="req-split-row" key={i}>
                    <span className="req-split-col-idx">{i + 1}</span>
                    <span className="req-split-col-title">
                      <input
                        className="form-input"
                        value={r.title}
                        placeholder={t('requirements.split.noTitle')}
                        onChange={(e) => update(i, { title: e.target.value })}
                      />
                    </span>
                    <span className="req-split-col-desc">
                      <textarea
                        className="form-input"
                        rows={2}
                        value={r.description}
                        onChange={(e) => update(i, { description: e.target.value })}
                      />
                      <textarea
                        className="form-input"
                        rows={2}
                        placeholder={t('requirements.split.acceptancePlaceholder')}
                        value={r.acceptance.join('\n')}
                        onChange={(e) => update(i, { acceptance: lines(e.target.value) })}
                      />
                    </span>
                    <span className="req-split-col-dep">
                      {possibleDeps(i).length === 0 ? (
                        <span className="req-split-muted">{t('requirements.split.dependsNone')}</span>
                      ) : (
                        possibleDeps(i).map((d) => (
                          <label key={d} className="req-split-check">
                            <input
                              type="checkbox"
                              checked={r.dependsOn.includes(d)}
                              onChange={() => toggleDependency(i, d)}
                            />
                            {d}
                          </label>
                        ))
                      )}
                      <span className="req-split-muted req-split-dep-hint">{t('requirements.split.dependsHint')}</span>
                    </span>
                    <span className="req-split-col-act">
                      <button
                        type="button"
                        className="btn btn-sm"
                        onClick={() => move(i, -1)}
                        disabled={i === 0 || busy}
                        aria-label={t('requirements.split.moveUp')}
                        title={t('requirements.split.moveUp')}
                      >
                        ↑
                      </button>
                      <button
                        type="button"
                        className="btn btn-sm"
                        onClick={() => move(i, 1)}
                        disabled={i === rows.length - 1 || busy}
                        aria-label={t('requirements.split.moveDown')}
                        title={t('requirements.split.moveDown')}
                      >
                        ↓
                      </button>
                      <button
                        type="button"
                        className="btn btn-sm"
                        onClick={() => remove(i)}
                        disabled={busy}
                        aria-label={t('requirements.split.remove')}
                        title={t('requirements.split.remove')}
                      >
                        ✕
                      </button>
                    </span>
                  </div>
                ))}
              </div>

              {referencePairs.length > 0 && (
                <div className="req-split-refs">
                  <div className="req-split-refs-title">{t('requirements.split.referencesTitle')}</div>
                  <div className="req-split-refs-hint">{t('requirements.split.referencesHint')}</div>

                  {/* ── AI 建议 ──
                      The model judged this layer (architect view): it picked
                      the pairs AND said why. The user confirms or disagrees
                      with one pair at a time instead of having to derive the
                      whole matrix from scratch. */}
                  {aiRefs.length > 0 && (
                    <div className="req-split-refs-ai">
                      <div className="req-split-refs-ai-title">
                        {t('requirements.split.referencesAiTitle')}
                      </div>
                      {aiRefs.map((r) => (
                        <label key={`ai-${r.a}-${r.b}`} className="req-split-ref-row">
                          <input
                            type="checkbox"
                            checked={pairSelected(references, r.a, r.b)}
                            onChange={() => toggleReference(r.a, r.b)}
                          />
                          <span className="req-split-ref-pair">
                            {r.a}↔{r.b}
                          </span>
                          <span className="req-split-ref-reason">
                            {r.reason || t('requirements.split.referencesNoReason')}
                          </span>
                        </label>
                      ))}
                    </div>
                  )}

                  {/* ── 全部组合 ──
                      The full matrix is long (11 candidates → 55 pairs), so
                      it stays collapsed until asked for. */}
                  <button
                    type="button"
                    className="req-split-refs-toggle"
                    onClick={() => setRefsExpanded((v) => !v)}
                    aria-expanded={refsExpanded}
                  >
                    {refsExpanded
                      ? t('requirements.split.referencesCollapse')
                      : t('requirements.split.referencesExpand')}
                  </button>
                  {refsExpanded && (
                    <div className="req-split-refs-grid">
                      {referencePairs.map(([a, b]) => {
                        const on = pairSelected(references, a, b);
                        return (
                          <label key={`${a}-${b}`} className="req-split-check">
                            <input
                              type="checkbox"
                              checked={on}
                              onChange={() => toggleReference(a, b)}
                            />
                            {a}↔{b}
                          </label>
                        );
                      })}
                    </div>
                  )}
                </div>
              )}

              <div className="req-split-hint req-split-hint-muted">{t('requirements.split.refreshHint')}</div>
            </>
          )}
        </div>

        <div className="modal-actions">
          <button type="button" className="btn btn-sm" onClick={close} disabled={busy}>
            {t('requirements.split.cancel')}
          </button>
          {stage === 'config' && (
            <button type="button" className="btn btn-primary btn-sm" onClick={() => void start()}>
              {t('requirements.split.start')}
            </button>
          )}
          {stage === 'running' && (
            <button type="button" className="btn btn-primary btn-sm" disabled>
              {t('requirements.split.generating')}
            </button>
          )}
          {stage === 'review' && (
            <>
              <button
                type="button"
                className="btn btn-sm"
                onClick={() => setStage('config')}
                disabled={busy}
              >
                {t('requirements.split.regenerate')}
              </button>
              <button type="button" className="btn btn-primary btn-sm" onClick={() => void confirm()} disabled={!canConfirm}>
                {busy
                  ? t('requirements.split.confirming', { sec: confirmElapsed })
                  : t('requirements.split.confirm', { count: rows.length })}
              </button>
            </>
          )}
        </div>
      </div>
    </div>
  );
}

export default RequirementSplitModal;
