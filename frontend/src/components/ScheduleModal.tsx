// ScheduleModal — schedules a design / coding run.
//
// The parent (RequirementDetail) passes taskType, requirementId and context
// defaults (initialModel / defaultBranchName / defaultBaseBranch /
// agentServers). The user picks a time + model and it POSTs to
// /api/schedules; on success the parent refreshes its "scheduled HH:MM"
// strip and closes the modal.
//
// The time field is a native `<input type="datetime-local">` whose value is
// the user's local time without a timezone suffix. Sent raw, the backend
// would parse it as *server* local time — a Docker/UTC server plus a CST
// user turns "9-7 23:30" into "9-8 07:30". To remove the ambiguity the
// submit path converts it with toRFC3339Local into an RFC3339 string carrying
// the user's own offset (e.g. `2026-09-07T23:30:00+08:00`), which the
// backend's parseRunAt handles timezone-independently. The hint under the
// field spells this out for the user.
//
// Model picking reuses `<ModelSelect stage>` — the same component drives the
// manual design/coding buttons and this modal, so the UI behaves identically.

import { useState, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import {
  schedulesApi,
  type CreateScheduleReq,
  type ScheduledTaskType,
  type ScheduledTask as ScheduledTaskRow,
} from '../api/client';
import ModelSelect from './ModelSelect';
import { toRFC3339Local } from '../utils/time';

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
  // Pre-selected model id (display value). '' = the stage's default model.
  initialModel: string;
  // Coding-only defaults (taken from the requirement's openBranchModal form).
  defaultBranchName?: string;
  defaultBaseBranch?: string;
  // Coding-only agent server picker. Empty array → local execution only.
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

// Convert the datetime-local string the picker hands us into an RFC3339
// string carrying the user's local timezone offset. The helper lives in
// utils/time so both this modal and the calendar RequirementModal reuse it;
// inline definition removed 2026-09 when the calendar view was added.

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
  const { t } = useTranslation();
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
  const titlePrefix = taskType === 'design' ? t('schedules.modal.titleDesign') : t('schedules.modal.titleCoding');

  if (!open) return null;

  const handleSubmit = async () => {
    setSubmitting(true);
    setErrorMsg('');
    try {
      const body: CreateScheduleReq = {
        requirement_id: requirementId,
        task_type: taskType,
        run_at: toRFC3339Local(runAt),
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
      // Friendly messages for the common 4xx codes.
      const code = err?.message?.split(':')?.[0] ?? '';
      if (code === 'ALREADY_SCHEDULED') {
        setErrorMsg(t('schedules.modal.errAlreadyScheduled'));
      } else if (code === 'RUN_AT_TOO_SOON') {
        setErrorMsg(t('schedules.modal.errRunAtTooSoon'));
      } else if (code === 'IDEA_NOT_DEVELOPABLE') {
        setErrorMsg(t('schedules.modal.errIdeaNotDevelopable'));
      } else {
        setErrorMsg(err?.message || t('schedules.modal.errSubmit'));
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
            aria-label={t('schedules.modal.close')}
          >
            ×
          </button>
        </div>

        <div className="modal-body">
          <p className="modal-confirm-text">
            {t('schedules.modal.introPrefix')}<strong>{requirementTitle || t('schedules.modal.sourceFallback')}</strong>{t('schedules.modal.introMiddle')}
            {taskType === 'design' ? t('schedules.modal.introDesign') : t('schedules.modal.introCoding')}{t('schedules.modal.introSuffix')}
          </p>

          <div className="modal-field">
            <label htmlFor="sched-run-at">{t('schedules.modal.runAtLabel')}</label>
            <input
              id="sched-run-at"
              className="form-input"
              type="datetime-local"
              value={runAt}
              min={minRunAtLocal()}
              onChange={e => setRunAt(e.target.value)}
            />
            <small style={{ color: '#64748B', marginTop: 4, display: 'block' }}>
              {t('schedules.modal.runAtHint')}
            </small>
          </div>

          <div className="modal-field">
            <label>{t('schedules.modal.modelLabel')}</label>
            <ModelSelect
              value={model}
              onChange={setModel}
              stage={stage}
              working={submitting}
            />
            <small style={{ color: '#64748B', marginTop: 4, display: 'block' }}>
              {t('schedules.modal.modelHint')}
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
              {t('schedules.modal.readKnowledge')}
            </label>
          </div>

          {taskType === 'coding' && (
            <>
              <div className="modal-field">
                <label htmlFor="sched-branch">{t('schedules.modal.branchLabel')}</label>
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
                <label htmlFor="sched-base">{t('schedules.modal.baseBranchLabel')}</label>
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
                <label htmlFor="sched-agent">{t('schedules.modal.agentLabel')}</label>
                <select
                  id="sched-agent"
                  className="form-input"
                  value={agentServerId}
                  onChange={e => setAgentServerId(e.target.value)}
                  disabled={submitting}
                >
                  <option value="">{t('schedules.modal.agentLocal')}</option>
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
                  {t('schedules.modal.splitTasks')}
                </label>
              </div>
            </>
          )}

          {errorMsg && (
            <div className="modal-risk-panel" style={{ marginTop: 12 }}>
              <div className="modal-risk-title">{t('schedules.modal.failTitle')}</div>
              <div className="modal-risk-desc">{errorMsg}</div>
            </div>
          )}
        </div>

        <div className="modal-actions">
          <button className="btn" onClick={onClose} disabled={submitting}>
            {t('schedules.modal.cancel')}
          </button>
          <button className="btn btn-primary" onClick={handleSubmit} disabled={submitting}>
            {submitting ? t('schedules.modal.submitting') : t('schedules.modal.submit')}
          </button>
        </div>
      </div>
    </div>
  );
}