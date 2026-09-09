// 定时任务列表（/schedules）
//
// 跨需求查看所有 scheduled_tasks 行：状态 / 类型筛选；操作列提供
// 取消 / 删除 / 查看日志（succeeded/failed 且有 job_id 时）。运行中行
// （status=running）的日志入口也在，因为 wizard exec body 写完
// job_logs 后服务端就立刻把 scheduled_tasks 翻到终态——所以"running + 有日志"
// 的窗口很短，UI 仍渲染入口以防后端延迟落库。
//
// 15s 自动轮询，仅在标签可见时进行（document.visibilityState==='visible'）。
//
// 视觉：每个任务卡片左侧有一条 4px 的"状态轨"（status rail），颜色编码
// 当前状态——这是与卡片网格区分的关键签名，让用户在长列表里不读徽章
// 也能扫到哪些任务在运行 / 失败。过滤器从裸下拉框改成横向药丸行
// （schedules-pill），匹配 kind-chip 的视觉语言。

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
import { relativeTime, formatDateTime } from '../utils/time';
import { IconClock, IconClose, IconAlert, IconHourglass, IconRocket } from '../components/icons';
import './SchedulesPage.css';

const STATUS_OPTIONS: { value: ScheduledTaskStatus; label: string }[] = [
  { value: 'pending', label: '待执行' },
  { value: 'running', label: '执行中' },
  { value: 'succeeded', label: '成功' },
  { value: 'failed', label: '失败' },
  { value: 'canceled', label: '已取消' },
];

const TYPE_OPTIONS: { value: ScheduledTaskType; label: string }[] = [
  { value: 'design', label: '方案' },
  { value: 'coding', label: '开发' },
];

const statusLabels: Record<ScheduledTaskStatus, string> = {
  pending: '待执行',
  running: '执行中',
  succeeded: '成功',
  failed: '失败',
  canceled: '已取消',
};
const typeLabels: Record<ScheduledTaskType, string> = {
  design: '方案',
  coding: '开发',
};

// Compact "xx 小时后 / 3 天前" formatter for the run_at column. Picks the
// two most-significant units so a 1y-old log reads as "11 个月前" instead
// of "11 个月 13 天 4 小时...". Locale-agnostic on purpose — these strings
// end up in a tight cell and a long humanized form blows the column width.
function relativeRunAt(iso: string, nowMs: number): string {
  const diff = new Date(iso).getTime() - nowMs;
  const abs = Math.abs(diff);
  const min = 60_000;
  const hour = 60 * min;
  const day = 24 * hour;
  if (abs < min) return diff >= 0 ? '即将' : '刚刚';
  if (abs < hour) {
    const m = Math.round(abs / min);
    return diff >= 0 ? `${m} 分钟后` : `${m} 分钟前`;
  }
  if (abs < day) {
    const h = Math.round(abs / hour);
    return diff >= 0 ? `${h} 小时后` : `${h} 小时前`;
  }
  const d = Math.round(abs / day);
  if (d < 30) return diff >= 0 ? `${d} 天后` : `${d} 天前`;
  const months = Math.round(d / 30);
  return diff >= 0 ? `${months} 个月后` : `${months} 个月前`;
}

