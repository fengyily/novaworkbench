// LaunchPlanSection — the optional "launch plan" block inside
// CreateRequirementForm. Lets the user decide at create-time how the new
// requirement should be executed: now vs scheduled, which model + exec
// env per stage, split / auto-push / dev-mode / sync-mode flags.
//
// The component is fully controlled by an external `onChange(spec | null)`
// callback — the parent form owns the create flow and decides whether to
// attach `launch` to the POST /api/requirements body. When the user hasn't
// picked a valid plan, onChange(null) is emitted so the parent's submit
// button can be disabled.
//
// Visual language
// ---------------
//   * Top row: a stage stepper ("DESIGN → CODE" / "CODE") — the signature
//     element. Mirrors the preflight flight strip in DesignCodingImmediate
//     Modal so the user recognizes the same family across both surfaces.
//   * Below the stepper: a 3-card mode picker (Manual / Immediate /
//     Scheduled). Each card carries an icon, a label, and a 1-line
//     consequence note so the cost of each choice is visible without
//     opening a tooltip.
//   * Stage sections (design / coding) reuse the established `.preflight-
//     section` + `.preflight-field-card` vocabulary from RequirementDetail
//     so a user moving between this form and the dev pre-flight modal sees
//     one family of UI rather than two competing patterns.
//   * Radio choices (dev-mode / sync-mode) use a dedicated `.launch-radio-
//     card` rather than the checkbox-style `.preflight-toggle` so the
//     affordance matches the input type.
//   * Schedule recurrence pills stay inside the body as a compact
//     segmented row (same visual family as the parent `flow` picker).
//
// Implementation notes
// --------------------
//   * Sections are gated by `resolveTaskType(flow, mode)`:
//       manual              → no section renders; parent emits null spec.
//       direct + immediate  → coding stage only.
//       skip-analysis + …   → both design + coding stages render.
//
//   * Every stage that actually runs renders BOTH <ModelSelect stage> and
//     <ExecEnvSelect>. That's a project hard rule (the historical
//     "autoStartDesign was downgraded because there was no env selector"
//     bug). Reviewers: this is the first thing to check if the panel ever
//     silently launches on the wrong host.
//
//   * Schedule controls mirror ScheduleModal.tsx: once → datetime-local,
//     daily/weekly → time + (weekly) weekday pills. The 0=Sun / 1=Mon… CSV
//     is built via WEEKDAY_INDEX_TO_DAYNUM (Monday-priority UI order ↔ JS
//     getDay() convention). The constant is duplicated from
//     ScheduleModal.tsx because utils/time.ts doesn't export it yet; a
//     one-line cross-file move would land both call sites on the same
//     definition but is out of scope for this task.

import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import ModelSelect from '../ModelSelect';
import { ExecEnvSelect, type ExecEnvServer } from '../ExecEnvSelect';
import { IconClock, IconHand, IconPlay, IconRocket } from '../icons';
import { toRFC3339Local, WEEK_LABELS } from '../../utils/time';
import type { AgentServer, LaunchSpec, ScheduleSpec } from '../../api/client';

type Flow = 'skip-analysis' | 'direct';
type Mode = 'manual' | 'immediate' | 'scheduled';
type Recurrence = 'once' | 'daily' | 'weekly';

// Monday-first UI ↔ JS getDay() (0=Sunday) backend convention. Same
// constant lives in ScheduleModal.tsx; deliberately not yet lifted into
// utils/time.ts so this change stays self-contained.
const WEEKDAY_INDEX_TO_DAYNUM: readonly number[] = [1, 2, 3, 4, 5, 6, 0];

// Decides which stages should render. The remaining flows always map to a
// valid combination — no flow/mode pair is launch-blocked.
type ResolvedTaskType = 'manual' | 'coding' | 'design_and_coding';

function resolveTaskType(flow: Flow, mode: Mode): ResolvedTaskType {
  // Manual = no dispatch at all; the requirement is created as-is and the
  // user manually triggers the stage from the detail page later. Valid for
  // every remaining flow, so short-circuit before the flow-specific mapping.
  if (mode === 'manual') return 'manual';
  if (flow === 'direct') return 'coding';
  // flow === 'skip-analysis' — both design + coding stages.
  return 'design_and_coding';
}

function pad(n: number): string { return `${n}`.padStart(2, '0'); }

