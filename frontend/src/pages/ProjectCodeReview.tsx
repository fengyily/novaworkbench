// ProjectCodeReview — the project detail page's "代码 Review" tab.
//
// This file is the container: it owns the state, the SSE plumbing and the
// handlers, and composes four panels that own no server state of their own
// (pages/codeReview/): ReviewComposer / ReviewReport / ReviewChat /
// ReviewHistory. It used to be one 1200-line component; splitting it is what
// made the result view's two-column layout and the sticky action bar
// tractable.
//
// Two modes share one output panel and one history list:
//
//   PR 模式     — list open PRs from the platform, review one, then post the
//                 report back as a comment.
//   分支 模式   — review any branch (including one that was never pushed),
//                 with a 改动目标 description and attachments as the context
//                 that lets the reviewer answer 目标是否达成.
//
// Both run on the backend's single JobStore+SSE channel
// (reviewApi.streamJobUrl) — branch review, 追问 and regeneration all share
// it rather than opening streams of their own.
//
// The draft row: a branch review is a persisted session (`crv_*`) and the
// row IS the attachment owner, so it has to exist before anything can be
// uploaded. That used to be a manual 「创建草稿」 button — an implementation
// detail the user had to perform. It is now created automatically (debounced)
// once a branch is picked; abandoned ones are hidden from the history list
// and can be purged in one click (see shared.isBlankDraft).
//
// The report → conversation → report loop:
//
//   report v1 → 追问 … → 基于对话重新生成 → report v2 (v1 still readable)
//
// Regeneration goes through the same job channel and the same grading
// contract, and the backend freezes the version it replaces first, so the
// version switcher in the report toolbar can always go back.

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  reviewApi, codeReviewsApi, agentServersApi,
  type PR, type PRListResponse, type Project,
  type CodeReview, type CodeReviewRunBinding, type CodeReviewRevision, type ReviewFinding,
  type CodeReviewFollowup,
  API_BASE, authedFetch,
} from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import type { ExecEnvServer } from '../components/ExecEnvSelect';
import { IconRobot } from '../components/icons';
import ReviewComposer, { type Mode } from './codeReview/ReviewComposer';
import ReviewReport from './codeReview/ReviewReport';
import ReviewChat from './codeReview/ReviewChat';
import ReviewHistory from './codeReview/ReviewHistory';
import type { StreamLine } from './codeReview/shared';
import { errorMessage } from '../utils/errMsg';
import { exportDesignPdf } from '../utils/exportDesignPdf';
import {
  buildReviewMarkdown, downloadReviewMarkdown, reviewExportTitle, reviewTargetLabel,
} from '../utils/reviewExport';
import '../styles/markdown-prose.css';
import './ProjectCodeReview.css';

/** How long to wait after the last branch keystroke before creating a draft. */
const DRAFT_DEBOUNCE_MS = 700;

interface Props {
  projectId: string;
  project: Project | null;
}

