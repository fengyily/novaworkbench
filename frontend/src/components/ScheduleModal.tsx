// ScheduleModal — schedules a design / coding / design_and_coding run.
//
// The parent (RequirementDetail) passes taskType, requirementId and context
// defaults (initialModel / initialCodingModel / defaultBranchName /
// defaultBaseBranch / agentServers). The user picks a time + (one or two)
// models + optional agent servers and it POSTs to /api/schedules; on
// success the parent refreshes its "scheduled HH:MM" strip and closes the
// modal.
//
// taskType === 'design_and_coding' renders two stacked configuration
// sections (方案设计 / 开发) sharing the same run-at picker and the same
// "读取知识库" checkbox. This keeps the merged-mode UX visually identical
// to the manual two-stage toolbar that the same RequirementDetail page
// uses: the user picks one architect model + one developer model and the
// scheduler chains them via the existing OnFinish callback.
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
  type ScheduledRecurrence,
  type ScheduledTaskType,
  type ScheduledTask as ScheduledTaskRow,
} from '../api/client';
import ModelSelect from './ModelSelect';
import { ExecEnvSelect } from './ExecEnvSelect';
import { toRFC3339Local, WEEK_LABELS } from '../utils/time';

// WEEK_LABELS is Monday-first (一二三四五六日); the backend recur_days uses
// 0=Sunday (JS getDay convention). This maps a WEEK_LABELS index to its
// day number so the two conventions never leak into each other.
const WEEKDAY_INDEX_TO_DAYNUM = [1, 2, 3, 4, 5, 6, 0];

