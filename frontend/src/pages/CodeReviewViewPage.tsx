// CodeReviewViewPage — `/reviews/:id` 独立阅读视图。
//
// 与 KnowledgeViewPage 同模式（RequireAuth 之内、Layout 之外）。区别在于
// 这页的 "数据" 比一条知识条目大得多：单条 Code Review 自带 followup 线程、
// 版本列表、可能仍在跑的 SSE job。所以这里不复制 KnowledgeViewPage 的
// `useState<LoadState>` 写法，而是直接复用 useReviewSession hook ——
// ProjectCodeReview 容器在 tab 里也是同一个 hook 的客户端，两处行为自然
// 对齐（追问 SSE 追加、版本回切、export 等等）。
//
// Loading 顺序：URL 上的 id 立即交给 hook，hook 自己拉 review / followups /
// revisions，并用 review.project_id 拼 SSE 地址；review 若仍在 running 会
// 自动接入同一条 job 流。`?run=1` 表示本窗口由「开始 Review」打开，start 可能
// 尚未落库，hook 会在 draft 状态下轮询等待。
//
// 关闭按钮沿用 KnowledgeViewPage 的智能回退（line 76-84）：新窗口里点
// 关闭能 window.close()，直接粘贴 URL 进入时退到 /projects，而不是 /knowledge
// —— review 是项目级资源。

import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate, useParams, useSearchParams } from 'react-router-dom';
import {
  codeReviewsApi, projectsApi,
  type CodeReviewFollowup, type ReviewFinding,
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

export default function CodeReviewViewPage() {
  const { id } = useParams<{ id: string }>();
  const [searchParams] = useSearchParams();
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [projectName, setProjectName] = useState<string>('');
  const [projectNameError, setProjectNameError] = useState<string>('');

  const session = useReviewSession(id ?? null, undefined, {
    waitForStart: searchParams.get('run') === '1',
  });
  const review = session.review;
  const projectId = review?.project_id ?? '';
  const loadingReview = !!id && session.detailLoading && !review;
  const notFound = !id || session.notFound;
  const loadError = !review && !session.detailLoading ? session.error : '';

  // Project display name for the topbar and exported PDF/MD header. Failure
  // is non-fatal — the topbar falls back to the project id.
  useEffect(() => {
    setProjectName('');
    setProjectNameError('');
    if (!projectId) return;
    let cancelled = false;
    projectsApi.get(projectId)
      .then((p) => { if (!cancelled) setProjectName(p?.name || ''); })
      .catch((err: unknown) => { if (!cancelled) setProjectNameError(errorMessage(err)); });
    return () => { cancelled = true; };
  }, [projectId]);

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
  // The hook owns the SSE stream, the followup thread and failures
  // (`session.error`); we just hand it the question text.
  const handleAsk = useCallback(async (q: string) => {
    if (!review || session.running) return;
    await session.askFollowup(q);
  }, [review, session]);

  const handleDeleteFollowup = useCallback(async (fu: CodeReviewFollowup) => {
    if (!window.confirm(t('projects.detail.reviewFollowupDeleteConfirm'))) return;
    try {
      await codeReviewsApi.removeFollowup(fu.review_id, fu.id);
      await session.reload();
    } catch (err: unknown) {
      setPageError(errorMessage(err));
    }
  }, [t, session]);

  // Single error surface: hook failures (load / ask / regenerate / rerun /
  // restore) plus page-level ones, shown above the report.
  const displayError = session.error || pageError;

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
      setPageError(t('projects.detail.reviewExportFailed') + errorMessage(err));
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
        {displayError && (
          <div className="cr-error">
            {t('projects.detail.reviewErrPrefix')}{displayError}
          </div>
        )}
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
