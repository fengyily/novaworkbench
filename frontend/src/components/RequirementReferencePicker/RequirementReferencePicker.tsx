// RequirementReferencePicker — the "引用需求" selector.
//
// The user picks the requirements this one is related to; at design time the
// backend injects each referenced requirement's **description + existing plan**
// into the architect's prompt so the new plan can reuse the settled decisions.
//
// Three things this component deliberately does NOT do:
//
//   - It never writes an inherited (`origin === 'parent'`) entry back. Those
//     come from the master requirement; writing them back would freeze them
//     as direct references and break the inheritance.
//   - It never surfaces the referenced requirement's body. The title is
//     enough to recognize it; the body is injected server-side.
//   - It never invents a search endpoint. Candidates are fetched once and
//     filtered client-side by title, the same shape as the "add child" popup
//     on the requirements list.
//
// The cap (8) mirrors the backend's maxReferenceCount so the UI can't offer
// something the server would silently drop.

import { useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  kindLabelKeys,
  projectsApi,
  requirementsApi,
  statusChipLabelKeys,
  type Requirement,
  type RequirementReference,
} from '../../api/client';
import { tLabel } from '../../i18n/label';
import './RequirementReferencePicker.css';

/** Must match the backend's maxReferenceCount (service/requirement_reference.go). */
export const MAX_REFERENCE_COUNT = 8;

/** How many candidates we render before collapsing the rest into a counter. */
const MAX_VISIBLE_CANDIDATES = 20;

export interface RequirementReferencePickerProps {
  value: RequirementReference[];
  onChange: (next: RequirementReference[]) => void;
  /** Restrict candidates to one project. Omit for cross-project selection. */
  projectId?: string;
  /** Ids that must not be offered (typically the requirement being edited). */
  excludeIds?: string[];
  disabled?: boolean;
}

