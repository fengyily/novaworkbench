import { useCallback, useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import {
  ApiError,
  crossRequirementsApi,
  projectsApi,
  type CrossRequirement,
  type CrossRequirementProgress,
  type Project,
} from '../api/client';
import CrossRequirementCreateForm from '../components/CrossRequirementForm/CrossRequirementCreateForm';
import CrossRequirementPurgeDialog from '../components/CrossRequirementPurgeDialog';
import { IconRefresh, IconTrash, IconTrashAlt } from '../components/icons';
import { errorMessage } from '../utils/errMsg';
import { fmtDateTime } from '../utils/intl';
import { crossStatusBadgeClass } from '../utils/crossStatus';
import './CrossRequirements.css';

// 跨项目需求列表页。
//
// 一个跨项目需求横跨多个仓库，所以列表页的核心信息是三件事：状态、涉及哪些
// 项目、整体进展（完成百分比 + 已完成 / 总数）。进展是后端**实时聚合**的，
// 页面不自己算 —— 否则页面与后端会对不上。
//
// 进度条 / 状态 chip 都走全站共享的类（.status-badge / --color-* 令牌），页面
// 私有样式表只放这一页独有的排版（统一 xreq- 前缀）。
//
// 「新建」是页面**内联的面板**（不是弹窗）—— 与普通需求的新建面板同构：
// 表单挂在列表上方，用户点完按钮就能看见它展开；弹窗留给确认类动作。
//
// ── 删除是两步的 ────────────────────────────────────────────────────────────
//
// 移进回收站（可恢复）→ 彻底删除（不可恢复）。**列表页只提供第一步**：
// 不可逆的那一步只能在回收站里发起，所以这一页只有一个破坏性入口，而且它是
// 可撤销的。
//
// 为什么值得多这一步：跨项目需求被拆过之后，整体方案 / 联调阶段 / 跨项目依赖
// 是**不可再生产**的东西 —— 子需求里的代码还在，但「三个仓库怎么对齐」的上下文
// 丢了就是丢了。
//
// 页面因此有**两个视图**：进行中（默认）与回收站。视图切换是页头的一个按钮，
// 与项目列表（ProjectsList）的 active / trash 双视图同一套词汇。

const STATUS_OPTIONS = ['draft', 'designing', 'designed', 'developing', 'integrating', 'done', 'archived'] as const;
type StatusFilter = '' | (typeof STATUS_OPTIONS)[number];

export default function CrossRequirementsList() {
  const { t } = useTranslation();
  const navigate = useNavigate();

  // 'active' — 进行中的跨项目需求；'trash' — 回收站。
  const [view, setView] = useState<'active' | 'trash'>('active');
  const [items, setItems] = useState<CrossRequirement[]>([]);
  const [progress, setProgress] = useState<Record<string, CrossRequirementProgress>>({});
  const [projects, setProjects] = useState<Project[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [status, setStatus] = useState<StatusFilter>('');
  const [keyword, setKeyword] = useState('');

  // The create panel is a mounted component, not a bag of draft state: it
  // owns the form fields AND the pre-created draft row (the row inline
  // images and attachments hang off).
  const [creating, setCreating] = useState(false);

  // 彻底删除的确认框：目标行 + 勾选确认（没勾就不能点确认按钮）。
  const [purgeTarget, setPurgeTarget] = useState<CrossRequirement | null>(null);
  const [purgeAck, setPurgeAck] = useState(false);
  const [purging, setPurging] = useState(false);

  const [toast, setToast] = useState<string | null>(null);
  useEffect(() => {
    if (!toast) return;
    const timer = window.setTimeout(() => setToast(null), 3200);
    return () => window.clearTimeout(timer);
  }, [toast]);

  const load = useCallback(() => {
    setLoading(true);
    setError(null);
    crossRequirementsApi.list({ status: status || undefined, keyword: keyword || undefined })
      .then(data => {
        setItems(Array.isArray(data?.items) ? data.items : []);
        setProgress(data?.progress || {});
      })
      .catch(err => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, [status, keyword]);

  const loadTrash = useCallback(() => {
    setLoading(true);
    setError(null);
    crossRequirementsApi.trashList()
      .then(data => {
        setItems(Array.isArray(data?.items) ? data.items : []);
        setProgress(data?.progress || {});
      })
      .catch(err => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, []);

  // The two views have different data sources, so the effect branches on the
  // view instead of making one loader serve both.
  useEffect(() => {
    if (view === 'trash') loadTrash();
    else load();
  }, [view, load, loadTrash]);

  useEffect(() => {
    projectsApi.list()
      .then(list => setProjects(Array.isArray(list) ? list : []))
      .catch(() => setProjects([]));
  }, []);

  const statusLabel = useCallback(
    (value: string) => t(`crossRequirements.status.${value}` as never),
    [t],
  );

  // ── 删除动作 ────────────────────────────────────────────────────────────

  // 移到回收站：软删，立即生效，无需确认框（可恢复，所以确认框只会变成噪音）。
  const moveToTrash = async (item: CrossRequirement) => {
    try {
      await crossRequirementsApi.trash(item.id);
      setToast(t('crossRequirements.trash.movedToTrash'));
      if (view === 'trash') loadTrash();
      else load();
    } catch (err) {
      // 连点两次：第二个请求撞上「已经在回收站里了」—— 那恰恰是想要的结果，
      // 不该报成错误。
      if (err instanceof ApiError && err.code === 'XREQ_IN_TRASH') {
        setToast(t('crossRequirements.trash.movedToTrash'));
        load();
        return;
      }
      setError(errorMessage(err));
    }
  };

  const restore = async (item: CrossRequirement) => {
    try {
      await crossRequirementsApi.restore(item.id);
      setToast(t('crossRequirements.trash.restored'));
      if (view === 'trash') loadTrash();
      else load();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const openPurge = (item: CrossRequirement) => {
    setPurgeAck(false);
    setPurgeTarget(item);
  };

  const closePurge = () => {
    if (purging) return;
    setPurgeTarget(null);
    setPurgeAck(false);
  };

  const confirmPurge = async () => {
    if (!purgeTarget || purging) return;
    setPurging(true);
    try {
      await crossRequirementsApi.purge(purgeTarget.id);
      setPurgeTarget(null);
      setPurgeAck(false);
      setToast(t('crossRequirements.trash.purged'));
      if (view === 'trash') loadTrash();
      else load();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setPurging(false);
    }
  };

  const open = (id: string) => navigate(`/cross-requirements/${id}`);

  // ── 渲染 ─────────────────────────────────────────────────────────────────

  // 彻底删除的确认框：与详情页共用同一个组件（同一套措辞、同一套风险面板），
  // 子需求那半句是关键 —— 它让人敢点这个按钮。
  const purgeChildCount = purgeTarget
    ? (progress[purgeTarget.id]?.total ?? 0)
    : 0;

  return (
    <div className="xreq-page">
      <div className="xreq-head">
        <div>
          <h1 className="xreq-title">
            {view === 'trash' ? t('crossRequirements.trash.title') : t('crossRequirements.title')}
          </h1>
          <p className="xreq-subtitle">
            {view === 'trash' ? t('crossRequirements.trash.subtitle') : t('crossRequirements.subtitle')}
          </p>
        </div>
        <div className="xreq-actions">
          {view === 'active' && (
            <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
              {t('crossRequirements.new')}
            </button>
          )}
          <button
            type="button"
            className="btn"
            onClick={() => setView(view === 'trash' ? 'active' : 'trash')}
          >
            {view === 'trash'
              ? t('crossRequirements.trash.backToList')
              : t('crossRequirements.trash.openTrash')}
          </button>
        </div>
      </div>

      {/* The create panel sits ABOVE the list — with a long list below the
          fold, a form at the bottom reads as "nothing happened". */}
      {creating && (
        <div className="xreq-create-anchor">
          <CrossRequirementCreateForm
            projects={projects}
            onCancel={() => setCreating(false)}
            onCreated={id => navigate(`/cross-requirements/${id}`)}
          />
        </div>
      )}

      {view === 'active' && (
        <div className="xreq-toolbar">
          <select
            className="form-input xreq-filter"
            value={status}
            onChange={e => setStatus(e.target.value as StatusFilter)}
            aria-label={t('crossRequirements.filterAll')}
          >
            <option value="">{t('crossRequirements.filterAll')}</option>
            {STATUS_OPTIONS.map(s => (
              <option key={s} value={s}>{statusLabel(s)}</option>
            ))}
          </select>
          <input
            className="form-input xreq-search"
            value={keyword}
            placeholder={t('crossRequirements.searchPlaceholder')}
            onChange={e => setKeyword(e.target.value)}
            aria-label={t('crossRequirements.searchPlaceholder')}
          />
        </div>
      )}

      {error && <div className="xreq-error">{error}</div>}

      {loading ? (
        <div className="xreq-empty">{t('crossRequirements.loading')}</div>
      ) : items.length === 0 ? (
        <div className="xreq-empty">
          <p className="xreq-empty-title">
            {view === 'trash' ? t('crossRequirements.trash.empty') : t('crossRequirements.empty')}
          </p>
          <p className="xreq-empty-hint">
            {view === 'trash' ? t('crossRequirements.trash.emptyHint') : t('crossRequirements.emptyHint')}
          </p>
        </div>
      ) : (
        <div className="xreq-list">
          {items.map(item => {
            const p = progress[item.id];
            const percent = p?.percent ?? 0;
            const done = p?.done ?? 0;
            const total = p?.total ?? 0;
            return (
              <div
                key={item.id}
                className="xreq-item"
                onClick={() => open(item.id)}
              >
                <div className="xreq-item-head">
                  <button
                    type="button"
                    className="xreq-item-title-btn"
                    onClick={e => { e.stopPropagation(); open(item.id); }}
                  >
                    {item.title}
                  </button>
                  <span
                    className={`status-badge ${crossStatusBadgeClass(item.status)}`}
                    // In the trash the badge shows the state the row was in
                    // when it was deleted — the tooltip says so explicitly.
                    title={view === 'trash'
                      ? t('crossRequirements.trash.originalStatus', { status: statusLabel(item.status) })
                      : undefined}
                  >
                    {statusLabel(item.status)}
                  </span>
                  {view === 'active' ? (
                    <button
                      type="button"
                      className="xreq-item-action"
                      title={t('crossRequirements.trash.moveToTrash')}
                      aria-label={t('crossRequirements.trash.moveToTrash')}
                      onClick={e => { e.stopPropagation(); moveToTrash(item); }}
                    >
                      <IconTrash size={15} />
                    </button>
                  ) : (
                    <span className="xreq-item-actions">
                      <button
                        type="button"
                        className="btn btn-sm"
                        onClick={e => { e.stopPropagation(); restore(item); }}
                      >
                        <IconRefresh size={13} />
                        {t('crossRequirements.trash.restore')}
                      </button>
                      <button
                        type="button"
                        className="btn btn-sm btn-danger"
                        onClick={e => { e.stopPropagation(); openPurge(item); }}
                      >
                        <IconTrashAlt size={13} />
                        {t('crossRequirements.trash.purge')}
                      </button>
                    </span>
                  )}
                </div>
                <div className="xreq-item-badges">
                  {(p?.projects || []).map(pp => (
                    <span key={pp.project_id} className="xreq-chip">
                      {pp.project_name || pp.project_id}
                      {pp.role ? ` · ${pp.role}` : ''}
                    </span>
                  ))}
                </div>
                {view === 'active' ? (
                  <div className="xreq-progress">
                    <div className="xreq-progress-track">
                      <div className="xreq-progress-fill" style={{ width: `${percent}%` }} />
                    </div>
                    <span className="xreq-progress-text">
                      {percent}% · {t('crossRequirements.childrenDone', { done, total })}
                    </span>
                  </div>
                ) : (
                  <div className="xreq-item-meta">
                    {t('crossRequirements.trash.deletedAt', { time: fmtDateTime(item.deleted_at) })}
                    {total > 0 && ` · ${t('crossRequirements.trash.childrenCount', { n: total })}`}
                  </div>
                )}
                {view === 'active' && (
                  <div className="xreq-item-meta">
                    {t('crossRequirements.updatedAt', { time: fmtDateTime(item.updated_at) })}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}

      <CrossRequirementPurgeDialog
        open={!!purgeTarget}
        busy={purging}
        title={purgeTarget?.title ?? ''}
        childCount={purgeChildCount}
        ack={purgeAck}
        onAckChange={setPurgeAck}
        onConfirm={confirmPurge}
        onCancel={closePurge}
      />
      {toast && <div className="xreq-toast">{toast}</div>}
    </div>
  );
}
