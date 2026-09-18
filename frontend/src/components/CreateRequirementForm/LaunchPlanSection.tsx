// LaunchPlanSection — the optional "start plan" block inside CreateRequirementForm.
//
// This file was originally planned as the deliverable of subtask 7 of the
// "create-time launch plan" feature. It is re-created here as a minimal,
// fully-functional stub so the wiring in CreateRequirementForm.tsx (the
// deliverable of subtask 8) compiles end-to-end. Behaviour and contract
// intentionally mirror ScheduleModal + DesignCodingImmediateModal so the
// later, full-featured subagent can drop in without touching the parent.
//
// Props contract (fixed):
//   flow        — 'direct' | 'skip-analysis' | 'full' (drives which sections render)
//   kind        — 'normal' | 'idea' (parent already hides this whole block on idea)
//   agentServers — ready servers only (RequirementDetail:674-677 contract)
//   disabled     — forwarding for the saving state
//   onChange     — emits a LaunchSpec or null when the spec is invalid
//
// Project hard rule:
//   Any entry point that can start work against a specific execution
//   environment MUST render the environment selector on the same screen.
//   ⇒ Every stage rendered below mounts an ExecEnvSelect.

import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import ModelSelect from '../ModelSelect';
import { ExecEnvSelect } from '../ExecEnvSelect';
import { toRFC3339Local, WEEK_LABELS } from '../../utils/time';
import type { AgentServer, Kind, LaunchSpec } from '../../api/client';

type Flow = 'full' | 'skip-analysis' | 'direct';
type Mode = 'immediate' | 'scheduled';
type Recurrence = 'once' | 'daily' | 'weekly';

// Mirrors ScheduleModal: WEEK_LABELS is Monday-first (一...日); the backend
// recur_days uses 0=Sunday (JS getDay). Mapping the index to its day number
// keeps the two conventions from leaking into each other.
const WEEKDAY_INDEX_TO_DAYNUM: Record<number, number> = { 0: 1, 1: 2, 2: 3, 3: 4, 4: 5, 5: 6, 6: 0 };

function resolveTaskType(flow: Flow): 'coding' | 'design_and_coding' {
  // Mirrors backend requirement_launch.go resolveLaunchTaskType.
  if (flow === 'direct') return 'coding';
  return 'design_and_coding';
}

function pad(n: number): string { return `${n}`.padStart(2, '0'); }

