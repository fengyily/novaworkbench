import { useState, useEffect, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import {
  issueMonitorApi, agentServersApi,
  type IssueMonitor, type UpdateIssueMonitorReq,
  type IssueMonitorLaunchMode, type IssueMonitorRecurrence,
  type IssuePreview,
} from '../api/client';
import { ExecEnvSelect, type ExecEnvServer } from '../components/ExecEnvSelect';
import ModelSelect from '../components/ModelSelect';
import { IconHand, IconRocket, IconClock } from '../components/icons';
import { errorMessage } from '../utils/errMsg';
import { fmtTime } from '../utils/intl';
import { WEEK_LABELS } from '../utils/time';
import './ProjectIssueMonitor.css';

// Per-project Issue-monitor settings tab. Mirrors the SettingsSubTask
// "单表单 + sticky-底部保存" layout, and its 执行方式 / 开发流程 / 阶段配置
// block mirrors CreateRequirementForm's 启动计划 (LaunchPlanSection) — the
// two surfaces configure the same dispatch pipeline, so they share
// vocabulary and the same backend path (handler.dispatchLaunch).
//
// It does NOT reuse LaunchPlanSection as a component: that one owns its
// internal state and only exposes onChange(spec|null), so it cannot hydrate
// from a persisted monitor row. The markup is replicated instead, with the
// CSS re-declared under .issue-monitor (LaunchPlanSection's .launch-* rules
// are scoped under .create-req-form).
//
// State model:
//   - monitor (IssueMonitor | null): the server-side row; null before
//     GET resolves OR when the project has no monitor configured yet
//     (404 → we seed an "empty" form so the first PUT creates).
//   - form (UpdateIssueMonitorReq): the user's in-progress edits,
//     committed to the server only on Save.
//   - serverList (ExecEnvServer[]): the ready servers for the ExecEnv
//     dropdown. Filter to ready so the operator never picks a target
//     that would fail at launch time.
export default function ProjectIssueMonitor({ projectId }: { projectId: string }) {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [info, setInfo] = useState('');
  const [savedAt, setSavedAt] = useState('');
  const [monitor, setMonitor] = useState<IssueMonitor | null>(null);
  const [form, setForm] = useState<UpdateIssueMonitorReq>(emptyForm());
  const [serverList, setServerList] = useState<ExecEnvServer[]>([]);
  const [testResult, setTestResult] = useState<{ issues: IssuePreview[]; count: number } | null>(null);
  const [testing, setTesting] = useState(false);

  const loadAll = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      const [mon, srvs] = await Promise.all([
        issueMonitorApi.get(projectId).catch((err: unknown) => {
          // 404 → no monitor yet: seed an empty form. Anything else bubbles.
          const code = (err as { code?: string }).code;
          if (code === 'NOT_CONFIGURED' || code === 'NOT_FOUND') return null;
          throw err;
        }),
        agentServersApi.list().catch(() => []),
      ]);
      setMonitor(mon);
      if (mon) setForm(toForm(mon));
      setServerList(((srvs as unknown as { items?: ExecEnvServer[] }).items ?? []) as ExecEnvServer[]);
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setLoading(false);
    }
  }, [projectId]);

  useEffect(() => { loadAll(); }, [loadAll]);

  const handleSave = async () => {
    setSaving(true);
    setError('');
    setInfo('');
    try {
      // Stamp the tz at save time so "每天 09:00" means the operator's local
      // wall clock, not the server's. The backend projects recur_time onto a
      // UTC instant using this value.
      const payload: UpdateIssueMonitorReq = {
        ...form,
        recur_tz: Intl.DateTimeFormat().resolvedOptions().timeZone,
      };
      const updated = await issueMonitorApi.upsert(projectId, payload);
      setMonitor(updated);
      setForm(toForm(updated));
      setSavedAt(fmtTime(new Date()));
      setInfo(t('settings.issueMonitor.toastSaved'));
    } catch (err: unknown) {
      setError(t('settings.issueMonitor.errSave', { message: errorMessage(err) }));
    } finally {
      setSaving(false);
    }
  };

  const handleDelete = async () => {
    if (!window.confirm(t('settings.issueMonitor.confirmDelete'))) return;
    setError('');
    setInfo('');
    try {
      await issueMonitorApi.delete(projectId);
      setMonitor(null);
      setForm(emptyForm());
      setTestResult(null);
      setInfo(t('settings.issueMonitor.toastDeleted'));
    } catch (err: unknown) {
      setError(errorMessage(err));
    }
  };

  const handleTest = async () => {
    setTesting(true);
    setTestResult(null);
    setError('');
    try {
      const resp = await issueMonitorApi.test(projectId);
      setTestResult({ issues: resp.issues, count: resp.count });
    } catch (err: unknown) {
      setError(t('settings.issueMonitor.errTest', { message: errorMessage(err) }));
    } finally {
      setTesting(false);
    }
  };

  if (loading) {
    return <div className="settings-empty">{t('settings.issueMonitor.loading')}</div>;
  }

  const dispatchEnabled = form.launch_mode !== 'create_only';
  const scheduled = form.launch_mode === 'scheduled';
  const weekly = form.recurrence === 'weekly';
  // Mirrors the backend's normalize guard: a scheduled monitor needs a
  // well-formed recur_time, and weekly additionally needs >=1 weekday. The
  // server re-validates — this only keeps Save from firing a doomed PUT.
  const scheduleValid = !scheduled
    || (form.recur_time.trim() !== '' && (!weekly || form.recur_days.trim() !== ''));
  const canSave = form.label.trim() !== '' && scheduleValid;

  return (
    <div className="settings-section issue-monitor">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">{t('settings.issueMonitor.title')}</h3>
          <p className="settings-section-desc">{t('settings.issueMonitor.desc')}</p>
        </div>
      </div>

      {error && <div className="form-error">{error}</div>}
      {info && <div className="form-info">{info}</div>}

      {/* ── Trigger ─────────────────────────────────────────────── */}
      <div className="form-group">
        <label>
          <input
            type="checkbox"
            checked={form.active}
            onChange={e => setForm({ ...form, active: e.target.checked })}
          /> {t('settings.issueMonitor.active')}
        </label>
        <span className="form-hint">{t('settings.issueMonitor.activeHint')}</span>
      </div>

      <div className="form-group">
        <label>{t('settings.issueMonitor.label')}</label>
        <input
          className="form-input"
          type="text"
          value={form.label}
          placeholder="needs-fix"
          onChange={e => setForm({ ...form, label: e.target.value })}
        />
        <span className="form-hint">{t('settings.issueMonitor.labelHint')}</span>
      </div>

      <div className="form-group">
        <label>{t('settings.issueMonitor.interval')}</label>
        <input
          className="form-input"
          type="number"
          min={1}
          max={10080}
          value={form.interval_minutes}
          onChange={e => setForm({ ...form, interval_minutes: Math.max(1, Number(e.target.value) || 1) })}
        />
        <span className="form-hint">{t('settings.issueMonitor.intervalHint')}</span>
      </div>

      {/* ── 执行方式 ────────────────────────────────────────────── */}
      <div className="form-group">
        <label>{t('settings.issueMonitor.launchMode')}</label>
        <div className="im-mode-row" role="radiogroup" aria-label={t('settings.issueMonitor.launchMode')}>
          {LAUNCH_MODES.map(m => {
            const Icon = m.icon;
            return (
              <button
                key={m.value}
                type="button"
                role="radio"
                aria-checked={form.launch_mode === m.value}
                className={`im-mode-card${form.launch_mode === m.value ? ' selected' : ''}`}
                onClick={() => setForm({ ...form, launch_mode: m.value })}
              >
                <span className="im-mode-card-head">
                  <Icon width={16} height={16} />
                  {t(`settings.issueMonitor.launchModeOptions.${m.labelKey}`)}
                </span>
                <span className="im-mode-card-note">
                  {t(`settings.issueMonitor.launchModeNotes.${m.labelKey}`)}
                </span>
              </button>
            );
          })}
        </div>
        <span className="form-hint">{t('settings.issueMonitor.launchModeHint')}</span>
      </div>

      {/* ── 定时 details (only when scheduled) ──────────────────── */}
      {scheduled && (
        <div className="im-schedule">
          <div>
            <label>{t('schedules.modal.recurrenceLabel')}</label>
            <div className="im-segment" role="radiogroup" aria-label={t('schedules.modal.recurrenceLabel')}>
              {RECURRENCES.map(r => (
                <button
                  key={r}
                  type="button"
                  role="radio"
                  aria-checked={form.recurrence === r}
                  className={`im-segment-tab${form.recurrence === r ? ' selected' : ''}`}
                  onClick={() => setForm({ ...form, recurrence: r })}
                >
                  {t(`schedules.modal.recurrence.${r}`)}
                </button>
              ))}
            </div>
          </div>

          <div>
            <label htmlFor="im-recur-time">{t('schedules.modal.recurTimeLabel')}</label>
            <input
              id="im-recur-time"
              className="form-input"
              type="time"
              value={form.recur_time}
              onChange={e => setForm({ ...form, recur_time: e.target.value })}
            />
            <span className="form-hint">{t('schedules.modal.recurTimeHint')}</span>
          </div>

          {weekly && (
            <div>
              <label>{t('schedules.modal.recurDaysLabel')}</label>
              <div className="im-weekdays">
                {WEEK_LABELS.map((label, idx) => {
                  const dayNum = WEEKDAY_INDEX_TO_DAYNUM[idx];
                  const selected = parseDays(form.recur_days).has(dayNum);
                  return (
                    <button
                      key={dayNum}
                      type="button"
                      aria-pressed={selected}
                      className={`im-weekday${selected ? ' selected' : ''}`}
                      onClick={() => setForm({ ...form, recur_days: toggleDay(form.recur_days, dayNum) })}
                    >
                      {label}
                    </button>
                  );
                })}
              </div>
            </div>
          )}

          <span className="form-hint">{t('settings.issueMonitor.scheduleHint')}</span>
        </div>
      )}

      {/* ── 开发流程 (only when dispatching) ────────────────────── */}
      {dispatchEnabled && (
        <div className="form-group">
          <label>{t('components.createRequirement.flow')}</label>
          <div className="im-segment" role="radiogroup" aria-label={t('components.createRequirement.flow')}>
            <button
              type="button"
              role="radio"
              aria-checked={!form.skip_design}
              className={`im-segment-tab${!form.skip_design ? ' selected' : ''}`}
              onClick={() => setForm({ ...form, skip_design: false })}
            >
              {t('components.createRequirement.flowSkipAnalysis')}
            </button>
            <button
              type="button"
              role="radio"
              aria-checked={form.skip_design}
              className={`im-segment-tab${form.skip_design ? ' selected' : ''}`}
              onClick={() => setForm({ ...form, skip_design: true })}
            >
              {t('components.createRequirement.flowDirect')}
            </button>
          </div>
          <span className="form-hint">{t('settings.issueMonitor.skipDesignHint')}</span>
        </div>
      )}

      {/* ── 设计阶段 (方案开发 only — 直接开发 has no design stage) ── */}
      {dispatchEnabled && !form.skip_design && (
        <div className="im-stage im-stage--design">
          <div className="im-stage-head">
            <span className="im-stage-chip">{t('components.createRequirement.launchPlan.designSection')}</span>
            <span className="im-stage-hint">{t('components.createRequirement.launchPlan.designSectionHint')}</span>
          </div>
          <div className="im-stage-field">
            <ModelSelect
              stage="architect"
              value={form.design_model}
              onChange={v => setForm({ ...form, design_model: v })}
              configId={form.design_claude_config_id}
              onConfigChange={v => setForm({ ...form, design_claude_config_id: v })}
              label={t('schedules.modal.designModelLabel')}
            />
            <span className="form-hint">{t('schedules.modal.modelHint')}</span>
          </div>
          <div className="im-stage-field">
            <label>{t('schedules.modal.designAgentServerLabelMerged')}</label>
            <ExecEnvSelect
              servers={serverList}
              value={form.design_agent_server_id}
              onChange={v => setForm({ ...form, design_agent_server_id: v })}
              localOptionLabel={t('schedules.modal.designAgentServerDefaultMerged')}
            />
          </div>
        </div>
      )}

      {/* ── 开发阶段 ────────────────────────────────────────────── */}
      {dispatchEnabled && (
        <div className="im-stage im-stage--coding">
          <div className="im-stage-head">
            <span className="im-stage-chip">{t('components.createRequirement.launchPlan.codingSection')}</span>
            <span className="im-stage-hint">{t('components.createRequirement.launchPlan.codingSectionHint')}</span>
          </div>
          <div className="im-stage-field">
            <ModelSelect
              stage="developer"
              value={form.coding_model}
              onChange={v => setForm({ ...form, coding_model: v })}
              configId={form.coding_claude_config_id}
              onConfigChange={v => setForm({ ...form, coding_claude_config_id: v })}
              label={t('schedules.modal.codingModelLabel')}
            />
            <span className="form-hint">{t('schedules.modal.modelHint')}</span>
          </div>
          <div className="im-stage-field">
            <label>{t('schedules.modal.codingAgentServerLabel')}</label>
            <ExecEnvSelect
              servers={serverList}
              value={form.coding_agent_server_id}
              onChange={v => setForm({ ...form, coding_agent_server_id: v })}
              localOptionLabel={t('schedules.modal.codingAgentServerDefault')}
            />
          </div>
        </div>
      )}

      {/* ── Post-development actions ────────────────────────────── */}
      <div className="form-group">
        <label className="im-check">
          <input
            type="checkbox"
            checked={form.auto_push}
            onChange={e => setForm({ ...form, auto_push: e.target.checked })}
          />
          <span>{t('settings.issueMonitor.autoPush')}</span>
        </label>
        <span className="form-hint">{t('settings.issueMonitor.autoPushHint')}</span>
      </div>

      <div className="form-group">
        <label className="im-check">
          <input
            type="checkbox"
            checked={form.comment_on_pr}
            disabled={!!(monitor && monitor.comment_posted)}
            onChange={e => setForm({ ...form, comment_on_pr: e.target.checked })}
          />
          <span>{t('settings.issueMonitor.commentOnPR')}</span>
        </label>
        <span className="form-hint">{t('settings.issueMonitor.commentOnPRHint')}</span>
        {!!(monitor && monitor.comment_posted) && (
          <span className="form-hint" style={{ color: '#10B981' }}>
            ✓ {t('settings.issueMonitor.commentPosted')}
          </span>
        )}
      </div>

      {/* Test preview row. */}
      <div className="form-group">
        <button
          type="button"
          className="btn btn-secondary"
          onClick={handleTest}
          disabled={testing || !form.label.trim()}
          style={{ marginRight: 12 }}
        >
          {testing ? t('settings.issueMonitor.testing') : t('settings.issueMonitor.test')}
        </button>
        {testResult && (
          <span className="form-hint" style={{ marginLeft: 8 }}>
            {testResult.count > 0
              ? t('settings.issueMonitor.testResultCount', { count: testResult.count })
              : t('settings.issueMonitor.testEmpty')}
          </span>
        )}
        {testResult && testResult.count > 0 && (
          <ul style={{ marginTop: 8, listStyle: 'none', padding: 0 }}>
            {testResult.issues.map((iss, i) => (
              <li key={i} style={{ padding: '4px 0', fontSize: 13 }}>
                <a href={iss.html_url} target="_blank" rel="noreferrer">#{iss.number}</a> ·{' '}
                <span>{iss.title}</span>
                {' · '}<span style={{ color: iss.state === 'open' ? '#10B981' : '#64748B' }}>{iss.state}</span>
              </li>
            ))}
          </ul>
        )}
      </div>

      {/* Sticky-bottom action row. */}
      <div className="settings-actions">
        <button
          className="btn btn-primary"
          onClick={handleSave}
          disabled={saving || !canSave}
          title={!scheduleValid ? t('schedules.modal.errWeekdaysRequired') : undefined}
        >
          {saving ? t('settings.issueMonitor.saving') : t('settings.issueMonitor.save')}
        </button>
        {savedAt && (
          <span className="settings-saved-at">{t('settings.issueMonitor.savedAt', { time: savedAt })}</span>
        )}
        {monitor && (
          <button
            className="btn btn-secondary"
            onClick={handleDelete}
            disabled={saving}
            style={{ marginLeft: 8 }}
          >
            {t('settings.issueMonitor.close')}
          </button>
        )}
      </div>
    </div>
  );
}

