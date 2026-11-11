// RequirementDependencyPanel — the 「前置依赖 / 后继任务」 block on a
// sub-requirement's detail page.
//
// This is the dependency (execution-order) view, NOT the reference (context
// sharing) view. The distinction matters: a dependency BLOCKS work — the
// backend's coding gate refuses to promote a requirement whose prerequisite
// is not done — while a reference only enriches the design prompt.
//
// The panel reports the unmet prerequisites back to the parent so the
// 「开始开发」 button can be disabled with a tooltip naming the blocker.
//
// A prerequisite that is done is NOT enough on its own: its PR still has to
// be merged into the base branch before the dependent's branch picks the
// code up. The panel says so, because "done" is otherwise read as "the code
// is on my branch".

import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { requirementsApi, type Requirement, type RequirementDependencyEdge } from '../api/client';
import './RequirementDependencyPanel.css';

export interface RequirementDependencyPanelProps {
  requirementId: string;
  /** Reports the titles of the prerequisites that are NOT done yet. */
  onUnmetChange?: (unmet: string[]) => void;
}

interface ChildData {
  children: Requirement[];
  dependencies: RequirementDependencyEdge[];
}

export function RequirementDependencyPanel({
  requirementId,
  onUnmetChange,
}: RequirementDependencyPanelProps) {
  const { t } = useTranslation();
  const [data, setData] = useState<ChildData | null>(null);

  const load = useCallback(async () => {
    try {
      const res = await requirementsApi.listChildren(requirementId);
      setData({ children: res.children ?? [], dependencies: res.dependencies ?? [] });
    } catch {
      setData({ children: [], dependencies: [] });
    }
  }, [requirementId]);

  useEffect(() => {
    void load();
  }, [load]);

  const titleOf = useMemo(() => {
    const map = new Map<string, string>();
    for (const c of data?.children ?? []) map.set(c.id, c.title);
    return map;
  }, [data]);

  const pre = useMemo(
    () => (data?.dependencies ?? []).filter((d) => d.requirement_id === requirementId),
    [data, requirementId],
  );
  const post = useMemo(
    () => (data?.dependencies ?? []).filter((d) => d.depends_on_id === requirementId),
    [data, requirementId],
  );

  // The unmet prerequisites — their titles drive the disabled state and the
  // tooltip on the 「开始开发」 button.
  const unmet = useMemo(
    () => pre.filter((d) => d.status !== 'done').map((d) => titleOf.get(d.depends_on_id) || d.depends_on_id),
    [pre, titleOf],
  );

  useEffect(() => {
    if (onUnmetChange) onUnmetChange(unmet);
  }, [unmet, onUnmetChange]);

  if (pre.length === 0 && post.length === 0) return null;

  return (
    <div className="req-dep-panel">
      <div className="req-dep-section">
        <div className="req-dep-title">{t('requirements.dependency.title')}</div>
        {pre.length === 0 ? (
          <div className="req-dep-empty">{t('requirements.dependency.none')}</div>
        ) : (
          <ul className="req-dep-list">
            {pre.map((d) => {
              const done = d.status === 'done';
              const title = titleOf.get(d.depends_on_id) || d.depends_on_id;
              return (
                <li key={`pre-${d.depends_on_id}`} className={done ? 'req-dep-item' : 'req-dep-item req-dep-item-unmet'}>
                  <Link to={`/requirements/${d.depends_on_id}`} className="req-dep-link">
                    {title}
                  </Link>
                  <span className="req-dep-status">
                    {done ? t('requirements.dependency.done') : t('requirements.dependency.pending')}
                  </span>
                </li>
              );
            })}
          </ul>
        )}
        {pre.length > 0 && (
          <div className="req-dep-hint">{t('requirements.dependency.mergeHint')}</div>
        )}
      </div>

      {post.length > 0 && (
        <div className="req-dep-section">
          <div className="req-dep-title">{t('requirements.dependency.postTitle')}</div>
          <ul className="req-dep-list">
            {post.map((d) => {
              const title = titleOf.get(d.requirement_id) || d.requirement_id;
              return (
                <li key={`post-${d.requirement_id}`} className="req-dep-item">
                  <Link to={`/requirements/${d.requirement_id}`} className="req-dep-link">
                    {title}
                  </Link>
                </li>
              );
            })}
          </ul>
        </div>
      )}
    </div>
  );
}

export default RequirementDependencyPanel;
