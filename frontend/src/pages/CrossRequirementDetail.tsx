import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import MarkdownRender from '../components/MarkdownRender';
import CrossJobPanel, { type CrossJobLine } from '../components/CrossJobPanel';
import CrossDesignAdjustPanel from '../components/CrossDesignAdjustPanel';
import CrossRequirementEditForm from '../components/CrossRequirementForm/CrossRequirementEditForm';
import CrossRequirementPurgeDialog from '../components/CrossRequirementPurgeDialog';
import { rewriteNovaLinks } from '../components/RichDescriptionEditor/rewriteNovaLinks';
import { createEventStream, type EventStream } from '../api/stream';
import {
  ApiError,
  crossRequirementsApi,
  fsApi,
  projectsApi,
  type CrossRequirementBase,
  type CrossRequirementChild,
  type CrossRequirementDetail as Detail,
  type CrossRequirementPhase,
  type CrossSplitCandidate,
  type Project,
} from '../api/client';
import { errorMessage } from '../utils/errMsg';
import { crossPhaseBadgeClass, crossPhaseCanVerify, crossStatusBadgeClass } from '../utils/crossStatus';
import '../styles/markdown-prose.css';
import './CrossRequirements.css';

// 跨项目需求详情页。
//
// 一页要回答四个问题：
//
//   1. 整体方案是什么？（跨仓库的职责边界 + 接口契约）
//   2. 每个仓库各自要做什么？（项目进度矩阵）
//   3. 谁卡在谁前面？（跨项目依赖 + 阻塞标记）
//   4. 联调到哪一步了？（阶段看板 + 验证报告）
//
// 三个 AI 作业（整体方案 / 按项目拆分 / 阶段验证）都用 SSE 回显，产出分别落库：
// 整体方案落 cross_requirements.design_docs，验证报告落阶段行 —— 后端重启后
// 仍然可回看。

/** design_docs 存的是 JSON 数组；这里取出 Markdown 正文。 */
function designMarkdown(raw: string): string {
  const trimmed = (raw || '').trim();
  if (!trimmed) return '';
  try {
    const arr = JSON.parse(trimmed);
    if (Array.isArray(arr)) {
      return arr.map(s => (typeof s === 'string' ? s : '')).filter(Boolean).join('\n\n');
    }
  } catch {
    // A legacy / hand-written value that is not JSON is rendered verbatim.
  }
  return trimmed;
}

// ── Jobs (overall design / project split / phase verification) ────────────
//
// All three jobs look the same on the page: start → attach one SSE stream →
// echo the log by kind → wrap up. They differ only in their product and in
// whether they can be recovered, so they share one mechanism:
//
//   * The job lives on the SERVER (JobStore). Closing the tab or reloading
//     does not kill it — the backend runs it with context.Background(),
//     detached from the request that started it.
//   * Its job id, however, only exists in the server's in-process registry,
//     so "find that job again" needs an anchor: the overall design and the
//     project split each have a state endpoint; phase verification has none.
//   * A dropped link is NOT a failure: judging a still-running job as failed
//     because the network hiccuped is the worst possible outcome. The page
//     converges through "backoff reconnect → fall back to polling the state",
//     and only a terminal state counts as the end of the job.

/** Which cross-requirement job a panel is showing. */
type CrossJobKind = 'design' | 'split' | 'verify';

/** The state payload shared by the design / split state endpoints. */
interface CrossJobState {
  status: string;
  job_id: string;
  running: boolean;
  error_message: string;
  design?: string;
  candidates?: CrossSplitCandidate[];
}

/** Everything the job driver needs to run / recover one kind of job. */
interface CrossJobBinding {
  kind: CrossJobKind;
  /** The SSE endpoint of one job. */
  streamUrl: (jobId: string) => string;
  /** The state endpoint used to recover / poll the job (absent = not recoverable). */
  state?: () => Promise<CrossJobState>;
  /** Maps a state payload to the same result shape the SSE terminal frame carries. */
  resultFromState?: (st: CrossJobState) => unknown;
  /** Called once the job finished successfully. */
  onDone: (result: any) => void;
}

/** The two halves of the job driver (they call each other — see driverRef). */
interface CrossJobDriver {
  attach: (jobId: string, binding: CrossJobBinding, attempt: number) => void;
  poll: (jobId: string, binding: CrossJobBinding) => void;
}

