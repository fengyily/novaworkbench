import { useCallback, useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import {
  crossRequirementsApi,
  projectsApi,
  type CrossRequirement,
  type CrossRequirementProgress,
  type Project,
} from '../api/client';
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

const STATUS_OPTIONS = ['draft', 'designing', 'designed', 'developing', 'integrating', 'done'] as const;
type StatusFilter = '' | (typeof STATUS_OPTIONS)[number];

const PRIORITY_OPTIONS = ['low', 'medium', 'high', 'urgent'] as const;

export default function CrossRequirementsList() {
  const { t } = useTranslation();
  const navigate = useNavigate();

  const [items, setItems] = useState<CrossRequirement[]>([]);
  const [progress, setProgress] = useState<Record<string, CrossRequirementProgress>>({});
  const [projects, setProjects] = useState<Project[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [status, setStatus] = useState<StatusFilter>('');
  const [keyword, setKeyword] = useState('');

  // create dialog state
  const [creating, setCreating] = useState(false);
  const [draftTitle, setDraftTitle] = useState('');
  const [draftDescription, setDraftDescription] = useState('');
  const [draftPriority, setDraftPriority] = useState<string>('medium');
  const [draftProjectIds, setDraftProjectIds] = useState<string[]>([]);
  const [submitting, setSubmitting] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);

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

  useEffect(() => { load(); }, [load]);

  useEffect(() => {
    projectsApi.list()
      .then(list => setProjects(Array.isArray(list) ? list : []))
      .catch(() => setProjects([]));
  }, []);

  const openCreate = () => {
    setDraftTitle('');
    setDraftDescription('');
    setDraftPriority('medium');
    setDraftProjectIds([]);
    setFormError(null);
    setCreating(true);
  };

  const toggleProject = (id: string) => {
    setDraftProjectIds(prev => (prev.includes(id) ? prev.filter(p => p !== id) : [...prev, id]));
  };

  const submitCreate = async () => {
    if (submitting) return;
    if (!draftTitle.trim()) {
      setFormError(t('crossRequirements.create.failed'));
      return;
    }
    if (draftProjectIds.length === 0) {
      setFormError(t('crossRequirements.noProjects'));
      return;
    }
    setSubmitting(true);
    setFormError(null);
    try {
      const created = await crossRequirementsApi.create({
        title: draftTitle.trim(),
        description: draftDescription,
        priority: draftPriority,
        project_ids: draftProjectIds,
      });
      setCreating(false);
      navigate(`/cross-requirements/${created.id}`);
    } catch (err) {
      setFormError(errorMessage(err));
    } finally {
      setSubmitting(false);
    }
  };

  const statusLabel = useCallback(
    (value: string) => t(`crossRequirements.status.${value}` as never),
    [t],
  );

  return (
    <div className="xreq-page">
      <div className="xreq-head">
        <div>
          <h1 className="xreq-title">{t('crossRequirements.title')}</h1>
          <p className="xreq-subtitle">{t('crossRequirements.subtitle')}</p>
        </div>
        <button type="button" className="btn btn-primary" onClick={openCreate}>
          {t('crossRequirements.new')}
        </button>
      </div>

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

      {error && <div className="xreq-error">{error}</div>}

      {loading ? (
        <div className="xreq-empty">{t('crossRequirements.loading')}</div>
      ) : items.length === 0 ? (
        <div className="xreq-empty">
          <p className="xreq-empty-title">{t('crossRequirements.empty')}</p>
          <p className="xreq-empty-hint">{t('crossRequirements.emptyHint')}</p>
        </div>
      ) : (
        <div className="xreq-list">
          {items.map(item => {
            const p = progress[item.id];
            const percent = p?.percent ?? 0;
            const done = p?.done ?? 0;
            const total = p?.total ?? 0;
            return (
              <button
                type="button"
                key={item.id}
                className="xreq-item"
                onClick={() => navigate(`/cross-requirements/${item.id}`)}
              >
                <div className="xreq-item-head">
                  <span className="xreq-item-title">{item.title}</span>
                  <span className={`status-badge ${crossStatusBadgeClass(item.status)}`}>
                    {statusLabel(item.status)}
                  </span>
                </div>
                <div className="xreq-item-badges">
                  {(p?.projects || []).map(pp => (
                    <span key={pp.project_id} className="xreq-chip">
                      {pp.project_name || pp.project_id}
                      {pp.role ? ` · ${pp.role}` : ''}
                    </span>
                  ))}
                </div>
                <div className="xreq-progress">
                  <div className="xreq-progress-track">
                    <div className="xreq-progress-fill" style={{ width: `${percent}%` }} />
                  </div>
                  <span className="xreq-progress-text">
                    {percent}% · {t('crossRequirements.childrenDone', { done, total })}
                  </span>
                </div>
                <div className="xreq-item-meta">
                  {t('crossRequirements.updatedAt', { time: fmtDateTime(item.updated_at) })}
                </div>
              </button>
            );
          })}
        </div>
      )}

      {creating && (
        <div className="xreq-modal-overlay" onClick={() => !submitting && setCreating(false)}>
          <div className="xreq-modal" onClick={e => e.stopPropagation()}>
            <h2 className="xreq-modal-title">{t('crossRequirements.create.title')}</h2>

            <div className="xreq-field">
              <label className="xreq-field-label" htmlFor="xreq-title-input">
                {t('crossRequirements.create.name')}
              </label>
              <input
                id="xreq-title-input"
                className="form-input"
                value={draftTitle}
                placeholder={t('crossRequirements.create.namePlaceholder')}
                onChange={e => setDraftTitle(e.target.value)}
              />
            </div>

            <div className="xreq-field">
              <label className="xreq-field-label" htmlFor="xreq-desc-input">
                {t('crossRequirements.create.description')}
              </label>
              <textarea
                id="xreq-desc-input"
                className="form-input"
                rows={4}
                value={draftDescription}
                placeholder={t('crossRequirements.create.descriptionPlaceholder')}
                onChange={e => setDraftDescription(e.target.value)}
              />
            </div>

            <div className="xreq-field">
              <label className="xreq-field-label" htmlFor="xreq-priority-input">
                {t('crossRequirements.create.priority')}
              </label>
              <select
                id="xreq-priority-input"
                className="form-input"
                value={draftPriority}
                onChange={e => setDraftPriority(e.target.value)}
              >
                {PRIORITY_OPTIONS.map(p => (
                  <option key={p} value={p}>{t(`crossRequirements.priority.${p}` as never)}</option>
                ))}
              </select>
            </div>

            <div className="xreq-field">
              <span className="xreq-field-label">{t('crossRequirements.create.projects')}</span>
              <p className="xreq-field-hint">{t('crossRequirements.create.projectsHint')}</p>
              <div className="xreq-picker">
                {projects.map(p => (
                  <label key={p.id} className="xreq-option">
                    <input
                      type="checkbox"
                      checked={draftProjectIds.includes(p.id)}
                      onChange={() => toggleProject(p.id)}
                    />
                    <span>{p.name}</span>
                  </label>
                ))}
              </div>
            </div>

            {formError && <div className="xreq-error">{formError}</div>}

            <div className="xreq-modal-actions">
              <button
                type="button"
                className="btn"
                onClick={() => setCreating(false)}
                disabled={submitting}
              >
                {t('crossRequirements.create.cancel')}
              </button>
              <button
                type="button"
                className="btn btn-primary"
                onClick={submitCreate}
                disabled={submitting}
              >
                {t('crossRequirements.create.submit')}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
