// SchedulesPage (/schedules) — every scheduled_tasks row across projects.
//
// Status / type filters; the action column offers cancel / delete / view log
// (for succeeded/failed rows with a job_id — and for running rows too,
// because the backend flips the row to its terminal state as soon as the
// wizard writes job_logs, so the "running + has log" window is short).
//
// 15s auto-refresh, only while the tab is visible.
//
// Visual: each row has a 4px status rail on the left colour-coding its
// state, so a long list is scannable without reading the badges. Filters are
// horizontal pill rows matching the kind-chip vocabulary.

import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import {
  schedulesApi,
  wizardApi,
  type ScheduledTask,
  type ScheduledTaskStatus,
  type ScheduledTaskType,
  type RunJob,
} from '../api/client';
import { useTranslation } from 'react-i18next';
import i18next from 'i18next';
import { fmtDateTime, fmtRelative } from '../utils/intl';
import { errorMessage } from '../utils/errMsg';
import { IconClock, IconClose, IconAlert, IconHourglass, IconRocket } from '../components/icons';
import './SchedulesPage.css';

// Filter options + row labels hold translation KEYS, resolved during render.
const STATUS_OPTIONS: { value: ScheduledTaskStatus; labelKey: string }[] = [
  { value: 'pending', labelKey: 'schedules.status.pending' },
  { value: 'running', labelKey: 'schedules.status.running' },
  { value: 'succeeded', labelKey: 'schedules.status.succeeded' },
  { value: 'failed', labelKey: 'schedules.status.failed' },
  { value: 'canceled', labelKey: 'schedules.status.canceled' },
];

const TYPE_OPTIONS: { value: ScheduledTaskType; labelKey: string }[] = [
  { value: 'design', labelKey: 'schedules.type.design' },
  { value: 'coding', labelKey: 'schedules.type.coding' },
];

const statusLabelKeys: Record<ScheduledTaskStatus, string> = {
  pending: 'schedules.status.pending',
  running: 'schedules.status.running',
  succeeded: 'schedules.status.succeeded',
  failed: 'schedules.status.failed',
  canceled: 'schedules.status.canceled',
};
const typeLabelKeys: Record<ScheduledTaskType, string> = {
  design: 'schedules.type.design',
  coding: 'schedules.type.coding',
};

// Compact "in 3 h / 3 d ago" formatter for the run_at column — picks the two
// most significant units so a year-old log still fits its tight cell.
// Phrasing comes from the time.* keys, read at call time so a language
// switch updates it on the next render.
function relativeRunAt(iso: string, nowMs: number): string {
  const diff = new Date(iso).getTime() - nowMs;
  const abs = Math.abs(diff);
  const min = 60_000;
  const hour = 60 * min;
  const day = 24 * hour;
  const tt = i18next.t as unknown as (k: string, o?: Record<string, unknown>) => string;
  if (abs < min) return diff >= 0 ? tt('time.soon') : tt('time.justNow');
  if (abs < hour) {
    const m = Math.round(abs / min);
    return diff >= 0 ? tt('time.inMinutes', { n: m }) : tt('time.minutesAgo', { n: m });
  }
  if (abs < day) {
    const h = Math.round(abs / hour);
    return diff >= 0 ? tt('time.inHours', { n: h }) : tt('time.hoursAgo', { n: h });
  }
  const d = Math.round(abs / day);
  if (d < 30) return diff >= 0 ? tt('time.inDays', { n: d }) : tt('time.daysAgo', { n: d });
  const months = Math.round(d / 30);
  return diff >= 0 ? tt('time.inMonths', { n: months }) : tt('time.monthsAgo', { n: months });
}

