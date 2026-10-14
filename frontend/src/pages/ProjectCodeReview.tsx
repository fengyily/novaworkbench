// ProjectCodeReview — the project detail page's "代码 Review" tab, lifted out
// of ProjectDetail.tsx (which was well past 2000 lines) into its own file,
// mirroring ProjectWeeklyReport / ProjectIssueMonitor.
//
// Two modes share one output panel and one history list:
//
//   PR 模式     — the original flow: list open PRs from the platform, review
//                 one, then post the report back as a comment. Behaviour is
//                 unchanged; the state / handlers / markup were moved here
//                 verbatim.
//   分支 模式   — the new flow: pick any branch (including one that was never
//                 pushed), write what the change was meant to achieve, attach
//                 supporting files, then run the review. The description and
//                 attachments are what let the reviewer answer "目标是否达成".
//
// Both modes run on the backend's single JobStore+SSE channel
// (reviewApi.streamJobUrl) — branch review deliberately does not open a
// second stream.
//
// A branch review is a persisted session (`crv_*`): the draft row has to
// exist before attachments can be uploaded, because the row IS the
// attachment owner. That's why the composer is two-phase (create → describe
// + attach → start) rather than one big form.
//
// The report reads top-down as verdict → ledger → findings → full prose:
//
//   verdict  — one coloured band answering "can this merge?" before any
//              reading is required (ReviewVerdict below derives it from
//              status + the persisted P0/P1 counts).
//   ledger   — three count tiles, coloured only when non-zero so a single
//              P0 is findable at a glance.
//   findings — the graded items, grouped by severity; an empty bucket
//              collapses to one quiet line instead of a full section.
//   prose    — result_md behind a disclosure, rendered with the shared
//              `.md-prose` typography (styles/markdown-prose.css) so the
//              report looks the same here as in the knowledge base and the
//              design-doc viewer. Before that class existed this surface had
//              no typography at all and fell back to browser defaults.
//
// Findings rendering degrades on purpose: when the model honoured the
// trailing-JSON contract we group by P0/P1/P3 with counts; when it didn't,
// findings_json is empty, the verdict is 'plain', and the Markdown
// disclosure opens by itself (the report's own ## P0/P1/P3 sections mean
// nothing is lost).
//
// 追问 (follow-ups) hang off the selected report: each question is its own
// job on the same SSE channel, answered by resuming the review's claude
// session, and the whole thread is persisted server-side so it survives a
// reload. A follow-up never re-grades the review — the P0/P1/P3 counts you
// see always belong to the report above it.

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  reviewApi, codeReviewsApi, agentServersApi, parseReviewFindings,
  type PR, type PRListResponse, type Project,
  type CodeReview, type CodeReviewRunBinding, type ReviewFinding, type ReviewSeverity, type GoalMet,
  type CodeReviewFollowup,
  API_BASE, authedFetch,
} from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import AttachmentPanel from '../components/AttachmentPanel';
import ModelSelect from '../components/ModelSelect';
import { ExecEnvSelect, type ExecEnvServer } from '../components/ExecEnvSelect';
import MarkdownRender from '../components/MarkdownRender';
import { IconBranch, IconClock, IconFileText, IconPlug, IconRobot } from '../components/icons';
import { errorMessage } from '../utils/errMsg';
import { fmtDate, fmtDateTime } from '../utils/intl';
import '../styles/markdown-prose.css';
import './ProjectCodeReview.css';

type Mode = 'pr' | 'branch';

/** Display order of the severity buckets. P2 is intentionally absent. */
const SEVERITIES: ReviewSeverity[] = ['P0', 'P1', 'P3'];

/**
 * Headline state of a finished review, derived from status + counts.
 *
 *   block   — at least one P0: must not merge
 *   warn    — no P0 but some P1: merge after fixing
 *   clean   — graded, nothing blocking
 *   error   — the run itself failed
 *   pending — draft or still running, so there is no result yet
 *   plain   — the model broke the trailing-JSON contract, nothing to grade
 */
type ReviewVerdict = 'error' | 'block' | 'warn' | 'clean' | 'pending' | 'plain';

interface Props {
  projectId: string;
  project: Project | null;
}