export function RequirementReferencePicker({
  value,
  onChange,
  projectId,
  excludeIds,
  disabled,
}: RequirementReferencePickerProps) {
  const { t } = useTranslation();
  const [query, setQuery] = useState('');
  const [candidates, setCandidates] = useState<Requirement[]>([]);
  const [projectNames, setProjectNames] = useState<Record<string, string>>({});
  const [loading, setLoading] = useState(false);
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  // Candidates are fetched once on mount. The list is small enough (one row
  // per requirement) and a title filter needs no round-trip.
  //
  // ALL requirements are fetched, not just the current project's: the
  // backend deliberately allows cross-project references ("参考另一个项目里
  // 已定稿的方案" is the feature's main value), and the candidate row shows
  // the project name precisely so a cross-project pick is visible. The
  // current project is not excluded — it is only sorted first (see `matched`).
  useEffect(() => {
    let alive = true;
    setLoading(true);
    requirementsApi.list()
      .then(list => { if (alive) setCandidates(list); })
      .catch(() => { if (alive) setCandidates([]); })
      .finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
  }, []);

  // Project names are only used as a hint on cross-project candidates.
  useEffect(() => {
    let alive = true;
    projectsApi.list()
      .then(rows => {
        if (!alive) return;
        const map: Record<string, string> = {};
        rows.forEach(p => { map[p.id] = p.name; });
        setProjectNames(map);
      })
      .catch(() => {});
    return () => { alive = false; };
  }, []);

  // Close the dropdown on an outside click — the picker is embedded in a form
  // that has its own submit affordances, so a stray open dropdown would
  // cover them.
  useEffect(() => {
    if (!open) return;
    const onDocClick = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDocClick);
    return () => document.removeEventListener('mousedown', onDocClick);
  }, [open]);

  const excluded = useMemo(
    () => new Set([...(excludeIds || []), ...value.map(r => r.requirement_id)]),
    [excludeIds, value],
  );

  // Only the direct entries count against the cap — an inherited entry is
  // not something the user added here.
  const directCount = value.filter(r => r.origin === 'direct').length;
  const atLimit = directCount >= MAX_REFERENCE_COUNT;

  const matched = useMemo(() => {
    const q = query.trim().toLowerCase();
    const hits = candidates.filter(c => {
      if (excluded.has(c.id)) return false;
      // Archived rows are historical — pointing a live design at one is
      // almost never what the user means.
      if (c.status === 'archived') return false;
      if (!q) return true;
      return (c.title || '').toLowerCase().includes(q);
    });
    // Same-project candidates first: cross-project references are allowed, but
    // the common case is a sibling requirement in the same project.
    if (projectId) {
      hits.sort((a, b) => {
        const aOwn = a.project_id === projectId ? 0 : 1;
        const bOwn = b.project_id === projectId ? 0 : 1;
        return aOwn - bOwn;
      });
    }
    return hits;
  }, [candidates, excluded, query, projectId]);

  const visible = matched.slice(0, MAX_VISIBLE_CANDIDATES);
  const hidden = matched.length - visible.length;

  const add = (c: Requirement) => {
    if (atLimit) return;
    const next: RequirementReference = {
      requirement_id: c.id,
      note: '',
      origin: 'direct',
      title: c.title || c.id,
      status: c.status || '',
      kind: c.kind,
      project_id: c.project_id || '',
      project_name: projectNames[c.project_id] || '',
      has_design: !!(c.design_docs && c.design_docs !== '[]'),
    };
    onChange([...value, next]);
    setQuery('');
    setOpen(false);
  };

  const remove = (id: string) => {
    onChange(value.filter(r => !(r.requirement_id === id && r.origin === 'direct')));
  };

  const setNote = (id: string, note: string) => {
    onChange(value.map(r => (r.requirement_id === id && r.origin === 'direct' ? { ...r, note } : r)));
  };

  return (
    <div className="req-ref-picker" ref={rootRef}>
      <div className="req-ref-search-row">
        <input
          className="form-input"
          value={query}
          placeholder={t('requirements.references.searchPlaceholder')}
          onChange={e => { setQuery(e.target.value); setOpen(true); }}
          onFocus={() => setOpen(true)}
          disabled={disabled || atLimit}
          aria-label={t('requirements.references.label')}
        />
        {atLimit && (
          <small className="req-ref-limit-hint">
            {t('requirements.references.limitReached', { n: MAX_REFERENCE_COUNT })}
          </small>
        )}
      </div>

      {open && !disabled && !atLimit && (
        <div className="req-ref-candidates" role="listbox">
          {loading && <div className="req-ref-candidate-empty">{t('requirements.references.loading')}</div>}
          {!loading && visible.length === 0 && (
            <div className="req-ref-candidate-empty">
              {query.trim() ? t('requirements.references.noMatch') : t('requirements.references.empty')}
            </div>
          )}
          {visible.map(c => (
            <button
              key={c.id}
              type="button"
              className="req-ref-candidate"
              onClick={() => add(c)}
              role="option"
              aria-selected={false}
            >
              <span className="req-ref-candidate-title">{c.title || c.id}</span>
              <span className="req-ref-candidate-meta">
                <span className="req-ref-mini-chip">
                  {tLabel(t, kindLabelKeys as Record<string, string>, c.kind || 'requirement')}
                </span>
                <span className="req-ref-mini-chip">
                  {tLabel(t, statusChipLabelKeys, c.status)}
                </span>
                {projectNames[c.project_id] && (
                  <span className="req-ref-mini-chip req-ref-mini-chip--project">
                    {projectNames[c.project_id]}
                  </span>
                )}
                {c.design_docs && c.design_docs !== '[]' && (
                  <span className="req-ref-mini-chip req-ref-mini-chip--has-design">
                    {t('requirements.references.hasDesign')}
                  </span>
                )}
              </span>
              <span className="req-ref-candidate-add">{t('requirements.references.add')}</span>
            </button>
          ))}
          {hidden > 0 && (
            <div className="req-ref-more">{t('requirements.references.moreResults', { n: hidden })}</div>
          )}
        </div>
      )}

      {value.length > 0 && (
        <div className="req-ref-selected">
          <div className="req-ref-selected-count">
            {t('requirements.references.count', { n: directCount, max: MAX_REFERENCE_COUNT })}
          </div>
          {value.map(r => {
            const inherited = r.origin === 'parent';
            return (
              <div key={`${r.requirement_id}-${r.origin}`} className={`req-ref-chip${inherited ? ' req-ref-chip--inherited' : ''}`}>
                <div className="req-ref-chip-head">
                  <span className="req-ref-chip-title" title={r.title || r.requirement_id}>
                    {r.title || r.requirement_id}
                  </span>
                  {inherited && (
                    <span className="req-ref-chip-origin">{t('requirements.references.originParent')}</span>
                  )}
                  {!inherited && (
                    <button
                      type="button"
                      className="req-ref-chip-remove"
                      onClick={() => remove(r.requirement_id)}
                      disabled={disabled}
                      title={t('requirements.references.remove')}
                      aria-label={t('requirements.references.remove')}
                    >
                      ✕
                    </button>
                  )}
                </div>
                {!inherited && (
                  <input
                    className="form-input req-ref-note-input"
                    value={r.note}
                    placeholder={t('requirements.references.notePlaceholder')}
                    onChange={e => setNote(r.requirement_id, e.target.value)}
                    disabled={disabled}
                    aria-label={t('requirements.references.noteLabel')}
                  />
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

export default RequirementReferencePicker;
