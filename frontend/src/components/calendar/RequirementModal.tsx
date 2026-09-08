// 日历条目编辑弹窗。
//
// 复用列表/详情页既有 modal-overlay/modal-box 样式与表单语言（同套视觉
// 语法）。可编辑字段：
//   - 标题（requirementsApi.update）
//   - 描述（requirementsApi.update）
//   - 优先级（requirementsApi.update）
//   - 计划开始/完成（requirementsApi.updateSchedule）
//   - 「进入详情」按钮 → navigate(/requirements/:id)
//
// 提交策略：先保存文本字段，再保存排期字段（两个独立 PATCH，失败各自
// 回滚；这样优先级 400 不会带走排期失败，反之亦然）。

import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import {
  requirementsApi,
  priorityLabels,
  type Requirement,
} from '../../api/client';
import { formatDateTime, fromLocalDateTime, toRFC3339Local } from '../../utils/time';

interface Props {
  open: boolean;
  requirement: Requirement;
  onClose: () => void;
  onSaved: (next: Requirement) => void;
}

export default function RequirementModal({ open, requirement, onClose, onSaved }: Props) {
  const navigate = useNavigate();
  const [title, setTitle] = useState(requirement.title);
  const [description, setDescription] = useState(requirement.description || '');
  const [priority, setPriority] = useState(requirement.priority || 'medium');
  const [startLocal, setStartLocal] = useState(
    requirement.planned_start_at
      ? fromLocalDateTime(new Date(requirement.planned_start_at))
      : fromLocalDateTime(new Date(requirement.created_at)),
  );
  const [endLocal, setEndLocal] = useState(
    requirement.planned_end_at
      ? fromLocalDateTime(new Date(requirement.planned_end_at))
      : requirement.planned_start_at
        ? fromLocalDateTime(new Date(requirement.planned_start_at))
        : '',
  );
  const [busy, setBusy] = useState(false);
  const [errMsg, setErrMsg] = useState('');

  useEffect(() => {
    if (!open) return;
    setTitle(requirement.title);
    setDescription(requirement.description || '');
    setPriority(requirement.priority || 'medium');
    setStartLocal(
      requirement.planned_start_at
        ? fromLocalDateTime(new Date(requirement.planned_start_at))
        : fromLocalDateTime(new Date(requirement.created_at)),
    );
    setEndLocal(
      requirement.planned_end_at
        ? fromLocalDateTime(new Date(requirement.planned_end_at))
        : requirement.planned_start_at
          ? fromLocalDateTime(new Date(requirement.planned_start_at))
          : '',
    );
    setErrMsg('');
  }, [open, requirement]);

  if (!open) return null;

  const save = async () => {
    setBusy(true);
    setErrMsg('');
    try {
      // 第一步：文本字段
      const next = await requirementsApi.update(requirement.id, {
        title, description, priority,
      });
      // 第二步：排期
      const startISO = toRFC3339Local(startLocal);
      const endISO = endLocal ? toRFC3339Local(endLocal) : startISO;
      const sched = await requirementsApi.updateSchedule(requirement.id, {
        planned_start_at: startISO,
        planned_end_at: endLocal ? endISO : null,
      });
      onSaved({ ...next, ...sched });
      onClose();
    } catch (err: any) {
      setErrMsg(err?.message || '保存失败');
    } finally {
      setBusy(false);
    }
  };

  const clearSchedule = async () => {
    setBusy(true);
    setErrMsg('');
    try {
      const next = await requirementsApi.updateSchedule(requirement.id, {
        planned_start_at: null,
        planned_end_at: null,
      });
      setStartLocal(fromLocalDateTime(new Date(requirement.created_at)));
      setEndLocal('');
      onSaved({ ...requirement, ...next });
    } catch (err: any) {
      setErrMsg(err?.message || '清除排期失败');
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={() => !busy && onClose()}>
      <div className="modal-box cal-modal" onClick={e => e.stopPropagation()}>
        <header className="modal-head">
          <h3>✏️ 编辑需求</h3>
          <button className="btn-icon" onClick={() => !busy && onClose()} aria-label="关闭">×</button>
        </header>
        <div className="modal-body">
          <label className="modal-field">
            <span className="modal-label">标题</span>
            <input
              className="form-input"
              value={title}
              onChange={e => setTitle(e.target.value)}
              disabled={busy}
            />
          </label>
          <label className="modal-field">
            <span className="modal-label">描述</span>
            <textarea
              className="form-input"
              rows={3}
              value={description}
              onChange={e => setDescription(e.target.value)}
              disabled={busy}
            />
          </label>
          <label className="modal-field">
            <span className="modal-label">优先级</span>
            <select
              className="form-input"
              value={priority}
              onChange={e => setPriority(e.target.value)}
              disabled={busy}
            >
              {Object.entries(priorityLabels).map(([k, label]) => (
                <option key={k} value={k}>{label}</option>
              ))}
            </select>
          </label>
          <fieldset className="modal-field">
            <legend className="modal-label">计划排期</legend>
            <div className="cal-modal-row">
              <label className="cal-modal-subfield">
                <span>开始</span>
                <input
                  type="datetime-local"
                  className="form-input"
                  value={startLocal}
                  onChange={e => setStartLocal(e.target.value)}
                  disabled={busy}
                />
              </label>
              <label className="cal-modal-subfield">
                <span>完成</span>
                <input
                  type="datetime-local"
                  className="form-input"
                  value={endLocal}
                  onChange={e => setEndLocal(e.target.value)}
                  disabled={busy}
                />
              </label>
            </div>
            <div className="cal-modal-meta">
              {requirement.scheduled_run_at && (
                <span className="cal-modal-schedule-hint">
                  🕐 已定时 {formatDateTime(requirement.scheduled_run_at)}
                </span>
              )}
              <button
                type="button"
                className="btn btn-sm"
                onClick={clearSchedule}
                disabled={busy}
              >
                清除排期
              </button>
            </div>
          </fieldset>
          {errMsg && <div className="modal-error">{errMsg}</div>}
        </div>
        <footer className="modal-actions btn-row-2col">
          <button className="btn" onClick={() => !busy && onClose()} disabled={busy}>
            取消
          </button>
          <button
            className="btn btn-primary"
            onClick={save}
            disabled={busy || !title.trim()}
          >
            {busy ? '保存中…' : '保存'}
          </button>
          <button
            className="btn"
            onClick={() => navigate(`/requirements/${requirement.id}`)}
            disabled={busy}
          >
            进入详情 →
          </button>
        </footer>
      </div>
    </div>
  );
}