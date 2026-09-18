// RequirementsList — the cross-project requirements list.
//
// The /requirements route used to point at a placeholder page with no way to
// create anything; this is the real list:
//   1. project / status selects + a search box at the top
//   2. desktop renders a table (kind / title / status / project / priority /
//      updated)
//   3. mobile renders a card list (a narrow table turns to mush)
//   4. "New requirement" opens the cross-project CreateRequirementForm
// Kind filter is multi-select; the backend List accepts a comma-separated
// kind list, so the toggles are joined into CSV.

import { useEffect, useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import {
  requirementsApi, projectsApi,
  statusLabelKeys, kindLabelKeys, kindShortLabelKeys, priorityLabelKeys, kindOf,
  MARK_PRESETS, parseMarks,
  type Kind, type Project, type Requirement,
} from '../api/client';
import { tLabel } from '../i18n/label';
import { CreateRequirementForm } from '../components/CreateRequirementForm/CreateRequirementForm';
import { DevSourceBadge } from '../components/DevSourceBadge';
import { StatusChips } from '../components/StatusChips';
import { fmtRelative, fmtDateTime, fmtRunAtRelative } from '../utils/intl';
import { IconClock } from '../components/icons';
import './RequirementsList.css';

// KIND_FILTERS holds translation KEYS for the chip labels (resolved on
// render via tLabel) — a literal would freeze the chip in whatever
// language was active at import time.
const KIND_FILTERS: { value: Kind; labelKey: string; emoji: string }[] = [
  { value: 'issue', labelKey: 'requirements.list.kindFilters.issue', emoji: '🐛' },
  { value: 'requirement', labelKey: 'requirements.list.kindFilters.requirement', emoji: '📋' },
  { value: 'idea', labelKey: 'requirements.list.kindFilters.idea', emoji: '💡' },
];

// MarksStrip renders the preset mark chip strip (重要 / 跟进 / 阻塞 / 风险)
// INLINE with the requirement title. Empty marks → nothing rendered. Used by
// both the desktop table row and the mobile card so the inline layout stays
// in sync across the two surfaces. Returns null when there are no marks so
// unmarked rows don't pick up extra spacing.
function MarksStrip({ marks }: { marks: string | undefined }) {
  const { t } = useTranslation();
  const rowMarks = parseMarks(marks);
  if (rowMarks.length === 0) return null;
  return (
    <div className="req-row-marks">
      {rowMarks.map(code => {
        const p = MARK_PRESETS.find(x => x.code === code);
        if (!p) return null;
        const label = t(`requirements.detail2.marksPreset.${p.code}`);
        return (
          <span
            key={code}
            className={`req-row-mark-tag ${p.code}`}
            style={{ color: p.color, background: p.bg, borderColor: p.color }}
            title={label}
          >
            <span aria-hidden>{p.icon}</span>
            <span>{label}</span>
          </span>
        );
      })}
    </div>
  );
}

export default function RequirementsList() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [projects, setProjects] = useState<Project[]>([]);
  const [requirements, setRequirements] = useState<Requirement[]>([]);
  const [loading, setLoading] = useState(false);

  // Filters
  const [projectFilter, setProjectFilter] = useState('');
  const [activeKinds, setActiveKinds] = useState<Set<Kind>>(
    new Set<Kind>(['issue', 'requirement', 'idea']),
  );
  const [statusFilter, setStatusFilter] = useState('');
  const [search, setSearch] = useState('');

  const [showCreate, setShowCreate] = useState(false);

  useEffect(() => {
    projectsApi.list().then(setProjects).catch(() => {});
  }, []);

  // Re-fetch when filters change. kind filter is comma-separated per backend
  // contract (splitKinds() in the Go service); toggling a chip rewrites it.
  useEffect(() => {
    loadRequirements();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectFilter, statusFilter, search]);

  const kindFilterCsv = useMemo(() => {
    if (activeKinds.size === 0 || activeKinds.size === 3) return '';
    return Array.from(activeKinds).join(',');
  }, [activeKinds]);

  useEffect(() => {
    loadRequirements();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [kindFilterCsv]);

  const loadRequirements = async () => {
    setLoading(true);
    try {
      const list = await requirementsApi.list({
        project_id: projectFilter || undefined,
        status: statusFilter || undefined,
        kind: kindFilterCsv || undefined,
      });
      const term = search.trim().toLowerCase();
      setRequirements(
        term
          ? list.filter(r =>
              (r.title || '').toLowerCase().includes(term) ||
              (r.description || '').toLowerCase().includes(term),
            )
          : list,
      );
    } catch (err: any) {
      console.error('load requirements failed', err);
      setRequirements([]);
    } finally {
      setLoading(false);
    }
  };

  const toggleKind = (k: Kind) => {
    setActiveKinds(prev => {
      const next = new Set(prev);
      if (next.has(k)) next.delete(k);
      else next.add(k);
      return next;
    });
  };

  const projectNameOf = (pid: string) =>
    projects.find(p => p.id === pid)?.name || pid;

  // The desktop "new requirement" button and the mobile FAB trigger the same
  // handler — one lives in the page header (desktop), one floats above
  // the tab bar (mobile). The desktop-only / fab rules in the CSS swap
  // their visibility at the 768px breakpoint so each surface is only
  // shown in its lane.
  const openCreate = () => setShowCreate(s => !s);

  // Inline helper for the "has a pending scheduled task" affordance. Renders
  // nothing when the row has no scheduled_run_at (omitted on the wire); the
  // hover title combines the absolute time with the same relative countdown
  // string SchedulesPage uses, so both surfaces speak the same vocabulary.
  // Wrapped in a small indigo chip (see .schedule-chip in index.css) — the
  // bare 13px outline icon was easy to miss next to the title text, the
  // tinted pill makes the "has a schedule" affordance unmissable in dense
  // cross-project lists.
  const scheduleClock = (r: Requirement) => {
    if (!r.scheduled_run_at) return null;
    return (
      <span
        className="schedule-chip"
        title={t('requirements.list.scheduledTooltip', {
          time: fmtDateTime(r.scheduled_run_at),
          rel: fmtRunAtRelative(r.scheduled_run_at),
        })}
        aria-label={t('requirements.list.scheduledAria')}
      >
        <IconClock size={12} className="schedule-chip-icon" />
        {fmtRunAtRelative(r.scheduled_run_at)}
      </span>
    );
  };

  return (
    <div className="requirements-list-page">
      <div className="page-header">
        <h2>{t('requirements.list.title')}</h2>
        <div className="page-header-actions desktop-only">
          <button
            className="btn"
            onClick={() => navigate('/requirements/calendar')}
          >
            📅 {t('requirements.list.calendar', '日历')}
          </button>
          <button
            className="btn btn-primary"
            onClick={openCreate}
          >
            {showCreate ? t('requirements.list.collapse') : t('requirements.list.create')}
          </button>
        </div>
        {/* 移动端：日历入口放在主操作旁 */}
        <button
          className="btn mobile-only"
          style={{ marginLeft: 'auto' }}
          onClick={() => navigate('/requirements/calendar')}
          aria-label={t('requirements.list.calendar', '日历')}
        >
          📅
        </button>
      </div>

      {showCreate && (
        <CreateRequirementForm
          projectOptions={projects.map(p => ({ id: p.id, name: p.name }))}
          onClose={() => setShowCreate(false)}
          onCreated={req => {
            setShowCreate(false);
            // 启动计划 dispatch failed server-side: the requirement exists but
            // nothing is running. Say so before landing on the detail page,
            // which carries the manual entry points.
            if (req.launch_error) {
              alert(t('components.createRequirement.launch.dispatchFailed', { reason: req.launch_error }));
            }
            navigate(`/requirements/${req.id}`);
          }}
        />
      )}

      {/* Filters bar */}
      <div className="req-filter-bar">
        <select
          className="form-input req-filter-select"
          value={projectFilter}
          onChange={e => setProjectFilter(e.target.value)}
        >
          <option value="">{t('requirements.list.allProjects')}</option>
          {projects.map(p => (
            <option key={p.id} value={p.id}>{p.name}</option>
          ))}
        </select>

        <select
          className="form-input req-filter-select"
          value={statusFilter}
          onChange={e => setStatusFilter(e.target.value)}
        >
          <option value="">{t('requirements.list.allStatuses')}</option>
          {Object.entries(statusLabelKeys).map(([k]) => (
            <option key={k} value={k}>{tLabel(t, statusLabelKeys, k)}</option>
          ))}
        </select>

        <div className="search-input req-filter-search">
          <span className="search-input-icon" aria-hidden>🔍</span>
          <input
            placeholder={t('requirements.list.searchPlaceholder')}
            value={search}
            onChange={e => setSearch(e.target.value)}
            aria-label={t('requirements.list.searchAriaLabel', '按标题或描述搜索')}
          />
          {search && (
            <button
              type="button"
              className="search-input-clear"
              aria-label={t('requirements.list.clearSearch', '清除搜索')}
              onClick={() => setSearch('')}
            >×</button>
          )}
        </div>
      </div>

      {/* Kind filter chips (multi-select) */}
      <div className="req-kind-chips">
        {KIND_FILTERS.map(k => (
          <button
            key={k.value}
            type="button"
            className={`req-kind-chip kind-${k.value}${activeKinds.has(k.value) ? ' active' : ''}`}
            onClick={() => toggleKind(k.value)}
            title={t(k.labelKey)}
          >
            {k.emoji} {t(k.labelKey)}
          </button>
        ))}
        <span className="req-kind-chip-hint">
          {activeKinds.size === 0
            ? t('requirements.list.chipsNone')
            : activeKinds.size === 3
              ? t('requirements.list.chipsAll')
              : t('requirements.list.chipsSelected', { n: activeKinds.size })}
        </span>
      </div>

      {/* Table (desktop view). `table-cards` is a no-op class on desktop
          (the rules only kick in below 768px); on mobile the table is
          hidden via .req-table { display: none } and the dedicated
          card list below takes over. data-label on each <td> is the
          fallback for environments that fall back to the table-cards
          utility (e.g. width between 720-768px). */}
      <div className="req-table-wrap desktop-only">
        {loading ? (
          <div className="tab-empty"><p>{t('requirements.list.loading')}</p></div>
        ) : requirements.length === 0 ? (
          <div className="tab-empty">
            <p>{t('requirements.list.empty')}</p>
          </div>
        ) : (
          <table className="req-table table-cards">
            <thead>
              <tr>
                <th>{t('requirements.list.colType')}</th>
                <th>{t('requirements.list.colTitle')}</th>
                <th>{t('requirements.list.colStatus')}</th>
                <th>{t('requirements.list.colModel')}</th>
                <th>{t('requirements.list.colProject')}</th>
                <th>{t('requirements.list.colPriority')}</th>
                <th>{t('requirements.list.colAgentServer')}</th>
                <th>{t('requirements.list.colUpdated')}</th>
              </tr>
            </thead>
            <tbody>
              {requirements.map(r => {
                const k: Kind = kindOf(r);
                return (
                  <tr key={r.id} onClick={() => navigate(`/requirements/${r.id}`)} className="req-row">
                    <td data-label={t('requirements.list.colType')}>
                      <span
                        className={`kind-badge kind-${k}`}
                        title={tLabel(t, kindLabelKeys, k)}
                      >
                        {tLabel(t, kindShortLabelKeys, k)}
                      </span>
                    </td>
                    <td className="req-row-title" data-label={t('requirements.list.colTitle')}>
                      <div className="req-row-title-text">
                        {/* Preset marks (important / follow_up / blocked / at_risk)
                            render INLINE with the title (chip strip BEFORE the
                            schedule clock + title text) so marked requirements
                            stay on a single line. Empty list → nothing rendered
                            (so unmarked rows don't get extra spacing). Colors
                            come from MARK_PRESETS in api/client.ts so the
                            detail-page editor stays in sync. */}
                        <MarksStrip marks={r.marks} />
                        {scheduleClock(r)}
                        <span className="req-row-title-text-label">
                          {r.title || <em style={{ color: '#94A3B8' }}>{t('requirements.list.noTitle')}</em>}
                        </span>
                      </div>
                      {/* Mirror the project-page chip strip: tags render
                          under the title as a 3-chip cap with +N overflow.
                          Force-closed rows carry a small amber badge so the
                          cross-project list reads consistently with the
                          in-project tab. */}
                      {(() => {
                        const raw = (r.tags || '').trim();
                        const list: string[] = (!raw || raw === '[]') ? [] : (() => {
                          try {
                            const parsed = JSON.parse(raw);
                            return Array.isArray(parsed) ? parsed.filter((x): x is string => typeof x === 'string') : [];
                          } catch { return []; }
                        })();
                        if (list.length === 0 && !r.closed_at) return null;
                        const visible = list.slice(0, 3);
                        const overflow = list.length - visible.length;
                        return (
                          <div className="req-row-title-tags">
                            {r.closed_at && (
                              <span className="req-tag-chip req-row-title-tag req-row-title-tag-closed" title={r.closed_reason || t('requirements.detail2.closedBadge')}>
                                {t('requirements.detail2.closedBadge')}
                              </span>
                            )}
                            {visible.map(tag => (
                              <span key={tag} className="req-tag-chip req-row-title-tag" title={tag}>{tag}</span>
                            ))}
                            {overflow > 0 && (
                              <span className="req-tag-chip req-row-title-tag req-row-title-tag-more" title={list.slice(3).join(', ')}>+{overflow}</span>
                            )}
                          </div>
                        );
                      })()}
                    </td>
                    <td data-label={t('requirements.list.colStatus')}>
                      <StatusChips req={r} />
                    </td>
                    {/* Model column — the developer-stage model actually
                        dispatched to the Claude CLI (the value pinned in
                        --settings env). Empty for requirements whose coding
                        stage hasn't run yet (or that predate this column);
                        we show a dim em-dash rather than guessing. */}
                    <td data-label={t('requirements.list.colModel')} className="req-row-model">
                      {r.developer_model ? (
                        <span className="req-row-model-tag" title={r.developer_model}>
                          🤖 {r.developer_model}
                        </span>
                      ) : (
                        <span className="req-row-dim">—</span>
                      )}
                    </td>
                    <td data-label={t('requirements.list.colProject')}>{projectNameOf(r.project_id)}</td>
                    <td data-label={t('requirements.list.colPriority')}>
                      {r.priority ? (
                        <span className={`priority-tag priority-${r.priority}`}>
                          {tLabel(t, priorityLabelKeys, r.priority)}
                        </span>
                      ) : (
                        <span className="req-row-dim">-</span>
                      )}
                    </td>
                    {/* Agent-server column: now carries the full DevSourceBadge
                        (local dev / 🛰️ agent server name) so users see where
                        the requirement actually ran. The previous plain
                        🖥️ <name> / local text was redundant with the old
                        dev-env column — merging them removes the
                        duplication. The badge itself falls back to nothing
                        when dev_source is empty (coding never ran). */}
                    <td data-label={t('requirements.list.colAgentServer')} className="req-row-devsource">
                      <DevSourceBadge req={r} compact />
                    </td>
                    <td
                      data-label={t('requirements.list.colUpdated')}
                      className="req-row-updated"
                      title={r.updated_at ? fmtRelative(r.updated_at) : undefined}
                    >
                      {fmtDateTime(r.updated_at)}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>

      {/* Mobile-only card list. Each card surfaces the kind + status as
          prominent badges, the title as the headline, then a compact
          footer with the project, priority and relative time. The whole
          card is a tappable row that navigates to the detail page. */}
      <div className="req-cards-mobile mobile-only">
        {loading ? (
          <div className="mobile-empty">
            <span className="mobile-empty-mark">⏳</span>
            <div className="mobile-empty-title">{t('requirements.list.mobileLoading')}</div>
          </div>
        ) : requirements.length === 0 ? (
          <div className="mobile-empty">
            <span className="mobile-empty-mark">📋</span>
            <div className="mobile-empty-title">{t('requirements.list.mobileEmpty')}</div>
            <p className="mobile-empty-desc">
              {t('requirements.list.mobileEmptyDesc')}
            </p>
          </div>
        ) : (
          requirements.map(r => {
            const k: Kind = kindOf(r);
            const projectName = projectNameOf(r.project_id);
            return (
              <button
                key={r.id}
                type="button"
                className="req-card-mobile"
                onClick={() => navigate(`/requirements/${r.id}`)}
              >
                <div className="req-card-mobile-head">
                  <span className={`kind-badge kind-${k}`} title={tLabel(t, kindLabelKeys, k)}>
                    {tLabel(t, kindShortLabelKeys, k)}
                  </span>
                  <StatusChips req={r} />
                </div>
                {/* Preset marks on mobile: same chip strip as the desktop
                    title column, now rendered INLINE with the title (chip
                    strip → schedule clock → title text) so marked cards
                    stay on a single line. Empty list → nothing rendered
                    (so unmarked cards keep their natural height). Mirrors
                    the design's "mark 比 priority 更显眼 — 因为它直接
                    影响排序" intent. */}
                <div className="req-card-mobile-title">
                  <MarksStrip marks={r.marks} />
                  {scheduleClock(r)}
                  <span className="req-row-title-text-label">
                    {r.title || <em style={{ color: '#94A3B8' }}>{t('requirements.list.noTitle')}</em>}
                  </span>
                </div>
                <div className="req-card-mobile-foot">
                  <span className="req-card-mobile-project" title={projectName}>
                    📁 {projectName}
                  </span>
                  <span className="req-card-mobile-spacer" />
                  {/* Dev-environment chip — mirrors the desktop Agent-server
                      column. DevSourceBadge returns null when the coding
                      stage hasn't run yet, so legacy rows simply render no
                      chip (no local-vs-agent guess). */}
                  <DevSourceBadge req={r} compact />
                  {r.priority && (
                    <span className={`priority-dot priority-${r.priority}`} title={t('requirements.list.priorityTitle', { label: tLabel(t, priorityLabelKeys, r.priority) })} />
                  )}
                  <span className="req-card-mobile-time">{fmtRelative(r.updated_at)}</span>
                </div>
              </button>
            );
          })
        )}
      </div>

      {/* Mobile FAB — mirrors the desktop new-requirement button. The label
          tells users at a glance what the floating button does. On
          desktop it's hidden by .fab's display:none rule. */}
      <button
        type="button"
        className="fab fab-extended"
        aria-label={t('requirements.list.createShort')}
        onClick={openCreate}
      >
        <span>＋</span>
        <span>{t('requirements.list.createShort')}</span>
      </button>
    </div>
  );
}