// Default recur_time = now + 5 minutes as "HH:MM" (matches the one-shot
// picker's default so switching frequency doesn't jump the time).
function defaultRecurTime(): string {
  const d = new Date(Date.now() + 5 * 60_000);
  const pad = (n: number) => `${n}`.padStart(2, '0');
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

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
  // Used for design and coding tasks; for design_and_coding this seeds the
  // design-stage model only (initialCodingModel seeds the coding stage).
  initialModel: string;
  // design_and_coding-only default for the developer-stage model. Ignored
  // when taskType !== 'design_and_coding'.
  initialCodingModel?: string;
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
  initialCodingModel,
  defaultBranchName,
  defaultBaseBranch,
  agentServers,
  onScheduled,
}: Props) {
  const { t } = useTranslation();
  const [runAt, setRunAt] = useState(defaultRunAtLocal);
  // Recurrence: 'once' (default, preserves the historical one-shot UX),
  // 'daily' or 'weekly'. daily/weekly use recurTime + (weekly) recurDays.
  const [recurrence, setRecurrence] = useState<ScheduledRecurrence>('once');
  const [recurTime, setRecurTime] = useState(defaultRecurTime);
  const [recurDays, setRecurDays] = useState<Set<number>>(new Set());
  const [model, setModel] = useState(initialModel || '');
  const [readKnowledge, setReadKnowledge] = useState(false);
  // Coding-only
  const [branchName, setBranchName] = useState(defaultBranchName || '');
  const [baseBranch, setBaseBranch] = useState(defaultBaseBranch || '');
  const [agentServerId, setAgentServerId] = useState('');
  const [splitTasks, setSplitTasks] = useState(true);
  // design_and_coding-only (developer stage)
  const [codingModel, setCodingModel] = useState(initialCodingModel || '');
  const [codingAgentServerId, setCodingAgentServerId] = useState('');

  const [submitting, setSubmitting] = useState(false);
  const [errorMsg, setErrorMsg] = useState('');

  const isMerged = taskType === 'design_and_coding';
  const isDesign = taskType === 'design' || taskType === 'design_and_coding';
  const isCoding = taskType === 'coding' || taskType === 'design_and_coding';

  const titlePrefix = useMemo(() => {
    if (taskType === 'design') return t('schedules.modal.titleDesign');
    if (taskType === 'coding') return t('schedules.modal.titleCoding');
    return t('schedules.modal.titleDesignCoding');
  }, [taskType, t]);

  if (!open) return null;

  const handleSubmit = async () => {
    setSubmitting(true);
    setErrorMsg('');
    // Client-side guard: weekly needs at least one weekday selected.
    if (recurrence === 'weekly' && recurDays.size === 0) {
      setErrorMsg(t('schedules.modal.errWeekdaysRequired'));
      setSubmitting(false);
      return;
    }
    try {
      const body: CreateScheduleReq = {
        requirement_id: requirementId,
        task_type: taskType,
        model: model || undefined,
        read_knowledge: readKnowledge,
      };
      if (recurrence === 'once') {
        body.run_at = toRFC3339Local(runAt);
      } else {
        // The server derives run_at from the rule; send the rule + the
        // browser tz so the wall-clock time projects onto the right instant.
        body.recurrence = recurrence;
        body.recur_time = recurTime;
        body.recur_tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
        if (recurrence === 'weekly') {
          body.recur_days = Array.from(recurDays).sort((a, b) => a - b).join(',');
        }
      }
      // agent_server_id is shared between design and coding (design-stage
      // remote execution was added in 2026-09; coding has had it longer).
      // Empty string means "local execution" on the backend side, so we
      // only set the field when the user picked something.
      if (agentServerId) {
        body.agent_server_id = agentServerId;
      }
      if (isCoding) {
        body.branch_name = branchName;
        body.base_branch = baseBranch;
        body.split_tasks = splitTasks;
      }
      if (isMerged) {
        body.coding_model = codingModel || undefined;
        body.coding_agent_server_id = codingAgentServerId || undefined;
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
      } else if (code === 'INVALID_RECUR_TIME') {
        setErrorMsg(t('schedules.modal.errRecurTime'));
      } else if (code === 'INVALID_RECUR_DAYS') {
        setErrorMsg(t('schedules.modal.errRecurDays'));
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
    <div className="modal-overlay" onClick={handleBackdropClick}>
      <div className="modal-box schedule-modal" onClick={e => e.stopPropagation()}>
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
            {isMerged
              ? t('schedules.modal.introDesignCoding')
              : (taskType === 'design' ? t('schedules.modal.introDesign') : t('schedules.modal.introCoding'))}
            {t('schedules.modal.introSuffix')}
          </p>

          {/* Execution frequency — defaults to 一次性 (once) so the existing
              one-shot UX is unchanged. Picking 每天 / 每周 swaps the run-at
              picker for a time-of-day (and, for weekly, a weekday) selector;
              the scheduler re-arms the row to the next occurrence after each
              run. Merged tasks still fire once per occurrence and chain the
              stages internally. */}
          <div className="modal-field">
            <label>{t('schedules.modal.recurrenceLabel')}</label>
            <div className="schedule-freq-row">
              {(['once', 'daily', 'weekly'] as ScheduledRecurrence[]).map(r => (
                <button
                  key={r}
                  type="button"
                  className={`schedule-freq-pill${recurrence === r ? ' active' : ''}`}
                  onClick={() => setRecurrence(r)}
                  disabled={submitting}
                >
                  {t(`schedules.modal.recurrence.${r}`)}
                </button>
              ))}
            </div>
          </div>

          {recurrence === 'once' ? (
            /* Run-at picker is shared across all stages of a merged task —
               the timer fires once and the scheduler chains the stages. */
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
          ) : (
            <>
              {recurrence === 'weekly' && (
                <div className="modal-field">
                  <label>{t('schedules.modal.recurDaysLabel')}</label>
                  <div className="schedule-weekday-row">
                    {WEEK_LABELS.map((label, idx) => {
                      const day = WEEKDAY_INDEX_TO_DAYNUM[idx];
                      const on = recurDays.has(day);
                      return (
                        <button
                          key={day}
                          type="button"
                          className={`schedule-weekday-pill${on ? ' active' : ''}`}
                          onClick={() => setRecurDays(prev => {
                            const next = new Set(prev);
                            if (next.has(day)) next.delete(day); else next.add(day);
                            return next;
                          })}
                          disabled={submitting}
                        >
                          {label}
                        </button>
                      );
                    })}
                  </div>
                </div>
              )}
              <div className="modal-field">
                <label htmlFor="sched-recur-time">{t('schedules.modal.recurTimeLabel')}</label>
                <input
                  id="sched-recur-time"
                  className="form-input"
                  type="time"
                  value={recurTime}
                  onChange={e => setRecurTime(e.target.value)}
                  disabled={submitting}
                />
                <small style={{ color: '#64748B', marginTop: 4, display: 'block' }}>
                  {t('schedules.modal.recurTimeHint')}
                </small>
              </div>
            </>
          )}

          {/* Design-stage section: rendered for both 'design' and
              'design_and_coding'. ModelSelect.stage="architect" keeps the
              chip colors consistent with the manual design toolbar. */}
          {isDesign && (
            <>
              {isMerged && (
                <div className="modal-section-title">{t('schedules.modal.sectionDesignTitle')}</div>
              )}
              <div className="modal-field">
                <label>{t('schedules.modal.designModelLabel')}</label>
                <ModelSelect
                  value={model}
                  onChange={setModel}
                  stage="architect"
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

              {/* Agent-server picker is shared between design (remote plan-mode
                  execution) and coding (remote CLI execution). The two stages
                  share the same server list — only the label/local-option
                  wording diverges, so we render the shared <ExecEnvSelect> for
                  both and branch on taskType for those two i18n strings. The
                  option list itself ("name (host)") is now identical in both
                  cases, matching the design toolbar and the sub-task composer.
                  The design-stage task only uses the selection when the user
                  picks one, otherwise the scheduler falls back to local
                  execution. */}
              <div className="modal-field">
                {/* Plain <label> sibling (no htmlFor) rather than a wrapping
                    label: ExecEnvSelect renders a bare <select> with no id, and
                    this matches the model field above, which also pairs a
                    for-less label with its control. */}
                <label>
                  {taskType === 'design'
                    ? t('schedules.modal.designAgentServerLabel')
                    : t('schedules.modal.designAgentServerLabelMerged')}
                </label>
                <ExecEnvSelect
                  servers={agentServers}
                  value={agentServerId}
                  onChange={setAgentServerId}
                  disabled={submitting}
                  title={
                    agentServers.length === 0
                      ? t('schedules.modal.designAgentServerEmptyTitle')
                      : ''
                  }
                  localOptionLabel={
                    taskType === 'design'
                      ? t('schedules.modal.designAgentServerDefault')
                      : t('schedules.modal.designAgentServerDefaultMerged')
                  }
                  style={{ minWidth: 160 }}
                />
                {agentServers.length === 0 && (
                  <small style={{ color: '#64748B', marginTop: 4, display: 'block' }}>
                    {t('schedules.modal.designAgentServerNoHint')}
                  </small>
                )}
              </div>
            </>
          )}

          {/* Coding-stage section: rendered for both 'coding' and
              'design_and_coding'. design_and_coding renders a second
              ModelSelect (stage="developer") and a developer-side ExecEnvSelect
              that feed the coding_model / coding_agent_server_id columns. */}
          {isCoding && (
            <>
              {isMerged && (
                <div className="modal-section-title">{t('schedules.modal.sectionCodingTitle')}</div>
              )}

              <div className="modal-field">
                <label>{isMerged ? t('schedules.modal.codingModelLabel') : t('schedules.modal.modelLabel')}</label>
                <ModelSelect
                  value={isMerged ? codingModel : model}
                  onChange={isMerged ? setCodingModel : setModel}
                  stage="developer"
                  working={submitting}
                />
                <small style={{ color: '#64748B', marginTop: 4, display: 'block' }}>
                  {t('schedules.modal.modelHint')}
                </small>
              </div>

              {isMerged && (
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
              )}

              <div className="modal-field">
                <label>
                  {isMerged
                    ? t('schedules.modal.codingAgentServerLabel')
                    : t('schedules.modal.agentLabel')}
                </label>
                <ExecEnvSelect
                  servers={agentServers}
                  value={isMerged ? codingAgentServerId : agentServerId}
                  onChange={isMerged ? setCodingAgentServerId : setAgentServerId}
                  disabled={submitting}
                  title={
                    agentServers.length === 0
                      ? t('schedules.modal.designAgentServerEmptyTitle')
                      : ''
                  }
                  localOptionLabel={
                    isMerged
                      ? t('schedules.modal.codingAgentServerDefault')
                      : t('schedules.modal.agentLocal')
                  }
                  style={{ minWidth: 160 }}
                />
                {agentServers.length === 0 && (
                  <small style={{ color: '#64748B', marginTop: 4, display: 'block' }}>
                    {t('schedules.modal.designAgentServerNoHint')}
                  </small>
                )}
              </div>

              <div className="modal-field">
                <label htmlFor="sched-branch">{t('schedules.modal.branchLabel')}</label>
                <input
                  id="sched-branch"
                  className="form-input"
                  value={branchName}
                  onChange={e => setBranchName(e.target.value)}
                  placeholder={defaultBranchName || 'feat/req-xxx'}
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
