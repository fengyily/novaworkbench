// CodeReviewViewPage — `/reviews/:id` 独立阅读视图。
//
// 与 KnowledgeViewPage 同模式（RequireAuth 之内、Layout 之外）。区别在于
// 这页的 "数据" 比一条知识条目大得多：单条 Code Review 自带 followup 线程、
// 版本列表、可能仍在跑的 SSE job。所以这里不复制 KnowledgeViewPage 的
// `useState<LoadState>` 写法，而是直接复用 useReviewSession hook ——
// ProjectCodeReview 容器在 tab 里也是同一个 hook 的客户端，两处行为自然
// 对齐（追问 SSE 追加、版本回切、export 等等）。
//
// Loading 顺序：URL 上的 id 立即拿到 → hook 用一个空 projectId 启动
//（hook 拿不到真实 projectId 就只能拿到空字符串，让 SSE 暂不订阅；不影响
// detail / followups / revisions 拉取）→ review 行一旦到位，projectId
// 重新喂入 hook，真正的 SSE 通道才挂上。这一段用一个 effect 触发
// `setKnownProjectId(review.project_id)`，与 hook 自身的"reviewId 变化时
// 重拉 detail" 行为叠加，确保切换 review 不会拖着一份陈旧的 liveLines
// 串到下一份。
//
// 关闭按钮沿用 KnowledgeViewPage 的智能回退（line 76-84）：新窗口里点
// 关闭能 window.close()，直接粘贴 URL 进入时退到 /projects，而不是 /knowledge
// —— review 是项目级资源。

import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate, useParams } from 'react-router-dom';
import {
  codeReviewsApi, projectsApi,
  type CodeReview, type CodeReviewFollowup, type ReviewFinding,
} from '../api/client';
import { errorMessage } from '../utils/errMsg';
import {
  buildReviewMarkdown, downloadReviewMarkdown, reviewExportTitle,
} from '../utils/reviewExport';
import { exportDesignPdf } from '../utils/exportDesignPdf';
import { useReviewSession } from '../hooks/useReviewSession';
import ReviewReport from './codeReview/ReviewReport';
import ReviewChat from './codeReview/ReviewChat';
import './CodeReviewViewPage.css';

// The hook signature wants projectId at construction time. We don't know it
// until the CodeReview row arrives, so we boot it with '' (which the hook
// treats as "no projectId yet, but still load detail" — see
// useReviewSession's reviewId effect, which only depends on reviewId) and
// upgrade the real id into a remount via a `key` on the inner session.
//
// Falling back to a remount (rather than the hook reacting to a changing
// projectId) is the simpler choice: the projectId is only consumed by the
// SSE URL builder, and remounting when the row arrives also clears any
// streaming log that may have leaked from a previous id, which we want
// anyway.
const PLACEHOLDER_PROJECT_ID = '';

