// LaunchPlanSection — the "启动计划" block of the create-requirement form.
//
// It answers, at creation time, the questions that until now could only be
// answered after landing on the requirement detail page: 立即执行还是定时执行、
// 方案/开发各用什么模型、在本地还是哪台 Agent Server 上跑、是否拆分子任务、
// 开发模式是「基于方案」还是「基于会话」。The collected object is handed to
// the parent form as a `LaunchSpec` and POSTed inside
// `requirementsApi.create({ ..., launch })`; the backend dispatches it
// atomically (see backend/internal/handler/requirement_launch.go).
//
// ── Project rule ──
// Any entry point that can start work against a specific execution
// environment MUST render the environment selector on the same screen.
// That rule is the entire reason this component exists: creating a
// requirement used to hand off an `autoStartDesign` intent to the detail
// page, which had to degrade into "scroll + highlight, let the user click"
// precisely because the create form had no <ExecEnvSelect>. Every stage
// rendered below therefore renders one. Do not remove them.
//
// Which stages render is derived from the parent's `flow`, mirroring the
// backend's mapping table exactly:
//
//   flow=direct        → 仅开发段（后端 task_type=coding）
//   flow=skip-analysis → 方案段 + 开发段（design_and_coding）
//   flow=full          → 方案段 + 开发段，但「立即执行」不可用：新建需求还没有
//                        分析会话，方案阶段必然撞 NO_SESSION。定时执行可用，
//                        因为触发时刻在未来，用户可以先把分析聊完。
//
// Controls are deliberately borrowed wholesale: <ModelSelect> /
// <ExecEnvSelect> for the per-stage config, and the recurrence pills +
// datetime-local / time inputs + weekday pills from ScheduleModal (via the
// shared toRFC3339Local / WEEK_LABELS / WEEKDAY_INDEX_TO_DAYNUM helpers in
// utils/time) for the scheduled mode.

import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import ModelSelect from '../ModelSelect';
import { ExecEnvSelect, type ExecEnvServer } from '../ExecEnvSelect';
import {
  type LaunchMode,
  type LaunchSpec,
  type ScheduledRecurrence,
} from '../../api/client';
import { toRFC3339Local, WEEK_LABELS, WEEKDAY_INDEX_TO_DAYNUM } from '../../utils/time';

export type LaunchFlow = 'full' | 'skip-analysis' | 'direct';

// resolveTaskType mirrors backend resolveLaunchTaskType: skip_design wins
// (仅开发), otherwise the run goes through the architect stage first.
function resolveTaskType(flow: LaunchFlow): 'coding' | 'design_and_coding' {
  return flow === 'direct' ? 'coding' : 'design_and_coding';
}

// 立即执行 is only legal when the requirement will have a usable starting
// point the moment it is created: flow=full keeps the analyst stage, and a
// brand-new row has no analyst session for the architect stage to fork.
function immediateAllowed(flow: LaunchFlow): boolean {
  return flow !== 'full';
}

function pad(n: number): string {
  return `${n}`.padStart(2, '0');
}