export default function ProjectCodeReview({ projectId, project }: Props) {
  const { t } = useTranslation();
  const [mode, setMode] = useState<Mode>('pr');

  // ── PR mode (moved verbatim from ProjectDetail) ──────────────────────────
  const [prData, setPrData] = useState<PRListResponse | null>(null);
  const [prsLoading, setPrsLoading] = useState(false);
  const [prsError, setPrsError] = useState('');
  const [reviewingPR, setReviewingPR] = useState<PR | null>(null);
  const [extraRequirements, setExtraRequirements] = useState('');
  const [commentBody, setCommentBody] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [submitMsg, setSubmitMsg] = useState('');
  const lastReviewedPRRef = useRef<number>(0);

  // ── Shared stream panel ──────────────────────────────────────────────────
  const [reviewLines, setReviewLines] = useState<{ type: string; content: string }[]>([]);
  const [reviewDone, setReviewDone] = useState(false);
  // Effective model the reviewer role ran with for the most recent review
  // (from the job_done SSE frame). Empty until a review completes.
  const [reviewModel, setReviewModel] = useState('');
  const [running, setRunning] = useState(false);
  const reviewEsRef = useRef<EventStream | null>(null);
  const reviewPanelRef = useRef<HTMLDivElement>(null);

  // ── Run bindings (shared by both modes) ──────────────────────────────────
  // Which model to review with and where to run it. Both are per-session and
  // persisted on the code_reviews row, so re-running one from history
  // reproduces the same setup. '' means "reviewer role default" / 本地.
  const [runModel, setRunModel] = useState('');
  const [runConfigId, setRunConfigId] = useState('');
  const [agentServerId, setAgentServerId] = useState('');
  const [agentServers, setAgentServers] = useState<ExecEnvServer[]>([]);

  // ── Branch mode ──────────────────────────────────────────────────────────
  const [branches, setBranches] = useState<string[]>([]);
  const [branch, setBranch] = useState('');
  const [baseBranch, setBaseBranch] = useState('');
  const [draftTitle, setDraftTitle] = useState('');
  const [description, setDescription] = useState('');
  const [descSavedAt, setDescSavedAt] = useState(0);
  const [session, setSession] = useState<CodeReview | null>(null);
  const [creating, setCreating] = useState(false);
  const [branchError, setBranchError] = useState('');

  // ── History + result ─────────────────────────────────────────────────────
  const [history, setHistory] = useState<CodeReview[]>([]);
  const [selected, setSelected] = useState<CodeReview | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [reportOpen, setReportOpen] = useState(false);

  // ── 追问 ─────────────────────────────────────────────────────────────────
  // The thread belongs to the report currently in the result view, and runs
  // on its own stream so asking a question never clobbers the review log
  // still on screen above it.
  const [followups, setFollowups] = useState<CodeReviewFollowup[]>([]);
  const [question, setQuestion] = useState('');
  const [askRunning, setAskRunning] = useState(false);
  const [askLines, setAskLines] = useState<{ type: string; content: string }[]>([]);
  const [askError, setAskError] = useState('');
  const askEsRef = useRef<EventStream | null>(null);
  const askPanelRef = useRef<HTMLDivElement>(null);

  // Close the SSE stream when the tab unmounts so a navigated-away review
  // doesn't keep a dangling reader around.
  useEffect(() => () => { reviewEsRef.current?.close(); askEsRef.current?.close(); }, []);

  // Keep the live stream pinned to the newest line (behaviour carried over
  // from ProjectDetail's review panel).
  useEffect(() => {
    if (reviewPanelRef.current) reviewPanelRef.current.scrollTop = reviewPanelRef.current.scrollHeight;
  }, [reviewLines]);

  useEffect(() => {
    if (askPanelRef.current) askPanelRef.current.scrollTop = askPanelRef.current.scrollHeight;
  }, [askLines]);

  // ── Loaders ──────────────────────────────────────────────────────────────

  const loadPRs = useCallback(() => {
    setPrsLoading(true);
    setPrsError('');
    reviewApi.listPRs(projectId)
      .then(setPrData)
      .catch(err => setPrsError(errorMessage(err)))
      .finally(() => setPrsLoading(false));
  }, [projectId]);

  const loadHistory = useCallback(() => {
    codeReviewsApi.list(projectId)
      .then(rows => setHistory(Array.isArray(rows) ? rows : []))
      .catch(() => setHistory([]));
  }, [projectId]);

  useEffect(() => { loadPRs(); }, [loadPRs]);
  useEffect(() => { loadHistory(); }, [loadHistory]);

  // The 追问 thread follows whichever report is in the result view. Keyed on
  // the id (not the object) so re-reading the same session after a run
  // doesn't refetch the thread it just updated.
  const selectedId = selected?.id ?? '';
  useEffect(() => {
    setAskLines([]);
    setAskError('');
    setQuestion('');
    if (!selectedId) { setFollowups([]); return; }
    let cancelled = false;
    codeReviewsApi.listFollowups(selectedId)
      .then(rows => { if (!cancelled) setFollowups(Array.isArray(rows) ? rows : []); })
      .catch(() => { if (!cancelled) setFollowups([]); });
    return () => { cancelled = true; };
  }, [selectedId]);

  // Only ready servers are offered — the backend refuses the rest, so listing
  // them would only mislead. A failure leaves the list empty, which still
  // renders the 本地 option.
  useEffect(() => {
    agentServersApi.list()
      .then(rows => setAgentServers((rows ?? []).filter(srv => srv.status === 'ready')))
      .catch(() => {/* 设置 → Agent 服务器 is the source of truth */});
  }, []);

  // Branch candidates for the two selectors. Best-effort, mirroring
  // ProjectDetail's existing fetch: on failure the list stays empty and the
  // user types the branch name by hand.
  useEffect(() => {
    const localPath = project?.local_path;
    if (mode !== 'branch' || !localPath) return;
    let cancelled = false;
    authedFetch(`${API_BASE}/api/fs/git-branches?path=${encodeURIComponent(localPath)}`)
      .then((r: Response) => r.json())
      .then((json: { success?: boolean; data?: { branches?: string[] } }) => {
        if (cancelled) return;
        setBranches(json?.success && Array.isArray(json.data?.branches) ? json.data.branches : []);
      })
      .catch(() => { if (!cancelled) setBranches([]); });
    return () => { cancelled = true; };
  }, [mode, project?.local_path]);

  // Default the base branch to the project's configured default once.
  useEffect(() => {
    if (!baseBranch && project?.default_branch) setBaseBranch(project.default_branch);
  }, [project?.default_branch, baseBranch]);

  // ── Stream plumbing (shared by both modes) ───────────────────────────────

  // subscribeJob wires the SSE stream for a started job. onDone receives the
  // concatenated assistant text so PR mode can seed its comment draft.
  const subscribeJob = useCallback((jobId: string, onDone: (text: string) => void) => {
    const messageLines: string[] = [];
    reviewEsRef.current?.close();
    reviewEsRef.current = createEventStream(
      reviewApi.streamJobUrl(projectId, jobId),
      (data) => {
        if (data.type === 'job_done') {
          setReviewDone(true);
          setReviewModel(data.model || '');
          setRunning(false);
          setReviewingPR(null);
          onDone(messageLines.join('\n\n'));
          reviewEsRef.current?.close();
          reviewEsRef.current = null;
        } else if (data.content) {
          setReviewLines(prev => [...prev, { type: data.type, content: data.content }]);
          if (data.type === 'message') messageLines.push(data.content);
        }
      },
      () => {
        setReviewLines(prev => [...prev, { type: 'error', content: t('projects.detail.reviewSseDrop') }]);
        setReviewDone(true);
        setRunning(false);
        setReviewingPR(null);
        onDone(messageLines.join('\n\n'));
        reviewEsRef.current = null;
      },
    );
  }, [projectId, t]);

  // The three toolbar picks, in the wire shape both entry points accept.
  const runBinding = useCallback((): CodeReviewRunBinding => ({
    model: runModel,
    claude_config_id: runConfigId,
    agent_server_id: agentServerId,
  }), [runModel, runConfigId, agentServerId]);

  const resetPanel = () => {
    setReviewLines([]);
    setReviewDone(false);
    setReviewModel('');
    setSubmitMsg('');
    setSelected(null);
  };

  // ── PR mode handlers ─────────────────────────────────────────────────────

  const handleReview = useCallback(async (pr: PR) => {
    if (running) return;
    lastReviewedPRRef.current = pr.number;
    setReviewingPR(pr);
    setRunning(true);
    setReviewLines([]);
    setReviewDone(false);
    setReviewModel('');
    setCommentBody('');
    setSubmitMsg('');
    setSelected(null);

    try {
      const { job_id } = await reviewApi.startReview(
        projectId, pr.head_branch, pr.base_branch, pr.number, pr.title, extraRequirements, runBinding());
      subscribeJob(job_id, (text) => { setCommentBody(text); loadHistory(); });
    } catch (err: unknown) {
      setReviewLines([{ type: 'error', content: errorMessage(err) }]);
      setReviewingPR(null);
      setRunning(false);
    }
  }, [projectId, running, extraRequirements, subscribeJob, loadHistory, runBinding]);

  const handleSubmitComment = async () => {
    if (!commentBody || lastReviewedPRRef.current === 0) return;
    setSubmitting(true);
    setSubmitMsg('');
    try {
      await reviewApi.submitComment(projectId, lastReviewedPRRef.current, commentBody);
      setSubmitMsg(t('projects.detail.reviewSubmitOk'));
    } catch (err: unknown) {
      setSubmitMsg(t('projects.detail.reviewErrPrefix') + errorMessage(err));
    } finally { setSubmitting(false); }
  };

  // ── Branch mode handlers ─────────────────────────────────────────────────

  // Creating the draft is what gives attachments an owner id, so the panel
  // only appears after this resolves.
  const handleCreateDraft = async () => {
    if (!branch.trim()) {
      setBranchError(t('projects.detail.reviewNeedBranch'));
      return;
    }
    setCreating(true);
    setBranchError('');
    try {
      const rv = await codeReviewsApi.create(projectId, {
        branch: branch.trim(),
        base_branch: baseBranch.trim(),
        title: draftTitle.trim(),
        description,
        ...runBinding(),
      });
      setSession(rv);
      setBaseBranch(rv.base_branch);
      loadHistory();
    } catch (err: unknown) {
      setBranchError(errorMessage(err));
    } finally { setCreating(false); }
  };

  // Description / title / branches are saved on blur rather than per
  // keystroke — the row is only read when the run starts.
  //
  // `override` exists for the toolbar pickers: they fire from an onChange, so
  // the state they just set isn't readable here yet. Passing the new value
  // explicitly lets a model / exec-env change persist immediately instead of
  // waiting for the next blur.
  const saveDraftField = async (override?: CodeReviewRunBinding) => {
    if (!session || session.status === 'running') return;
    try {
      const rv = await codeReviewsApi.update(session.id, {
        branch: branch.trim(),
        base_branch: baseBranch.trim(),
        title: draftTitle.trim(),
        description,
        ...runBinding(),
        ...(override ?? {}),
      });
      setSession(rv);
      setDescSavedAt(Date.now());
    } catch { /* non-fatal: the start call re-sends nothing, so surface on start instead */ }
  };

  const handleStartBranchReview = async () => {
    if (!session || running) return;
    setRunning(true);
    setReviewLines([]);
    setReviewDone(false);
    setReviewModel('');
    setSelected(null);
    setBranchError('');
    try {
      // Flush any un-blurred edits so the run sees the latest description.
      await saveDraftField();
      const { job_id } = await codeReviewsApi.start(session.id);
      subscribeJob(job_id, () => {
        loadHistory();
        // Re-read the session so the graded findings (written by the stream
        // tail) land in the result view.
        codeReviewsApi.get(session.id).then(rv => {
          setSession(rv);
          setSelected(rv);
        }).catch(() => {});
      });
    } catch (err: unknown) {
      setReviewLines([{ type: 'error', content: errorMessage(err) }]);
      setRunning(false);
    }
  };

  const handleNewSession = () => {
    setSession(null);
    setDescription('');
    setDraftTitle('');
    setDescSavedAt(0);
    setRunModel('');
    setRunConfigId('');
    setAgentServerId('');
    resetPanel();
  };

  const handleDelete = async (rv: CodeReview) => {
    if (!window.confirm(t('projects.detail.reviewDeleteConfirm'))) return;
    try {
      await codeReviewsApi.remove(rv.id);
      if (session?.id === rv.id) handleNewSession();
      if (selected?.id === rv.id) setSelected(null);
      loadHistory();
    } catch (err: unknown) {
      setBranchError(errorMessage(err));
    }
  };

  // Clicking a history row loads its persisted report into the result view.
  const handleOpenHistory = async (rv: CodeReview) => {
    setDetailLoading(true);
    try {
      const full = await codeReviewsApi.get(rv.id);
      setSelected(full);
      // Seed the toolbar from the stored row so 重跑 reproduces the same
      // model + execution environment. The backend stamps `model` with the
      // EFFECTIVE model after a run; ModelSelect maps its DefaultModelLabel
      // literal back to "use the role default".
      setRunModel(full.model);
      setRunConfigId(full.claude_config_id);
      setAgentServerId(full.agent_server_id);
      if (full.review_type === 'branch') {
        setMode('branch');
        setSession(full);
        setBranch(full.branch);
        setBaseBranch(full.base_branch);
        setDraftTitle(full.title);
        setDescription(full.description);
      }
    } catch { setSelected(rv); } finally { setDetailLoading(false); }
  };

  const handleRerun = async (rv: CodeReview) => {
    await handleOpenHistory(rv);
    setRunning(true);
    setReviewLines([]);
    setReviewDone(false);
    try {
      const { job_id } = await codeReviewsApi.start(rv.id);
      subscribeJob(job_id, () => {
        loadHistory();
        codeReviewsApi.get(rv.id).then(setSelected).catch(() => {});
      });
    } catch (err: unknown) {
      setReviewLines([{ type: 'error', content: errorMessage(err) }]);
      setRunning(false);
    }
  };

  // ── 追问 handlers ────────────────────────────────────────────────────────

  const reloadFollowups = useCallback((reviewId: string) => {
    codeReviewsApi.listFollowups(reviewId)
      .then(rows => setFollowups(Array.isArray(rows) ? rows : []))
      .catch(() => {/* the turn is persisted either way; the next open re-reads it */});
  }, []);

  // One question = one job on the shared review SSE channel. The answer is
  // persisted by the backend, so when the job ends we just re-read the thread
  // instead of trying to reconstruct it from the stream.
  const handleAsk = async () => {
    if (!selected || askRunning || !question.trim()) return;
    const reviewId = selected.id;
    setAskRunning(true);
    setAskError('');
    setAskLines([]);
    try {
      const { job_id } = await codeReviewsApi.ask(reviewId, question.trim());
      setQuestion('');
      askEsRef.current?.close();
      askEsRef.current = createEventStream(
        reviewApi.streamJobUrl(projectId, job_id),
        (data) => {
          if (data.type === 'job_done') {
            setAskRunning(false);
            askEsRef.current?.close();
            askEsRef.current = null;
            reloadFollowups(reviewId);
          } else if (data.content) {
            setAskLines(prev => [...prev, { type: data.type, content: data.content }]);
          }
        },
        () => {
          setAskRunning(false);
          setAskError(t('projects.detail.reviewSseDrop'));
          askEsRef.current = null;
          // The run itself may well have finished — re-read before concluding
          // anything from the dropped stream.
          reloadFollowups(reviewId);
        },
      );
    } catch (err: unknown) {
      setAskError(errorMessage(err));
      setAskRunning(false);
    }
  };

  const handleDeleteFollowup = async (fu: CodeReviewFollowup) => {
    if (!window.confirm(t('projects.detail.reviewFollowupDeleteConfirm'))) return;
    try {
      await codeReviewsApi.removeFollowup(fu.review_id, fu.id);
      reloadFollowups(fu.review_id);
    } catch (err: unknown) {
      setAskError(errorMessage(err));
    }
  };

  // ── Derived ──────────────────────────────────────────────────────────────

  const findings = useMemo(() => parseReviewFindings(selected), [selected]);
  const grouped = useMemo(() => {
    const m = new Map<ReviewSeverity, ReviewFinding[]>(SEVERITIES.map(s => [s, []]));
    for (const f of findings) m.get(f.severity)?.push(f);
    return m;
  }, [findings]);

  // Counts come from the persisted columns rather than the decoded array —
  // the backend derived both from the same JSON, and the columns stay
  // meaningful (all zero) on the degraded path where findings_json is empty.
  const counts = useMemo<Record<ReviewSeverity, number>>(() => ({
    P0: selected?.p0_count ?? 0,
    P1: selected?.p1_count ?? 0,
    P3: selected?.p3_count ?? 0,
  }), [selected]);

  // The verdict is the one thing a reader wants before anything else: can
  // this be merged? It drives the report's accent colour and headline.
  // 'plain' is the degraded path — the model broke the trailing-JSON
  // contract, so there is nothing to grade and the Markdown report leads.
  const verdict = useMemo<ReviewVerdict>(() => {
    if (!selected) return 'plain';
    if (selected.status === 'error') return 'error';
    // A draft picked from history has never produced a report — saying
    // "ungraded" there would read as a model failure rather than "not run".
    if (selected.status === 'draft' || selected.status === 'running') return 'pending';
    if (!selected.findings_json) return 'plain';
    if (counts.P0 > 0) return 'block';
    if (counts.P1 > 0) return 'warn';
    return 'clean';
  }, [selected, counts]);
  // Same TitleCase-the-discriminant convention as `statusLabel` below.
  const verdictKey = verdict.charAt(0).toUpperCase() + verdict.slice(1);

  // The full Markdown report is a disclosure: when the findings are graded
  // they lead and the prose is backup, but on the degraded path the prose is
  // all there is, so it opens by itself.
  useEffect(() => {
    setReportOpen(!selected?.findings_json);
  }, [selected?.id, selected?.findings_json]);

  const statusLabel = (s: CodeReview['status']) => t(`projects.detail.reviewStatus${s.charAt(0).toUpperCase()}${s.slice(1)}`);
  const goalLabel = (g: GoalMet) => {
    switch (g) {
      case 'yes': return t('projects.detail.reviewGoalYes');
      case 'partial': return t('projects.detail.reviewGoalPartial');
      case 'no': return t('projects.detail.reviewGoalNo');
      default: return t('projects.detail.reviewGoalUnknown');
    }
  };

  // ── Render ───────────────────────────────────────────────────────────────

  return (
    <div className="tab-content project-code-review">
      {/* Mode switch */}
      <div className="cr-mode-bar">
        <div className="cr-mode-tabs">
          <button
            className={`cr-mode-tab${mode === 'pr' ? ' active' : ''}`}
            onClick={() => setMode('pr')}
            disabled={running}
          >{t('projects.detail.reviewModePR')}</button>
          <button
            className={`cr-mode-tab${mode === 'branch' ? ' active' : ''}`}
            onClick={() => setMode('branch')}
            disabled={running}
          >{t('projects.detail.reviewModeBranch')}</button>
        </div>
        <span className="cr-mode-hint">
          {mode === 'pr' ? t('projects.detail.reviewModeHintPR') : t('projects.detail.reviewModeHintBranch')}
        </span>
      </div>

      {/* Run bindings — shared by both modes. The picks live on the review
          session row, so a 重跑 from history reuses the same model and the
          same execution environment. */}
      <div className="cr-run-bar">
        <div className="cr-run-field">
          <label className="cr-label">{t('projects.detail.reviewModelLabel')}</label>
          <ModelSelect
            value={runModel}
            onChange={(m) => { setRunModel(m); if (session) void saveDraftField({ model: m }); }}
            configId={runConfigId}
            onConfigChange={(c) => { setRunConfigId(c); if (session) void saveDraftField({ claude_config_id: c }); }}
            disabled={running}
            title={t('projects.detail.reviewModelHint')}
          />
        </div>
        <div className="cr-run-field">
          <label className="cr-label">{t('projects.detail.reviewExecEnvLabel')}</label>
          <ExecEnvSelect
            servers={agentServers}
            value={agentServerId}
            onChange={(v) => { setAgentServerId(v); if (session) void saveDraftField({ agent_server_id: v }); }}
            disabled={running}
            title={t('projects.detail.reviewExecEnvHint')}
          />
          <span className="cr-hint">
            {agentServerId ? t('projects.detail.reviewExecEnvRemoteHint') : t('projects.detail.reviewExecEnvLocalHint')}
          </span>
        </div>
      </div>

      {/* ── PR mode ── */}
      {mode === 'pr' && (
        <>
          {!prsLoading && prData && !prData.configured && (
            <div className="review-unconfigured">
              <div className="review-unconfigured-icon"><IconPlug size={28} /></div>
              <p>{t('projects.detail.reviewUnconfiguredTitle')}</p>
              <p>
                {t('projects.detail.reviewUnconfiguredHint')}
                <a href="/settings" style={{ color: 'var(--color-primary)', margin: '0 4px' }}>{t('projects.detail.platformSettingsLink')}</a>
                {t('projects.detail.reviewUnconfiguredHint2')}
              </p>
            </div>
          )}

          {prsLoading && <div className="tab-empty">{t('projects.detail.reviewLoading')}</div>}
          {prsError && <div className="tab-empty" style={{ color: 'var(--color-error)' }}>{prsError}</div>}

          {!prsLoading && prData && (prData.configured || prData.prs.length > 0) && (
            <>
              {prData.prs.length === 0 && (
                <div className="tab-empty"><p>{t('projects.detail.reviewNoPRs')}</p></div>
              )}
              {prData.prs.length > 0 && (
                <>
                  <div className="review-requirements-bar">
                    <label className="review-requirements-label">{t('projects.detail.reviewExtraLabel')}</label>
                    <input
                      className="form-input review-requirements-input"
                      placeholder={t('projects.detail.reviewExtraPlaceholder')}
                      value={extraRequirements}
                      onChange={e => setExtraRequirements(e.target.value)}
                      disabled={running}
                    />
                  </div>
                  <div className="pr-list">
                    <table className="pr-table table-cards">
                      <thead>
                        <tr>
                          <th style={{ width: 52 }}>{t('projects.detail.reviewColNumber')}</th>
                          <th>{t('projects.detail.reviewColTitle')}</th>
                          <th style={{ width: 260 }}>{t('projects.detail.reviewColBranch')}</th>
                          <th style={{ width: 100 }}>{t('projects.detail.reviewColAuthor')}</th>
                          <th style={{ width: 90 }}>{t('projects.detail.reviewColUpdatedAt')}</th>
                          <th style={{ width: 120 }}>{t('projects.detail.reviewColActions')}</th>
                        </tr>
                      </thead>
                      <tbody>
                        {prData.prs.map(pr => (
                          <tr key={pr.number} className={reviewingPR?.number === pr.number ? 'pr-row-active' : ''}>
                            <td data-label="#" style={{ color: 'var(--color-text-muted)', fontFamily: 'var(--font-mono)', fontSize: 12 }}>#{pr.number}</td>
                            <td data-label={t('projects.detail.reviewColTitle')} className="pr-title">
                              <a href={pr.html_url} target="_blank" rel="noreferrer" style={{ color: 'var(--color-primary)' }}>
                                {pr.title}
                              </a>
                            </td>
                            <td data-label={t('projects.detail.reviewColBranch')}><code className="pr-branch">{pr.head_branch}</code> ← <code className="pr-branch">{pr.base_branch}</code></td>
                            <td data-label={t('projects.detail.reviewColAuthor')}>{pr.author}</td>
                            <td data-label={t('projects.detail.reviewColUpdatedAt')}>{pr.updated_at ? fmtDate(pr.updated_at) : '—'}</td>
                            <td data-label={t('projects.detail.reviewColActions')}>
                              <button
                                className="btn btn-primary btn-sm"
                                onClick={() => handleReview(pr)}
                                disabled={running}
                              >
                                {reviewingPR?.number === pr.number ? t('projects.detail.reviewBtnActive') : t('projects.detail.reviewBtn')}
                              </button>
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                </>
              )}
            </>
          )}
        </>
      )}

      {/* ── Branch mode composer ── */}
      {mode === 'branch' && (
        <div className="cr-composer">
          <div className="cr-field-row">
            <div className="cr-field">
              <label className="cr-label">{t('projects.detail.reviewBranchLabel')}</label>
              <input
                className="form-input"
                list="cr-branch-options"
                placeholder={t('projects.detail.reviewBranchPlaceholder')}
                value={branch}
                onChange={e => setBranch(e.target.value)}
                onBlur={() => { if (session) void saveDraftField(); }}
                disabled={running}
              />
            </div>
            <div className="cr-field">
              <label className="cr-label">{t('projects.detail.reviewBaseLabel')}</label>
              <input
                className="form-input"
                list="cr-branch-options"
                placeholder={t('projects.detail.reviewBasePlaceholder')}
                value={baseBranch}
                onChange={e => setBaseBranch(e.target.value)}
                onBlur={() => { if (session) void saveDraftField(); }}
                disabled={running}
              />
            </div>
            <datalist id="cr-branch-options">
              {branches.map(b => <option key={b} value={b} />)}
            </datalist>
          </div>
          <p className="cr-hint">{t('projects.detail.reviewBranchesHint')}</p>

          <div className="cr-field">
            <label className="cr-label">{t('projects.detail.reviewTitleLabel')}</label>
            <input
              className="form-input"
              placeholder={t('projects.detail.reviewTitlePlaceholder')}
              value={draftTitle}
              onChange={e => setDraftTitle(e.target.value)}
              onBlur={() => { if (session) void saveDraftField(); }}
              disabled={running}
            />
          </div>

          <div className="cr-field">
            <label className="cr-label">
              {t('projects.detail.reviewDescLabel')}
              {descSavedAt > 0 && <span className="cr-saved">{t('projects.detail.reviewDescSaved')}</span>}
            </label>
            <textarea
              className="form-input cr-desc"
              rows={5}
              placeholder={t('projects.detail.reviewDescPlaceholder')}
              value={description}
              onChange={e => setDescription(e.target.value)}
              onBlur={() => { if (session) void saveDraftField(); }}
              disabled={running}
            />
          </div>

          {/* Attachments need the draft row as their owner, so they only
              appear once the session exists. */}
          {session ? (
            <div className="cr-attach">
              <div className="cr-attach-head">
                <span className="cr-label">{t('projects.detail.reviewAttachTitle')}</span>
                <span className="cr-hint">{t('projects.detail.reviewAttachHint')}</span>
              </div>
              <AttachmentPanel ownerId={session.id} resource="code-reviews" />
            </div>
          ) : (
            <p className="cr-hint">{t('projects.detail.reviewCreateDraftHint')}</p>
          )}

          {branchError && <div className="cr-error">{branchError}</div>}

          <div className="cr-actions">
            {!session ? (
              <button className="btn btn-primary" onClick={handleCreateDraft} disabled={creating}>
                {creating ? t('projects.detail.reviewCreating') : t('projects.detail.reviewCreateDraft')}
              </button>
            ) : (
              <>
                <button className="btn btn-primary" onClick={handleStartBranchReview} disabled={running}>
                  {running ? t('projects.detail.reviewStartBusy') : t('projects.detail.reviewStartBtn')}
                </button>
                <button className="btn btn-secondary" onClick={handleNewSession} disabled={running}>
                  {t('projects.detail.reviewResetBtn')}
                </button>
                <button className="btn btn-secondary" onClick={() => handleDelete(session)} disabled={running}>
                  {t('projects.detail.reviewDeleteBtn')}
                </button>
              </>
            )}
          </div>
        </div>
      )}

      {/* ── Live stream (shared) ── */}
      {(reviewLines.length > 0 || running) && (
        <div className="review-panel-wrap cr-stream">
          <div className="review-panel-header">
            <span className="cr-stream-title">
              {running && !reviewDone && <span className="cr-pulse" aria-hidden="true" />}
              {t('projects.detail.reviewHeader')}
              {reviewingPR ? `：#${reviewingPR.number} ${reviewingPR.title}` : ''}
              {reviewDone ? t('projects.detail.reviewHeaderDone') : ''}
            </span>
            {reviewDone && (
              <button className="btn btn-secondary btn-sm" onClick={resetPanel}>
                {t('projects.detail.reviewDoneBtn')}
              </button>
            )}
          </div>
          <div className="coding-panel" ref={reviewPanelRef} style={{ maxHeight: 360 }}>
            {reviewLines.map((line, i) => (
              <div key={i} className={`coding-line coding-line-${line.type}`}>{line.content}</div>
            ))}
            {running && !reviewDone && (
              <div className="coding-line coding-line-tool_call">{t('projects.detail.reviewProgress')}</div>
            )}
          </div>
        </div>
      )}

      {/* ── Graded result ── */}
      {detailLoading && <div className="tab-empty">{t('projects.detail.reviewLoadingDetail')}</div>}
      {selected && !detailLoading && (
        <article className={`cr-report cr-report-${verdict}`}>
          {/* Verdict band — the "can this merge?" answer, stated once, in
              colour, above everything else. */}
          <header className="cr-verdict">
            <span className="cr-verdict-mark" aria-hidden="true" />
            <div className="cr-verdict-text">
              <h3 className="cr-verdict-title">{t(`projects.detail.reviewVerdict${verdictKey}`)}</h3>
              <p className="cr-verdict-sub">{t(`projects.detail.reviewVerdictHint${verdictKey}`)}</p>
            </div>
            <div className="cr-verdict-meta">
              <span className={`cr-goal cr-goal-${selected.goal_met || 'unknown'}`}>
                {t('projects.detail.reviewGoalLabel')}：{goalLabel(selected.goal_met)}
              </span>
              <span className="cr-meta-chip">
                <IconBranch size={12} />
                <code>{selected.review_type === 'pr' && selected.pr_number > 0 ? `#${selected.pr_number}` : selected.branch}</code>
                {selected.base_branch ? <> ← <code>{selected.base_branch}</code></> : null}
              </span>
              {selected.model && (
                <span className="cr-meta-chip" title={t('projects.detail.reviewModelLine', { model: selected.model })}>
                  <IconRobot size={12} />{selected.model}
                </span>
              )}
              <span className="cr-meta-chip">
                <IconClock size={12} />{fmtDateTime(selected.finished_at || selected.created_at)}
              </span>
            </div>
          </header>

          {/* Severity ledger — three tiles, one glance. Zero stays grey so
              the eye lands only on what actually needs attention. */}
          {selected.findings_json ? (
            <div className="cr-ledger">
              {SEVERITIES.map(sev => (
                <div key={sev} className={`cr-ledger-cell cr-ledger-${sev}${counts[sev] === 0 ? ' is-zero' : ''}`}>
                  <span className="cr-ledger-num">{counts[sev]}</span>
                  <span className="cr-ledger-label">
                    <span className="cr-ledger-tag">{sev}</span>
                    {t(`projects.detail.reviewSevShort${sev}`)}
                  </span>
                </div>
              ))}
            </div>
          ) : null}

          {findings.length > 0 && (
            <div className="cr-findings">
              {SEVERITIES.map(sev => {
                const items = grouped.get(sev) ?? [];
                // An empty bucket collapses to one quiet line — rendering a
                // full empty section for each would triple the noise on a
                // report that only found P3s.
                if (items.length === 0) {
                  return (
                    <p key={sev} className="cr-sev-empty">
                      <span className={`cr-sev-badge cr-sev-badge-${sev}`}>{sev}</span>
                      {t('projects.detail.reviewSevEmpty')}
                    </p>
                  );
                }
                return (
                  <section key={sev} className={`cr-sev cr-sev-${sev}`}>
                    <h4 className="cr-sev-head">
                      <span className={`cr-sev-badge cr-sev-badge-${sev}`}>{sev}</span>
                      <span className="cr-sev-name">{t(`projects.detail.reviewSev${sev}`)}</span>
                      <span className="cr-sev-count">{items.length}</span>
                    </h4>
                    <ol className="cr-finding-list">
                      {items.map((f, i) => (
                        <li key={i} className="cr-finding">
                          <div className="cr-finding-head">
                            <span className="cr-finding-idx">{sev}-{i + 1}</span>
                            <span className="cr-finding-title">{f.title}</span>
                            {(f.file || f.line) && (
                              <code className="cr-finding-loc">{f.file}{f.line ? `:${f.line}` : ''}</code>
                            )}
                          </div>
                          {f.detail && <p className="cr-finding-detail">{f.detail}</p>}
                          {f.suggestion && (
                            <p className="cr-finding-suggestion">
                              <span className="cr-finding-suggestion-tag">{t('projects.detail.reviewFindingSuggestion')}</span>
                              {f.suggestion}
                            </p>
                          )}
                        </li>
                      ))}
                    </ol>
                  </section>
                );
              })}
            </div>
          )}

          {/* Full Markdown report. Collapsed behind a disclosure when the
              findings above already carry the verdict; open by default when
              they don't (degraded path), so nothing is ever hidden. */}
          {selected.result_md && (
            <details className="cr-report-md" open={reportOpen} onToggle={e => setReportOpen(e.currentTarget.open)}>
              <summary className="cr-report-md-summary">
                <IconFileText size={13} />
                {t('projects.detail.reviewFullReport')}
              </summary>
              <div className="cr-report-md-body md-prose">
                <MarkdownRender content={selected.result_md} />
              </div>
            </details>
          )}

          {/* ── 追问 ── Asks run against THIS report; they never change its
              findings or counts. */}
          <div className="cr-followups">
            <div className="cr-followups-head">
              <span className="cr-label">{t('projects.detail.reviewFollowupTitle')}</span>
              <span className="cr-hint">{t('projects.detail.reviewFollowupHint')}</span>
            </div>

            {followups.length === 0 && !askRunning && (
              <p className="cr-sev-empty">{t('projects.detail.reviewFollowupEmpty')}</p>
            )}

            {followups.map(fu => (
              <article key={fu.id} className={`cr-followup cr-followup-${fu.status}`}>
                <div className="cr-followup-q">
                  <span className="cr-followup-tag">{t('projects.detail.reviewFollowupQ')}</span>
                  <span className="cr-followup-qtext">{fu.question}</span>
                  <button
                    className="btn btn-sm cr-followup-del"
                    onClick={() => handleDeleteFollowup(fu)}
                    disabled={fu.status === 'running'}
                  >{t('projects.detail.reviewDeleteBtn')}</button>
                </div>
                {fu.status === 'running' ? (
                  <p className="cr-followup-pending">{t('projects.detail.reviewFollowupRunning')}</p>
                ) : fu.status === 'error' ? (
                  <p className="cr-followup-errtext">{t('projects.detail.reviewFollowupFailed')}：{fu.answer}</p>
                ) : (
                  <div className="cr-followup-a">
                    <span className="cr-followup-tag">{t('projects.detail.reviewFollowupA')}</span>
                    <MarkdownRender content={fu.answer} />
                  </div>
                )}
                <div className="cr-followup-meta">
                  <span>{fmtDateTime(fu.created_at)}</span>
                  {fu.model && <span><IconRobot size={12} /> {fu.model}</span>}
                  {/* Only worth flagging the degraded case: a resumed answer
                      is the expectation, a fresh one explains why an answer
                      may be shallower than the report it talks about. */}
                  {fu.status === 'done' && !fu.resumed && (
                    <span className="cr-followup-fresh">{t('projects.detail.reviewFollowupFresh')}</span>
                  )}
                </div>
              </article>
            ))}

            {askRunning && (
              <div className="coding-panel cr-followup-stream" ref={askPanelRef} style={{ maxHeight: 220 }}>
                {askLines.map((line, i) => (
                  <div key={i} className={`coding-line coding-line-${line.type}`}>{line.content}</div>
                ))}
                <div className="coding-line coding-line-tool_call">{t('projects.detail.reviewFollowupRunning')}</div>
              </div>
            )}

            {askError && <div className="cr-error">{askError}</div>}

            {selected.result_md ? (
              <div className="cr-followup-composer">
                <textarea
                  className="form-input cr-desc"
                  rows={3}
                  placeholder={t('projects.detail.reviewFollowupPlaceholder')}
                  value={question}
                  onChange={e => setQuestion(e.target.value)}
                  disabled={askRunning || selected.status === 'running'}
                />
                <button
                  className="btn btn-primary"
                  onClick={handleAsk}
                  disabled={askRunning || !question.trim() || selected.status === 'running'}
                >
                  {askRunning ? t('projects.detail.reviewFollowupAsking') : t('projects.detail.reviewFollowupAsk')}
                </button>
              </div>
            ) : (
              <p className="cr-hint">{t('projects.detail.reviewFollowupNoReport')}</p>
            )}
          </div>
        </article>
      )}

      {/* ── PR comment editor — PR mode only, unchanged behaviour ── */}
      {mode === 'pr' && reviewDone && lastReviewedPRRef.current > 0 && (
        <div className="review-comment-wrap">
          <div className="review-panel-header">
            <span>{t('projects.detail.reviewCommentHeader', { n: lastReviewedPRRef.current })}</span>
          </div>
          <div className="review-model-line">
            <IconRobot size={13} /> {t('projects.detail.reviewModelLine', { model: reviewModel || t('projects.detail.reviewDefaultModel') })}
          </div>
          <textarea
            className="review-comment-editor"
            value={commentBody}
            onChange={e => setCommentBody(e.target.value)}
            rows={12}
            placeholder={t('projects.detail.reviewEditorPlaceholder')}
          />
          <div className="review-comment-actions">
            {submitMsg && (
              <span className={submitMsg.startsWith('✅') ? 'submit-ok' : 'submit-err'}>{submitMsg}</span>
            )}
            <button
              className="btn btn-primary"
              onClick={handleSubmitComment}
              disabled={submitting || !commentBody}
            >
              {submitting ? t('projects.detail.reviewSubmitBusy') : t('projects.detail.reviewSubmit')}
            </button>
          </div>
        </div>
      )}

      {/* ── History ── */}
      <div className="cr-history">
        <div className="review-panel-header">
          <span>{t('projects.detail.reviewHistoryTitle')}</span>
        </div>
        {history.length === 0 ? (
          <div className="tab-empty"><p>{t('projects.detail.reviewHistoryEmpty')}</p></div>
        ) : (
          <table className="pr-table table-cards">
            <thead>
              <tr>
                <th style={{ width: 90 }}>{t('projects.detail.reviewHistoryColType')}</th>
                <th>{t('projects.detail.reviewHistoryColTarget')}</th>
                <th style={{ width: 90 }}>{t('projects.detail.reviewHistoryColStatus')}</th>
                <th style={{ width: 150 }}>{t('projects.detail.reviewHistoryColFindings')}</th>
                <th style={{ width: 150 }}>{t('projects.detail.reviewHistoryColTime')}</th>
                <th style={{ width: 160 }}>{t('projects.detail.reviewHistoryColActions')}</th>
              </tr>
            </thead>
            <tbody>
              {history.map(rv => (
                <tr key={rv.id} className={selected?.id === rv.id ? 'pr-row-active' : ''}>
                  <td data-label={t('projects.detail.reviewHistoryColType')}>
                    {rv.review_type === 'pr' ? t('projects.detail.reviewModePR') : t('projects.detail.reviewModeBranch')}
                  </td>
                  <td data-label={t('projects.detail.reviewHistoryColTarget')}>
                    {rv.review_type === 'pr' && rv.pr_number > 0 && <code className="pr-branch">#{rv.pr_number}</code>}{' '}
                    <code className="pr-branch">{rv.branch}</code>
                    {rv.base_branch ? <> ← <code className="pr-branch">{rv.base_branch}</code></> : null}
                    {rv.title ? <div className="cr-history-title">{rv.title}</div> : null}
                  </td>
                  <td data-label={t('projects.detail.reviewHistoryColStatus')}>
                    <span className={`cr-status cr-status-${rv.status}`}>{statusLabel(rv.status)}</span>
                  </td>
                  <td data-label={t('projects.detail.reviewHistoryColFindings')}>
                    {/* Zero counts stay grey so a row with a single P0 reads
                        at a glance instead of blending into three chips. */}
                    {SEVERITIES.map(sev => {
                      const n = sev === 'P0' ? rv.p0_count : sev === 'P1' ? rv.p1_count : rv.p3_count;
                      return (
                        <span key={sev} className={`cr-count cr-count-${sev}${n === 0 ? ' is-zero' : ''}`}>
                          {sev} {n}
                        </span>
                      );
                    })}
                  </td>
                  <td data-label={t('projects.detail.reviewHistoryColTime')}>{fmtDateTime(rv.created_at)}</td>
                  <td data-label={t('projects.detail.reviewHistoryColActions')}>
                    <button className="btn btn-sm" onClick={() => handleOpenHistory(rv)}>
                      {t('projects.detail.reviewReportTitle')}
                    </button>
                    <button className="btn btn-sm" onClick={() => handleRerun(rv)} disabled={running || rv.status === 'running'}>
                      {t('projects.detail.reviewRerunBtn')}
                    </button>
                    <button className="btn btn-sm" onClick={() => handleDelete(rv)} disabled={rv.status === 'running'}>
                      {t('projects.detail.reviewDeleteBtn')}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
