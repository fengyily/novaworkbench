// ⏰ 定时任务创建弹窗（技术方案 / 开始开发）。
//
// 父组件（RequirementDetail）传入 taskType、requirementId 与上下文默认值
// （initialModel / defaultBranchName / defaultBaseBranch / agentServers），用户
// 填写时间 + 模型后提交到 POST /api/schedules。提交成功 → 父组件刷新
// "已定时 HH:MM" 提示条并关闭弹层。
//
// 时间字段是原生 `<input type="datetime-local">`，后端接受
// `YYYY-MM-DDTHH:MM`（本地时区）或 RFC3339；前端只发 datetime-local 字面量，
// 后端用 ParseInLocation(time.Local) 解析。下方灰字提示用户"将在服务器
// 本地时间执行"，避免时区歧义。
//
// 模型选择复用 `<ModelSelect stage>`——同一个组件同时驱动手动"生成方案/
// 开始开发"按钮和定时弹窗，UI 行为完全一致。

import { useState, useMemo } from 'react';
import {
  schedulesApi,
  type CreateScheduleReq,
  type ScheduledTaskType,
  type ScheduledTask as ScheduledTaskRow,
} from '../api/client';
import ModelSelect from './ModelSelect';

export interface AgentServerOption {
  id: string;
  name: string;
  host: string;
}

interface Props {
  open: boolean;
  onClose: () => void;
  taskType: ScheduledTaskType;
  requirementId: string;
  requirementTitle: string;
  // Pre-selected model id (display value). '' means "默认模型".
  initialModel: string;
  // Coding-only defaults (taken from the requirement's openBranchModal form).
  defaultBranchName?: string;
  defaultBaseBranch?: string;
  // Coding-only agent server picker. Empty array → only "本地执行" option.
  agentServers: AgentServerOption[];
  onScheduled: (t: ScheduledTaskRow) => void;
}