export default function SchedulesPage() {
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

  // Re-render every 30s so the relative "X 小时后" labels stay fresh
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
    if (!confirm('确认取消该定时任务？')) return;
    try {
      await schedulesApi.cancel(id);
    } catch (err) {
      // Mirror handleDelete's error surfacing — silent failure on cancel
      // looks identical to a stale row and confuses the user about whether
      // the button worked.
      const msg = err instanceof Error ? err.message : String(err);
      alert(`取消失败：${msg}`);
    }
    load();
  };
  const handleDelete = async (id: string) => {
    if (!confirm('确认删除该定时任务？')) return;
    try {
      await schedulesApi.remove(id);
    } catch (err) {
      // Surface the failure so the user knows the row is still on disk —
      // otherwise they click 删除, see no visible change, and assume a
      // bug. The message comes pre-formatted "<code>: <msg>" from the
      // request wrapper, which is enough to point at the cause.
      const msg = err instanceof Error ? err.message : String(err);
      alert(`删除失败：${msg}`);
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
          定时任务
          <span className="schedules-header-meta">
            {rows.length === 0
              ? '· 暂无任务'
              : `· ${rows.length} 条`}
          </span>
        </h1>
        {nextLabel != null && (
          <span className={`schedules-next-fire${isOverdue ? ' overdue' : ''}`}>
            <IconHourglass size={12} />
            <span className="schedules-next-fire-label">下一次执行</span>
            <span className="schedules-next-fire-value">
              {isOverdue ? `${nextLabel} · 已过期` : nextLabel}
            </span>
          </span>
        )}
      </div>

      {/* Filter pills — segmented status row + a smaller type chip row.
          Both inline counts so the user sees "what would change" before
          clicking. Matches the .req-kind-chip vocabulary. */}
      <div className="schedules-filter-row">
        <div className="schedules-filter-group">
          <span className="schedules-filter-label">状态</span>
          <button
            type="button"
            className={`schedules-pill${statusFilter === '' ? ' active' : ''}`}
            onClick={() => setStatusFilter('')}
          >
            全部
            <span className="schedules-pill-count">{counts.all}</span>
          </button>
          {STATUS_OPTIONS.map(o => (
            <button
              key={o.value}
              type="button"
              className={`schedules-pill schedules-pill-${o.value}${statusFilter === o.value ? ' active' : ''}`}
              onClick={() => setStatusFilter(statusFilter === o.value ? '' : o.value)}
            >
              {o.label}
              <span className="schedules-pill-count">{counts[o.value] ?? 0}</span>
            </button>
          ))}
        </div>
        <div className="schedules-filter-group">
          <span className="schedules-filter-label">类型</span>
          <button
            type="button"
            className={`schedules-type-chip${typeFilter === '' ? ' active' : ''}`}
            onClick={() => setTypeFilter('')}
          >
            全部
          </button>
          {TYPE_OPTIONS.map(o => (
            <button
              key={o.value}
              type="button"
              className={`schedules-type-chip schedules-type-${o.value}${typeFilter === o.value ? ' active' : ''}`}
              onClick={() => setTypeFilter(typeFilter === o.value ? '' : o.value)}
            >
              {o.label}
            </button>
          ))}
        </div>
      </div>

      {/* List */}
      {loading && rows.length === 0 ? (
        <div className="schedules-loading">
          <IconHourglass size={14} /> 加载中...
        </div>
      ) : sorted.length === 0 ? (
        <div className="schedules-empty">
          <span className="schedules-empty-mark">
            <IconClock size={28} />
          </span>
          <div className="schedules-empty-title">暂无定时任务</div>
          <p className="schedules-empty-desc">
            可在需求详情页为「方案」或「开发」设置定时执行；任务开启后这里会按计划时间排序显示。
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
            title={isDesign ? '定时方案生成' : '定时开发'}
          >
            {isDesign ? '📐' : <IconRocket size={11} />}
            {typeLabels[t.task_type]}
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
            title={formatDateTime(t.run_at)}
          >
            <span className="schedules-meta-icon">
              <IconClock size={12} />
            </span>
            {formatDateTime(t.run_at)}
            <span style={{ color: 'var(--color-text-muted)' }}>·</span>
            <span>{relativeRunAt(t.run_at, now)}</span>
            {showOverdue && <span>· 已过期</span>}
          </span>
          <span className="schedules-meta-item" title="模型">
            <span className="schedules-meta-model">{t.model || '默认模型'}</span>
          </span>
          <span className="schedules-meta-item" title="创建时间">
            创建于 {relativeTime(t.created_at)}
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
          {statusLabels[t.status]}
        </span>
        <div className="schedules-row-actions">
          {t.status === 'pending' && (
            <button className="btn btn-sm" onClick={onCancel}>取消</button>
          )}
          {canLog && (
            <button className="btn btn-sm" onClick={onOpenLog}>查看日志</button>
          )}
          <button
            className="btn btn-sm schedules-btn-danger"
            onClick={onDelete}
            title="删除该定时任务"
          >
            删除
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
  const lines = job?.log ?? [];
  return (
    <div className="modal-overlay" onClick={onClose}>
      <div
        className="modal-box schedules-log-modal"
        onClick={e => e.stopPropagation()}
      >
        <div className="modal-header">
          <h3>📜 执行日志</h3>
          <button
            className="btn btn-sm"
            onClick={onClose}
            aria-label="关闭"
            title="关闭"
          >
            <IconClose size={14} />
          </button>
        </div>
        <div className="modal-body">
          {loading ? (
            <div className="schedules-loading">
              <IconHourglass size={14} /> 加载中...
            </div>
          ) : !job ? (
            <div className="schedules-empty">
              <span className="schedules-empty-mark">
                <IconAlert size={26} />
              </span>
              <div className="schedules-empty-title">未能加载日志</div>
              <p className="schedules-empty-desc">
                任务可能仍在执行且未落库，稍后重试。
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
                  状态: <strong>{statusLabels[job.status as ScheduledTaskStatus] || job.status}</strong>
                </span>
                {job.model && (
                  <span className="schedules-log-meta-item">
                    模型: <strong>{job.model}</strong>
                  </span>
                )}
                <span className={`status-badge status-${(job.status || 'pending') as ScheduledTaskStatus}`}>
                  {statusLabels[(job.status || 'pending') as ScheduledTaskStatus] || job.status}
                </span>
              </div>
              <div className="schedules-log-body">
                {lines.length === 0 ? (
                  <div className="schedules-log-empty">（暂无日志）</div>
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
                  任务仍在执行，日志将持续追加。
                </div>
              )}
            </>
          )}
        </div>
        <div className="modal-actions">
          <button className="btn" onClick={onClose}>关闭</button>
        </div>
      </div>
    </div>
  );
}