function defaultRunAtLocal(): string {
  const d = new Date(Date.now() + 5 * 60_000);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function defaultRecurTime(): string {
  const d = new Date(Date.now() + 5 * 60_000);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function defaultMinRunAtLocal(): string {
  const d = new Date(Date.now() + 60_000);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export interface LaunchPlanSectionProps {
  flow: Flow;
  kind: Kind;
  agentServers: AgentServer[];
  disabled?: boolean;
  onChange: (spec: LaunchSpec | null) => void;
}

export default function LaunchPlanSection({
  flow,
  kind,
  agentServers,
  disabled,
  onChange,
}: LaunchPlanSectionProps) {
  const { t } = useTranslation();
  const taskType = resolveTaskType(flow);

  // `full` is the only flow that can't run immediately (architect stage
  // hard-gates on having an analysis session id). We still expose schedule
  // because that's the documented "do the talk today, fire tonight" use
  // case — see requirements.design_docs.
  const immediateDisabled = flow === 'full';

  const [mode, setMode] = useState<Mode>('immediate');
  const [designModel, setDesignModel] = useState('');
  const [designAgentServerId, setDesignAgentServerId] = useState('');
  const [codingModel, setCodingModel] = useState('');
  const [codingAgentServerId, setCodingAgentServerId] = useState('');
  const [readKnowledge, setReadKnowledge] = useState(false);
  const [splitTasks, setSplitTasks] = useState(false);
  const [autoPushPr, setAutoPushPr] = useState(false);
  const [recurrence, setRecurrence] = useState<Recurrence>('once');
  const [runAt, setRunAt] = useState(defaultRunAtLocal);
  const [recurTime, setRecurTime] = useState(defaultRecurTime);
  const [recurDays, setRecurDays] = useState<number[]>([1, 3, 5]); // Mon/Wed/Fri

  // Build the outgoing spec. null when the spec is incomplete / invalid.
  const spec = useMemo<LaunchSpec | null>(() => {
    if (kind === 'idea') return null;
    const base: LaunchSpec = { mode };
    if (taskType === 'design_and_coding') {
      base.design_model = designModel;
      base.design_agent_server_id = designAgentServerId || undefined;
    }
    base.coding_model = codingModel;
    base.coding_agent_server_id = codingAgentServerId || undefined;
    base.split_tasks = splitTasks;
    base.auto_push_pr = autoPushPr;
    base.read_knowledge = readKnowledge;
    if (mode === 'scheduled') {
      if (recurrence === 'once') {
        base.schedule = {
          recurrence: 'once',
          run_at: toRFC3339Local(runAt),
        };
      } else if (recurrence === 'daily') {
        base.schedule = {
          recurrence: 'daily',
          recur_time: recurTime,
        };
      } else {
        if (recurDays.length === 0) return null;
        base.schedule = {
          recurrence: 'weekly',
          recur_time: recurTime,
          recur_days: recurDays
            .map((idx) => WEEKDAY_INDEX_TO_DAYNUM[idx])
            .sort((a, b) => a - b)
            .join(','),
        };
      }
    }
    return base;
  }, [
    kind, taskType, mode,
    designModel, designAgentServerId,
    codingModel, codingAgentServerId,
    splitTasks, autoPushPr, readKnowledge,
    recurrence, runAt, recurTime, recurDays,
  ]);

  useEffect(() => { onChange(spec); }, [spec, onChange]);

  return (
    <div className="launch-section__body">
      {/* Mode picker */}
      <div className="launch-section__row" role="radiogroup" aria-label={t('components.createRequirement.launchPlan.title')}>
        <button
          type="button"
          role="radio"
          aria-checked={mode === 'immediate'}
          className={`launch-section__mode${mode === 'immediate' ? ' selected' : ''}`}
          onClick={() => setMode('immediate')}
          disabled={disabled || immediateDisabled}
          title={immediateDisabled ? t('components.createRequirement.launchPlan.fullFlowImmediateDisabled') : undefined}
        >
          {t('components.createRequirement.launchPlan.immediate')}
        </button>
        <button
          type="button"
          role="radio"
          aria-checked={mode === 'scheduled'}
          className={`launch-section__mode${mode === 'scheduled' ? ' selected' : ''}`}
          onClick={() => setMode('scheduled')}
          disabled={disabled}
        >
          {t('components.createRequirement.launchPlan.scheduled')}
        </button>
      </div>

      {immediateDisabled && (
        <p className="launch-section__note">{t('components.createRequirement.launchPlan.fullFlowImmediateDisabled')}</p>
      )}
      {flow === 'full' && mode === 'scheduled' && (
        <p className="launch-section__note">{t('components.createRequirement.launchPlan.fullFlowScheduledHint')}</p>
      )}

      {/* Schedule fields */}
      {mode === 'scheduled' && (
        <div className="launch-section__schedule">
          <div className="launch-section__row" role="radiogroup" aria-label={t('components.createRequirement.launchPlan.recurrence')}>
            {(['once', 'daily', 'weekly'] as Recurrence[]).map((r) => (
              <button
                key={r}
                type="button"
                role="radio"
                aria-checked={recurrence === r}
                className={`launch-section__mode${recurrence === r ? ' selected' : ''}`}
                onClick={() => setRecurrence(r)}
                disabled={disabled}
              >
                {r === 'once' && t('components.createRequirement.launchPlan.scheduleOnce')}
                {r === 'daily' && t('components.createRequirement.launchPlan.scheduleDaily')}
                {r === 'weekly' && t('components.createRequirement.launchPlan.scheduleWeekly')}
              </button>
            ))}
          </div>

          {recurrence === 'once' && (
            <div className="launch-section__field">
              <label>{t('components.createRequirement.launchPlan.runAt')}</label>
              <input
                type="datetime-local"
                className="form-input"
                value={runAt}
                min={defaultMinRunAtLocal()}
                onChange={(e) => setRunAt(e.target.value)}
                disabled={disabled}
              />
            </div>
          )}

          {(recurrence === 'daily' || recurrence === 'weekly') && (
            <div className="launch-section__field">
              <label>{t('components.createRequirement.launchPlan.recurTime')}</label>
              <input
                type="time"
                className="form-input"
                value={recurTime}
                onChange={(e) => setRecurTime(e.target.value)}
                disabled={disabled}
              />
            </div>
          )}

          {recurrence === 'weekly' && (
            <div className="launch-section__field">
              <label>{t('components.createRequirement.launchPlan.recurDays')}</label>
              <div className="launch-section__weekdays">
                {WEEK_LABELS.map((label, idx) => {
                  const on = recurDays.includes(idx);
                  return (
                    <button
                      key={idx}
                      type="button"
                      className={`launch-section__weekday${on ? ' selected' : ''}`}
                      onClick={() => {
                        setRecurDays((prev) => on ? prev.filter((d) => d !== idx) : [...prev, idx].sort((a, b) => a - b));
                      }}
                      disabled={disabled}
                      aria-pressed={on}
                    >
                      {label}
                    </button>
                  );
                })}
              </div>
            </div>
          )}
        </div>
      )}

      {/* Stage sections */}
      {taskType === 'design_and_coding' && (
        <div className="launch-section__stage">
          <div className="launch-section__stage-label">{t('components.createRequirement.launchPlan.designSection')}</div>
          <ModelSelect
            value={designModel}
            onChange={setDesignModel}
            stage="architect"
            working={disabled}
          />
          <ExecEnvSelect
            servers={agentServers}
            value={designAgentServerId}
            onChange={setDesignAgentServerId}
            disabled={disabled}
            className="form-input"
          />
          <label className="launch-section__toggle">
            <input
              type="checkbox"
              checked={readKnowledge}
              onChange={(e) => setReadKnowledge(e.target.checked)}
              disabled={disabled}
            />
            <span>{t('requirements.detail2.preflightReadKnowledge')}</span>
          </label>
        </div>
      )}

      <div className="launch-section__stage">
        <div className="launch-section__stage-label">{t('components.createRequirement.launchPlan.codingSection')}</div>
        <ModelSelect
          value={codingModel}
          onChange={setCodingModel}
          stage="developer"
          working={disabled}
        />
        <ExecEnvSelect
          servers={agentServers}
          value={codingAgentServerId}
          onChange={setCodingAgentServerId}
          disabled={disabled}
          className="form-input"
        />
        <div className="launch-section__toggles">
          <label className="launch-section__toggle">
            <input type="checkbox" checked={splitTasks} onChange={(e) => setSplitTasks(e.target.checked)} disabled={disabled} />
            <span>{t('requirements.detail2.preflightSplitTasks')}</span>
          </label>
          <label className="launch-section__toggle">
            <input type="checkbox" checked={autoPushPr} onChange={(e) => setAutoPushPr(e.target.checked)} disabled={disabled} />
            <span>{t('requirements.detail2.preflightAutoPushPR')}</span>
          </label>
        </div>
      </div>
    </div>
  );
}