export default function CrossRequirementDetailPage() {
  const { id } = useParams();
  const navigate = useNavigate();
  const { t } = useTranslation();

  const [detail, setDetail] = useState<Detail | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [toast, setToast] = useState<string | null>(null);

  // ── 回收站 ──────────────────────────────────────────────────────────────
  //
  // 删除是两步的：移进回收站（可恢复）→ 彻底删除（不可恢复）。这一页的
  // 「移到回收站」是软删 —— 不需要确认框，做完给一条提示就行（可撤销的
  // 动作，确认框只会变成噪音）。
  //
  // 回收站里的行是**只读**的：整体方案 / 拆分 / 联调都做不了，后端也一律
  // 拒绝（409 XREQ_IN_TRASH）。所以这一页要显示一条横幅说清楚现在是什么状态，
  // 并把写入口置灰 —— 否则用户会以为按钮坏了。
  const [purgeOpen, setPurgeOpen] = useState(false);
  const [purgeAck, setPurgeAck] = useState(false);
  const [purging, setPurging] = useState(false);

  // design editing
  const [editingDesign, setEditingDesign] = useState(false);
  const [designDraft, setDesignDraft] = useState('');

  // description editing — the inline panel replaces the read-only view.
  const [editingDescription, setEditingDescription] = useState(false);

  // split confirmation
  const [candidates, setCandidates] = useState<CrossSplitCandidate[]>([]);
  const [confirming, setConfirming] = useState(false);
  // splitPanelRef — the inline confirmation panel. It used to be a modal
  // (fixed overlay, always in view); as an inline panel it has to be
  // scrolled to, or the user presses 「拆分」 and sees nothing happen.
  const splitPanelRef = useRef<HTMLElement | null>(null);
  // confirmingSplit — the confirm request is in flight. The button is
  // disabled while it is: a double click would otherwise fire two confirms.
  // The server refuses the second one (409 XREQ_ALREADY_SPLIT), but the
  // user should not have to see that error.
  const [confirmingSplit, setConfirmingSplit] = useState(false);

  // phase creation
  const [addingPhase, setAddingPhase] = useState(false);
  const [phaseName, setPhaseName] = useState('');
  const [phaseMembers, setPhaseMembers] = useState<string[]>([]);

  // ── participating projects (role + base branch) ──────────────────────────
  //
  // The base branch is PER PROJECT: the repositories of a cross-project
  // requirement often sit on different release lines (A on develop, B on
  // release/2026q1), so one global value cannot express it. Empty = that
  // repository keeps using its own default branch.
  const [editingProjects, setEditingProjects] = useState(false);
  const [projectDrafts, setProjectDrafts] = useState<
    { project_id: string; role: string; base_branch: string }[]
  >([]);
  const [savingProjects, setSavingProjects] = useState(false);
  // draftCache — the last role / base branch of every project that was
  // checked at least once, so un-checking and re-checking a project restores
  // its values instead of resetting them.
  const [draftCache, setDraftCache] = useState<
    Record<string, { role: string; base_branch: string }>
  >({});
  // branchOptions — the branch candidates of each project (from
  // git-branches), used as datalist suggestions for the base-branch input.
  // A failed fetch only means "no suggestions" — the user can still type a
  // branch name by hand.
  const [branchOptions, setBranchOptions] = useState<Record<string, string[]>>({});

  // ── job panel ─────────────────────────────────────────────────────────
  //
  // jobLines — the log lines of the running job.
  // jobReconnecting — the SSE link dropped; the job is still alive and the
  //   page is retrying. NOT a failure.
  // jobPolling — the reconnects gave up; the job state is being polled.
  // jobLost — the job could not be found any more (server restarted).
  const [jobLines, setJobLines] = useState<CrossJobLine[]>([]);
  const [jobRunning, setJobRunning] = useState(false);
  const [jobReconnecting, setJobReconnecting] = useState(false);
  const [jobPolling, setJobPolling] = useState(false);
  const [jobLost, setJobLost] = useState(false);
  const [jobKind, setJobKind] = useState<CrossJobKind | null>(null);
  const esRef = useRef<EventStream | null>(null);
  // streamToken — every attach bumps it. A stream whose token is stale
  // (superseded by a reconnect, or the page unmounted) must not touch the
  // state any more.
  const streamTokenRef = useRef(0);
  // pollTimerRef — the pending state poll, cleared on unmount.
  const pollTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // driverRef — the job driver (attach / poll). It is a ref because the two
  // halves call each other; see the CrossJobDriver comment.
  const driverRef = useRef<CrossJobDriver>({ attach: () => {}, poll: () => {} });

  // allProjects — every project (used for its local_path when fetching the
  // branch candidates, and to render the project picker).
  const [allProjects, setAllProjects] = useState<Project[]>([]);

  const xreqId = id || '';

  const load = useCallback(() => {
    if (!xreqId) return;
    setLoading(true);
    setError(null);
    crossRequirementsApi.get(xreqId)
      .then(setDetail)
      .catch(err => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, [xreqId]);

  useEffect(() => { load(); }, [load]);

  // ── 回收站动作 ──────────────────────────────────────────────────────────

  // 移到回收站：软删，可恢复 —— 所以没有确认框，只给一条提示。
  const moveToTrash = async () => {
    if (!xreqId) return;
    try {
      await crossRequirementsApi.trash(xreqId);
      setToast(t('crossRequirements.trash.movedToTrash'));
      await load();
    } catch (err) {
      // 连点两次：第二个请求撞上「已经在回收站里了」—— 那恰恰是想要的结果。
      if (err instanceof ApiError && err.code === 'XREQ_IN_TRASH') {
        setToast(t('crossRequirements.trash.movedToTrash'));
        await load();
        return;
      }
      setError(errorMessage(err));
    }
  };

  const restoreFromTrash = async () => {
    if (!xreqId) return;
    try {
      await crossRequirementsApi.restore(xreqId);
      setToast(t('crossRequirements.detail.restored'));
      await load();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const confirmPurge = async () => {
    if (!xreqId || purging) return;
    setPurging(true);
    try {
      await crossRequirementsApi.purge(xreqId);
      setPurgeOpen(false);
      setPurgeAck(false);
      // 行没了 —— 回列表，不要停在一个永远 404 的详情页上。
      navigate('/cross-requirements');
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setPurging(false);
    }
  };

  useEffect(() => {
    projectsApi.list()
      .then(list => setAllProjects(Array.isArray(list) ? list : []))
      .catch(() => setAllProjects([]));
  }, []);

  // The split confirmation is an inline panel now, so it has to be scrolled
  // into view — as a modal it was always in the user's face, and pressing
  // 「拆分」 with nothing visibly happening reads as a broken button.
  useEffect(() => {
    if (!confirming) return;
    splitPanelRef.current?.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  }, [confirming]);

  // Seed the drafts from the current participating project set (with their
  // role and base branch) when the editor opens.
  useEffect(() => {
    if (!editingProjects) return;
    const rows = (detail?.projects || []).map(p => ({
      project_id: p.project_id,
      role: p.role || '',
      base_branch: p.base_branch || '',
    }));
    setProjectDrafts(rows);
  }, [editingProjects, detail]);

  // Fetch the branch candidates of every participating project, used as the
  // datalist suggestions for the base-branch inputs. A failed fetch only
  // means "no suggestions" — the user can still type a branch name.
  //
  // The dependency is the SET of project ids, not the draft itself: the
  // draft changes on every keystroke, and depending on it would turn
  // "open the editor" into "fetch the branch list per character typed".
  const draftProjectKey = useMemo(
    () => projectDrafts.map(d => d.project_id).sort().join(','),
    [projectDrafts],
  );
  useEffect(() => {
    if (!editingProjects || draftProjectKey === '') return;
    let cancelled = false;
    (async () => {
      const ids = draftProjectKey.split(',');
      const entries = await Promise.all(ids.map(async pid => {
        const proj = allProjects.find(p => p.id === pid);
        if (!proj || !proj.local_path) return null;
        try {
          const resp = await fsApi.gitBranches({ path: proj.local_path });
          return [pid, resp?.branches || []] as const;
        } catch {
          return null;
        }
      }));
      if (cancelled) return;
      const next: Record<string, string[]> = {};
      entries.forEach(e => { if (e) next[e[0]] = e[1]; });
      setBranchOptions(next);
    })();
    return () => { cancelled = true; };
  }, [editingProjects, draftProjectKey, allProjects]);

  useEffect(() => () => {
    // Invalidate the pending callbacks of any stream / poll, then tear down.
    streamTokenRef.current++;
    esRef.current?.close();
    esRef.current = null;
    if (pollTimerRef.current) {
      clearTimeout(pollTimerRef.current);
      pollTimerRef.current = null;
    }
  }, []);

  // ── Re-attach to a running job after the page was reopened ─────────────
  //
  // The job lives on the server (closing the tab or reloading does not kill
  // it), but its job id only exists in the server's in-process registry — the
  // state endpoint is the only way to find it again. After re-attaching the
  // panel REPLAYS the whole history and continues live; it never starts a
  // second run of the same job.
  useEffect(() => {
    if (!xreqId) return;
    let cancelled = false;

    const designBinding: CrossJobBinding = {
      kind: 'design',
      streamUrl: jobId => crossRequirementsApi.streamDesignUrl(xreqId, jobId),
      state: () => crossRequirementsApi.designState(xreqId),
      resultFromState: st => ({ design: st.design || '' }),
      onDone: () => setToast(t('crossRequirements.detail.designSaved')),
    };
    const splitBinding: CrossJobBinding = {
      kind: 'split',
      streamUrl: jobId => crossRequirementsApi.streamSplitUrl(xreqId, jobId),
      state: () => crossRequirementsApi.splitState(xreqId),
      resultFromState: st => ({ candidates: st.candidates || [] }),
      onDone: (result) => {
        const list = Array.isArray(result?.candidates) ? result.candidates : [];
        setCandidates(list);
        setConfirming(true);
      },
    };

    void (async () => {
      try {
        const [design, split] = await Promise.all([
          crossRequirementsApi.designState(xreqId),
          crossRequirementsApi.splitState(xreqId),
        ]);
        if (cancelled) return;

        if (design.status === 'running' && design.job_id) {
          driverRef.current.attach(design.job_id, designBinding, 0);
        } else if (design.status === 'error') {
          setJobKind('design');
          appendLine({
            type: 'error',
            content: design.error_message || t('crossRequirements.job.failed'),
          });
        }

        if (split.status === 'running' && split.job_id) {
          // A split that is already running wins over a design recovery —
          // only one job can drive the panel at a time.
          driverRef.current.attach(split.job_id, splitBinding, 0);
        } else if (split.status === 'done' && (split.candidates || []).length > 0) {
          // The split finished but the user has not confirmed the candidates
          // yet — offer them again. (Confirming consumes the run, so the state
          // endpoint answers "none" afterwards and the modal does not pop up
          // a second time.)
          setCandidates(split.candidates || []);
          setConfirming(true);
        } else if (split.status === 'error') {
          setJobKind('split');
          appendLine({
            type: 'error',
            content: split.error_message || t('crossRequirements.job.failed'),
          });
        }
      } catch {
        // The state endpoints are unavailable (older backend) — degrade to the
        // previous behaviour instead of blocking the page.
      }
    })();

    return () => { cancelled = true; };
    // The recovery runs ONCE per cross requirement: re-running it on every
    // `t` change (a language switch) would re-attach the stream and rebuild
    // the panel for no reason.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [xreqId]);

  useEffect(() => {
    if (!toast) return;
    const timer = setTimeout(() => setToast(null), 4000);
    return () => clearTimeout(timer);
  }, [toast]);

  const statusLabel = useCallback((v: string) => t(`crossRequirements.status.${v}` as never), [t]);
  const phaseStatusLabel = useCallback((v: string) => t(`crossRequirements.phaseStatus.${v}` as never), [t]);

  const children = detail?.children || [];
  const childrenByProject = useMemo(() => {
    const map = new Map<string, CrossRequirementChild[]>();
    (detail?.children || []).forEach(c => {
      const list = map.get(c.project_id) || [];
      list.push(c);
      map.set(c.project_id, list);
    });
    return map;
  }, [detail]);

  const titleById = useMemo(() => {
    const map = new Map<string, string>();
    (detail?.children || []).forEach(c => map.set(c.id, c.title));
    return map;
  }, [detail]);

  // ── jobs ────────────────────────────────────────────────────────────────

  const appendLine = useCallback((line: CrossJobLine) => {
    setJobLines(prev => [...prev, line]);
  }, []);

  // attachJob opens (or re-opens) the SSE stream of a job.
  //
  // A dropped link is NOT a failure: the job keeps running on the server, the
  // link is what broke. The page converges through "backoff reconnect
  // (1s→2s→4s→8s) → fall back to polling the state"; only a terminal state
  // (job_done / state=error) ends the job.
  //
  // Every attach clears the line array first: the backend's Subscribe
  // pre-seeds the channel with ALL existing log lines, so a reconnect
  // rebuilds the panel completely — no duplicated lines, and no extra
  // snapshot request.
  const attachJob = useCallback<CrossJobDriver['attach']>((jobId, binding, attempt) => {
    esRef.current?.close();
    esRef.current = null;
    const token = ++streamTokenRef.current;

    setJobKind(binding.kind);
    setJobRunning(true);
    setJobLost(false);
    setJobPolling(false);
    setJobReconnecting(attempt > 0);
    setJobLines([]);

    const es = createEventStream(
      binding.streamUrl(jobId),
      (data) => {
        if (streamTokenRef.current !== token) return;
        if (data.type === 'job_done') {
          es.close();
          if (esRef.current === es) esRef.current = null;
          setJobRunning(false);
          setJobReconnecting(false);
          setJobPolling(false);
          if (data.status === 'done') {
            binding.onDone(data.result || {});
            load();
          } else {
            appendLine({
              type: 'error',
              content: data.error_message || t('crossRequirements.job.failed'),
            });
          }
          return;
        }
        if (data.content) {
          appendLine({ type: data.type || 'message', content: String(data.content) });
        }
      },
      () => {
        if (streamTokenRef.current !== token) return;
        // The link dropped mid-flight. The job keeps running on the server,
        // so this is a reconnect — never a failure.
        setJobReconnecting(true);
        if (attempt < 4) {
          const delay = Math.min(1000 * Math.pow(2, attempt), 10000);
          setTimeout(() => {
            if (streamTokenRef.current !== token) return;
            driverRef.current.attach(jobId, binding, attempt + 1);
          }, delay);
          return;
        }
        if (binding.state) {
          setJobReconnecting(false);
          setJobPolling(true);
          driverRef.current.poll(jobId, binding);
          return;
        }
        // No state endpoint to fall back to (phase verification) — the job
        // cannot be recovered, so the page stops waiting for it.
        setJobRunning(false);
        setJobReconnecting(false);
        appendLine({ type: 'error', content: t('crossRequirements.job.failed') });
      },
    );
    esRef.current = es;
  }, [appendLine, load, t]);

  // pollJobState is the fallback once the reconnects gave up: it asks the
  // server whether the job is still alive and, if it is, re-attaches the
  // stream (so the live output resumes as soon as the link comes back).
  const pollJobState = useCallback<CrossJobDriver['poll']>((jobId, binding) => {
    const state = binding.state;
    if (!state) return;
    const token = streamTokenRef.current;

    const tick = async () => {
      if (streamTokenRef.current !== token) return;
      let st: CrossJobState | null = null;
      try {
        st = await state();
      } catch {
        st = null;
      }
      if (streamTokenRef.current !== token) return;
      if (!st) {
        // Transient failure — keep polling.
        pollTimerRef.current = setTimeout(() => { void tick(); }, 3000);
        return;
      }
      if (st.status === 'done') {
        setJobRunning(false);
        setJobPolling(false);
        setJobReconnecting(false);
        binding.onDone(binding.resultFromState ? binding.resultFromState(st) : {});
        load();
        return;
      }
      if (st.status === 'error') {
        setJobRunning(false);
        setJobPolling(false);
        appendLine({ type: 'error', content: st.error_message || t('crossRequirements.job.failed') });
        return;
      }
      if (st.status === 'running') {
        // Still alive — re-attach the stream (the backend replays the whole
        // history, so the panel rebuilds itself from scratch).
        driverRef.current.attach(st.job_id || jobId, binding, 0);
        return;
      }
      // status = none → the job is gone (server restarted, or it was evicted).
      setJobRunning(false);
      setJobPolling(false);
      setJobLost(true);
      appendLine({ type: 'error', content: t('crossRequirements.job.lost') });
    };

    void tick();
  }, [appendLine, load, t]);

  // Keep the driver ref pointing at the current closures.
  useEffect(() => {
    driverRef.current = { attach: attachJob, poll: pollJobState };
  }, [attachJob, pollJobState]);

  // startJob launches a job and attaches its stream.
  const startJob = useCallback((
    binding: CrossJobBinding,
    start: () => Promise<{ job_id: string }>,
  ) => {
    setJobRunning(true);
    setJobLines([]);
    setJobKind(binding.kind);
    setJobLost(false);
    setJobReconnecting(false);
    setJobPolling(false);
    start()
      .then(({ job_id }) => {
        driverRef.current.attach(job_id, binding, 0);
      })
      .catch(err => {
        setJobRunning(false);
        appendLine({ type: 'error', content: errorMessage(err) });
      });
  }, [appendLine]);

  // apply_discussion — 把「方案调整对话」的讨论记录整段喂进提示词，让这一版
  // 方案按讨论结论修订。
  //
  // 它是显式的：页头的「重新生成整体方案」是「从头再来」，调整面板里的
  // 「按讨论结论重新补全方案」是「按我们谈拢的改」。两个意图必须由用户自己
  // 区分 —— 讨论记录里混着用户还没拍板的问答，自动注入等于 AI 替用户拍板。
  const startDesign = (opts?: { apply_discussion?: boolean }) => {
    startJob(
      {
        kind: 'design',
        streamUrl: jobId => crossRequirementsApi.streamDesignUrl(xreqId, jobId),
        state: () => crossRequirementsApi.designState(xreqId),
        resultFromState: st => ({ design: st.design || '' }),
        onDone: () => setToast(t('crossRequirements.detail.designSaved')),
      },
      () => crossRequirementsApi.startDesign(xreqId, opts ?? {}),
    );
  };

  const startSplit = () => {
    startJob(
      {
        kind: 'split',
        streamUrl: jobId => crossRequirementsApi.streamSplitUrl(xreqId, jobId),
        state: () => crossRequirementsApi.splitState(xreqId),
        resultFromState: st => ({ candidates: st.candidates || [] }),
        onDone: (result) => {
          const list = Array.isArray(result?.candidates) ? result.candidates : [];
          setCandidates(list);
          setConfirming(true);
        },
      },
      () => crossRequirementsApi.startSplit(xreqId),
    );
  };

  const startVerify = (phaseId: string) => {
    startJob(
      {
        // Phase verification has no state endpoint yet (its job id is
        // persisted on the phase but is never cleared on completion, so
        // "id present" cannot mean "running"). The job is therefore not
        // recoverable across a page reload — but a dropped link inside one
        // session still reconnects.
        kind: 'verify',
        streamUrl: jobId => crossRequirementsApi.streamVerifyUrl(xreqId, phaseId, jobId),
        onDone: () => setToast(t('crossRequirements.detail.verdictSaved')),
      },
      () => crossRequirementsApi.startVerify(xreqId, phaseId),
    );
  };

  // ── actions ─────────────────────────────────────────────────────────────

  const saveDesign = async () => {
    try {
      await crossRequirementsApi.saveDesign(xreqId, designDraft);
      setEditingDesign(false);
      setToast(t('crossRequirements.detail.designSaved'));
      load();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const confirmSplit = async () => {
    // Guard against a double click: the second call would be refused by the
    // server anyway (409 XREQ_ALREADY_SPLIT), but sending it at all would
    // show the user an error for a request they never intended.
    if (confirmingSplit) return;
    setConfirmingSplit(true);
    setError(null);
    try {
      await crossRequirementsApi.confirmSplit(xreqId, candidates);
      setConfirming(false);
      setCandidates([]);
      setToast(t('crossRequirements.split.confirmed', { count: candidates.length }));
      load();
    } catch (err) {
      setError(errorMessage(err));
      // The cross requirement was already split elsewhere (another tab, or a
      // previous confirm that went through) — the candidates are stale.
      if ((err as { code?: string })?.code === 'XREQ_ALREADY_SPLIT') {
        setConfirming(false);
        setCandidates([]);
        load();
      }
    } finally {
      setConfirmingSplit(false);
    }
  };

  const updateCandidateProject = (index: number, projectId: string) => {
    setCandidates(prev => prev.map((c, i) => (i === index ? { ...c, project_id: projectId } : c)));
  };

  // ── participating projects ───────────────────────────────────────────────

  const updateProjectDraft = (projectId: string, patch: { role?: string; base_branch?: string }) => {
    setProjectDrafts(prev => prev.map(d => (d.project_id === projectId ? { ...d, ...patch } : d)));
  };

  const toggleProjectDraft = (projectId: string) => {
    setProjectDrafts(prev => {
      const current = prev.find(d => d.project_id === projectId);
      if (current) {
        // Remember the values so re-checking the project restores them
        // instead of resetting the role / base branch to empty.
        setDraftCache(cache => ({ ...cache, [projectId]: { role: current.role, base_branch: current.base_branch } }));
        return prev.filter(d => d.project_id !== projectId);
      }
      const cached = draftCache[projectId];
      return [...prev, {
        project_id: projectId,
        role: cached?.role || '',
        base_branch: cached?.base_branch || '',
      }];
    });
  };

  const saveProjects = async () => {
    if (savingProjects) return;
    setSavingProjects(true);
    setError(null);
    try {
      await crossRequirementsApi.setProjects(xreqId, projectDrafts.map(d => ({
        project_id: d.project_id,
        role: d.role,
        base_branch: d.base_branch.trim(),
      })));
      setEditingProjects(false);
      setToast(t('crossRequirements.detail.projectsSaved'));
      load();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSavingProjects(false);
    }
  };

  const createPhase = async () => {
    if (!phaseName.trim()) return;
    try {
      await crossRequirementsApi.createPhase(xreqId, {
        name: phaseName.trim(),
        requirement_ids: phaseMembers,
      });
      setAddingPhase(false);
      setPhaseName('');
      setPhaseMembers([]);
      load();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const setPhaseStatus = async (phase: CrossRequirementPhase, status: string) => {
    try {
      await crossRequirementsApi.updatePhase(xreqId, phase.id, { status });
      load();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const removePhase = async (phase: CrossRequirementPhase) => {
    try {
      await crossRequirementsApi.deletePhase(xreqId, phase.id);
      setToast(t('crossRequirements.detail.phaseDeleted'));
      load();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  if (loading) {
    return <div className="xreq-empty">{t('crossRequirements.loading')}</div>;
  }
  if (error && !detail) {
    return (
      <div className="xreq-empty">
        <p className="xreq-error">{error}</p>
        <button type="button" className="btn" onClick={load}>{t('crossRequirements.retry')}</button>
      </div>
    );
  }
  if (!detail) return null;

  const design = designMarkdown(detail.design_docs);
  const progress = detail.progress;
  const projectRows = detail.projects || [];
  // Baseline ledger: which commit each repository's design was written
  // against (written by the overall-design job).
  const baselineByProject: Record<string, CrossRequirementBase> = {};
  for (const b of detail.bases || []) baselineByProject[b.project_id] = b;
  // The panel title names the job that is currently driving it.
  const jobTitle = jobKind === 'split'
    ? t('crossRequirements.job.splitTitle')
    : jobKind === 'verify'
      ? t('crossRequirements.job.verifyTitle')
      : t('crossRequirements.job.designTitle');
  // Already split? Two sources: the batch id on the parent row (the normal
  // path) and the children themselves (rows written by an earlier build
  // carry no batch id). Either way the server refuses a second split.
  const alreadySplit = !!detail.split_batch_id || children.length > 0;
  // 回收站里的行是只读的 —— 后端一律拒绝写操作（409 XREQ_IN_TRASH），所以
  // 写入口要在 UI 上也置灰，并给一条横幅说明现在是什么状态。
  const inTrash = !!detail.deleted_at;

  return (
    <div className="xreq-page">
      <div className="xreq-head">
        <div className="xreq-head-main">
          <button type="button" className="btn btn-sm xreq-back" onClick={() => navigate('/cross-requirements')}>
            ← {t('crossRequirements.detail.back')}
          </button>
          <h1 className="xreq-title">{detail.title}</h1>
          <span className={`status-badge ${crossStatusBadgeClass(detail.status)}`}>
            {statusLabel(detail.status)}
          </span>
        </div>
        <div className="xreq-actions">
          <button
            type="button"
            className="btn"
            onClick={() => { startDesign(); }}
            disabled={jobRunning || inTrash || projectRows.length === 0}
            title={inTrash ? t('crossRequirements.detail.inTrash') : undefined}
          >
            {design
              ? t('crossRequirements.detail.regenerateDesign')
              : t('crossRequirements.detail.generateDesign')}
          </button>
          <button
            type="button"
            className="btn btn-primary"
            onClick={startSplit}
            // A cross requirement is split at most once — the server refuses
            // a second split (409 XREQ_ALREADY_SPLIT), so the button is
            // disabled instead of leading the user into a dead end.
            disabled={jobRunning || inTrash || !design || alreadySplit}
            title={inTrash
              ? t('crossRequirements.detail.inTrash')
              : alreadySplit ? t('crossRequirements.detail.alreadySplit') : undefined}
          >
            {t('crossRequirements.detail.split')}
          </button>
          {/* 回收站：软删（可恢复，无需确认框）与恢复 / 彻底删除。 */}
          {inTrash ? (
            <>
              <button
                type="button"
                className="btn"
                onClick={restoreFromTrash}
                disabled={purging}
              >
                {t('crossRequirements.trash.restore')}
              </button>
              <button
                type="button"
                className="btn btn-danger"
                onClick={() => { setPurgeAck(false); setPurgeOpen(true); }}
                disabled={purging}
              >
                {t('crossRequirements.trash.purge')}
              </button>
            </>
          ) : (
            <button type="button" className="btn" onClick={moveToTrash}>
              {t('crossRequirements.trash.moveToTrash')}
            </button>
          )}
        </div>
      </div>

      {/* 回收站横幅：这一页此刻是只读的，把原因和出路都写清楚。 */}
      {inTrash && (
        <div className="xreq-trash-banner">
          <strong>{t('crossRequirements.detail.inTrash')}</strong>
          <span>{t('crossRequirements.detail.inTrashHint')}</span>
        </div>
      )}

      {/* ── split confirmation ───────────────────────────────────────── */}
      {/*
          The split confirmation used to be a modal. It is an inline panel
          now — and it sits right under the header rather than at the
          bottom of the page, because the user just pressed 「拆分」 at the
          top and has to be able to see what they are confirming without
          scrolling past the whole page.
      */}
      {confirming && (
        <section className="xreq-section xreq-split-panel" ref={splitPanelRef}>
          <h2 className="xreq-section-title">{t('crossRequirements.split.title')}</h2>
          {candidates.length === 0 ? (
            <div className="xreq-hint">{t('crossRequirements.split.empty')}</div>
          ) : (
            <div className="xreq-candidates">
              {candidates.map((c, i) => (
                <div key={i} className="xreq-candidate">
                  <div className="xreq-candidate-head">
                    <span className="xreq-candidate-title">{c.title}</span>
                    <span className="xreq-candidate-order">#{c.order}</span>
                  </div>
                  <div className="xreq-field">
                    <span className="xreq-field-label">{t('crossRequirements.split.project')}</span>
                    <select
                      className="form-input"
                      value={c.project_id}
                      onChange={e => updateCandidateProject(i, e.target.value)}
                    >
                      <option value="">{'-'}</option>
                      {projectRows.map(p => (
                        <option key={p.project_id} value={p.project_id}>
                          {p.project_name || p.project_id}
                        </option>
                      ))}
                    </select>
                  </div>
                  {c.depends_on.length > 0 && (
                    <div className="xreq-hint">
                      {t('crossRequirements.split.dependsOn')}：
                      {c.depends_on.map(d => titleById.get(String(d)) || `#${d}`).join('、')}
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}
          <div className="xreq-panel-actions">
            <button type="button" className="btn" onClick={() => setConfirming(false)}>
              {t('crossRequirements.common.cancel')}
            </button>
            <button
              type="button"
              className="btn btn-primary"
              onClick={confirmSplit}
              disabled={candidates.length === 0 || confirmingSplit}
            >
              {confirmingSplit
                ? t('crossRequirements.split.confirming')
                : t('crossRequirements.split.confirm')}
            </button>
          </div>
        </section>
      )}

      {error && <div className="xreq-error">{error}</div>}
      <CrossRequirementPurgeDialog
        open={purgeOpen}
        busy={purging}
        title={detail.title}
        childCount={children.length}
        ack={purgeAck}
        onAckChange={setPurgeAck}
        onConfirm={confirmPurge}
        onCancel={() => { if (!purging) { setPurgeOpen(false); setPurgeAck(false); } }}
      />
      {toast && <div className="xreq-toast">{toast}</div>}
      {projectRows.length === 0 && (
        <div className="xreq-hint">{t('crossRequirements.noProjects')}</div>
      )}

      {/* ── overview ─────────────────────────────────────────────────── */}
      <section className="xreq-section">
        <div className="xreq-meta">
          <span>{t('crossRequirements.detail.status')}：{statusLabel(detail.status)}</span>
          <span>{t('crossRequirements.detail.priority')}：{t(`crossRequirements.priority.${detail.priority}` as never)}</span>
          <span>{t('crossRequirements.projectsCount', { count: projectRows.length })}</span>
        </div>
        <div className="xreq-progress">
          <div className="xreq-progress-track">
            <div className="xreq-progress-fill" style={{ width: `${progress?.percent ?? 0}%` }} />
          </div>
          <span className="xreq-progress-text">
            {progress?.percent ?? 0}% ·{' '}
            {t('crossRequirements.childrenDone', { done: progress?.done ?? 0, total: progress?.total ?? 0 })}
          </span>
        </div>
      </section>

      {/* ── description ─────────────────────────────────────────────── */}
      {/*
          The description is the requirement's own brief: it goes to the AI
          together with the overall design, and it is where inline images
          live. It used to be invisible on this page — the row carried a
          description nobody could read back.

          The nova:// markers are rewritten here (not in the editor): the
          stored text keeps the stable `nova://attachment/<id>` form, and
          only the rendered copy points at the real bytes.
      */}
      <section className="xreq-section">
        <div className="xreq-section-head">
          <h2 className="xreq-section-title">{t('crossRequirements.detail.description')}</h2>
          <button
            type="button"
            className="btn btn-sm"
            onClick={() => setEditingDescription(v => !v)}
            disabled={inTrash}
          >
            {editingDescription
              ? t('crossRequirements.common.cancel')
              : t('crossRequirements.detail.edit')}
          </button>
        </div>

        {editingDescription ? (
          <CrossRequirementEditForm
            requirement={detail}
            onCancel={() => setEditingDescription(false)}
            onSaved={async () => {
              setEditingDescription(false);
              await load();
              setToast(t('crossRequirements.detail.edited'));
            }}
          />
        ) : detail.description ? (
          <div className="md-prose">
            <MarkdownRender
              content={rewriteNovaLinks(detail.description, detail.id, 'cross-requirements')}
            />
          </div>
        ) : (
          <div className="xreq-hint">{t('crossRequirements.detail.descriptionEmpty')}</div>
        )}
      </section>

      {/* ── participating projects ───────────────────────────────────── */}
      <section className="xreq-section">
        <div className="xreq-section-head">
          <h2 className="xreq-section-title">{t('crossRequirements.detail.projects')}</h2>
          <button
            type="button"
            className="btn btn-sm"
            onClick={() => setEditingProjects(v => !v)}
            disabled={inTrash}
          >
            {editingProjects
              ? t('crossRequirements.common.cancel')
              : t('crossRequirements.detail.editProjects')}
          </button>
        </div>

        {editingProjects ? (
          <div className="xreq-editor">
            <p className="xreq-field-hint">{t('crossRequirements.detail.baseBranchHint')}</p>
            <div className="xreq-picker">
              {allProjects.map(p => {
                const draft = projectDrafts.find(d => d.project_id === p.id);
                const checked = !!draft;
                return (
                  <div key={p.id} className="xreq-option">
                    <input
                      type="checkbox"
                      checked={checked}
                      onChange={() => toggleProjectDraft(p.id)}
                    />
                    <span>{p.name}</span>
                    {checked && (
                      <>
                        <input
                          className="form-input xreq-project-base"
                          list={`xreq-branches-${p.id}`}
                          placeholder={t('crossRequirements.detail.baseBranchPlaceholder')}
                          value={draft.base_branch}
                          onChange={e => updateProjectDraft(p.id, { base_branch: e.target.value })}
                        />
                        <input
                          className="form-input xreq-project-role"
                          placeholder={t('crossRequirements.roleLabel')}
                          value={draft.role}
                          onChange={e => updateProjectDraft(p.id, { role: e.target.value })}
                        />
                        <datalist id={`xreq-branches-${p.id}`}>
                          {(branchOptions[p.id] || []).map(b => (
                            <option key={b} value={b} />
                          ))}
                        </datalist>
                      </>
                    )}
                  </div>
                );
              })}
            </div>
            <div className="xreq-panel-actions">
              <button type="button" className="btn" onClick={() => setEditingProjects(false)}>
                {t('crossRequirements.common.cancel')}
              </button>
              <button
                type="button"
                className="btn btn-primary"
                onClick={saveProjects}
                disabled={savingProjects || projectDrafts.length === 0}
              >
                {t('crossRequirements.detail.saveProjects')}
              </button>
            </div>
          </div>
        ) : projectRows.length === 0 ? (
          <div className="xreq-hint">{t('crossRequirements.detail.projectsEmpty')}</div>
        ) : (
          <table className="xreq-table">
            <thead>
              <tr>
                <th>{t('crossRequirements.projectLabel')}</th>
                <th>{t('crossRequirements.roleLabel')}</th>
                <th>{t('crossRequirements.detail.baseBranch')}</th>
                <th>{t('crossRequirements.detail.designBaseline')}</th>
                <th>{t('crossRequirements.detail.matrix')}</th>
                <th>{t('crossRequirements.detail.progress')}</th>
              </tr>
            </thead>
            <tbody>
              {projectRows.map(p => {
                const base = baselineByProject[p.project_id];
                return (
                  <tr key={p.project_id}>
                    <td>{p.project_name || p.project_id}</td>
                    <td>{p.role || '-'}</td>
                    <td>{p.base_branch || t('crossRequirements.detail.baseBranchDefault')}</td>
                    <td>
                      {base?.base_sha
                        ? base.base_sha.slice(0, 7)
                        : base?.degraded
                          ? t('crossRequirements.detail.designBaselineUnlocked')
                          : '-'}
                    </td>
                    <td>{p.total}</td>
                    <td>{p.done}/{p.total}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </section>

      {/* ── overall design ───────────────────────────────────────────── */}
      <section className="xreq-section">
        <div className="xreq-section-head">
          <h2 className="xreq-section-title">{t('crossRequirements.detail.design')}</h2>
          {design && !editingDesign && (
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => { setDesignDraft(design); setEditingDesign(true); }}
              disabled={inTrash}
            >
              {t('crossRequirements.detail.editDesign')}
            </button>
          )}
        </div>
        {editingDesign ? (
          <div className="xreq-editor">
            <p className="xreq-field-hint">{t('crossRequirements.detail.editDesignHint')}</p>
            <textarea
              className="form-input"
              rows={18}
              value={designDraft}
              onChange={e => setDesignDraft(e.target.value)}
            />
            <div className="xreq-panel-actions">
              <button type="button" className="btn" onClick={() => setEditingDesign(false)}>
                {t('crossRequirements.common.cancel')}
              </button>
              <button type="button" className="btn btn-primary" onClick={saveDesign}>
                {t('crossRequirements.detail.saveDesign')}
              </button>
            </div>
          </div>
        ) : design ? (
          <>
            <div className="md-prose xreq-design">
              <MarkdownRender content={design} />
            </div>
            {/* 方案出来之后往往留着「待决策事项」——追加调整面板就是用来跟 AI
                把这些点谈拢的，谈完再重跑一次方案作业把结论吃进去。 */}
            <CrossDesignAdjustPanel
              xreqId={xreqId}
              pendingDecisions={detail.pending_decisions || []}
              disabled={inTrash || detail.status === 'archived'}
              onApplyDiscussion={() => startDesign({ apply_discussion: true })}
            />
          </>
        ) : (
          <div className="xreq-empty">
            <p className="xreq-empty-title">{t('crossRequirements.detail.designEmpty')}</p>
            <p className="xreq-empty-hint">{t('crossRequirements.detail.designEmptyHint')}</p>
          </div>
        )}
      </section>

      {/* ── sub-requirement matrix ───────────────────────────────────── */}
      <section className="xreq-section">
        <h2 className="xreq-section-title">{t('crossRequirements.detail.matrix')}</h2>
        {children.length === 0 ? (
          <div className="xreq-empty">
            <p className="xreq-empty-title">{t('crossRequirements.detail.matrixEmpty')}</p>
            <p className="xreq-empty-hint">{t('crossRequirements.detail.matrixEmptyHint')}</p>
          </div>
        ) : (
          <div className="xreq-matrix">
            {projectRows.map(p => {
              const cells = childrenByProject.get(p.project_id) || [];
              if (!cells.length) return null;
              return (
                <div key={p.project_id} className="xreq-matrix-row">
                  <div className="xreq-matrix-project">{p.project_name || p.project_id}</div>
                  <div className="xreq-matrix-cells">
                    {cells.map(c => {
                      const missing = c.blocked_by
                        .map(b => titleById.get(b) || b)
                        .join('、');
                      return (
                        <button
                          type="button"
                          key={c.id}
                          className="xreq-matrix-cell"
                          title={missing ? t('crossRequirements.detail.blockedBy', { titles: missing }) : c.title}
                          onClick={() => navigate(`/requirements/${c.id}`)}
                        >
                          <span className="xreq-cell-title">{c.title}</span>
                          <span className={`status-badge ${crossStatusBadgeClass(c.status)}`}>
                            {c.status}
                          </span>
                          {c.blocked_by.length > 0 && (
                            <span className="xreq-cell-blocked">⏳</span>
                          )}
                        </button>
                      );
                    })}
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </section>

      {/* ── cross-project dependencies ───────────────────────────────── */}
      <section className="xreq-section">
        <h2 className="xreq-section-title">{t('crossRequirements.detail.dependencies')}</h2>
        {(detail.dependencies || []).length === 0 ? (
          <div className="xreq-hint">{t('crossRequirements.detail.dependenciesEmpty')}</div>
        ) : (
          <ul className="xreq-deps">
            {detail.dependencies.map(dep => (
              <li key={`${dep.requirement_id}>${dep.depends_on_id}`} className="xreq-dep">
                <span className="xreq-dep-text">
                  {dep.requirement_title} ← {dep.depends_on_title}
                </span>
                {dep.cross_project && (
                  <span className="xreq-chip">{t('crossRequirements.detail.crossProject')}</span>
                )}
                {!dep.met && <span className="xreq-chip xreq-chip-warn">⏳</span>}
              </li>
            ))}
          </ul>
        )}
      </section>

      {/* ── phases ───────────────────────────────────────────────────── */}
      <section className="xreq-section">
        <div className="xreq-section-head">
          <h2 className="xreq-section-title">{t('crossRequirements.detail.phases')}</h2>
          <button type="button" className="btn btn-sm" onClick={() => setAddingPhase(v => !v)} disabled={inTrash}>
            {t('crossRequirements.detail.addPhase')}
          </button>
        </div>

        {addingPhase && (
          <div className="xreq-editor">
            <div className="xreq-field">
              <span className="xreq-field-label">{t('crossRequirements.detail.phaseName')}</span>
              <input
                className="form-input"
                value={phaseName}
                placeholder={t('crossRequirements.detail.phaseNamePlaceholder')}
                onChange={e => setPhaseName(e.target.value)}
              />
            </div>
            <div className="xreq-field">
              <span className="xreq-field-label">{t('crossRequirements.detail.phaseMembers')}</span>
              <div className="xreq-picker">
                {children.map(c => (
                  <label key={c.id} className="xreq-option">
                    <input
                      type="checkbox"
                      checked={phaseMembers.includes(c.id)}
                      onChange={() => setPhaseMembers(prev => (
                        prev.includes(c.id) ? prev.filter(x => x !== c.id) : [...prev, c.id]
                      ))}
                    />
                    <span>{c.title}</span>
                  </label>
                ))}
              </div>
            </div>
            <div className="xreq-panel-actions">
              <button type="button" className="btn" onClick={() => setAddingPhase(false)}>
                {t('crossRequirements.common.cancel')}
              </button>
              <button type="button" className="btn btn-primary" onClick={createPhase}>
                {t('crossRequirements.detail.phaseSave')}
              </button>
            </div>
          </div>
        )}

        {(detail.phases || []).length === 0 && !addingPhase ? (
          <div className="xreq-empty">
            <p className="xreq-empty-title">{t('crossRequirements.detail.phasesEmpty')}</p>
            <p className="xreq-empty-hint">{t('crossRequirements.detail.phasesEmptyHint')}</p>
          </div>
        ) : (
          <div className="xreq-phases">
            {detail.phases.map(phase => (
              <div key={phase.id} className="xreq-phase">
                <div className="xreq-phase-head">
                  <span className="xreq-phase-name">{phase.name}</span>
                  <span className={`status-badge ${crossPhaseBadgeClass(phase.status)}`}>
                    {phaseStatusLabel(phase.status)}
                  </span>
                  <button
                    type="button"
                    className="btn btn-sm"
                    onClick={() => startVerify(phase.id)}
                    disabled={jobRunning || inTrash || !crossPhaseCanVerify(phase.status)}
                  >
                    {phase.verify_report
                      ? t('crossRequirements.detail.reverify')
                      : t('crossRequirements.detail.verify')}
                  </button>
                  {/* Rework: a failed phase has to be able to go back to the
                      ready state — otherwise it is stuck on the verdict
                      forever: no new verification round, no way to re-judge. */}
                  {phase.status === 'failed' && (
                    <button
                      type="button"
                      className="btn btn-sm"
                      onClick={() => setPhaseStatus(phase, 'ready')}
                      disabled={inTrash}
                    >
                      {t('crossRequirements.detail.phaseReset')}
                    </button>
                  )}
                  <button
                    type="button"
                    className="btn btn-sm"
                    onClick={() => setPhaseStatus(phase, 'passed')}
                    disabled={inTrash}
                  >
                    {t('crossRequirements.detail.verifyPassed')}
                  </button>
                  <button
                    type="button"
                    className="btn btn-sm"
                    onClick={() => setPhaseStatus(phase, 'failed')}
                    disabled={inTrash}
                  >
                    {t('crossRequirements.detail.verifyFailed')}
                  </button>
                  <button
                    type="button"
                    className="btn btn-sm"
                    onClick={() => removePhase(phase)}
                    disabled={inTrash}
                  >
                    {t('crossRequirements.detail.phaseDelete')}
                  </button>
                </div>
                {phase.items.length > 0 && (
                  <div className="xreq-phase-members">
                    {phase.items.map(item => (
                      <span key={item.id} className="xreq-chip">
                        {titleById.get(item.requirement_id) || item.requirement_id}
                      </span>
                    ))}
                  </div>
                )}
                {phase.verify_report ? (
                  <div className="md-prose xreq-report">
                    <MarkdownRender content={phase.verify_report} />
                  </div>
                ) : (
                  <div className="xreq-hint">{t('crossRequirements.detail.verifyReportEmpty')}</div>
                )}
              </div>
            ))}
          </div>
        )}
      </section>

      {/* ── job panel ────────────────────────────────────────────────── */}
      {(jobRunning || jobLines.length > 0 || jobReconnecting || jobPolling) && (
        <section className="xreq-section">
          <CrossJobPanel
            title={jobTitle}
            running={jobRunning}
            reconnecting={jobReconnecting}
            polling={jobPolling}
            lost={jobLost}
            lines={jobLines}
          />
        </section>
      )}

    </div>
  );
}
