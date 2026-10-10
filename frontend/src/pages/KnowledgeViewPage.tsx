import { useEffect, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { knowledgeApi, ApiError, type KnowledgeItem } from '../api/client';
import { errorMessage } from '../utils/errMsg';
import { fmtDateTime } from '../utils/intl';
import KnowledgeMarkdown from '../components/KnowledgeMarkdown';
import DiagramGenerator from '../components/DiagramGenerator';
import { exportDesignPdf } from '../utils/exportDesignPdf';
import { IconArrowLeft, IconClose, IconFileText, IconHourglass } from '../components/icons';
import './KnowledgeViewPage.css';

// v0.5 之前有过"窄/中/宽"三档切换（localStorage key = knowledge.viewWidth，
// CSS 用 [data-width="..."] 覆写 max-width）。后来用户反馈最大档（1100px）
// 在大屏上仍然两侧留白过多，索性取消切换让正文撑满 .kvp-article，仅保留
// 左右 padding 作边界。WIDTH_STORAGE_KEY 仍保留以便旧的 localStorage entry
// 自然被忽略，不需要清理脚本。

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
  const [showDiagramGenerator, setShowDiagramGenerator] = useState(false);
  // PDF 导出在长文档 + mermaid 渲染上耗时不可忽略，单独加 busy 状态防双击。
  const [exporting, setExporting] = useState(false);
  // 编辑模式：扫描/归档后标题常常不规范 (e.g. 截断的 sentence、空白残留、
  // category 错位)，给用户一个轻量入口直接覆盖。state.editing 持有暂存草稿，
  // 保存成功才把字段合并回 LoadState，避免误触丢稿。
  const [editing, setEditing] = useState<null | {
    title: string;
    content: string;
    category: string;
    saving: boolean;
  }>(null);

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

  const handleExportPdf = async () => {
    if (state.kind !== 'ok') return;
    setExporting(true);
    try {
      // 直接复用 requirements 详情页同款管线（prebakeMermaid + marked +
      // html2pdf），只是把文件名后缀换成「知识条目」让下载文件一眼能区分；
      // meta 行带上项目 ID 和 source_type，便于离线归档时溯源。
      await exportDesignPdf({
        title: state.item.title || t('knowledge.view.noTitle'),
        meta: [
          state.item.project_id ? t('knowledge.view.metaProject', { id: state.item.project_id }) : '',
          t('knowledge.view.metaSource', { type: state.item.source_type }),
        ].filter(Boolean).join(' · '),
        markdown: state.item.content || '',
        filename: state.item.title || `knowledge-${state.item.id}`,
        pdfLabel: t('knowledge.view.pdfLabel'),
      });
    } catch (err: unknown) {
      alert(t('knowledge.view.pdfExportFailPrefix') + errorMessage(err));
    } finally {
      setExporting(false);
    }
  };

  const startEditing = () => {
    if (state.kind !== 'ok') return;
    setEditing({
      title: state.item.title || '',
      content: state.item.content || '',
      category: state.item.category || '',
      saving: false,
    });
  };

  const cancelEditing = () => {
    if (editing?.saving) return;
    setEditing(null);
  };

  const saveEditing = async () => {
    if (state.kind !== 'ok' || !editing) return;
    const title = editing.title.trim();
    const content = editing.content;
    const category = editing.category.trim() || 'general';
    if (!content.trim()) {
      alert(t('knowledge.view.editContentRequired'));
      return;
    }
    setEditing({ ...editing, saving: true });
    try {
      // 后端 Update 当前用 CreateKnowledgeReq 全字段覆盖；前端必须把不可
      // 修改的字段（project_id / source_type / source_ref）原样回传，避免
      // 误把归档来源擦成空串。
      const updated = await knowledgeApi.update(state.item.id, {
        title,
        content,
        category,
        project_id: state.item.project_id,
        source_type: state.item.source_type,
        source_ref: state.item.source_ref,
      });
      // 把后端返回的最新行合并进 LoadState，title / content / category /
      // updated_at 都用服务端版本；用户保持当前阅读视图无打断。
      setState({ kind: 'ok', item: updated });
      setEditing(null);
    } catch (err: unknown) {
      alert(t('knowledge.view.editSaveFailPrefix') + errorMessage(err));
      setEditing({ ...editing, saving: false });
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
        <div className="kvp-topbar-actions">
          {state.kind === 'ok' && !editing && (
            <button
              className="btn btn-sm kvp-topbar-btn"
              onClick={startEditing}
              title={t('knowledge.view.editBtnTitle')}
            >
              <PencilIcon size={14} />
              <span>{t('knowledge.view.editBtn')}</span>
            </button>
          )}
          {state.kind === 'ok' && !editing && (
            <button
              className="btn btn-sm kvp-topbar-btn"
              onClick={handleExportPdf}
              disabled={exporting}
              title={t('knowledge.view.exportPdfTitle')}
            >
              {exporting ? (
                <>
                  <IconHourglass size={14} />
                  <span>{t('knowledge.view.exportingPdf')}</span>
                </>
              ) : (
                <>
                  <IconFileText size={14} />
                  <span>{t('knowledge.view.exportPdfBtn')}</span>
                </>
              )}
            </button>
          )}
          <button
            className="btn btn-sm kvp-topbar-btn"
            onClick={handleClose}
            aria-label={t('knowledge.view.closeTab')}
          >
            <IconClose size={14} />
            <span>{t('knowledge.view.closeTab')}</span>
          </button>
        </div>
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
            {/* Action strip: "以此内容重新生成" 直接复用当前条目的 content
                作为 DiagramGenerator 的初始 input;新行落地后跳到新行 view。
                编辑模式下隐藏，避免"重新生成"和"保存编辑"互相干扰。 */}
            {!editing && (
              <div className="kvp-actions">
                <button
                  className="btn btn-sm btn-primary"
                  onClick={() => setShowDiagramGenerator(true)}
                >
                  🔄 以此重新生成架构图
                </button>
              </div>
            )}
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

          {/* Body — 编辑模式渲染表单，浏览模式走 KnowledgeMarkdown。表单不
              嵌进 .kvp-body 的 max-width 限制里，避免在大屏上 textarea 太窄
              没法用；用 .kvp-edit-form 独立撑满 .kvp-article 宽度。 */}
          {editing ? (
            <EditForm
              draft={editing}
              onChange={(patch) => setEditing({ ...editing, ...patch })}
              onCancel={cancelEditing}
              onSave={saveEditing}
            />
          ) : (
            <div className="kvp-body">
              {state.item.content ? (
                <KnowledgeMarkdown content={state.item.content} />
              ) : (
                <i className="kvp-empty-body">{t('knowledge.view.noTitle')}</i>
              )}
            </div>
          )}
        </article>
      )}

      {/* AI 重新生成 Dialog —— initialInput 用当前条目 content,让用户看到
          系统是理解「把这段再画一次」的。默认会打开新行 view。 */}
      {state.kind === 'ok' && (
        <DiagramGenerator
          open={showDiagramGenerator}
          projectId={state.item.project_id}
          initialInput={state.item.content || state.item.title}
          onClose={() => setShowDiagramGenerator(false)}
          onSaved={(row) => {
            setShowDiagramGenerator(false);
            window.open(`/knowledge/view/${row.id}`, '_blank');
          }}
        />
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

// PencilIcon — 轻量 inline SVG，避免再为单个图标走 IconRegistry。
// 14px viewBox 与 KnowledgeViewPage 顶栏按钮的图标尺寸对齐。
function PencilIcon({ size = 14 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M11.5 2.5l2 2-8 8H3.5v-2l8-8z" />
      <path d="M10 4l2 2" />
    </svg>
  );
}

// EditForm — 知识条目编辑表单。title / content / category 三字段独立更新；
// 保存触发 saveEditing → knowledgeApi.update；saving=true 时禁用按钮 + 显
// 示「保存中…」。category 用 select + 默认值 'general'，与 KnowledgePage 的
// 类型徽章配色保持兼容；后端允许任意 category 字符串，前端不强制枚举。
const CATEGORY_OPTIONS = [
  'business_context',
  'technical_debt',
  'design_rationale',
  'code_explanation',
  'dev_report',
  'subtask_report',
  'general',
];

function EditForm({
  draft,
  onChange,
  onCancel,
  onSave,
}: {
  draft: { title: string; content: string; category: string; saving: boolean };
  onChange: (patch: Partial<{ title: string; content: string; category: string }>) => void;
  onCancel: () => void;
  onSave: () => void;
}) {
  const { t } = useTranslation();
  return (
    <form
      className="kvp-edit-form"
      onSubmit={(e) => {
        e.preventDefault();
        if (!draft.saving) onSave();
      }}
    >
      <label className="kvp-edit-field">
        <span className="kvp-edit-label">{t('knowledge.view.editTitleLabel')}</span>
        <input
          className="form-input"
          type="text"
          value={draft.title}
          onChange={(e) => onChange({ title: e.target.value })}
          placeholder={t('knowledge.view.editTitlePlaceholder')}
          disabled={draft.saving}
          autoFocus
        />
      </label>
      <label className="kvp-edit-field">
        <span className="kvp-edit-label">{t('knowledge.view.editCategoryLabel')}</span>
        <select
          className="form-input"
          value={draft.category || 'general'}
          onChange={(e) => onChange({ category: e.target.value })}
          disabled={draft.saving}
        >
          {CATEGORY_OPTIONS.map((c) => (
            <option key={c} value={c}>
              {t(`knowledge.memType.${c}` as any, { defaultValue: c })}
            </option>
          ))}
        </select>
      </label>
      <label className="kvp-edit-field">
        <span className="kvp-edit-label">{t('knowledge.view.editContentLabel')}</span>
        <textarea
          className="form-input kvp-edit-textarea"
          value={draft.content}
          onChange={(e) => onChange({ content: e.target.value })}
          placeholder={t('knowledge.view.editContentPlaceholder')}
          disabled={draft.saving}
          rows={20}
        />
      </label>
      <div className="kvp-edit-actions">
        <button
          type="button"
          className="btn"
          onClick={onCancel}
          disabled={draft.saving}
        >
          {t('knowledge.view.editCancelBtn')}
        </button>
        <button
          type="submit"
          className="btn btn-primary"
          disabled={draft.saving || !draft.content.trim()}
        >
          {draft.saving
            ? t('knowledge.view.editSavingBtn')
            : t('knowledge.view.editSaveBtn')}
        </button>
      </div>
    </form>
  );
}