// Pad single-digit hour/minute with zero so datetime-local accepts the
// value. Default = now + 5 minutes (matches backend's 30s floor + a small
// grace period so the user isn't surprised by "too soon" right after
// opening the modal).
function defaultRunAtLocal(): string {
  const d = new Date(Date.now() + 5 * 60_000);
  const pad = (n: number) => `${n}`.padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// Minimum allowed by <input type="datetime-local" min=...>: now + 1 minute
// (so the picker can't be set in the past — the backend enforces a 30s
// floor regardless, but a tighter min here prevents confusing 422s).
function minRunAtLocal(): string {
  const d = new Date(Date.now() + 60_000);
  const pad = (n: number) => `${n}`.padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function ScheduleModal({
  open,
  onClose,
  taskType,
  requirementId,
  requirementTitle,
  initialModel,
  defaultBranchName,
  defaultBaseBranch,
  agentServers,
  onScheduled,
}: Props) {
  const [runAt, setRunAt] = useState(defaultRunAtLocal);
  const [model, setModel] = useState(initialModel || '');
  const [readKnowledge, setReadKnowledge] = useState(false);
  // Coding-only
  const [branchName, setBranchName] = useState(defaultBranchName || '');
  const [baseBranch, setBaseBranch] = useState(defaultBaseBranch || '');
  const [agentServerId, setAgentServerId] = useState('');
  const [splitTasks, setSplitTasks] = useState(true);

  const [submitting, setSubmitting] = useState(false);
  const [errorMsg, setErrorMsg] = useState('');

  // The wizard stage this modal maps to. Reuses ModelSelect's chip colors
  // so the user sees the same accent rail as the manual buttons.
  const stage = useMemo<'architect' | 'developer'>(
    () => (taskType === 'design' ? 'architect' : 'developer'),
    [taskType],
  );
  const titlePrefix = taskType === 'design' ? '⏰ 定时生成方案' : '⏰ 定时开发';

  if (!open) return null;

  const handleSubmit = async () => {
    setSubmitting(true);
    setErrorMsg('');
    try {
      const body: CreateScheduleReq = {
        requirement_id: requirementId,
        task_type: taskType,
        run_at: runAt,
        model: model || undefined,
        read_knowledge: readKnowledge,
      };
      if (taskType === 'coding') {
        body.branch_name = branchName;
        body.base_branch = baseBranch;
        body.agent_server_id = agentServerId;
        body.split_tasks = splitTasks;
      }
      const created = await schedulesApi.create(body);
      onScheduled(created);
    } catch (err: any) {
      // 友好翻译几个常见 4xx
      const code = err?.message?.split(':')?.[0] ?? '';
      if (code === 'ALREADY_SCHEDULED') {
        setErrorMsg('已存在一条 pending 定时任务，请先到「定时任务」页取消或删除。');
      } else if (code === 'RUN_AT_TOO_SOON') {
        setErrorMsg('计划时间距现在太近，请选一个至少 30 秒后的时间。');
      } else if (code === 'IDEA_NOT_DEVELOPABLE') {
        setErrorMsg('「想法」类需求不能直接安排开发，请先转为需求。');
      } else {
        setErrorMsg(err?.message || '提交失败');
      }
    } finally {
      setSubmitting(false);
    }
  };

  // Click-outside-to-dismiss — disabled while submitting so the user can't
  // accidentally lose a queued row mid-create.
  const handleBackdropClick = () => {
    if (!submitting) onClose();
  };

  return (
    <div className="modal-backdrop" onClick={handleBackdropClick}>
      <div className="modal-card schedule-modal" onClick={e => e.stopPropagation()}>
        <div className="modal-header">
          <h3>{titlePrefix}</h3>
          <button
            className="btn btn-sm"
            onClick={onClose}
            disabled={submitting}
            aria-label="关闭"
          >
            ×
          </button>
        </div>

        <div className="modal-body">
          <p className="modal-confirm-text">
            为「<strong>{requirementTitle || '该需求'}</strong>」设置定时
            {taskType === 'design' ? '生成技术方案' : '开始开发'}。
          </p>

          <div className="modal-field">
            <label htmlFor="sched-run-at">计划执行时间</label>
            <input
              id="sched-run-at"
              className="form-input"
              type="datetime-local"
              value={runAt}
              min={minRunAtLocal()}
              onChange={e => setRunAt(e.target.value)}
            />
            <small style={{ color: '#64748B', marginTop: 4, display: 'block' }}>
              将在服务器本地时间执行
            </small>
          </div>

          <div className="modal-field">
            <label>模型</label>
            <ModelSelect
              value={model}
              onChange={setModel}
              stage={stage}
              working={submitting}
            />
            <small style={{ color: '#64748B', marginTop: 4, display: 'block' }}>
              留空 = 该阶段角色默认模型
            </small>
          </div>

          <div className="modal-field modal-check-row">
            <label>
              <input
                type="checkbox"
                checked={readKnowledge}
                onChange={e => setReadKnowledge(e.target.checked)}
                disabled={submitting}
              />
              读取项目知识库（与手动按钮一致）
            </label>
          </div>

          {taskType === 'coding' && (
            <>
              <div className="modal-field">
                <label htmlFor="sched-branch">开发分支</label>
                <input
                  id="sched-branch"
                  className="form-input"
                  value={branchName}
                  onChange={e => setBranchName(e.target.value)}
                  placeholder="feat/req-xxx"
                  disabled={submitting}
                />
              </div>
              <div className="modal-field">
                <label htmlFor="sched-base">基础分支</label>
                <input
                  id="sched-base"
                  className="form-input"
                  value={baseBranch}
                  onChange={e => setBaseBranch(e.target.value)}
                  placeholder="main"
                  disabled={submitting}
                />
              </div>
              <div className="modal-field">
                <label htmlFor="sched-agent">执行环境</label>
                <select
                  id="sched-agent"
                  className="form-input"
                  value={agentServerId}
                  onChange={e => setAgentServerId(e.target.value)}
                  disabled={submitting}
                >
                  <option value="">本地执行</option>
                  {agentServers.map(s => (
                    <option key={s.id} value={s.id}>
                      {s.name} ({s.host})
                    </option>
                  ))}
                </select>
              </div>
              <div className="modal-field modal-check-row">
                <label>
                  <input
                    type="checkbox"
                    checked={splitTasks}
                    onChange={e => setSplitTasks(e.target.checked)}
                    disabled={submitting}
                  />
                  拆分任务（与手动按钮一致；Agent-Server 模式下被忽略）
                </label>
              </div>
            </>
          )}

          {errorMsg && (
            <div className="modal-risk-panel" style={{ marginTop: 12 }}>
              <div className="modal-risk-title">⚠️ 创建失败</div>
              <div className="modal-risk-desc">{errorMsg}</div>
            </div>
          )}
        </div>

        <div className="modal-actions">
          <button className="btn" onClick={onClose} disabled={submitting}>
            取消
          </button>
          <button className="btn btn-primary" onClick={handleSubmit} disabled={submitting}>
            {submitting ? '提交中…' : '创建定时任务'}
          </button>
        </div>
      </div>
    </div>
  );
}