export default function SchedulesPage() {
  const { t } = useTranslation();
  const [rows, setRows] = useState<ScheduledTask[]>([]);
  const [loading, setLoading] = useState(false);
  const [statusFilter, setStatusFilter] = useState<'' | ScheduledTaskStatus>('');
  const [typeFilter, setTypeFilter] = useState<'' | ScheduledTaskType>('');
  // Log-popup state. Holds the selected job id; null = closed. The popup
  // fetches the wizard job snapshot once on open.
  const [logJobId, setLogJobId] = useState<string | null>(null);
  const [logJob, setLogJob] = useState<RunJob | null>(null);
  const [logLoading, setLogLoading] = useState(false);
  // Tick used to recompute relative time labels every 30s so the "next
  // fire" countdown stays accurate without a full list refresh.
  const [, setTick] = useState(0);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const params: { status?: string; task_type?: string } = {};
      if (statusFilter) params.status = statusFilter;
      if (typeFilter) params.task_type = typeFilter;
      const list = await schedulesApi.list(params);
      setRows(list ?? []);
    } catch {
      setRows([]);
    } finally {
      setLoading(false);
    }
  }, [statusFilter, typeFilter]);

  useEffect(() => {
    load();
  }, [load]);

  // 15s background refresh — only when the tab is visible so backgrounded
  // pages don't fire GETs that the user can't see.
  useEffect(() => {
    const id = window.setInterval(() => {
      if (document.visibilityState === 'visible') load();
    }, 15_000);
    return () => window.clearInterval(id);
  }, [load]);

  // Re-render every 30s so the relative "in X h" labels stay fresh
  // (we don't refetch, just bump a counter).
  useEffect(() => {
    const id = window.setInterval(() => setTick(t => t + 1), 30_000);
    return () => window.clearInterval(id);
  }, []);

  // Counts per status across the unfiltered list — used to badge the
  // status pills. Computing client-side means the user gets immediate
  // feedback on "how many of each" without round-tripping the filter
  // state through the server.
  const counts = useMemo(() => {
    const c: Record<string, number> = { all: rows.length };
    for (const s of STATUS_OPTIONS) c[s.value] = 0;
    for (const r of rows) c[r.status] = (c[r.status] ?? 0) + 1;
    return c;
  }, [rows]);

  // Sort: pending tasks by run_at ASC (soonest first), then everything else
  // by run_at DESC (most recent first). The split keeps the live-action
  // items at the top of the page and the audit trail below.
  const sorted = useMemo(() => {
    const pending = rows
      .filter(r => r.status === 'pending' || r.status === 'running')
      .sort((a, b) => new Date(a.run_at).getTime() - new Date(b.run_at).getTime());
    const rest = rows
      .filter(r => r.status !== 'pending' && r.status !== 'running')
      .sort((a, b) => new Date(b.run_at).getTime() - new Date(a.run_at).getTime());
    return [...pending, ...rest];
  }, [rows]);

  // "Next fire" countdown for the header chip — null when there's no
  // pending task in scope. Shows "overdue" (red) when the soonest
  // pending task is already past.
  const now = Date.now();
  const nextPending = rows
    .filter(r => r.status === 'pending')
    .map(r => new Date(r.run_at).getTime())
    .sort((a, b) => a - b)[0];
  const nextLabel = nextPending != null ? relativeRunAt(new Date(nextPending).toISOString(), now) : null;
  const isOverdue = nextPending != null && nextPending < now;

  const handleCancel = async (id: string) => {
    if (!confirm(t('schedules.cancelConfirm'))) return;
    try {
      await schedulesApi.cancel(id);
    } catch (err) {
      // Mirror handleDelete's error surfacing — silent failure on cancel
      // looks identical to a stale row and confuses the user about whether
      // the button worked.
      alert(t('schedules.cancelFailed', { msg: errorMessage(err) }));
    }
    load();
  };
  const handleDelete = async (id: string) => {
    if (!confirm(t('schedules.deleteConfirm'))) return;
    try {
      await schedulesApi.remove(id);
    } catch (err) {
      // Surface the failure so the user knows the row is still on disk —
      // otherwise they click delete, see no visible change, and assume a bug.
      alert(t('schedules.deleteFailed', { msg: errorMessage(err) }));
    }
    load();
  };
  const handleOpenLog = async (jobId: string) => {
    setLogJobId(jobId);
    setLogJob(null);
    setLogLoading(true);
    try {
      const j = await wizardApi.getJob(jobId);
      setLogJob(j);
    } catch {
      setLogJob(null);
    } finally {
      setLogLoading(false);
    }
  };

  return (
    <div className="schedules-page">
      <div className="schedules-header">
        <h1 className="schedules-header-title">
          <span className="schedules-header-title-icon" aria-hidden="true">
            <IconClock size={16} />
          </span>
          {t('schedules.title')}
          <span className="schedules-header-meta">
            {rows.length === 0
              ? t('schedules.metaNone')
              : t('schedules.metaCount', { n: rows.length })}
          </span>
        </h1>
        {nextLabel != null && (
          <span className={`schedules-next-fire${isOverdue ? ' overdue' : ''}`}>
            <IconHourglass size={12} />
            <span className="schedules-next-fire-label">{t('schedules.nextFire')}</span>
            <span className="schedules-next-fire-value">
              {isOverdue ? `${nextLabel} · ${t("schedules.overdue")}` : nextLabel}
            </span>
          </span>
        )}
      </div>

      {/* Filter pills — segmented status row + a smaller type chip row.
          Both inline counts so the user sees "what would change" before
          clicking. Matches the .req-kind-chip vocabulary. */}
      <div className="schedules-filter-row">
        <div className="schedules-filter-group">
          <span className="schedules-filter-label">{t('schedules.filterStatus')}</span>
          <button
            type="button"
            className={`schedules-pill${statusFilter === '' ? ' active' : ''}`}
            onClick={() => setStatusFilter('')}
          >
            {t('schedules.all')}
            <span className="schedules-pill-count">{counts.all}</span>
          </button>
          {STATUS_OPTIONS.map(o => (
            <button
              key={o.value}
              type="button"
              className={`schedules-pill schedules-pill-${o.value}${statusFilter === o.value ? ' active' : ''}`}
              onClick={() => setStatusFilter(statusFilter === o.value ? '' : o.value)}
            >
              {t(o.labelKey)}
              <span className="schedules-pill-count">{counts[o.value] ?? 0}</span>
            </button>
          ))}
        </div>
        <div className="schedules-filter-group">
          <span className="schedules-filter-label">{t('schedules.filterType')}</span>
          <button
            type="button"
            className={`schedules-type-chip${typeFilter === '' ? ' active' : ''}`}
            onClick={() => setTypeFilter('')}
          >
            {t('schedules.all')}
          </button>
          {TYPE_OPTIONS.map(o => (
            <button
              key={o.value}
              type="button"
              className={`schedules-type-chip schedules-type-${o.value}${typeFilter === o.value ? ' active' : ''}`}
              onClick={() => setTypeFilter(typeFilter === o.value ? '' : o.value)}
            >
              {t(o.labelKey)}
            </button>
          ))}
        </div>
      </div>

      {/* List */}
      {loading && rows.length === 0 ? (
        <div className="schedules-loading">
          <IconHourglass size={14} /> {t('schedules.loading')}
        </div>
      ) : sorted.length === 0 ? (
        <div className="schedules-empty">
          <span className="schedules-empty-mark">
            <IconClock size={28} />
          </span>
          <div className="schedules-empty-title">{t('schedules.emptyTitle')}</div>
          <p className="schedules-empty-desc">
            {t('schedules.emptyDesc')}
          </p>
        </div>
      ) : (
        <div className="schedules-list">
          {sorted.map(t => (
            <ScheduleRow
              key={t.id}
              t={t}
              now={now}
              onCancel={() => handleCancel(t.id)}
              onDelete={() => handleDelete(t.id)}
              onOpenLog={() => handleOpenLog(t.job_id)}
            />
          ))}
        </div>
      )}

      {logJobId && (
        <ScheduleLogModal
          jobId={logJobId}
          job={logJob}
          loading={logLoading}
          onClose={() => {
            setLogJobId(null);
            setLogJob(null);
          }}
        />
      )}
    </div>
  );
}

