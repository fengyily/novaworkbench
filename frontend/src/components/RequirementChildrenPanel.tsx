// RequirementChildrenPanel — the 「子需求」 block on a master requirement's
// detail page.
//
// It lists the sub-requirements a split produced, each with its status, the
// base branch it inherits, and the dependency markers (「依赖 ①」 /
// 「被 ② 依赖」).
//
// The child count comes from the CHILDREN ARRAY, not from
// requirement.child_count: the backend only fills child_count on the LIST
// endpoint, and this panel talks to the dedicated children endpoint.
//
// The dependency markers use the batch ordinals (the order the children were
// created in), not ids — an ordinal is what the user sees in the split
// preview, so "依赖 ②" points at the row they remember.

import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { requirementsApi, type Requirement, type RequirementDependencyEdge } from '../api/client';
import { StatusChips } from './StatusChips';
import './RequirementChildrenPanel.css';

export interface RequirementChildrenPanelProps {
  requirementId: string;
  /** Fired when the user asks to start a split (the parent owns the modal). */
  onRequestSplit?: () => void;
  /** Bumped by the parent when the children list may have changed (a split
   *  was just confirmed). The panel re-fetches on change.
   *
   *  This is a TOKEN, not a callback, on purpose. The previous shape was
   *  `onChanged` + a `load` that called it — the parent's `onChanged` was an
   *  inline arrow (`() => void refresh()`), so it got a NEW identity on
   *  every parent render. `onChanged` sat in `load`'s useCallback deps, so
   *  every parent render rebuilt `load`, which re-fired the `useEffect`
   *  below, which re-fetched the children and called `onChanged()` again →
   *  the parent refreshed → new render → ∞. Symptom: the requirement detail
   *  page hammered /api/requirements/{id}/children + /api/requirements/{id} +
   *  /api/usage/... + /api/schedules in a tight loop. A token keeps the
   *  fetch effect keyed on something that only changes when the data
   *  actually changed. */
  reloadToken?: number;
  /** The child's count is reported back so the parent can show the
   *  「开发已下放给 N 个子需求」 banner. */
  onChildCountChange?: (n: number) => void;
}

interface ChildData {
  children: Requirement[];
  dependencies: RequirementDependencyEdge[];
}

export function RequirementChildrenPanel({
  requirementId,
  onRequestSplit,
  reloadToken,
  onChildCountChange,
}: RequirementChildrenPanelProps) {
  const { t } = useTranslation();
  const [data, setData] = useState<ChildData | null>(null);
  const [error, setError] = useState('');

  // NOTE: no parent callback in this dep array — see the `reloadToken`
  // comment above. `load` must stay stable across parent renders, or the
  // effect below refires on every render.
  const load = useCallback(async () => {
    try {
      const res = await requirementsApi.listChildren(requirementId);
      setData({ children: res.children ?? [], dependencies: res.dependencies ?? [] });
      setError('');
    } catch {
      setError(t('requirements.split.errGeneric', { msg: '' }));
    }
  }, [requirementId, t]);

  useEffect(() => {
    void load();
  }, [load, reloadToken]);

  useEffect(() => {
    if (onChildCountChange) onChildCountChange(data?.children.length ?? 0);
  }, [data, onChildCountChange]);

  // Ordinal (1-based) → child row, so the markers can point at "②" the way
  // the split preview numbered them.
  //
  // The number comes from the child's PERSISTED batch ordinal (split_order),
  // not from its position in this array: the backend orders the children by
  // that ordinal, so the two agree for a split batch — but a manually
  // attached child has no split_order and falls back to its position.
  const ordinalOf = useMemo(() => {
    const map = new Map<string, number>();
    (data?.children ?? []).forEach((c, i) =>
      map.set(c.id, c.split_order && c.split_order > 0 ? c.split_order : i + 1),
    );
    return map;
  }, [data]);

  // Edge id → the ids on the other side. The ORDINALS the user sees are
  // derived from these (see ordinalOf).
  const dependsOnOf = useMemo(() => {
    const map = new Map<string, string[]>();
    for (const edge of data?.dependencies ?? []) {
      const list = map.get(edge.requirement_id) ?? [];
      list.push(edge.depends_on_id);
      map.set(edge.requirement_id, list);
    }
    return map;
  }, [data]);

  const dependedOnBy = useMemo(() => {
    const map = new Map<string, string[]>();
    for (const edge of data?.dependencies ?? []) {
      const list = map.get(edge.depends_on_id) ?? [];
      list.push(edge.requirement_id);
      map.set(edge.depends_on_id, list);
    }
    return map;
  }, [data]);

  if (error) {
    return (
      <div className="req-children-panel">
        <div className="req-children-error">{error}</div>
      </div>
    );
  }

  const children = data?.children ?? [];

  return (
    <div className="req-children-panel">
      <div className="req-children-head">
        <span className="req-children-title">{t('requirements.split.childrenTitle')}</span>
        {children.length > 0 && (
          <button type="button" className="btn btn-sm" onClick={() => void load()}>
            {t('requirements.split.refresh')}
          </button>
        )}
      </div>

      {children.length === 0 ? (
        <div className="req-children-empty">
          <div className="req-children-empty-text">{t('requirements.split.emptyTitle')}</div>
          {onRequestSplit && (
            <button type="button" className="btn btn-sm" onClick={onRequestSplit}>
              🧩 {t('requirements.split.btn')}
            </button>
          )}
        </div>
      ) : (
        <div className="req-children-list">
          {children.map((c, i) => {
            const deps = (dependsOnOf.get(c.id) ?? [])
              .map((id) => ordinalOf.get(id))
              .filter((n): n is number => n !== undefined);
            const dependents = (dependedOnBy.get(c.id) ?? [])
              .map((id) => ordinalOf.get(id))
              .filter((n): n is number => n !== undefined);
            // The badge shows the batch ordinal (the number the split
            // preview used), not the array position.
            const ordinal = ordinalOf.get(c.id) ?? i + 1;
            return (
              <div className="req-child-card" key={c.id}>
                <div className="req-child-main">
                  <span className="req-child-idx">{ordinal}</span>
                  <Link className="req-child-title" to={`/requirements/${c.id}`}>
                    {c.title || t('requirements.split.noTitle')}
                  </Link>
                  <StatusChips req={c} />
                </div>
                <div className="req-child-meta">
                  <span className="req-child-meta-item">
                    {t('requirements.split.baseFollows', { base: c.base_branch || 'main' })}
                  </span>
                  {deps.length > 0 && (
                    <span className="req-child-meta-item req-child-meta-dep">
                      {t('requirements.dependency.dependsOn', { list: deps.join('、') })}
                    </span>
                  )}
                  {dependents.length > 0 && (
                    <span className="req-child-meta-item">
                      {t('requirements.dependency.dependedBy', { list: dependents.join('、') })}
                    </span>
                  )}
                  {c.split_batch_id && (
                    <span className="req-child-batch" title={t('requirements.split.batchTitle')}>
                      {c.split_batch_id.slice(0, 8)}
                    </span>
                  )}
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

export default RequirementChildrenPanel;
