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
// Implementation notes
// --------------------
//   * Sections are gated by `resolveTaskType(flow, mode)`:
//       direct           → only the coding stage renders.
//       skip-analysis    → both design + coding stages render.
//       full + immediate → blocked (radio disabled + onChange(null)).
//       full + scheduled → both stages render with a "must finish analysis"
//                          hint that survives into the UI.
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
      {/* Top-level mode toggle: "手动执行 / 立即执行 / 定时执行". Manual is
          the default so a stray submit never kicks off a 30-minute coding
          job; the user must actively opt into immediate/scheduled. */}
      <div className="modal-field">
        <div
          className="schedule-freq-row"
          role="radiogroup"
          aria-label={t('components.createRequirement.launchPlan.title')}
        >
          <button
            type="button"
            role="radio"
            aria-checked={mode === 'manual'}
            className={`schedule-freq-pill${mode === 'manual' ? ' active' : ''}`}
            onClick={() => setMode('manual')}
            disabled={fieldDisabled}
          >
            {t('components.createRequirement.launchPlan.manual')}
          </button>
          <button
            type="button"
            role="radio"
            aria-checked={mode === 'immediate'}
            className={`schedule-freq-pill${mode === 'immediate' ? ' active' : ''}`}
            onClick={() => setMode('immediate')}
            disabled={fieldDisabled}
            title=""
          >
            {t('components.createRequirement.launchPlan.immediate')}
          </button>
          <button
            type="button"
            role="radio"
            aria-checked={mode === 'scheduled'}
            className={`schedule-freq-pill${mode === 'scheduled' ? ' active' : ''}`}
            onClick={() => setMode('scheduled')}
            disabled={fieldDisabled}
          >
            {t('components.createRequirement.launchPlan.scheduled')}
          </button>
        </div>
        {mode === 'manual' && (
          <small className="launch-plan-section__note">
            {t('components.createRequirement.launchPlan.manualHint')}
          </small>
        )}
        {/* `full` removed — no scheduled-with-analysis-pending hint needed. */}
      </div>

      {/* Schedule controls — rendered only when the user picked the
          scheduled mode. Mirrors ScheduleModal.tsx:255-332 verbatim so
          the behaviour and the conversion to RFC3339 stay identical. */}
      {mode === 'scheduled' && (
        <>
          <div className="modal-field">
            <label>{t('schedules.modal.recurrenceLabel')}</label>
            <div className="schedule-freq-row">
              {(['once', 'daily', 'weekly'] as Recurrence[]).map(r => (
                <button
                  key={r}
                  type="button"
                  className={`schedule-freq-pill${recurrence === r ? ' active' : ''}`}
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
                          disabled={fieldDisabled}
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
        </>
      )}

      {/* Design stage — only when resolved task_type is design_and_coding
          (skip-analysis, or full + scheduled). Re-uses the same
          preflight section/card vocabulary as DesignCodingImmediateModal
          so the UI feels like one family. */}
      {showDesignSection && (
        <div className="preflight-section">
          <div className="preflight-section-label">
            {t('schedules.modal.sectionDesignTitle')}
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
            <label>{t('schedules.modal.designAgentServerLabelMerged')}</label>
            <ExecEnvSelect
              className="form-input"
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
            {servers.length === 0 && (
              <small className="launch-plan-section__note">
                {t('requirements.detail2.preflightNoAgentHint')}
              </small>
            )}
          </div>
        </div>
      )}

      {/* Coding stage — renders for coding (flow=direct) and for the
          coding half of design_and_coding. ModelSelect + ExecEnvSelect
          are both required (project hard rule, see file header). */}
      {showCodingSection && (
        <div className="preflight-section">
          <div className="preflight-section-label">
            {t('schedules.modal.sectionCodingTitle')}
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
            <label>{t('schedules.modal.codingAgentServerLabel')}</label>
            <ExecEnvSelect
              className="form-input"
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
            {servers.length === 0 && (
              <small className="launch-plan-section__note">
                {t('requirements.detail2.preflightNoAgentHint')}
              </small>
            )}
          </div>

          <div className="modal-field">
            <label htmlFor="launch-plan-base-branch">{t('schedules.modal.baseBranchLabel')}</label>
            <input
              id="launch-plan-base-branch"
              className="form-input"
              value={baseBranch}
              onChange={e => setBaseBranch(e.target.value)}
              placeholder="main"
              disabled={fieldDisabled}
            />
          </div>
          <div className="modal-field">
            <label htmlFor="launch-plan-new-branch">{t('schedules.modal.branchLabel')}</label>
            <input
              id="launch-plan-new-branch"
              className="form-input"
              value={branchName}
              onChange={e => setBranchName(e.target.value)}
              placeholder="feat/req-xxx"
              disabled={fieldDisabled}
            />
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

          {/* Dev-mode picker — radio group inside a preflight-toggle
              card. Mirrors the developer-stage preflight panel. */}
          <div className="preflight-toggle" style={{ display: 'block' }}>
            <div className="preflight-toggle-body">
              <div className="preflight-toggle-title">
                {t('requirements.detail2.preflightDevModeTitle')}
              </div>
              <div className="preflight-toggle-desc" style={{ marginBottom: 8 }}>
                {t('requirements.detail2.preflightDevModeHint')}
              </div>
              <div style={{ display: 'flex', gap: 16, flexWrap: 'wrap' }}>
                <label style={{ display: 'inline-flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}>
                  <input
                    type="radio"
                    name="launchPlanDevMode"
                    value="design"
                    checked={devMode === 'design'}
                    onChange={() => setDevMode('design')}
                    disabled={fieldDisabled}
                  />
                  {t('requirements.detail2.preflightDevModeDesign')}
                </label>
                <label style={{ display: 'inline-flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}>
                  <input
                    type="radio"
                    name="launchPlanDevMode"
                    value="session"
                    checked={devMode === 'session'}
                    onChange={() => setDevMode('session')}
                    disabled={fieldDisabled}
                  />
                  {t('requirements.detail2.preflightDevModeSession')}
                </label>
              </div>
            </div>
          </div>

          {/* sync_mode — radio group. The backend treats '' as the
              default (remote Git sync); 'local' is the no-remote
              fallback used for self-hosted repos. */}
          <div className="preflight-toggle" style={{ display: 'block' }}>
            <div className="preflight-toggle-body">
              <div className="preflight-toggle-title">
                {t('requirements.detail2.syncModeLabel')}
              </div>
              <div className="preflight-toggle-desc" style={{ marginBottom: 8 }}>
                {t('requirements.detail2.syncModeRemoteHint')}
              </div>
              <div style={{ display: 'flex', gap: 16, flexWrap: 'wrap' }}>
                <label style={{ display: 'inline-flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}>
                  <input
                    type="radio"
                    name="launchPlanSyncMode"
                    value=""
                    checked={syncMode === ''}
                    onChange={() => setSyncMode('')}
                    disabled={fieldDisabled}
                  />
                  {t('requirements.detail2.syncModeRemote')}
                </label>
                <label style={{ display: 'inline-flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}>
                  <input
                    type="radio"
                    name="launchPlanSyncMode"
                    value="local"
                    checked={syncMode === 'local'}
                    onChange={() => setSyncMode('local')}
                    disabled={fieldDisabled}
                  />
                  {t('requirements.detail2.syncModeLocal')}
                </label>
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}