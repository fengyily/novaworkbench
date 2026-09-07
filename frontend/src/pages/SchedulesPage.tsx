// 定时任务列表（/schedules）
//
// 跨需求查看所有 scheduled_tasks 行：状态 / 类型 / 计划时间筛选；操作列提供
// 取消 / 删除 / 查看日志（succeeded/failed 且有 job_id 时）。运行中行
// （status=running）的日志入口也在，因为 wizard exec body 写完
// job_logs 后服务端就立刻把 scheduled_tasks 翻到终态——所以"running + 有日志"
// 的窗口很短，UI 仍渲染入口以防后端延迟落库。
//
// 15s 自动轮询，仅在标签可见时进行（document.visibilityState==='visible'）。
// 复用 RequirementsList 的 .req-table.table-cards 桌面/移动响应式样式。

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
import './RequirementsList.css';

const STATUS_OPTIONS: { value: '' | ScheduledTaskStatus; label: string }[] = [
  { value: '', label: '全部状态' },
  { value: 'pending', label: '待执行' },
  { value: 'running', label: '执行中' },
  { value: 'succeeded', label: '成功' },
  { value: 'failed', label: '失败' },
  { value: 'canceled', label: '已取消' },
];

const TYPE_OPTIONS: { value: '' | ScheduledTaskType; label: string }[] = [
  { value: '', label: '全部类型' },
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

  // Sort by run_at ASC so the soonest-to-fire pending rows are at the top;
  // for terminal rows this puts recently-fired jobs near the top of their
  // group.
  const sorted = useMemo(
    () =>
      [...rows].sort(
        (a, b) => new Date(a.run_at).getTime() - new Date(b.run_at).getTime(),
      ),
    [rows],
  );

  const handleCancel = async (id: string) => {
    if (!confirm('确认取消该定时任务？')) return;
    await schedulesApi.cancel(id);
    load();
  };
  const handleDelete = async (id: string) => {
    if (!confirm('确认删除该定时任务？')) return;
    await schedulesApi.remove(id);
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
    <div className="requirements-list-page">
      <div className="page-header">
        <h2>⏰ 定时任务</h2>
      </div>

      {/* Filters bar — same component as RequirementsList so the visual
          treatment matches across pages. */}
      <div className="req-filter-bar">
        <select
          className="form-input req-filter-select"
          value={statusFilter}
          onChange={e => setStatusFilter(e.target.value as any)}
        >
          {STATUS_OPTIONS.map(o => (
            <option key={o.value || 'all'} value={o.value}>{o.label}</option>
          ))}
        </select>
        <select
          className="form-input req-filter-select"
          value={typeFilter}
          onChange={e => setTypeFilter(e.target.value as any)}
        >
          {TYPE_OPTIONS.map(o => (
            <option key={o.value || 'all'} value={o.value}>{o.label}</option>
          ))}
        </select>
      </div>

      {/* Table */}
      <div className="req-table-wrap">
        {loading && rows.length === 0 ? (
          <div className="tab-empty"><p>⏳ 加载中...</p></div>
        ) : sorted.length === 0 ? (
          <div className="tab-empty">
            <p>暂无定时任务。可在需求详情页为技术方案或开发设置定时执行。</p>
          </div>
        ) : (
          <div className="req-table table-cards">
            <div className="req-table-head">
              <div>类型</div>
              <div>需求</div>
              <div>计划时间</div>
              <div>模型</div>
              <div>状态</div>
              <div>创建时间</div>
              <div>操作</div>
            </div>
            {sorted.map(t => (
              <ScheduleRow
                key={t.id}
                t={t}
                onCancel={() => handleCancel(t.id)}
                onDelete={() => handleDelete(t.id)}
                onOpenLog={() => handleOpenLog(t.job_id)}
              />
            ))}
          </div>
        )}
      </div>

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
  onCancel,
  onDelete,
  onOpenLog,
}: {
  t: ScheduledTask;
  onCancel: () => void;
  onDelete: () => void;
  onOpenLog: () => void;
}) {
  const runAt = new Date(t.run_at);
  const isPast = runAt.getTime() < Date.now();
  // pending 且已过期 → 红字「已过期」(虽然后端 maxLateness=24h 兜底回收，
  // 但 24h 内的过期仍然 pending 待派发，UI 提前提示)。
  const showOverdue = t.status === 'pending' && isPast;
  const canLog = (t.status === 'succeeded' || t.status === 'failed' || t.status === 'running') && !!t.job_id;
  return (
    <div className="req-table-row">
      <div data-label="类型">
        <span className={`status-badge status-${t.task_type === 'design' ? 'designed' : 'developing'}`}>
          {typeLabels[t.task_type]}
        </span>
      </div>
      <div data-label="需求">
        <Link to={`/requirements/${t.requirement_id}`}>{t.requirement_title || t.requirement_id}</Link>
      </div>
      <div data-label="计划时间" style={showOverdue ? { color: '#dc2626', fontWeight: 600 } : undefined}>
        {formatDateTime(t.run_at)}
        {showOverdue && <span style={{ marginLeft: 6 }}>· 已过期</span>}
      </div>
      <div data-label="模型">{t.model || '默认模型'}</div>
      <div data-label="状态">
        <span className={`status-badge status-${t.status}`}>{statusLabels[t.status]}</span>
        {t.error_message && (
          <div style={{ fontSize: 11, color: '#dc2626', marginTop: 4 }}>{t.error_message}</div>
        )}
      </div>
      <div data-label="创建时间">{relativeTime(t.created_at)}</div>
      <div data-label="操作" style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
        {t.status === 'pending' && (
          <button className="btn btn-sm" onClick={onCancel}>取消</button>
        )}
        {canLog && (
          <button className="btn btn-sm" onClick={onOpenLog}>查看日志</button>
        )}
        <button className="btn btn-sm" onClick={onDelete}>删除</button>
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
  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal-card schedule-log-modal" onClick={e => e.stopPropagation()}>
        <div className="modal-header">
          <h3>📜 执行日志</h3>
          <button className="btn btn-sm" onClick={onClose} aria-label="关闭">×</button>
        </div>
        <div className="modal-body">
          {loading ? (
            <div className="tab-empty"><p>⏳ 加载中...</p></div>
          ) : !job ? (
            <div className="tab-empty"><p>未能加载日志（任务可能仍在执行且未落库）。</p></div>
          ) : (
            <>
              <div style={{ fontSize: 12, color: '#64748B', marginBottom: 8 }}>
                Job: <code>{jobId}</code> · 状态: <strong>{job.status}</strong>
                {job.model && <> · 模型: {job.model}</>}
              </div>
              <div className="schedule-log-body">
                {(job.log ?? []).map((line, i) => (
                  <div key={i} className={`schedule-log-line schedule-log-${line.type}`}>
                    <span className="schedule-log-type">{line.type}</span>
                    <span className="schedule-log-content">{line.content}</span>
                  </div>
                ))}
                {(job.log ?? []).length === 0 && (
                  <div style={{ fontSize: 12, color: '#64748B' }}>（暂无日志）</div>
                )}
              </div>
              {job.status === 'running' && (
                <div style={{ fontSize: 12, color: '#64748B', marginTop: 8 }}>
                  任务仍在执行，请稍候…
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