// Defaults copied from ScheduleModal so switching between the two surfaces
// doesn't jump the time: now + 5 minutes (the backend floor is 30s).
function defaultRunAtLocal(): string {
  const d = new Date(Date.now() + 5 * 60_000);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function minRunAtLocal(): string {
  const d = new Date(Date.now() + 60_000);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function defaultRecurTime(): string {
  const d = new Date(Date.now() + 5 * 60_000);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export interface LaunchPlanSectionProps {
  flow: LaunchFlow;
  /** Ready Agent Servers; empty array → 本地执行 only. */
  agentServers: ExecEnvServer[];
  disabled?: boolean;
  /**
   * Emits the spec to POST, or null when the current selection can't be
   * dispatched (e.g. 立即执行 picked then flow switched to 完整流程, or a
   * weekly rule with no weekday selected). A null spec means the parent
   * submits without a `launch` key — i.e. plain creation.
   */
  onChange: (spec: LaunchSpec | null) => void;
}

export function LaunchPlanSection({
  flow,
  agentServers,
  disabled = false,
  onChange,
}: LaunchPlanSectionProps) {
  const { t } = useTranslation();

  const taskType = resolveTaskType(flow);
  const canImmediate = immediateAllowed(flow);

  const [mode, setMode] = useState<LaunchMode>(canImmediate ? 'immediate' : 'scheduled');
  // Design stage.
  const [designModel, setDesignModel] = useState('');
  const [designAgentServerId, setDesignAgentServerId] = useState('');
  const [readKnowledge, setReadKnowledge] = useState(false);
  // Coding stage.
  const [codingModel, setCodingModel] = useState('');
  const [codingAgentServerId, setCodingAgentServerId] = useState('');
  const [branchName, setBranchName] = useState('');
  const [baseBranch, setBaseBranch] = useState('');
  const [splitTasks, setSplitTasks] = useState(true);
  const [devMode, setDevMode] = useState<'session' | 'design'>('design');
  // Scheduled mode.
  const [recurrence, setRecurrence] = useState<ScheduledRecurrence>('once');
  const [runAt, setRunAt] = useState(defaultRunAtLocal);
  const [recurTime, setRecurTime] = useState(defaultRecurTime);
  const [recurDays, setRecurDays] = useState<Set<number>>(new Set());

  // flow=full disables 立即执行. If the user had it selected and then switched
  // the workflow picker, silently fall back to 定时执行 rather than emitting a
  // spec the backend is guaranteed to reject.
  useEffect(() => {
    if (!canImmediate && mode === 'immediate') setMode('scheduled');
  }, [canImmediate, mode]);

  // Weekly rules need at least one weekday; until then the plan is
  // incomplete and we emit null so the parent creates without launching
  // (the inline hint below tells the user why).
  const weeklyIncomplete = mode === 'scheduled' && recurrence === 'weekly' && recurDays.size === 0;

  const spec = useMemo<LaunchSpec | null>(() => {
    if (mode === 'immediate' && !canImmediate) return null;
    if (weeklyIncomplete) return null;
    const out: LaunchSpec = {
      mode,
      read_knowledge: readKnowledge,
      coding_model: codingModel || undefined,
      coding_agent_server_id: codingAgentServerId || undefined,
      branch_name: branchName,
      base_branch: baseBranch,
      split_tasks: splitTasks,
    };
    if (taskType === 'design_and_coding') {
      out.design_model = designModel || undefined;
      out.design_agent_server_id = designAgentServerId || undefined;
      // dev_mode only means something when a design doc / design session
      // will exist. For 直接开发 there is neither, so we leave the field
      // unset and let the backend apply its own default.
      out.dev_mode = devMode;
    }
    if (mode === 'scheduled') {
      if (recurrence === 'once') {
        // RFC3339 with the browser's own offset — see toRFC3339Local: a bare
        // datetime-local string would be read as *server* local time.
        out.run_at = toRFC3339Local(runAt);
      } else {
        out.recurrence = recurrence;
        out.recur_time = recurTime;
        out.recur_tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
        if (recurrence === 'weekly') {
          out.recur_days = Array.from(recurDays).sort((a, b) => a - b).join(',');
        }
      }
    }
    return out;
  }, [
    mode, canImmediate, weeklyIncomplete, taskType, readKnowledge,
    designModel, designAgentServerId, codingModel, codingAgentServerId,
    branchName, baseBranch, splitTasks, devMode,
    recurrence, runAt, recurTime, recurDays,
  ]);

  useEffect(() => { onChange(spec); }, [spec, onChange]);

  const noAgentHint = agentServers.length === 0
    ? t('components.createRequirement.launch.noAgentHint')
    : '';

  return (
    <div className="create-req-launch-body">
      {/* Mode switcher — reuses the segmented-control look of the kind /
          flow pickers so the panel still reads as one visual family. */}
      <div className="form-group">
        <label>{t('components.createRequirement.launch.modeLabel')}</label>
        <div
          className="create-req-flow"
          role="radiogroup"
          aria-label={t('components.createRequirement.launch.modeLabel')}
        >
          {(['immediate', 'scheduled'] as LaunchMode[]).map(m => (
            <button
              key={m}
              type="button"
              role="radio"
              aria-checked={mode === m}
              className={`create-req-kind-tab${mode === m ? ' selected' : ''}`}
              onClick={() => setMode(m)}
              disabled={disabled || (m === 'immediate' && !canImmediate)}
              title={m === 'immediate' && !canImmediate
                ? t('components.createRequirement.launch.immediateBlocked')
                : ''}
              data-launch-mode={m}
            >
              {m === 'immediate'
                ? t('components.createRequirement.launch.modeImmediate')
                : t('components.createRequirement.launch.modeScheduled')}
            </button>
          ))}
        </div>
        {!canImmediate && (
          <p className="create-req-launch-warn">
            {t('components.createRequirement.launch.immediateBlocked')}
          </p>
        )}
        {mode === 'scheduled' && flow === 'full' && (
          <p className="create-req-launch-warn">
            {t('components.createRequirement.launch.scheduledFullHint')}
          </p>
        )}
      </div>

      {/* Scheduled-mode timing controls — recurrence pills + (once)
          datetime-local / (daily,weekly) time, with Monday-first weekday
          pills for weekly. Identical control set to ScheduleModal. */}
      {mode === 'scheduled' && (
        <div className="form-group">
          <label>{t('components.createRequirement.launch.recurrenceLabel')}</label>
          <div className="create-req-launch-pills">
            {(['once', 'daily', 'weekly'] as ScheduledRecurrence[]).map(r => (
              <button
                key={r}
                type="button"
                className={`schedule-freq-pill${recurrence === r ? ' active' : ''}`}
                onClick={() => setRecurrence(r)}
                disabled={disabled}
              >
                {t(`schedules.modal.recurrence.${r}`)}
              </button>
            ))}
          </div>

          {recurrence === 'once' ? (
            <>
              <input
                className="form-input"
                type="datetime-local"
                value={runAt}
                min={minRunAtLocal()}
                onChange={e => setRunAt(e.target.value)}
                disabled={disabled}
                aria-label={t('schedules.modal.runAtLabel')}
              />
              <small className="form-hint">{t('schedules.modal.runAtHint')}</small>
            </>
          ) : (
            <>
              {recurrence === 'weekly' && (
                <div className="create-req-launch-pills">
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
                        disabled={disabled}
                      >
                        {label}
                      </button>
                    );
                  })}
                </div>
              )}
              <input
                className="form-input"
                type="time"
                value={recurTime}
                onChange={e => setRecurTime(e.target.value)}
                disabled={disabled}
                aria-label={t('schedules.modal.recurTimeLabel')}
              />
              <small className="form-hint">{t('schedules.modal.recurTimeHint')}</small>
            </>
          )}
          {weeklyIncomplete && (
            <p className="create-req-launch-warn">{t('schedules.modal.errWeekdaysRequired')}</p>
          )}
        </div>
      )}

      {/* Design stage — only when the architect stage will actually run. */}
      {taskType === 'design_and_coding' && (
        <div className="create-req-launch-stage">
          <div className="create-req-launch-stage-label">
            {t('components.createRequirement.launch.designSection')}
          </div>
          <div className="form-group">
            <ModelSelect
              value={designModel}
              onChange={setDesignModel}
              stage="architect"
              working={false}
              label={t('schedules.modal.designModelLabel')}
            />
          </div>
          <div className="form-group">
            <label>{t('components.createRequirement.launch.execEnvLabel')}</label>
            <ExecEnvSelect
              servers={agentServers}
              value={designAgentServerId}
              onChange={setDesignAgentServerId}
              disabled={disabled}
              localOptionLabel={t('components.createRequirement.launch.localExec')}
            />
            {noAgentHint && <small className="form-hint">{noAgentHint}</small>}
          </div>
          <label className="create-req-launch-check">
            <input
              type="checkbox"
              checked={readKnowledge}
              onChange={e => setReadKnowledge(e.target.checked)}
              disabled={disabled}
            />
            {t('components.createRequirement.launch.readKnowledge')}
          </label>
        </div>
      )}

      {/* Coding stage — always rendered: both task types end in development. */}
      <div className="create-req-launch-stage">
        <div className="create-req-launch-stage-label">
          {t('components.createRequirement.launch.codingSection')}
        </div>
        <div className="form-group">
          <ModelSelect
            value={codingModel}
            onChange={setCodingModel}
            stage="developer"
            working={false}
            label={t('schedules.modal.codingModelLabel')}
          />
        </div>
        <div className="form-group">
          <label>{t('components.createRequirement.launch.execEnvLabel')}</label>
          <ExecEnvSelect
            servers={agentServers}
            value={codingAgentServerId}
            onChange={setCodingAgentServerId}
            disabled={disabled}
            localOptionLabel={t('components.createRequirement.launch.localExec')}
          />
          {noAgentHint && <small className="form-hint">{noAgentHint}</small>}
        </div>
        <div className="create-req-launch-branches">
          <div className="form-group">
            <label htmlFor="create-req-launch-base">{t('components.createRequirement.launch.baseBranch')}</label>
            <input
              id="create-req-launch-base"
              className="form-input"
              value={baseBranch}
              onChange={e => setBaseBranch(e.target.value)}
              placeholder="main"
              disabled={disabled}
            />
          </div>
          <div className="form-group">
            <label htmlFor="create-req-launch-branch">{t('components.createRequirement.launch.newBranch')}</label>
            <input
              id="create-req-launch-branch"
              className="form-input"
              value={branchName}
              onChange={e => setBranchName(e.target.value)}
              placeholder={t('components.createRequirement.launch.newBranchPlaceholder')}
              disabled={disabled}
            />
          </div>
        </div>
        <label className="create-req-launch-check">
          <input
            type="checkbox"
            checked={splitTasks}
            onChange={e => setSplitTasks(e.target.checked)}
            disabled={disabled}
          />
          {t('components.createRequirement.launch.splitTasks')}
        </label>
        {/* Knowledge pre-read is a single shared option per run. For a
            two-stage plan it belongs to the design section above (it loads
            context before the architect persona starts); a coding-only plan
            has nowhere else to put it. */}
        {taskType === 'coding' && (
          <label className="create-req-launch-check">
            <input
              type="checkbox"
              checked={readKnowledge}
              onChange={e => setReadKnowledge(e.target.checked)}
              disabled={disabled}
            />
            {t('components.createRequirement.launch.readKnowledge')}
          </label>
        )}
        {/* Dev mode — 基于方案 starts a fresh session seeded with the stored
            design doc; 基于会话 forks the design/analysis conversation.
            Only offered for design_and_coding: 直接开发 has neither a design
            doc nor a prior session, so the two options are indistinguishable
            and showing them would just invite a meaningless choice. */}
        {taskType === 'design_and_coding' && (
          <div className="form-group">
            <label>{t('components.createRequirement.launch.devModeLabel')}</label>
            <div className="create-req-launch-radios">
              <label>
                <input
                  type="radio"
                  name="createReqLaunchDevMode"
                  value="design"
                  checked={devMode === 'design'}
                  onChange={() => setDevMode('design')}
                  disabled={disabled}
                />
                {t('components.createRequirement.launch.devModeDesign')}
              </label>
              <label>
                <input
                  type="radio"
                  name="createReqLaunchDevMode"
                  value="session"
                  checked={devMode === 'session'}
                  onChange={() => setDevMode('session')}
                  disabled={disabled}
                />
                {t('components.createRequirement.launch.devModeSession')}
              </label>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

export default LaunchPlanSection;