function ScheduleRow({
  t,
  now,
  onCancel,
  onDelete,
  onOpenLog,
}: {
  t: ScheduledTask;
  now: number;
  onCancel: () => void;
  onDelete: () => void;
  onOpenLog: () => void;
}) {
  // The row prop is also called `t` (the ScheduledTask), so the translate
  // function is aliased `tr` here — `tk` is the loose variant for keys that
  // come out of the label-key maps.
  const { t: trStrict } = useTranslation();
  const tr = trStrict as unknown as (k: string, o?: Record<string, unknown>) => string;
  const runAtMs = new Date(t.run_at).getTime();
  const isPast = runAtMs < now;
  const showOverdue = t.status === 'pending' && isPast;
  const canLog = (t.status === 'succeeded' || t.status === 'failed' || t.status === 'running') && !!t.job_id;
  const rowClass = `schedules-row schedules-row-${t.status}${showOverdue ? ' schedules-row-overdue' : ''}`;
  const isDesign = t.task_type === 'design';
  return (
    <div className={rowClass}>
      <div className="schedules-row-main">
        <div className="schedules-row-top">
          <span
            className={`schedules-type-badge type-${t.task_type}`}
            title={isDesign ? tr('schedules.typeDesignTitle') : tr('schedules.typeCodingTitle')}
          >
            {isDesign ? '📐' : <IconRocket size={11} />}
            {tr(typeLabelKeys[t.task_type])}
          </span>
          <span className="schedules-row-title">
            {t.requirement_title ? (
              <Link to={`/requirements/${t.requirement_id}`}>{t.requirement_title}</Link>
            ) : (
              <span className="schedules-row-title-missing">
                {t.requirement_id}
              </span>
            )}
          </span>
        </div>
        <div className="schedules-row-meta">
          <span
            className={`schedules-meta-item${showOverdue ? ' schedules-meta-overdue' : ''}`}
            title={fmtDateTime(t.run_at)}
          >
            <span className="schedules-meta-icon">
              <IconClock size={12} />
            </span>
            {fmtDateTime(t.run_at)}
            <span style={{ color: 'var(--color-text-muted)' }}>·</span>
            <span>{relativeRunAt(t.run_at, now)}</span>
            {showOverdue && <span>· {tr('schedules.overdue')}</span>}
          </span>
          <span className="schedules-meta-item" title={tr('schedules.modelTitle')}>
            <span className="schedules-meta-model">{t.model || tr('schedules.defaultModel')}</span>
          </span>
          <span className="schedules-meta-item" title={tr('schedules.createdTitle')}>
            {tr('schedules.createdPrefix')} {fmtRelative(t.created_at)}
          </span>
        </div>
        {t.error_message && (
          <div className="schedules-error-message">
            <IconAlert size={12} /> {t.error_message}
          </div>
        )}
      </div>
      <div className="schedules-row-side">
        <span className={`status-badge status-${t.status}`}>
          {tr(statusLabelKeys[t.status])}
        </span>
        <div className="schedules-row-actions">
          {t.status === 'pending' && (
            <button className="btn btn-sm" onClick={onCancel}>{tr('schedules.cancel')}</button>
          )}
          {canLog && (
            <button className="btn btn-sm" onClick={onOpenLog}>{tr('schedules.viewLog')}</button>
          )}
          <button
            className="btn btn-sm schedules-btn-danger"
            onClick={onDelete}
            title={tr('schedules.deleteTitle')}
          >
            {tr('schedules.delete')}
          </button>
        </div>
      </div>
    </div>
  );
}