// 执行方式 cards, in display order. labelKey indexes into
// settings.issueMonitor.launchModeOptions / launchModeNotes.
const LAUNCH_MODES: { value: IssueMonitorLaunchMode; icon: typeof IconHand; labelKey: string }[] = [
  { value: 'create_only', icon: IconHand, labelKey: 'createOnly' },
  { value: 'immediate', icon: IconRocket, labelKey: 'immediate' },
  { value: 'scheduled', icon: IconClock, labelKey: 'scheduled' },
];

// The only two repeat forms the backend accepts. 'once' is deliberately
// absent: the poller resolves this window into a single concrete run_at, so
// an absolute time would carry no meaning.
const RECURRENCES: IssueMonitorRecurrence[] = ['daily', 'weekly'];

// Monday-first display order → the weekday number used on the wire
// (0 = Sunday, the JS getDay convention). Same mapping LaunchPlanSection
// uses, so the two weekday pickers agree.
const WEEKDAY_INDEX_TO_DAYNUM = [1, 2, 3, 4, 5, 6, 0];

// parseDays reads the CSV wire format into a Set of weekday numbers,
// ignoring anything malformed.
function parseDays(csv: string): Set<number> {
  const out = new Set<number>();
  for (const part of csv.split(',')) {
    const n = Number(part.trim());
    if (part.trim() !== '' && Number.isInteger(n) && n >= 0 && n <= 6) out.add(n);
  }
  return out;
}

