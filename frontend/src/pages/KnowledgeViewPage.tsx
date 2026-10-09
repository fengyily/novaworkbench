import { useEffect, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { knowledgeApi, ApiError, type KnowledgeItem } from '../api/client';
import { errorMessage } from '../utils/errMsg';
import { fmtDateTime } from '../utils/intl';
import KnowledgeMarkdown from '../components/KnowledgeMarkdown';
import { IconArrowLeft, IconClose } from '../components/icons';
import './KnowledgeViewPage.css';

// KnowledgeViewPage — `/knowledge/view/:id` 路由使用的独立阅读视图。
// 不裹 Layout（清爽独占视口）；新 tab 打开后 AuthProvider 自动复用同源
// localStorage 里的 session token，过期走 handleUnauthorized 跳 /login。
type LoadState =
  | { kind: 'loading' }
  | { kind: 'ok'; item: KnowledgeItem }
  | { kind: 'error'; message: string; code?: string }
  | { kind: 'notfound' };

export default function KnowledgeViewPage() {
  const { id } = useParams<{ id: string }>();
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [state, setState] = useState<LoadState>({ kind: 'loading' });

  const load = () => {
    if (!id) {
      setState({ kind: 'error', message: 'Missing id' });
      return;
    }
    setState({ kind: 'loading' });
    knowledgeApi
      .get(id)
      .then((item) => setState({ kind: 'ok', item }))
      .catch((err: unknown) => {
        if (err instanceof ApiError && err.code === 'NOT_FOUND') {
          setState({ kind: 'notfound' });
          return;
        }
        setState({ kind: 'error', message: errorMessage(err) });
      });
  };

  useEffect(load, [id]);

  const handleBack = () => {
    // 优先 navigate；用 window.history.length > 1 避免从直接粘贴 URL 进
    // 入时跳到一个"非知识库"的回退页。
    if (window.history.length > 1) {
      navigate(-1);
    } else {
      navigate('/knowledge');
    }
  };

  const handleClose = () => {
    // 在由用户点击 window.open 触发的新 tab 中可关闭；直接打开 URL 时
    // 浏览器可能拒绝（无脚本历史），退化为 navigate('/knowledge')。
    try {
      window.close();
    } catch {
      navigate('/knowledge');
    }
  };

  return (
    <div className="kvp-page">
      {/* Top bar — 返回 + 关闭（不依赖 Layout 顶栏，独立新窗口清爽视口）。 */}
      <header className="kvp-topbar">
        <button
          className="btn btn-sm kvp-topbar-btn"
          onClick={handleBack}
          aria-label={t('knowledge.view.backToList')}
        >
          <IconArrowLeft size={14} />
          <span>{t('knowledge.view.backToList')}</span>
        </button>
        <button
          className="btn btn-sm kvp-topbar-btn"
          onClick={handleClose}
          aria-label={t('knowledge.view.closeTab')}
        >
          <IconClose size={14} />
          <span>{t('knowledge.view.closeTab')}</span>
        </button>
      </header>

      {state.kind === 'loading' && (
        <div className="kvp-state">
          <i>{t('knowledge.view.loading')}</i>
        </div>
      )}

      {state.kind === 'notfound' && (
        <div className="kvp-state">
          <div className="kvp-state-msg">{t('knowledge.view.notFound')}</div>
          <button className="btn btn-primary" onClick={load}>
            {t('knowledge.view.retry')}
          </button>
        </div>
      )}

      {state.kind === 'error' && (
        <div className="kvp-state">
          <div className="kvp-state-msg">
            {t('knowledge.view.loadFailed', { msg: state.message })}
          </div>
          <button className="btn btn-primary" onClick={load}>
            {t('knowledge.view.retry')}
          </button>
        </div>
      )}

      {state.kind === 'ok' && (
        <article className="kvp-article">
          {/* Title area — 类型徽章 + 状态徽章 + 标题。 */}
          <div className="kvp-title-area">
            <div className="kvp-badges">
              <span className={`kb-type-badge type-${state.item.category || 'general'}`}>
                {t(`knowledge.memType.${state.item.category}` as any, {
                  defaultValue: state.item.category || 'general',
                })}
              </span>
              <ReviewBadge item={state.item} />
            </div>
            <h1 className="kvp-title">
              {state.item.title || t('knowledge.view.noTitle')}
            </h1>
          </div>

          {/* Meta strip — 12px muted 字段：来源 / 项目 / 创建 / 更新 / 审核状态。 */}
          <div className="kvp-meta">
            <span>
              {t('knowledge.view.metaSource', { type: state.item.source_type })}
            </span>
            {state.item.project_id && (
              <span>
                {t('knowledge.view.metaProject', { id: state.item.project_id })}
              </span>
            )}
            <span>
              {t('knowledge.view.metaCreated', { date: fmtDateTime(state.item.created_at) })}
            </span>
            {state.item.updated_at && state.item.updated_at !== state.item.created_at && (
              <span>
                {t('knowledge.view.metaUpdated', { date: fmtDateTime(state.item.updated_at) })}
              </span>
            )}
            {!state.item.is_approved && state.item.is_reviewed && (
              <span className="kvp-meta-status">
                {t('knowledge.view.reviewStatus', {
                  status: t('knowledge.view.statusRejected'),
                })}
              </span>
            )}
            {!state.item.is_reviewed && (
              <span className="kvp-meta-status">
                {t('knowledge.view.reviewStatus', {
                  status: t('knowledge.view.statusPending'),
                })}
              </span>
            )}
            {state.item.is_approved && state.item.is_reviewed && (
              <span className="kvp-meta-status">
                {t('knowledge.view.reviewStatus', {
                  status: t('knowledge.view.statusApproved'),
                })}
              </span>
            )}
          </div>

          {/* Body — max-width 760px 居中，由 KnowledgeMarkdown 渲染完整 Markdown。 */}
          <div className="kvp-body">
            {state.item.content ? (
              <KnowledgeMarkdown content={state.item.content} />
            ) : (
              <i className="kvp-empty-body">{t('knowledge.view.noTitle')}</i>
            )}
          </div>
        </article>
      )}
    </div>
  );
}

// ReviewBadge — 审核状态徽章。复用 KnowledgePage.css 的 .kb-unreviewed / .kb-rejected
// 颜色 token 但通过 @import './KnowledgePage.css' 引入（仅复用 .kb-type-badge）。
function ReviewBadge({ item }: { item: KnowledgeItem }) {
  const { t } = useTranslation();
  if (!item.is_reviewed) {
    return <span className="kb-unreviewed">{t('knowledge.unreviewed')}</span>;
  }
  if (!item.is_approved) {
    return <span className="kb-rejected">{t('knowledge.rejected')}</span>;
  }
  return null;
}