export default function CodeReviewViewPage() {
  const { id } = useParams<{ id: string }>();
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [knownProjectId, setKnownProjectId] = useState<string>(PLACEHOLDER_PROJECT_ID);
  const [projectName, setProjectName] = useState<string>('');
  const [projectNameError, setProjectNameError] = useState<string>('');

  // Track the review locally so the hook can boot with PLACEHOLDER_PROJECT_ID
  // and we can re-mount the hook with the real one once the row arrives.
  // The hook itself returns the same `review` we mirror here, but a mirror
  // lets us read project_id without subscribing to the session shape.
  const [review, setReview] = useState<CodeReview | null>(null);
  const [loadingReview, setLoadingReview] = useState<boolean>(true);
  const [loadError, setLoadError] = useState<string>('');
  const [notFound, setNotFound] = useState<boolean>(false);

  // Initial load: pull just the review row. The hook does this in parallel
  // with followups/revisions, but doing it separately here gives us a
  // dedicated error/not-found path before any SSE is wired.
  useEffect(() => {
    if (!id) { setNotFound(true); setLoadingReview(false); return; }
    let cancelled = false;
    setLoadingReview(true);
    setNotFound(false);
    setLoadError('');
    setReview(null);
    setKnownProjectId(PLACEHOLDER_PROJECT_ID);
    setProjectName('');
    setProjectNameError('');
    codeReviewsApi.get(id)
      .then((rv) => {
        if (cancelled) return;
        setReview(rv);
        setKnownProjectId(rv.project_id);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        const status = (err as { status?: number }).status;
        if (status === 404) setNotFound(true);
        else setLoadError(errorMessage(err));
      })
      .finally(() => { if (!cancelled) setLoadingReview(false); });
    return () => { cancelled = true; };
  }, [id]);

  // Once we know the project, look up its display name for the topbar and
  // the exported PDF/MD header. 404 here is non-fatal — we just fall back
  // to the project id (matches the tab's behaviour where `project` may
  // be null while detail is loading).
  useEffect(() => {
    if (!knownProjectId || knownProjectId === PLACEHOLDER_PROJECT_ID) return;
    let cancelled = false;
    projectsApi.get(knownProjectId)
      .then((p) => { if (!cancelled) setProjectName(p?.name || ''); })
      .catch((err: unknown) => { if (!cancelled) setProjectNameError(errorMessage(err)); });
    return () => { cancelled = true; };
  }, [knownProjectId]);

  // The hook is keyed on the known projectId so the SSE re-binds as soon as
  // the row arrives. The placeholder + remount path above is what lets
  // `useReviewSession` get a stable `projectId` string in its argument.
  const session = useReviewSession(
    id ?? null,
    knownProjectId || PLACEHOLDER_PROJECT_ID,
  );

  // The hook also re-fetches the review when reviewId changes, so once
  // knownProjectId triggers a remount and the hook re-runs its detail
  // effect, this setReview can become redundant. We only need to keep it
  // in sync if the hook's `review` advances (e.g. after rerun/regenerate).
  useEffect(() => {
    if (session.review && (!review || session.review.id === review.id)) {
      setReview(session.review);
    }
  }, [session.review, review]);

  // ── Closing — same smart fallback as KnowledgeViewPage.tsx:76-84 ──────────
  const handleClose = useCallback(() => {
    if (window.history.length > 1) {
      navigate(-1);
    } else {
      navigate('/projects');
    }
  }, [navigate]);

  // ── Per-card / per-finding handlers ──────────────────────────────────────
  // Push the Ask-About template into ReviewChat via its externalQuestion
  // prop; the chat picks it up, replaces the textarea content, and clears
  // the slot for the next click.
  const [askTemplate, setAskTemplate] = useState<string>('');
  const handleAskAbout = useCallback((finding: ReviewFinding, ref: string) => {
    const loc = finding.file ? `（${finding.file}${finding.line ? `:${finding.line}` : ''}）` : '';
    setAskTemplate(t('projects.detail.reviewAskAboutTemplate', { ref, title: finding.title, loc }));
    document.querySelector('.crv-main .cr-desc')?.scrollIntoView({ behavior: 'smooth', block: 'center' });
  }, [t]);

  const [creatingIssues, setCreatingIssues] = useState<Record<string, boolean>>({});
  const [createdIssues, setCreatedIssues] = useState<Record<string, string>>({});
  // A single error banner shared by ask / delete-followup / create-issue /
  // export / etc. The hook's `liveLines` is the read-only SSE feed and
  // pushing into it from the outside is a no-op (no re-render, the local
  // pageError is what actually surfaces to the user).
  const [pageError, setPageError] = useState<string>('');
  const handleCreateIssue = useCallback(async (ref: string) => {
    if (!review || creatingIssues[ref]) return;
    setCreatingIssues((prev) => ({ ...prev, [ref]: true }));
    try {
      const created = await codeReviewsApi.createIssues(review.id, { finding_ref: ref });
      const first = Array.isArray(created) && created.length > 0 ? created[0] : null;
      if (first) setCreatedIssues((prev) => ({ ...prev, [ref]: first.id }));
    } catch (err: unknown) {
      setPageError(errorMessage(err));
    } finally {
      setCreatingIssues((prev) => {
        const { [ref]: _drop, ...rest } = prev;
        return rest;
      });
    }
  }, [review, creatingIssues]);

  const [bulkCreating, setBulkCreating] = useState<boolean>(false);
  const handleBulkCreateIssues = useCallback(async () => {
    if (!review || bulkCreating) return;
    if (Object.values(creatingIssues).some(Boolean)) return;
    setBulkCreating(true);
    try {
      const created = await codeReviewsApi.createIssues(review.id, {
        batch: true,
        severities: ['P0', 'P1'],
      });
      if (Array.isArray(created)) {
        setCreatedIssues((prev) => {
          const next = { ...prev };
          for (const req of created) {
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
      setPageError(errorMessage(err));
    } finally {
      setBulkCreating(false);
    }
  }, [review, bulkCreating, creatingIssues]);

  // ── 追问 handlers ────────────────────────────────────────────────────────
  // The hook owns the SSE stream + the followup thread; we just hand it
  // the question text. ReviewChat owns its own textarea, so the only
  // local state is the inline error string surfaced when the POST fails.
  const [askError, setAskError] = useState<string>('');
  const handleAsk = useCallback(async (q: string) => {
    if (!review || session.running) return;
    setAskError('');
    try {
      await session.askFollowup(q);
    } catch (err: unknown) {
      setAskError(errorMessage(err));
    }
  }, [review, session]);

  const handleDeleteFollowup = useCallback(async (fu: CodeReviewFollowup) => {
    if (!window.confirm(t('projects.detail.reviewFollowupDeleteConfirm'))) return;
    try {
      await codeReviewsApi.removeFollowup(fu.review_id, fu.id);
      await session.reload();
    } catch (err: unknown) {
      setAskError(errorMessage(err));
    }
  }, [t, session]);

  // Single error surface — askError plus pageError fold into one banner
  // rendered just above the chat (so a single slot catches every error
  // path on the page, not two stacked ones).
  const displayError = askError || pageError;

  // ── Export ───────────────────────────────────────────────────────────────
  const [exportingPdf, setExportingPdf] = useState<boolean>(false);
  const handleExportMd = useCallback(() => {
    if (!review) return;
    downloadReviewMarkdown({
      review, followups: session.followups, projectName,
    });
  }, [review, session.followups, projectName]);

  const handleExportPdf = useCallback(async () => {
    if (!review) return;
    setExportingPdf(true);
    try {
      await exportDesignPdf({
        title: reviewExportTitle(review),
        meta: `${projectName}${projectName ? ' · ' : ''}${review.branch}${review.base_branch ? ` ← ${review.base_branch}` : ''}`,
        markdown: buildReviewMarkdown({
          review, followups: session.followups, projectName,
        }),
        filename: reviewExportTitle(review),
        pdfLabel: t('projects.detail.reviewExportPdfLabel'),
      });
    } catch (err: unknown) {
      setAskError(t('projects.detail.reviewExportFailed') + errorMessage(err));
    } finally {
      setExportingPdf(false);
    }
  }, [review, session.followups, projectName, t]);

  // ── Computed for the report's pending-followups CTA ──────────────────────
  const answeredFollowups = useMemo(
    () => session.followups.filter((fu) => fu.status === 'done').length,
    [session.followups],
  );
  const foldedFollowups = useMemo(() => {
    const current = session.revisions.find((r) => r.rev_no === review?.revision_no);
    return current?.followup_count ?? 0;
  }, [session.revisions, review?.revision_no]);
  const pendingFollowups = Math.max(0, answeredFollowups - foldedFollowups);
  const canRegenerate = !!review?.result_md && review.status !== 'running';

  // ── Render ───────────────────────────────────────────────────────────────
  if (loadingReview) {
    return (
      <div className="crv-page">
        <header className="crv-topbar">
          <div className="crv-topbar-title">{t('codeReviews.view.loading')}</div>
          <button className="btn" onClick={handleClose}>
            {t('codeReviews.view.close')}
          </button>
        </header>
        <main className="crv-main crv-loading">
          <div className="tab-empty">{t('codeReviews.view.loading')}</div>
        </main>
      </div>
    );
  }

  if (notFound) {
    return (
      <div className="crv-page">
        <header className="crv-topbar">
          <div className="crv-topbar-title">{t('codeReviews.view.notFound')}</div>
          <button className="btn" onClick={handleClose}>
            {t('codeReviews.view.close')}
          </button>
        </header>
        <main className="crv-main crv-loading">
          <div className="tab-empty">
            <p>{t('codeReviews.view.notFound')}</p>
            <button className="btn btn-primary" onClick={handleClose}>
              {t('codeReviews.view.backToProject')}
            </button>
          </div>
        </main>
      </div>
    );
  }

  if (loadError) {
    return (
      <div className="crv-page">
        <header className="crv-topbar">
          <div className="crv-topbar-title">{t('codeReviews.view.notFound')}</div>
          <button className="btn" onClick={handleClose}>
            {t('codeReviews.view.close')}
          </button>
        </header>
        <main className="crv-main crv-loading">
          <div className="tab-empty">
            <p>{t('projects.detail.reviewErrPrefix')}{loadError}</p>
            <button className="btn" onClick={handleClose}>
              {t('codeReviews.view.backToProject')}
            </button>
          </div>
        </main>
      </div>
    );
  }

  return (
    <div className="crv-page">
      <header className="crv-topbar">
        <div className="crv-topbar-title" title={review?.title || ''}>
          {/* Project name + review title. Falls back to the project id
              when the project row isn't loadable (e.g. deleted) so the
              topbar still tells the user what they're looking at. */}
          {projectName || projectNameError || review?.project_id || t('codeReviews.view.projectFallback')}
          {review?.title ? <span className="crv-topbar-sep"> · </span> : null}
          {review?.title}
        </div>
        <button className="btn" onClick={handleClose}>
          {t('codeReviews.view.close')}
        </button>
      </header>
      <main className="crv-main">
        <ReviewReport
          review={session.review}
          revisions={session.revisions}
          activeRevisionNo={session.activeRevisionNo}
          onSetActiveRevision={session.setActiveRevisionNo}
          onRestoreRevision={session.restoreRevision}
          busy={session.running}
          pendingFollowups={pendingFollowups}
          canRegenerate={canRegenerate}
          onExportMd={handleExportMd}
          onExportPdf={handleExportPdf}
          exportingPdf={exportingPdf}
          onRegenerate={session.regenerate}
          onRerun={session.rerun}
          onAskAbout={handleAskAbout}
          onCreateIssue={handleCreateIssue}
          creatingIssues={creatingIssues}
          createdIssues={createdIssues}
          onBulkCreateIssues={handleBulkCreateIssues}
          bulkCreating={bulkCreating}
        />
        {review ? (
          <aside className="crv-chat-wrap">
            {displayError && <div className="cr-error">{displayError}</div>}
            <ReviewChat
              followups={session.followups}
              liveLines={session.liveLines}
              onAsk={handleAsk}
              onDeleteFollowup={handleDeleteFollowup}
              currentJobId={session.currentJobId}
              externalQuestion={askTemplate}
            />
          </aside>
        ) : null}
      </main>
    </div>
  );
}