export default function ProjectCodeReview({ projectId, project }: Props) {
  const { t } = useTranslation();
  const [mode, setMode] = useState<Mode>('pr');

  // ── PR mode ──────────────────────────────────────────────────────────────
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
  const [reviewLines, setReviewLines] = useState<StreamLine[]>([]);
  const [reviewDone, setReviewDone] = useState(false);
  // Effective model the reviewer role ran with for the most recent review
  // (from the job_done SSE frame). Empty until a review completes.
  const [reviewModel, setReviewModel] = useState('');
  const [running, setRunning] = useState(false);
  const [regenerating, setRegenerating] = useState(false);
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
  const [revisions, setRevisions] = useState<CodeReviewRevision[]>([]);
  const [detailLoading, setDetailLoading] = useState(false);
  const [exportingPdf, setExportingPdf] = useState(false);

  // ── 追问 ─────────────────────────────────────────────────────────────────
  // The thread belongs to the report currently in the result view, and runs
  // on its own stream so asking a question never clobbers the review log
  // still on screen above it.
  const [followups, setFollowups] = useState<CodeReviewFollowup[]>([]);
  const [question, setQuestion] = useState('');
  const [askRunning, setAskRunning] = useState(false);
  const [askLines, setAskLines] = useState<StreamLine[]>([]);
  const [askError, setAskError] = useState('');
  const askEsRef = useRef<EventStream | null>(null);

  // Close the SSE streams when the tab unmounts so a navigated-away review
  // doesn't keep a dangling reader around.
  useEffect(() => () => { reviewEsRef.current?.close(); askEsRef.current?.close(); }, []);

  // Keep the live stream pinned to the newest line.
  useEffect(() => {
    if (reviewPanelRef.current) reviewPanelRef.current.scrollTop = reviewPanelRef.current.scrollHeight;
  }, [reviewLines]);

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

  // The 追问 thread and the version list both follow whichever report is in
  // the result view. Keyed on the id (not the object) so re-reading the same
  // session after a run doesn't refetch what it just updated.
  const selectedId = selected?.id ?? '';
  useEffect(() => {
    setAskLines([]);
    setAskError('');
    setQuestion('');
    if (!selectedId) { setFollowups([]); setRevisions([]); return; }
    let cancelled = false;
    codeReviewsApi.listFollowups(selectedId)
      .then(rows => { if (!cancelled) setFollowups(Array.isArray(rows) ? rows : []); })
      .catch(() => { if (!cancelled) setFollowups([]); });
    codeReviewsApi.listRevisions(selectedId)
      .then(rows => { if (!cancelled) setRevisions(Array.isArray(rows) ? rows : []); })
      .catch(() => { if (!cancelled) setRevisions([]); });
    return () => { cancelled = true; };
  }, [selectedId]);

  const reloadRevisions = useCallback((reviewId: string) => {
    codeReviewsApi.listRevisions(reviewId)
      .then(rows => setRevisions(Array.isArray(rows) ? rows : []))
      .catch(() => {/* the switcher just stays on the current version */});
  }, []);

  // Only ready servers are offered — the backend refuses the rest, so listing
  // them would only mislead. A failure leaves the list empty, which still
  // renders the 本地 option.
  useEffect(() => {
    agentServersApi.list()
      .then(rows => setAgentServers((rows ?? []).filter(srv => srv.status === 'ready')))
      .catch(() => {/* 设置 → Agent 服务器 is the source of truth */});
  }, []);

  // Branch candidates for the two selectors. Best-effort: on failure the
  // list stays empty and the user types the branch name by hand.
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
          setRegenerating(false);
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
        setRegenerating(false);
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

  const handleReviewPR = useCallback(async (pr: PR) => {
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

  // Description / title / branches are saved on blur rather than per
  // keystroke — the row is only read when the run starts.
  //
  // `override` exists for the toolbar pickers: they fire from an onChange, so
  // the state they just set isn't readable here yet. Passing the new value
  // explicitly lets a model / exec-env change persist immediately instead of
  // waiting for the next blur.
  const saveDraftField = useCallback(async (override?: CodeReviewRunBinding) => {
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
    } catch { /* non-fatal: surfaced on start instead */ }
  }, [session, branch, baseBranch, draftTitle, description, runBinding]);

  // ensureDraft returns the review row for the current composer, creating it
  // on first need and never creating a second one.
  //
  // The row exists only so attachments have an owner id, which is not the
  // user's problem — making them press 「创建草稿」 first was exposing our
  // schema as a workflow step. Two callers race for it (the debounce timer
  // below and the 开始 button), so an in-flight create is cached in a ref and
  // handed to whoever asks next; without that, clicking 开始 inside the
  // debounce window would leave a stray blank draft behind.
  //
  // Form values are read through formRef rather than captured, which keeps
  // this callback stable — a callback that changed on every keystroke in the
  // description box would restart the debounce timer forever.
  const sessionRef = useRef<CodeReview | null>(null);
  const draftPromiseRef = useRef<Promise<CodeReview> | null>(null);
  const formRef = useRef({ branch, baseBranch, draftTitle, description, binding: runBinding() });
  formRef.current = { branch, baseBranch, draftTitle, description, binding: runBinding() };
  useEffect(() => { sessionRef.current = session; }, [session]);

  const ensureDraft = useCallback(async (): Promise<CodeReview | null> => {
    if (sessionRef.current) return sessionRef.current;
    if (draftPromiseRef.current) return draftPromiseRef.current;
    const form = formRef.current;
    if (!form.branch.trim()) return null;
    setCreating(true);
    const pending = codeReviewsApi.create(projectId, {
      branch: form.branch.trim(),
      base_branch: form.baseBranch.trim(),
      title: form.draftTitle.trim(),
      description: form.description,
      ...form.binding,
    })
      .then(rv => {
        // Written straight to the ref as well as to state: a caller awaiting
        // this promise must not be able to observe "no session" because the
        // re-render hasn't happened yet.
        sessionRef.current = rv;
        setSession(rv);
        setBaseBranch(prev => prev || rv.base_branch);
        setBranchError('');
        loadHistory();
        return rv;
      })
      .finally(() => {
        draftPromiseRef.current = null;
        setCreating(false);
      });
    draftPromiseRef.current = pending;
    return pending;
  }, [projectId, loadHistory]);

  // Debounced because `branch` is a free-text input with a datalist: typing
  // "feat/pagination" by hand would otherwise create a row per keystroke.
  useEffect(() => {
    if (mode !== 'branch' || session || running || !branch.trim()) return;
    const timer = window.setTimeout(() => {
      void ensureDraft().catch((err: unknown) => setBranchError(errorMessage(err)));
    }, DRAFT_DEBOUNCE_MS);
    return () => window.clearTimeout(timer);
  }, [mode, branch, session, running, ensureDraft]);

  const handleStartBranchReview = async () => {
    if (running) return;
    if (!branch.trim()) {
      setBranchError(t('projects.detail.reviewNeedBranch'));
      return;
    }
    setRunning(true);
    setReviewLines([]);
    setReviewDone(false);
    setReviewModel('');
    setSelected(null);
    setBranchError('');
    try {
      const existing = session;
      // Joins the debounced create when one is already in flight, so hitting
      // 开始 immediately after typing a branch starts THAT row.
      const rv = existing ?? await ensureDraft();
      if (!rv) {
        setBranchError(t('projects.detail.reviewNeedBranch'));
        setRunning(false);
        return;
      }
      // A row ensureDraft just created already carries the current form
      // values; an existing one may be holding un-blurred edits.
      if (existing) await saveDraftField();
      const reviewId = rv.id;
      const { job_id } = await codeReviewsApi.start(reviewId);
      subscribeJob(job_id, () => {
        loadHistory();
        // Re-read the session so the graded findings (written by the stream
        // tail) land in the result view.
        codeReviewsApi.get(reviewId).then(full => {
          setSession(full);
          setSelected(full);
          reloadRevisions(reviewId);
        }).catch(() => {});
      });
    } catch (err: unknown) {
      setReviewLines([{ type: 'error', content: errorMessage(err) }]);
      setRunning(false);
    }
  };

  const handleNewSession = () => {
    setSession(null);
    setBranch('');
    setDescription('');
    setDraftTitle('');
    setDescSavedAt(0);
    setRunModel('');
    setRunConfigId('');
    setAgentServerId('');
    setBranchError('');
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

  // Abandoned auto-drafts. Deleted without a per-row confirm because by
  // definition they hold nothing: no description, no report, no attachments
  // the user ever got to upload.
  const handlePurgeBlankDrafts = async (drafts: CodeReview[]) => {
    if (!window.confirm(t('projects.detail.reviewBlankDraftsConfirm', { n: drafts.length }))) return;
    const keepId = session?.id;
    await Promise.allSettled(
      drafts.filter(d => d.id !== keepId).map(d => codeReviewsApi.remove(d.id)),
    );
    loadHistory();
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
      return full;
    } catch {
      setSelected(rv);
      return rv;
    } finally { setDetailLoading(false); }
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
        reloadRevisions(rv.id);
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

  // Pre-fill the question box with a reference the model can resolve
  // unambiguously ("P0-2 …"), plus the finding's own title so the question
  // still reads as a sentence in the thread.
  const handleAskAbout = (finding: ReviewFinding, ref: string) => {
    const loc = finding.file ? `（${finding.file}${finding.line ? `:${finding.line}` : ''}）` : '';
    setQuestion(t('projects.detail.reviewAskAboutTemplate', {
      ref, title: finding.title, loc,
    }));
    document.querySelector('.cr-chat .cr-desc')?.scrollIntoView({ behavior: 'smooth', block: 'center' });
  };

  // ── Regeneration + versions ──────────────────────────────────────────────

  // Answered turns the current report hasn't taken in yet. The newest
  // revision records how many were folded in when it was produced, so the
  // difference is exactly "what would change if you regenerated now".
  const answeredFollowups = useMemo(
    () => followups.filter(fu => fu.status === 'done').length, [followups]);
  const foldedFollowups = useMemo(() => {
    const current = revisions.find(r => r.rev_no === selected?.revision_no);
    return current?.followup_count ?? 0;
  }, [revisions, selected?.revision_no]);
  const pendingFollowups = Math.max(0, answeredFollowups - foldedFollowups);

  const handleRegenerate = async () => {
    if (!selected || running || regenerating) return;
    const reviewId = selected.id;
    setRegenerating(true);
    setRunning(true);
    setReviewLines([]);
    setReviewDone(false);
    setBranchError('');
    try {
      const { job_id } = await codeReviewsApi.regenerate(reviewId);
      subscribeJob(job_id, () => {
        loadHistory();
        codeReviewsApi.get(reviewId).then(full => {
          setSelected(full);
          if (session?.id === reviewId) setSession(full);
        }).catch(() => {});
        reloadRevisions(reviewId);
      });
    } catch (err: unknown) {
      setReviewLines([{ type: 'error', content: errorMessage(err) }]);
      setRegenerating(false);
      setRunning(false);
    }
  };

  // Switching versions rewrites the review row, so the whole result view
  // follows — including the 追问 thread's "pending" count, which is relative
  // to the version on screen.
  const handleSelectRevision = async (revNo: number) => {
    if (!selected) return;
    try {
      const updated = await codeReviewsApi.restoreRevision(selected.id, revNo);
      setSelected(updated);
      if (session?.id === updated.id) setSession(updated);
      loadHistory();
    } catch (err: unknown) {
      setAskError(errorMessage(err));
    }
  };

  // ── Export ───────────────────────────────────────────────────────────────

  const handleExportMarkdown = () => {
    if (!selected) return;
    downloadReviewMarkdown({ review: selected, followups, projectName: project?.name });
  };

  const handleExportPdf = async () => {
    if (!selected) return;
    setExportingPdf(true);
    try {
      await exportDesignPdf({
        title: reviewExportTitle(selected),
        meta: `${project?.name ?? ''}${project?.name ? ' · ' : ''}${reviewTargetLabel(selected)}`,
        markdown: buildReviewMarkdown({ review: selected, followups, projectName: project?.name }),
        filename: reviewExportTitle(selected),
        pdfLabel: t('projects.detail.reviewExportPdfLabel'),
      });
    } catch (err: unknown) {
      setAskError(t('projects.detail.reviewExportFailed') + errorMessage(err));
    } finally {
      setExportingPdf(false);
    }
  };

  // ── Render ───────────────────────────────────────────────────────────────

  return (
    <div className="tab-content project-code-review">
      <ReviewComposer
        mode={mode}
        onModeChange={setMode}
        running={running}
        bindings={{
          model: runModel,
          configId: runConfigId,
          agentServerId,
          servers: agentServers,
          onModelChange: (m) => { setRunModel(m); if (session) void saveDraftField({ model: m }); },
          onConfigChange: (c) => { setRunConfigId(c); if (session) void saveDraftField({ claude_config_id: c }); },
          onAgentServerChange: (v) => { setAgentServerId(v); if (session) void saveDraftField({ agent_server_id: v }); },
        }}
        prData={prData}
        prsLoading={prsLoading}
        prsError={prsError}
        reviewingPR={reviewingPR}
        extraRequirements={extraRequirements}
        onExtraRequirementsChange={setExtraRequirements}
        onReviewPR={handleReviewPR}
        branchForm={{
          branches,
          branch,
          baseBranch,
          title: draftTitle,
          description,
          onBranchChange: setBranch,
          onBaseBranchChange: setBaseBranch,
          onTitleChange: setDraftTitle,
          onDescriptionChange: setDescription,
          onFieldBlur: () => { if (session) void saveDraftField(); },
          session,
          creating,
          savedAt: descSavedAt,
          error: branchError,
          onStart: handleStartBranchReview,
          onReset: handleNewSession,
          onDelete: () => { if (session) void handleDelete(session); },
        }}
      />

      {/* ── Live stream (shared by review / 追问 / regeneration) ── */}
      {(reviewLines.length > 0 || running) && (
        <div className="review-panel-wrap cr-stream">
          <div className="review-panel-header">
            <span className="cr-stream-title">
              {running && !reviewDone && <span className="cr-pulse" aria-hidden="true" />}
              {regenerating ? t('projects.detail.reviewRegenerateHeader') : t('projects.detail.reviewHeader')}
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

      {/* ── Result ── Report and conversation side by side on a wide screen:
          the questions worth asking come from the report you are looking at,
          and scrolling between the two was the main friction in the old
          single-column stack. Narrow screens fall back to one column. */}
      {detailLoading && <div className="tab-empty">{t('projects.detail.reviewLoadingDetail')}</div>}
      {selected && !detailLoading && (
        <div className="cr-result-grid">
          <ReviewReport
            review={selected}
            revisions={revisions}
            busy={running || regenerating}
            pendingFollowups={pendingFollowups}
            canRegenerate={!!selected.result_md && selected.status !== 'running'}
            onExportMarkdown={handleExportMarkdown}
            onExportPdf={handleExportPdf}
            exportingPdf={exportingPdf}
            onRegenerate={handleRegenerate}
            onRerun={() => void handleRerun(selected)}
            onSelectRevision={(n) => void handleSelectRevision(n)}
            onAskAbout={handleAskAbout}
          />
          <aside className="cr-result-aside">
            <ReviewChat
              review={selected}
              followups={followups}
              question={question}
              onQuestionChange={setQuestion}
              asking={askRunning}
              askLines={askLines}
              askError={askError}
              onAsk={handleAsk}
              onDeleteFollowup={handleDeleteFollowup}
              pendingFollowups={pendingFollowups}
              canRegenerate={!!selected.result_md && selected.status !== 'running'}
              regenerating={regenerating}
              onRegenerate={handleRegenerate}
            />
          </aside>
        </div>
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

      <ReviewHistory
        history={history}
        selectedId={selectedId}
        running={running}
        onOpen={(rv) => void handleOpenHistory(rv)}
        onRerun={(rv) => void handleRerun(rv)}
        onDelete={(rv) => void handleDelete(rv)}
        onPurgeBlankDrafts={(drafts) => void handlePurgeBlankDrafts(drafts)}
      />
    </div>
  );
}