// datetime-local default value (now + 5 minutes, no offset). Matches
// ScheduleModal.tsx so the one-shot picker behaviour is unchanged.
function defaultRunAtLocal(): string {
  const d = new Date(Date.now() + 5 * 60_000);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// datetime-local min attribute (now + 1 minute). Tighter than the
// backend's 30s floor — a 422 on submit is jarring; the tighter min
// makes "time in the past" impossible to pick.
function minRunAtLocal(): string {
  const d = new Date(Date.now() + 60_000);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// HH:MM default for daily/weekly. now + 5 minutes.
function defaultRecurTime(): string {
  const d = new Date(Date.now() + 5 * 60_000);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export interface LaunchPlanSectionProps {
  flow: Flow;
  // The parent's `kind` value: requirement; issue; idea. The parent
  // already hides this whole block for idea, but we re-check as a
  // defensive guard so a misconfigured caller can't dispatch a launch
  // for an idea (the backend rejects anyway, but better UX to never
  // show the form).
  kind: 'requirement' | 'issue' | 'idea';
  // Caller pre-filters to status === 'ready'; the backend refuses any
  // other state. Re-project to the minimal ExecEnvServer shape so the
  // shared <ExecEnvSelect> renders without an extra cast.
  agentServers: AgentServer[];
  disabled?: boolean;
  onChange: (spec: LaunchSpec | null) => void;
}

// Mode card metadata — drives the 3-card mode picker below the stage
// stepper. Each entry pairs a recognizable icon with a label + a 1-line
// consequence note so the user reads the cost of each choice at a glance
// rather than discovering it after submit.
const MODE_CARDS: { value: Mode; Icon: typeof IconHand; labelKey: string; noteKey: string }[] = [
  { value: 'manual',   Icon: IconHand,   labelKey: 'components.createRequirement.launchPlan.manual',   noteKey: 'components.createRequirement.launchPlan.manualNote' },
  { value: 'immediate',Icon: IconRocket, labelKey: 'components.createRequirement.launchPlan.immediate',noteKey: 'components.createRequirement.launchPlan.immediateNote' },
  { value: 'scheduled',Icon: IconClock,  labelKey: 'components.createRequirement.launchPlan.scheduled', noteKey: 'components.createRequirement.launchPlan.scheduledNote' },
];

// Dev-mode radio card metadata.
const DEV_MODE_OPTIONS: { value: 'design' | 'session'; labelKey: string }[] = [
  { value: 'design',  labelKey: 'requirements.detail2.preflightDevModeDesign' },
  { value: 'session', labelKey: 'requirements.detail2.preflightDevModeSession' },
];

// Sync-mode radio card metadata.
const SYNC_MODE_OPTIONS: { value: '' | 'local'; labelKey: string }[] = [
  { value: '',     labelKey: 'requirements.detail2.syncModeRemote' },
  { value: 'local', labelKey: 'requirements.detail2.syncModeLocal' },
];

export default function LaunchPlanSection({
  flow,
  kind,
  agentServers,
  disabled,
  onChange,
}: LaunchPlanSectionProps) {
  const { t } = useTranslation();

  // NOTE: the `kind === 'idea' → return null` guard lives BELOW all hook
  // calls so React's rules-of-hooks (same order every render) is honored.
  // Hooks come first; the guard is purely a render-time decision.

  // Project to ExecEnvServer (only id/name/host are read by the shared
  // <ExecEnvSelect>).
  const servers: ExecEnvServer[] = useMemo(
    () => agentServers.map(s => ({ id: s.id, name: s.name, host: s.host })),
    [agentServers],
  );

  // Top-level dispatch mode. Defaults to 'manual' so creating a requirement
  // never auto-starts a long-running job by accident — the user must
  // explicitly opt into immediate/scheduled. Manual emits no launch spec
  // (see the effect below), so it behaves exactly like leaving the launch
  // plan collapsed.
  const [mode, setMode] = useState<Mode>('manual');

  // Schedule sub-state. Same defaults ScheduleModal uses.
  const [recurrence, setRecurrence] = useState<Recurrence>('once');
  const [runAt, setRunAt] = useState<string>(defaultRunAtLocal);
  const [recurTime, setRecurTime] = useState<string>(defaultRecurTime);
  const [recurDays, setRecurDays] = useState<Set<number>>(new Set());

  // Design-stage state (only used when design_and_coding renders).
  const [designModel, setDesignModel] = useState('');
  const [designClaudeConfigId, setDesignClaudeConfigId] = useState('');
  const [designAgentServerId, setDesignAgentServerId] = useState('');
  // Read-knowledge is a single checkbox (shared between the design and
  // coding stages in DesignCodingImmediateModal).
  const [readKnowledge, setReadKnowledge] = useState(false);

  // Coding-stage state.
  const [codingModel, setCodingModel] = useState('');
  const [codingClaudeConfigId, setCodingClaudeConfigId] = useState('');
  const [codingAgentServerId, setCodingAgentServerId] = useState('');
  const [branchName, setBranchName] = useState('');
  const [baseBranch, setBaseBranch] = useState('');
  // Default OFF to match the developer-stage preflight default: split
  // tasks runs an extra dispatch round and is the slower path.
  const [splitTasks, setSplitTasks] = useState(false);
  // Auto-push default ON so the common "create + ship" path stays
  // zero-config.
  const [autoPushPR, setAutoPushPR] = useState(true);
  // '' = remote-Git sync (default); 'local' = no-remote fallback.
  const [syncMode, setSyncMode] = useState<'' | 'local'>('');
  // 'design' = hand the stored design doc via -p; matches the
  // RequirementDetail dev-mode picker default.
  const [devMode, setDevMode] = useState<'session' | 'design'>('design');

  const taskType = resolveTaskType(flow, mode);
  const showDesignSection = taskType === 'design_and_coding';
  const showCodingSection = taskType === 'coding' || taskType === 'design_and_coding';
  const noStages = taskType === 'manual';

  // Build the LaunchSpec and emit on every dependency change. The
  // parent's onChange should be stable (useCallback) — emitting on
  // every parent re-render would cause an update-loop, hence the
  // eslint-disable.
  useEffect(() => {
    // Manual = no launch spec. The parent sends `launch: undefined`, the
    // backend skips dispatch, and ProjectDetail's onCreated falls through
    // to the original skip-based navigation (autoStartDesign guide for
    // skip-analysis, branch modal for skip-design). Functionally identical
    // to leaving the launch-plan section collapsed.
    if (mode === 'manual') {
      onChange(null);
      return;
    }

    // Schedule block only meaningful when mode === 'scheduled'; the
    // backend ignores it otherwise. weekly requires at least one day;
    // recurring requires a recur_time. Anything missing → invalid → null.
    let schedule: ScheduleSpec | undefined;
    if (mode === 'scheduled') {
      schedule = {};
      if (recurrence === 'once') {
        if (!runAt) { onChange(null); return; }
        schedule.recurrence = 'once';
        schedule.run_at = toRFC3339Local(runAt);
      } else {
        if (!recurTime) { onChange(null); return; }
        schedule.recurrence = recurrence;
        schedule.recur_time = recurTime;
        schedule.recur_tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
        if (recurrence === 'weekly') {
          if (recurDays.size === 0) { onChange(null); return; }
          schedule.recur_days = Array.from(recurDays).sort((a, b) => a - b).join(',');
        }
      }
    }

    const spec: LaunchSpec = {
      mode,
      design_model: designModel || undefined,
      design_claude_config_id: designClaudeConfigId || undefined,
      design_agent_server_id: designAgentServerId || undefined,
      coding_model: codingModel || undefined,
      coding_claude_config_id: codingClaudeConfigId || undefined,
      coding_agent_server_id: codingAgentServerId || undefined,
      branch_name: branchName || undefined,
      base_branch: baseBranch || undefined,
      split_tasks: splitTasks || undefined,
      auto_push_pr: autoPushPR,
      read_knowledge: readKnowledge || undefined,
      sync_mode: syncMode || undefined,
      dev_mode: devMode || undefined,
      schedule,
    };
    onChange(spec);
    // onChange intentionally omitted — see the effect comment above.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [
    mode, recurrence, runAt, recurTime, recurDays,
    designModel, designClaudeConfigId, designAgentServerId,
    codingModel, codingClaudeConfigId, codingAgentServerId,
    branchName, baseBranch, splitTasks, autoPushPR, readKnowledge,
    syncMode, devMode,
  ]);

  const fieldDisabled = !!disabled;

  // Defensive: the parent already hides this component for idea, but
  // re-check so onChange can never emit a launch spec for an idea. This
  // runs AFTER every hook above so the hook order is stable.
  if (kind === 'idea') return null;

  return (
    <div className="launch-plan-section">
      {/* ----- Signature: stage stepper --------------------------------
          Visualizes the launch pipeline at a glance: a thin rail of stage
          chips connected by chevrons. The active stages light up with their
          section tint (violet for design, cyan for coding) so the user
          knows which sections below will dispatch. Manual mode dims the
          whole rail. */}
      <div
        className={`launch-stages${noStages ? ' is-dimmed' : ''}`}
        aria-hidden={noStages}
      >
        <span
          className={`launch-stage-chip launch-stage-chip--design${showDesignSection ? ' is-on' : ''}`}
          title={t('components.createRequirement.launchPlan.designSection')}
        >
          {t('components.createRequirement.launchPlan.designSection')}
        </span>
        <span className="launch-stage-arrow" aria-hidden>
          <IconPlay size={10} />
        </span>
        <span
          className={`launch-stage-chip launch-stage-chip--coding${showCodingSection ? ' is-on' : ''}`}
          title={t('components.createRequirement.launchPlan.codingSection')}
        >
          {t('components.createRequirement.launchPlan.codingSection')}
        </span>
      </div>

      {/* ----- Mode picker (3 cards) ------------------------------------
          Each card carries an icon, a label, and a 1-line consequence
          note. The selected card gets a strong left rail + filled
          surface; the unselected cards stay quiet with a hover state. */}
      <div className="modal-field">
        <div
          className="launch-mode-row"
          role="radiogroup"
          aria-label={t('components.createRequirement.launchPlan.title')}
        >
          {MODE_CARDS.map(({ value, Icon, labelKey, noteKey }) => {
            const selected = mode === value;
            return (
              <button
                key={value}
                type="button"
                role="radio"
                aria-checked={selected}
                className={`launch-mode-card${selected ? ' selected' : ''}`}
                onClick={() => setMode(value)}
                disabled={fieldDisabled}
                data-mode={value}
              >
                <span className="launch-mode-card-icon" aria-hidden>
                  <Icon size={16} />
                </span>
                <span className="launch-mode-card-label">{t(labelKey)}</span>
                <span className="launch-mode-card-note">{t(noteKey)}</span>
              </button>
            );
          })}
        </div>
      </div>

      {/* ----- Schedule controls (recurrence + datetime) ---------------
          Renders only when the user picked the scheduled mode. Mirrors
          ScheduleModal.tsx so the conversion + UX stay identical. */}
      {mode === 'scheduled' && (
        <div className="launch-schedule">
          <div className="modal-field">
            <label>{t('schedules.modal.recurrenceLabel')}</label>
            <div className="launch-segment">
              {(['once', 'daily', 'weekly'] as Recurrence[]).map(r => (
                <button
                  key={r}
                  type="button"
                  className={`launch-segment-tab${recurrence === r ? ' selected' : ''}`}
                  onClick={() => setRecurrence(r)}
                  disabled={fieldDisabled}
                >
                  {t(`schedules.modal.recurrence.${r}`)}
                </button>
              ))}
            </div>
          </div>

          {recurrence === 'once' ? (
            <div className="modal-field">
              <label htmlFor="launch-plan-run-at">{t('components.createRequirement.launchPlan.runAt')}</label>
              <input
                id="launch-plan-run-at"
                className="form-input"
                type="datetime-local"
                value={runAt}
                min={minRunAtLocal()}
                onChange={e => setRunAt(e.target.value)}
                disabled={fieldDisabled}
              />
              <small className="launch-plan-section__note">
                {t('schedules.modal.runAtHint')}
              </small>
            </div>
          ) : (
            <>
              {recurrence === 'weekly' && (
                <div className="modal-field">
                  <label>{t('components.createRequirement.launchPlan.recurDays')}</label>
                  <div className="launch-weekdays">
                    {WEEK_LABELS.map((label, idx) => {
                      const day = WEEKDAY_INDEX_TO_DAYNUM[idx];
                      const on = recurDays.has(day);
                      return (
                        <button
                          key={day}
                          type="button"
                          className={`launch-weekday${on ? ' selected' : ''}`}
                          onClick={() => setRecurDays(prev => {
                            const next = new Set(prev);
                            if (next.has(day)) next.delete(day); else next.add(day);
                            return next;
                          })}
                          disabled={fieldDisabled}
                          aria-pressed={on}
                        >
                          {label}
                        </button>
                      );
                    })}
                  </div>
                </div>
              )}
              <div className="modal-field">
                <label htmlFor="launch-plan-recur-time">{t('components.createRequirement.launchPlan.recurTime')}</label>
                <input
                  id="launch-plan-recur-time"
                  className="form-input"
                  type="time"
                  value={recurTime}
                  onChange={e => setRecurTime(e.target.value)}
                  disabled={fieldDisabled}
                />
                <small className="launch-plan-section__note">
                  {t('schedules.modal.recurTimeHint')}
                </small>
              </div>
            </>
          )}
        </div>
      )}

      {/* ----- Design stage --------------------------------------------
          Renders when resolved task_type is design_and_coding. Section
          carries the violet rail that mirrors the architect-stage
          ModelSelect + the EXEC accent on the dev pre-flight modal, so
          the user reads this band as the "design" half of the pipeline. */}
      {showDesignSection && (
        <div className="launch-stage-section launch-stage-section--design">
          <div className="launch-stage-section-label">
            <span className="launch-stage-section-chip" aria-hidden>
              {t('components.createRequirement.launchPlan.designSection')}
            </span>
            <span className="launch-stage-section-hint">
              {t('components.createRequirement.launchPlan.designSectionHint')}
            </span>
          </div>

          <div className="modal-field">
            <ModelSelect
              value={designModel}
              onChange={setDesignModel}
              stage="architect"
              working={fieldDisabled}
              configId={designClaudeConfigId}
              onConfigChange={setDesignClaudeConfigId}
              label={t('schedules.modal.designModelLabel')}
            />
            <small className="launch-plan-section__note">
              {t('schedules.modal.modelHint')}
            </small>
          </div>

          <label className={`preflight-toggle ${readKnowledge ? 'is-checked' : ''}`}>
            <input
              type="checkbox"
              checked={readKnowledge}
              onChange={e => setReadKnowledge(e.target.checked)}
              disabled={fieldDisabled}
            />
            <div className="preflight-toggle-body">
              <div className="preflight-toggle-title">
                {t('requirements.detail2.preflightReadKnowledge')}
              </div>
              <div className="preflight-toggle-desc">
                {t('requirements.detail2.preflightReadKnowledgeHint')}
              </div>
            </div>
          </label>

          <div className="modal-field">
            <div className="preflight-field-card preflight-field-card--exec">
              <span className="preflight-field-chip" aria-hidden="true">ENV</span>
              <ExecEnvSelect
                className="form-input preflight-field-input"
                servers={servers}
                value={designAgentServerId}
                onChange={setDesignAgentServerId}
                disabled={fieldDisabled}
                title={
                  servers.length === 0
                    ? t('requirements.detail2.preflightAgentEmptyTitle')
                    : ''
                }
                localOptionLabel={t('schedules.modal.designAgentServerDefaultMerged')}
              />
            </div>
            {servers.length === 0 && (
              <small className="launch-plan-section__note">
                {t('requirements.detail2.preflightNoAgentHint')}
              </small>
            )}
          </div>
        </div>
      )}

      {/* ----- Coding stage --------------------------------------------
          Renders for both task types (coding-only flow and the coding
          half of design_and_coding). Section carries the cyan rail that
          mirrors the developer-stage ModelSelect + the GIT accent on the
          dev pre-flight modal. Branch / base inputs use the
          `.preflight-field-card` + chip vocabulary established by
          DesignCodingImmediateModal — the same family the user sees in
          the dev pre-flight modal minutes later. */}
      {showCodingSection && (
        <div className="launch-stage-section launch-stage-section--coding">
          <div className="launch-stage-section-label">
            <span className="launch-stage-section-chip" aria-hidden>
              {t('components.createRequirement.launchPlan.codingSection')}
            </span>
            <span className="launch-stage-section-hint">
              {t('components.createRequirement.launchPlan.codingSectionHint')}
            </span>
          </div>

          <div className="modal-field">
            <ModelSelect
              value={codingModel}
              onChange={setCodingModel}
              stage="developer"
              working={fieldDisabled}
              configId={codingClaudeConfigId}
              onConfigChange={setCodingClaudeConfigId}
              label={t('schedules.modal.codingModelLabel')}
            />
            <small className="launch-plan-section__note">
              {t('schedules.modal.modelHint')}
            </small>
          </div>

          <div className="modal-field">
            <div className="preflight-field-card preflight-field-card--git">
              <span className="preflight-field-chip" aria-hidden="true">ENV</span>
              <ExecEnvSelect
                className="form-input preflight-field-input"
                servers={servers}
                value={codingAgentServerId}
                onChange={setCodingAgentServerId}
                disabled={fieldDisabled}
                title={
                  servers.length === 0
                    ? t('requirements.detail2.preflightAgentEmptyTitle')
                    : ''
                }
                localOptionLabel={t('schedules.modal.codingAgentServerDefault')}
              />
            </div>
            {servers.length === 0 && (
              <small className="launch-plan-section__note">
                {t('requirements.detail2.preflightNoAgentHint')}
              </small>
            )}
          </div>

          <div className="modal-field">
            <div className="preflight-field-card preflight-field-card--git">
              <span className="preflight-field-chip" aria-hidden="true">BASE</span>
              <input
                className="form-input preflight-field-input"
                value={baseBranch}
                onChange={e => setBaseBranch(e.target.value)}
                placeholder="main"
                disabled={fieldDisabled}
              />
            </div>
          </div>

          <div className="modal-field">
            <div className="preflight-field-card preflight-field-card--git">
              <span className="preflight-field-chip" aria-hidden="true">NEW</span>
              <input
                className="form-input preflight-field-input"
                value={branchName}
                onChange={e => setBranchName(e.target.value)}
                placeholder="feat/req-xxx"
                disabled={fieldDisabled}
              />
            </div>
          </div>

          <label className={`preflight-toggle ${splitTasks ? 'is-checked' : ''}`}>
            <input
              type="checkbox"
              checked={splitTasks}
              onChange={e => setSplitTasks(e.target.checked)}
              disabled={fieldDisabled}
            />
            <div className="preflight-toggle-body">
              <div className="preflight-toggle-title">
                {t('requirements.detail2.preflightSplitTasks')}
              </div>
              <div className="preflight-toggle-desc">
                {t('requirements.detail2.preflightSplitTasksHint')}
              </div>
            </div>
          </label>

          <label className={`preflight-toggle ${autoPushPR ? 'is-checked' : ''}`}>
            <input
              type="checkbox"
              checked={autoPushPR}
              onChange={e => setAutoPushPR(e.target.checked)}
              disabled={fieldDisabled}
            />
            <div className="preflight-toggle-body">
              <div className="preflight-toggle-title">
                {t('requirements.detail2.preflightAutoPushPR')}
              </div>
              <div className="preflight-toggle-desc">
                {t('requirements.detail2.preflightAutoPushPRHint')}
              </div>
            </div>
          </label>

          {/* Dev-mode picker — radio card (single-select among named
              dev strategies). Wrapped in a dedicated `.launch-radio-card`
              instead of the checkbox `.preflight-toggle` so the visual
              affordance matches the input type (radio, not checkbox). */}
          <div className="launch-radio-card">
            <div className="launch-radio-card-title">
              {t('requirements.detail2.preflightDevModeTitle')}
            </div>
            <div className="launch-radio-card-desc">
              {t('requirements.detail2.preflightDevModeHint')}
            </div>
            <div className="launch-radio-options">
              {DEV_MODE_OPTIONS.map(opt => (
                <label
                  key={opt.value}
                  className={`launch-radio-option${devMode === opt.value ? ' selected' : ''}`}
                >
                  <input
                    type="radio"
                    name="launchPlanDevMode"
                    value={opt.value}
                    checked={devMode === opt.value}
                    onChange={() => setDevMode(opt.value)}
                    disabled={fieldDisabled}
                  />
                  <span>{t(opt.labelKey)}</span>
                </label>
              ))}
            </div>
          </div>

          {/* Sync-mode picker — same radio-card treatment. */}
          <div className="launch-radio-card">
            <div className="launch-radio-card-title">
              {t('requirements.detail2.syncModeLabel')}
            </div>
            <div className="launch-radio-card-desc">
              {t('requirements.detail2.syncModeRemoteHint')}
            </div>
            <div className="launch-radio-options">
              {SYNC_MODE_OPTIONS.map(opt => (
                <label
                  key={opt.value || 'remote'}
                  className={`launch-radio-option${syncMode === opt.value ? ' selected' : ''}`}
                >
                  <input
                    type="radio"
                    name="launchPlanSyncMode"
                    value={opt.value}
                    checked={syncMode === opt.value}
                    onChange={() => setSyncMode(opt.value)}
                    disabled={fieldDisabled}
                  />
                  <span>{t(opt.labelKey)}</span>
                </label>
              ))}
            </div>
          </div>
        </div>
      )}
    </div>
  );
}