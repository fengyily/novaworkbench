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
  reviewApi, codeReviewsApi, agentServersApi, fsApi,
  type PR, type PRListResponse, type Project,
  type CodeReview, type CodeReviewRunBinding, type ReviewFinding,
  type CodeReviewFollowup,
  type FsBranchesResponse,
} from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import { useReviewSession } from '../hooks/useReviewSession';
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
  const [branchesLoading, setBranchesLoading] = useState(false);
  const [branchesError, setBranchesError] = useState('');

  // ── History + result ─────────────────────────────────────────────────────
  const [history, setHistory] = useState<CodeReview[]>([]);
  const [resultReviewId, setResultReviewId] = useState<string | null>(null);
  // Composer is on-demand: the page opens to a history list, and the user
  // presses the header button to summon the form. Toggled by the page-header
  // primary button (and the mobile FAB); the composer's own "collapse" button
  // and the success path of 开始 also flip it back off.
  const [showCreate, setShowCreate] = useState(false);
  const [exportingPdf, setExportingPdf] = useState(false);

  // ── 创建 issue ───────────────────────────────────────────────────────────
  // Per-finding "Create Issue" — keyed by the same ref string the report
  // renders ("P0-1", "P1-2"…) so the in-flight and success state can be
  // looked up from the button without a parent re-render. The map is
  // cleared when the user opens a different review so a stale success chip
  // doesn't leak across runs (createdIssues is intentionally NOT cleared
  // when switching to another review, because the "✓ 已创建 →" link IS the
  // record of what happened).
  const [creatingIssues, setCreatingIssues] = useState<Record<string, boolean>>({});
  const [createdIssues, setCreatedIssues] = useState<Record<string, string>>({});
  const [bulkCreating, setBulkCreating] = useState(false);

  // ── 追问 ─────────────────────────────────────────────────────────────────
  // The thread belongs to the report currently in the result view. The
  // chat's textarea state lives inside ReviewChat; the SSE channel, the
  // live stream, the current job id, the followup list and the version
  // list all live in the `reviewSession` hook. The page only holds the
  // one-shot pre-fill slot.
  const [askTemplate, setAskTemplate] = useState<string>('');

  // Hook owns: detail row, followup thread, version list, active
  // revision, current SSE job id, merged live stream, running flag, and
  // all four actions (ask / regenerate / rerun / restoreRevision). The
  // page just plumbs its UI bits to those handlers. `resultReviewId` is
  // the only thing the page needs to drive the result view; the hook
  // reloads detail / followups / revisions whenever it changes.
  const reviewSession = useReviewSession(resultReviewId, projectId);

  // Read-through aliases for the rest of the file. The hook owns the
  // canonical state; the page used to hold its own copies and the
  // downstream JSX reads `selected` / `revisions` / `detailLoading` by
  // those names. Keeping the names lets the rest of the file stay close
  // to its old shape so the diff stays localised.
  const selected = reviewSession.review;
  const revisions = reviewSession.revisions;
  const detailLoading = reviewSession.detailLoading;
  const followups = reviewSession.followups;

  // Close the PR-mode SSE when the tab unmounts so a navigated-away
  // review doesn't keep a dangling reader around. The branch-mode SSE
  // lives inside `reviewSession` and is closed on its own unmount.
  useEffect(() => () => { reviewEsRef.current?.close(); }, []);

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

  // The 追问 thread and the version list now follow whichever report is
  // in the result view via `reviewSession`; the hook reloads them
  // whenever `resultReviewId` changes and again after every run. This
  // page only keeps `askTemplate` (the one-shot pre-fill slot the
  // report's 「针对 P0-2 提问」 button pushes into the chat).
  // The local `selected` reference is still used to derive
  // `resultReviewId` above and to gate UI on having a review loaded.

  // Only ready servers are offered — the backend refuses the rest, so listing
  // them would only mislead. A failure leaves the list empty, which still
  // renders the 本地 option.
  useEffect(() => {
    agentServersApi.list()
      .then(rows => setAgentServers((rows ?? []).filter(srv => srv.status === 'ready')))
      .catch(() => {/* 设置 → Agent 服务器 is the source of truth */});
  }, []);

  // Branch candidates for the two selectors. Triggered on entering branch
  // mode; refreshBranches(forceRefresh=true) drives the manual refresh button.
  // Errors are surfaced to the UI (不再静默吞错) so users can tell a fetch
  // failure from a genuinely-empty list.
  const refreshBranches = async (forceRefresh = false) => {
    const localPath = project?.local_path;
    if (mode !== 'branch' || !localPath) return;
    setBranchesLoading(true);
    setBranchesError('');
    try {
      const resp: FsBranchesResponse = await fsApi.gitBranches({ path: localPath, refresh: forceRefresh });
      setBranches(resp.branches ?? []);
      if (resp.fetch_warning) {
        setReviewLines(prev => [...prev, { type: 'message', content: '⚠️ ' + t('projects.detail.reviewFetchWarning', { stderr: resp.fetch_warning }) }]);
      }
    } catch (err) {
      setBranches([]);
      setBranchesError(errorMessage(err));
    } finally {
      setBranchesLoading(false);
    }
  };
  useEffect(() => {
    void refreshBranches(false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
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
    setResultReviewId(null);
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
    setResultReviewId(null);

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

  const ensureDraft = useCallback(async (mode: 'immediate' | 'debounced' = 'debounced'): Promise<CodeReview | null> => {
    if (sessionRef.current) return sessionRef.current;
    if (draftPromiseRef.current) return draftPromiseRef.current;
    const form = formRef.current;
    if (!form.branch.trim()) return null;
    // The `mode` argument distinguishes the composer's on-mount call (no
    // 700ms wait) from the branch-typing debounce. Today both paths share
    // the same create() — the empty-branch guard above is the one
    // invariant that matters in either case — so the value is currently
    // documentation. Keep the param so call sites make the intent obvious.
    void mode;
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
        setBranchesError('');
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
  // `mode='immediate'` is reserved for the composer's on-mount useEffect,
  // which calls ensureDraft() without sitting behind a 700ms wait. The
  // branch-typing effect always uses the debounced path.
  // The `showCreate` gate is what stops an auto-draft from being created
  // while the page is showing only the history list: the composer itself
  // is not even mounted, so there is no form to attach the row to.
  useEffect(() => {
    if (mode !== 'branch' || !showCreate || session || running || !branch.trim()) return;
    const timer = window.setTimeout(() => {
      void ensureDraft('debounced').catch((err: unknown) => setBranchesError(errorMessage(err)));
    }, DRAFT_DEBOUNCE_MS);
    return () => window.clearTimeout(timer);
  }, [mode, showCreate, branch, session, running, ensureDraft]);

  const handleStartBranchReview = async () => {
    if (running) return;
    if (!branch.trim()) {
      setBranchesError(t('projects.detail.reviewNeedBranch'));
      return;
    }
    setBranchesError('');
    try {
      const existing = session;
      // Joins the debounced create when one is already in flight, so hitting
      // 开始 immediately after typing a branch starts THAT row.
      const rv = existing ?? await ensureDraft();
      if (!rv) {
        setBranchesError(t('projects.detail.reviewNeedBranch'));
        return;
      }
      // A row ensureDraft just created already carries the current form
      // values; an existing one may be holding un-blurred edits.
      if (existing) await saveDraftField();
      const reviewId = rv.id;
      // Make sure the result view picks up this review before the hook
      // starts the SSE. For a brand-new row this flips the id from
      // null; for an existing one it is a no-op.
      setResultReviewId(reviewId);
      // Close the composer and pop the dedicated viewer in a new tab.
      // The window.open call sits in the same click handler chain as
      // `await reviewSession.rerun()` below, so the browser still
      // treats it as user-initiated and does not block the popup.
      setShowCreate(false);
      window.open(`/reviews/${reviewId}?run=1`, '_blank', 'noopener,noreferrer');
      await reviewSession.rerun();
      loadHistory();
    } catch (err: unknown) {
      // The hook owns the branch-mode live stream; surface the error
      // via the same branchesError slot used for "branch required"
      // style validation so the user sees it.
      setBranchesError(errorMessage(err));
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
    setBranchesError('');
    resetPanel();
  };

  const handleDelete = async (rv: CodeReview) => {
    if (!window.confirm(t('projects.detail.reviewDeleteConfirm'))) return;
    try {
      await codeReviewsApi.remove(rv.id);
      if (session?.id === rv.id) handleNewSession();
      if (resultReviewId === rv.id) setResultReviewId(null);
      loadHistory();
    } catch (err: unknown) {
      setBranchesError(errorMessage(err));
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
  // Stable identity is required because handleOpenDraftRow and the
  // history-row click handler both depend on it; without useCallback here
  // those consumers would re-fire every render.
  const handleOpenHistory = useCallback(async (rv: CodeReview) => {
    try {
      const full = await codeReviewsApi.get(rv.id);
      setResultReviewId(full.id);
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
      setResultReviewId(rv.id);
      return rv;
    }
  }, []);

  const handleRerun = async (rv: CodeReview) => {
    // The hook is keyed on `resultReviewId` — make sure it points at
    // the row we want to rerun before the hook starts the SSE. (For
    // history-row rerun the row is already the current selection, so
    // this is a no-op in the common path.)
    setResultReviewId(rv.id);
    try {
      await reviewSession.rerun();
      loadHistory();
    } catch (err: unknown) {
      setReviewLines(prev => [...prev, { type: 'error', content: errorMessage(err) }]);
      setRunning(false);
    }
  };

  // History row click handlers — split by status (ReviewHistory dispatches
  // based on the row's status field, but the actual side effects live here).
  //
  // Drafts need to come back into the composer with their fields pre-filled
  // (the /reviews/:id reader has nothing to show for a draft). Loading the
  // full row seeds the toolbar's model / exec-env picks so 重跑 reproduces
  // the same setup — same code path as the existing "查看" button.
  const handleOpenDraftRow = useCallback((rv: CodeReview) => {
    setShowCreate(true);
    void handleOpenHistory(rv);
  }, [handleOpenHistory]);

  // Non-draft rows jump to the dedicated reader in a new tab. We do the
  // `window.open` from this click handler chain (not from an async
  // .then()) so Chromium still treats it as a user-initiated popup and
  // doesn't block it.
  const handleOpenViewerRow = useCallback((rv: CodeReview) => {
    window.open(`/reviews/${rv.id}`, '_blank', 'noopener,noreferrer');
  }, []);

  // ── 追问 handlers ────────────────────────────────────────────────────────

  // One question = one job on the shared review SSE channel. The answer is
  // persisted by the backend, so when the job ends we just re-read the thread
  // instead of trying to reconstruct it from the stream. The chat owns the
  // textarea and passes us the trimmed question string here. The hook
  // owns the SSE, the current job id, the live stream and the
  // post-completion followup reload — the page just hands the question
  // over.
  const handleAsk = async (q: string) => {
    if (!selected || reviewSession.running) return;
    try {
      await reviewSession.askFollowup(q);
    } catch (err: unknown) {
      // The hook already appended an error line into liveLines; surface
      // it via the same branchesError slot used for branch-mode
      // validation so the user can see the failure without it being
      // swallowed silently.
      setBranchesError(errorMessage(err));
    }
  };

  const handleDeleteFollowup = async (fu: CodeReviewFollowup) => {
    if (!window.confirm(t('projects.detail.reviewFollowupDeleteConfirm'))) return;
    try {
      await codeReviewsApi.removeFollowup(fu.review_id, fu.id);
      await reviewSession.reload();
    } catch (err: unknown) {
      // Followup thread is re-read on next render, so a stale row clears
      // itself. Log for debugging; the user already saw the row disappear
      // (delete is optimistic) and a toast here would be noise.
      console.warn('deleteFollowup failed', err);
    }
  };

  // Pre-fill the question box with a reference the model can resolve
  // unambiguously ("P0-2 …"), plus the finding's own title so the question
  // still reads as a sentence in the thread. We push the template into
  // `askTemplate`, which ReviewChat consumes via its `externalQuestion`
  // prop and clears the slot for the next click.
  const handleAskAbout = (finding: ReviewFinding, ref: string) => {
    const loc = finding.file ? `（${finding.file}${finding.line ? `:${finding.line}` : ''}）` : '';
    setAskTemplate(t('projects.detail.reviewAskAboutTemplate', {
      ref, title: finding.title, loc,
    }));
    document.querySelector('.cr-chat .cr-desc')?.scrollIntoView({ behavior: 'smooth', block: 'center' });
  };

  // ── 创建 issue handlers ──────────────────────────────────────────────────

  // Per-finding creation: one round-trip per click, the busy state is held
  // by `creatingIssues[ref]` so a second click on the same card is a no-op
  // (button stays disabled). On success the response carries the new
  // requirement id, which is dropped into `createdIssues` — the per-card
  // button swaps itself for a "✓ 已创建 →" link so the user can jump
  // straight to /requirements/:new_id.
  const handleCreateIssue = useCallback(async (ref: string) => {
    if (!selected || creatingIssues[ref]) return;
    setCreatingIssues(prev => ({ ...prev, [ref]: true }));
    try {
      const created = await codeReviewsApi.createIssues(selected.id, { finding_ref: ref });
      const first = Array.isArray(created) && created.length > 0 ? created[0] : null;
      if (first) setCreatedIssues(prev => ({ ...prev, [ref]: first.id }));
    } catch (err: unknown) {
      // The user-visible error already lands in setBranchesError elsewhere;
      // we don't have a review-specific error slot, so the most local
      // honest place is to reuse it. (a separate error surface here —
      // it's for the 追问 stream.)
      setBranchesError(errorMessage(err));
    } finally {
      setCreatingIssues(prev => {
        const { [ref]: _drop, ...rest } = prev;
        return rest;
      });
    }
  }, [selected, creatingIssues]);

  // Batch creation: one round-trip turns every P0 + P1 finding into an
  // issue-kind requirement. The success response is a list — every ref in
  // that list gets its own success chip, so the user sees a per-finding
  // "✓ 已创建 →" link rather than a single bulk confirmation that doesn't
  // tell them which finding got which id. The button stays disabled while
  // ANY per-card creation is in flight so the two paths can't race.
  const handleBulkCreateIssues = useCallback(async () => {
    if (!selected || bulkCreating) return;
    if (Object.values(creatingIssues).some(Boolean)) return;
    setBulkCreating(true);
    try {
      const created = await codeReviewsApi.createIssues(selected.id, {
        batch: true,
        severities: ['P0', 'P1'],
      });
      if (Array.isArray(created)) {
        setCreatedIssues(prev => {
          const next = { ...prev };
          for (const req of created) {
            // source_external_id is "<reviewId>#<ref>"; we built it that way
            // in review_issue.go specifically so the round-trip stays
            // ref-keyed without a server-side join.
            const ext = (req as { source_external_id?: string }).source_external_id ?? '';
            const hash = ext.indexOf('#');
            if (hash < 0) continue;
            const ref = ext.slice(hash + 1);
            if (ref) next[ref] = req.id;
          }
          return next;
        });
      }
    } catch (err: unknown) {
      setBranchesError(errorMessage(err));
    } finally {
      setBulkCreating(false);
    }
  }, [selected, bulkCreating, creatingIssues]);

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
    setRegenerating(true);
    try {
      await reviewSession.regenerate();
      loadHistory();
    } catch (err: unknown) {
      // The hook already appended an error line into liveLines; surface
      // it via the same branchesError slot used for branch-mode
      // validation so the user can see the failure without it being
      // swallowed silently.
      setBranchesError(errorMessage(err));
      setRegenerating(false);
    }
  };

  // Switching versions rewrites the review row, so the whole result view
  // follows — including the 追问 thread's "pending" count, which is relative
  // to the version on screen.
  const handleSelectRevision = async (revNo: number) => {
    if (!selected) return;
    try {
      await reviewSession.restoreRevision(revNo);
      loadHistory();
    } catch (err: unknown) {
      setBranchesError(errorMessage(err));
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
      // PDF export failure: surface in reviewLines so the user sees the
      // reason in the same panel they'd see a review error.
      setReviewLines(prev => [...prev, {
        type: 'error',
        content: t('projects.detail.reviewExportFailed') + errorMessage(err),
      }]);
    } finally {
      setExportingPdf(false);
    }
  };

  // ── Render ───────────────────────────────────────────────────────────────

  return (
    <div className="tab-content project-code-review">
      {/* ── Page header + on-demand composer toggle. Mirrors the
          RequirementsList pattern (page-header / page-header-actions) so
          the two list pages read the same way: title on the left, primary
          action on the right, no form visible until the user asks. */}
      <div className="page-header">
        <h2 className="page-header-title">{t('codeReviews.page.title')}</h2>
        <div className="page-header-actions desktop-only">
          <button
            type="button"
            className="btn btn-primary"
            onClick={() => setShowCreate(v => !v)}
          >
            {showCreate ? t('codeReviews.create.toggleClose') : t('codeReviews.create.toggleOpen')}
          </button>
        </div>
      </div>

      {showCreate && (
        <ReviewComposer
          mode={mode}
          onModeChange={setMode}
          running={running}
          defaultBranch={project?.default_branch || 'main'}
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
            defaultBranch: project?.default_branch || 'main',
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
            error: branchesError,
            onStart: handleStartBranchReview,
            onReset: handleNewSession,
            onDelete: () => { if (session) void handleDelete(session); },
            onRefreshBranches: () => void refreshBranches(true),
            refreshing: branchesLoading,
          }}
          onClose={() => setShowCreate(false)}
          onEnsureDraft={() => ensureDraft('immediate')}
        />
      )}

      {/* Mobile-only floating action button — mirrors the RequirementsList
          page: the desktop button is in `.page-header-actions.desktop-only`,
          the FAB is the same toggle on phones. Hidden on desktop via the
          `.fab` rule in index.css (display:none outside the 768px media
          query). */}
      <button
        type="button"
        className="fab fab-extended"
        onClick={() => setShowCreate(v => !v)}
        aria-label={showCreate ? t('codeReviews.create.toggleClose') : t('codeReviews.create.toggleOpen')}
      >
        {showCreate
          ? `✕ ${t('codeReviews.create.toggleClose')}`
          : `+ ${t('codeReviews.create.toggleOpen')}`}
      </button>

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
            onExportMd={handleExportMarkdown}
            onExportPdf={handleExportPdf}
            exportingPdf={exportingPdf}
            onRegenerate={handleRegenerate}
            onRerun={() => void handleRerun(selected)}
            activeRevisionNo={selected.revision_no}
            onSetActiveRevision={() => { /* hook owns the live active pick */ }}
            onRestoreRevision={(n: number) => void handleSelectRevision(n)}
            onAskAbout={handleAskAbout}
            onCreateIssue={handleCreateIssue}
            creatingIssues={creatingIssues}
            createdIssues={createdIssues}
            onBulkCreateIssues={() => void handleBulkCreateIssues()}
            bulkCreating={bulkCreating}
          />
          <aside className="cr-result-aside">
            <ReviewChat
              followups={reviewSession.followups}
              liveLines={reviewSession.liveLines}
              onAsk={handleAsk}
              onDeleteFollowup={handleDeleteFollowup}
              currentJobId={reviewSession.currentJobId}
              externalQuestion={askTemplate}
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
        running={running}
        onOpenDraft={handleOpenDraftRow}
        onOpenViewer={handleOpenViewerRow}
        onRerun={(rv) => void handleRerun(rv)}
        onDelete={(rv) => void handleDelete(rv)}
        onPurgeBlankDrafts={(drafts) => void handlePurgeBlankDrafts(drafts)}
      />
    </div>
  );
}