// ScheduleLogModal renders the wizard job's full LogLine array in a
// read-only popup so the user can see exactly what happened during a
// scheduled run. Mirrors the existing /wizard/jobs/{id} GET response
// shape; the wizard already persists job_logs on finish (added in this
// same feature), so even after the JobStore ring buffer evicts the job
// the snapshot stays.
function ScheduleLogModal({
  jobId,
  job,
  loading,
  onClose,
}: {
  jobId: string;
  job: RunJob | null;
  loading: boolean;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const tk = t as unknown as (k: string, o?: Record<string, unknown>) => string;
  const lines = job?.log ?? [];
  return (
    <div className="modal-overlay" onClick={onClose}>
      <div
        className="modal-box schedules-log-modal"
        onClick={e => e.stopPropagation()}
      >
        <div className="modal-header">
          <h3>{t('schedules.log.title')}</h3>
          <button
            className="btn btn-sm"
            onClick={onClose}
            aria-label={t('schedules.log.close')}
            title={t('schedules.log.close')}
          >
            <IconClose size={14} />
          </button>
        </div>
        <div className="modal-body">
          {loading ? (
            <div className="schedules-loading">
              <IconHourglass size={14} /> {t('schedules.loading')}
            </div>
          ) : !job ? (
            <div className="schedules-empty">
              <span className="schedules-empty-mark">
                <IconAlert size={26} />
              </span>
              <div className="schedules-empty-title">{t('schedules.log.loadFailedTitle')}</div>
              <p className="schedules-empty-desc">
                {t('schedules.log.loadFailedDesc')}
              </p>
            </div>
          ) : (
            <>
              <div className="schedules-log-meta">
                <span className="schedules-log-meta-item">
                  <span className="schedules-log-meta-label">Job</span>
                  <code className="schedules-log-meta-id">{jobId}</code>
                </span>
                <span className="schedules-log-meta-spacer" />
                <span className="schedules-log-meta-item">
                  {t('schedules.log.statusLabel')}: <strong>{tk(statusLabelKeys[(job.status || 'pending') as ScheduledTaskStatus] || job.status)}</strong>
                </span>
                {job.model && (
                  <span className="schedules-log-meta-item">
                    {t('schedules.log.modelLabel')}: <strong>{job.model}</strong>
                  </span>
                )}
                <span className={`status-badge status-${(job.status || 'pending') as ScheduledTaskStatus}`}>
                  {tk(statusLabelKeys[(job.status || 'pending') as ScheduledTaskStatus] || job.status)}
                </span>
              </div>
              <div className="schedules-log-body">
                {lines.length === 0 ? (
                  <div className="schedules-log-empty">{t('schedules.log.empty')}</div>
                ) : (
                  lines.map((line, i) => (
                    <div key={i} className={`schedules-log-line schedules-log-${line.type}`}>
                      <span className="schedules-log-type">{line.type}</span>
                      <span className="schedules-log-content">{line.content}</span>
                    </div>
                  ))
                )}
              </div>
              {job.status === 'running' && (
                <div className="schedules-log-running-hint">
                  <span className="schedules-log-running-dot" />
                  {t('schedules.log.runningHint')}
                </div>
              )}
            </>
          )}
        </div>
        <div className="modal-actions">
          <button className="btn" onClick={onClose}>{t('schedules.log.close')}</button>
        </div>
      </div>
    </div>
  );
}