// toggleDay flips one weekday and re-serializes ascending — the backend
// validates the CSV but does not require an order, and a stable order keeps
// the payload diffable.
function toggleDay(csv: string, day: number): string {
  const days = parseDays(csv);
  if (days.has(day)) days.delete(day);
  else days.add(day);
  return Array.from(days).sort((a, b) => a - b).join(',');
}

// Empty form defaults — no dispatch until the user opts in, matching the
// backend's launch_mode default so the first PUT "saves the defaults"
// without surprising the user.
function emptyForm(): UpdateIssueMonitorReq {
  return {
    label: '',
    launch_mode: 'create_only',
    skip_design: false,
    design_model: '',
    design_claude_config_id: '',
    design_agent_server_id: '',
    coding_model: '',
    coding_claude_config_id: '',
    coding_agent_server_id: '',
    recurrence: 'daily',
    recur_time: '09:00',
    recur_days: '',
    recur_tz: '',
    auto_push: false,
    comment_on_pr: false,
    interval_minutes: 5,
    active: false,
  };
}

// Hydrate the form from the persisted row. Field-by-field copy so a
// hidden detail (last_run_at, comment_posted) doesn't leak into the
// form state.
function toForm(mon: IssueMonitor): UpdateIssueMonitorReq {
  return {
    label: mon.label,
    launch_mode: mon.launch_mode,
    skip_design: mon.skip_design,
    design_model: mon.design_model,
    design_claude_config_id: mon.design_claude_config_id,
    design_agent_server_id: mon.design_agent_server_id,
    coding_model: mon.coding_model,
    coding_claude_config_id: mon.coding_claude_config_id,
    coding_agent_server_id: mon.coding_agent_server_id,
    recurrence: mon.recurrence,
    recur_time: mon.recur_time || '09:00',
    recur_days: mon.recur_days,
    recur_tz: mon.recur_tz,
    auto_push: mon.auto_push,
    comment_on_pr: mon.comment_on_pr,
    interval_minutes: mon.interval_minutes || 5,
    active: mon.active,
  };
}
