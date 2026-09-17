// DesignCodingImmediateModal — immediately fires the chained "design +
// coding" run for one requirement.
//
// Parallel to <ScheduleModal taskType="design_and_coding"> but without a
// run_at picker: the user picks the two-stage configuration and the
// backend's wizard_immediate.go chains architect-design → start-coding
// synchronously (no scheduled_tasks row is ever written). The modal only
// collects the configuration; the design phase's JobStore id is returned
// to the parent so RequirementDetail can hook it into streamDesignJob
// immediately. The coding stage's job id lands on
// requirements.coding_job_id via the backend's chained callback; the
// page's existing active-jobs poll picks it up without further wiring.
//
// Layout language borrows from the RequirementDetail "启动开发会话"
// preflight panel: `.preflight-box` container, `.preflight-header`
// (eyebrow / title / subtitle), a `.flight-strip` mission summary
// showing the two stages' env + model side by side, then
// `.preflight-section` blocks (DESIGN / CODING) using the same field
// card (rail + chip + control) vocabulary the developer-stage toolbar
// already established. Boolean options use `.preflight-toggle` cards;
// the launch action lives in `.preflight-launch` so the button stays
// pinned at the bottom while the body scrolls.

import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  wizardApi,
  type DesignCodingImmediateReq,
} from '../api/client';
import { ApiError } from '../api/client';
import ModelSelect from './ModelSelect';
import { ExecEnvSelect, type ExecEnvServer } from './ExecEnvSelect';
import { IconArrowRight, IconBook, IconPin, IconRocket } from './icons';

interface Props {
  open: boolean;
  requirementId: string;
  requirementTitle?: string;
  // Pre-selected model ids (display values). '' = stage's default model.
  initialDesignModel?: string;
  initialCodingModel?: string;
  defaultBranchName?: string;
  defaultBaseBranch?: string;
  agentServers: ExecEnvServer[];
  // Initial dev mode (mirrors the picker on RequirementDetail). 'design' =
  // fresh session that hands the design doc to the Agent; 'session' =
  // resume the design session. '' = backend falls back to the requirement
  // row / 'session'.
  initialDevMode?: 'session' | 'design' | '';
  // Future revisions may lift claude_config state up here so the parent
  // can send design_claude_config_id / coding_claude_config_id
  // explicitly; until then the per-stage ModelSelect self-fetches.
  onLaunched: (designJobId: string) => void;
  onClose: () => void;
}

export function DesignCodingImmediateModal({
  open,
  requirementId,
  requirementTitle,
  initialDesignModel,
  initialCodingModel,
  defaultBranchName,
  defaultBaseBranch,
  agentServers,
  initialDevMode,
  onLaunched,
  onClose,
}: Props) {
  const { t } = useTranslation();
  // Design-stage state.
  const [designModel, setDesignModel] = useState(initialDesignModel || '');
  const [designAgentServerId, setDesignAgentServerId] = useState('');
  const [readKnowledge, setReadKnowledge] = useState(false);
  // Coding-stage state.
  const [codingModel, setCodingModel] = useState(initialCodingModel || '');
  const [codingAgentServerId, setCodingAgentServerId] = useState('');
  const [branchName, setBranchName] = useState(defaultBranchName || '');
  const [baseBranch, setBaseBranch] = useState(defaultBaseBranch || '');
  const [splitTasks, setSplitTasks] = useState(true);
  // Dev mode: defaults to 'design' (matches the RequirementDetail picker).
  const [devMode, setDevMode] = useState<'session' | 'design'>(
    initialDevMode === 'session' ? 'session' : 'design'
  );

  const [submitting, setSubmitting] = useState(false);
  const [errorMsg, setErrorMsg] = useState('');

  if (!open) return null;

  const handleSubmit = async () => {
    setSubmitting(true);
    setErrorMsg('');
    try {
      const body: DesignCodingImmediateReq = {
        read_knowledge: readKnowledge,
        design_model: designModel || undefined,
        design_agent_server_id: designAgentServerId || undefined,
        coding_model: codingModel || undefined,
        coding_agent_server_id: codingAgentServerId || undefined,
        branch_name: branchName,
        base_branch: baseBranch,
        split_tasks: splitTasks,
        // Mirrors the devMode picker on RequirementDetail. The backend
        // (wizard_coding.go UpdateDevMode) uses the requirement row's
        // persisted value when DevMode is empty; we always send an
        // explicit value here so a stale row can't override the user's
        // modal choice.
        dev_mode: devMode,
      };
      const resp = await wizardApi.startDesignAndCoding(requirementId, body);
      onLaunched(resp.design_job_id);
    } catch (err) {
      // Translate stable error.code values into friendly, locale-aware
      // messages. The backend surfaces structured {code, message} on every
      // 4xx via the standard envelope; ApiError exposes `code` directly.
      // Anything we don't recognize falls back to the generic submit-failed
      // string already in schedules.modal.
      let code = '';
      if (err instanceof ApiError) {
        code = err.code;
      } else if (typeof (err as any)?.message === 'string') {
        // Legacy compatibility — older call sites split the message string
        // ("CODE: detail") because ApiError didn't exist yet. Keep that
        // path working so a non-ApiError throw still maps to something
        // sensible.
        code = (err as any).message.split(':')?.[0] ?? '';
      }
      switch (code) {
        case 'IDEA_NOT_DEVELOPABLE':
          setErrorMsg(t('schedules.modal.errIdeaNotDevelopable'));
          break;
        case 'TERMINAL':
          setErrorMsg(t('requirements.detail2.immediateErrTerminal'));
          break;
        case 'DESIGN_JOB_ACTIVE':
        case 'CODING_JOB_ACTIVE':
          setErrorMsg(t('requirements.detail2.immediateErrBusy'));
          break;
        case 'INVALID_STATUS':
          setErrorMsg(t('requirements.detail2.immediateErrStatus'));
          break;
        case 'NO_SESSION':
          setErrorMsg(t('schedules.modal.errNoSession'));
          break;
        default:
          setErrorMsg(t('schedules.modal.errGeneric'));
      }
    } finally {
      setSubmitting(false);
    }
  };

  // Click-outside-to-dismiss — disabled while submitting so the user
  // can't lose a queued design+coding chain mid-create.
  const handleBackdropClick = () => {
    if (!submitting) onClose();
  };

  // Flight-strip display values. Mirrors the preflight modal's
  // monospace mission summary: shorten model ids to "default" when the
  // stage hasn't been picked yet, fall back to "local" when no agent
  // server is selected.
  const designEnvName = designAgentServerId
    ? (agentServers.find(s => s.id === designAgentServerId)?.name || 'remote')
    : t('requirements.detail2.preflightLocalExec');
  const designModelName = designModel || 'default';
  const codingEnvName = codingAgentServerId
    ? (agentServers.find(s => s.id === codingAgentServerId)?.name || 'remote')
    : t('requirements.detail2.preflightLocalExec');
  const codingModelName = codingModel || 'default';
  const stripBase = baseBranch || 'main';
  const stripNew = branchName || `feat/${requirementId.replace(/^req_/, '')}`;

  return (
    <div className="modal-overlay" onClick={handleBackdropClick}>
      <div className="modal-box preflight-box" onClick={e => e.stopPropagation()}>
        <div className="preflight-header">
          <div className="preflight-eyebrow">{t('requirements.detail2.immediateEyebrow')}</div>
          <h3 className="preflight-title">{t('requirements.detail2.immediateDesignCodingTitle')}</h3>
          <p className="preflight-subtitle">
            {t('requirements.detail2.immediateSubtitle', {
              title: requirementTitle || t('requirements.detail2.preflightSubtitle'),
            })}
          </p>
        </div>

        {/* Flight strip — the signature element of the preflight family.
            A monospace HUD that compresses both stages' env + model into
            one line the user can scan before clicking launch. Same
            visual vocabulary as the developer-stage preflight modal —
            GIT/EXEC legs → READY dot — extended to a two-stage DESIGN →
            CODING chain. */}
        <div
          className="flight-strip"
          aria-label={t('requirements.detail2.immediateFlightAria')}
        >
          <span className="flight-leg">
            <span className="flight-leg-label">DESIGN</span>
            <span className="flight-leg-value" title={designEnvName}>{designEnvName}</span>
            <span className="flight-arrow">·</span>
            <span className="flight-leg-value" title={designModelName}>{designModelName}</span>
          </span>
          <IconArrowRight
            size={12}
            className="flight-arrow"
            aria-hidden
            style={{ margin: '0 10px' }}
          />
          <span className="flight-leg">
            <span className="flight-leg-label">CODING</span>
            <span className="flight-leg-value" title={codingEnvName}>{codingEnvName}</span>
            <span className="flight-arrow">·</span>
            <span className="flight-leg-value" title={codingModelName}>{codingModelName}</span>
          </span>
          <span className="flight-sep">·</span>
          <span className="flight-leg">
            <span className="flight-leg-label">GIT</span>
            <span className="flight-leg-value" title={stripBase}>{stripBase}</span>
            <IconArrowRight size={11} className="flight-arrow" aria-hidden />
            <span className="flight-leg-value" title={stripNew}>{stripNew}</span>
          </span>
          <span className="flight-ready" aria-live="polite">
            <span className="flight-ready-dot" />
            {t('requirements.detail2.preflightLaunchBtn').toUpperCase()}
          </span>
        </div>

        <div className="preflight-body">
          {/* Design-stage section — violet accent rail mirrors the
              architect stage on ModelSelect and the EXEC accent on the
              developer preflight modal. */}
          <div className="preflight-section">
            <div className="preflight-section-label">{t('requirements.detail2.immediateDesignSection')}</div>

            <div className="modal-field">
              {/* ModelSelect renders its own stage chip + rail, so it
                  stands in for the field card without an extra wrap. */}
              <ModelSelect
                value={designModel}
                onChange={setDesignModel}
                stage="architect"
                working={submitting}
                label={t('schedules.modal.designModelLabel')}
              />
              <small style={{ color: 'var(--color-text-muted)', marginTop: 4, display: 'block' }}>
                {t('schedules.modal.modelHint')}
              </small>
            </div>

            {/* Read-knowledge toggle — same card styling as the
                developer preflight modal. Knowledge pre-read runs once
                before the design stage so the architect persona has the
                context loaded. */}
            <label className={`preflight-toggle ${readKnowledge ? 'is-checked' : ''}`}>
              <input
                type="checkbox"
                checked={readKnowledge}
                onChange={e => setReadKnowledge(e.target.checked)}
                disabled={submitting}
              />
              <div className="preflight-toggle-body">
                <div className="preflight-toggle-title">
                  <IconBook size={14} className="icon-mr" />
                  {t('requirements.detail2.preflightReadKnowledge')}
                </div>
                <div className="preflight-toggle-desc">
                  {t('requirements.detail2.preflightReadKnowledgeHint')}
                </div>
              </div>
            </label>

            <div className="modal-field">
              <label>{t('requirements.detail2.preflightExecEnvLabel')}</label>
              <div className="preflight-field-card preflight-field-card--exec">
                <span className="preflight-field-chip" aria-hidden="true">ENV</span>
                <ExecEnvSelect
                  className="form-input preflight-field-input"
                  servers={agentServers}
                  value={designAgentServerId}
                  onChange={setDesignAgentServerId}
                  disabled={submitting}
                  title={
                    agentServers.length === 0
                      ? t('requirements.detail2.preflightAgentEmptyTitle')
                      : ''
                  }
                  localOptionLabel={t('requirements.detail2.preflightLocalExec')}
                />
              </div>
              {agentServers.length === 0 && (
                <div style={{ fontSize: 12, color: 'var(--color-text-muted)', marginTop: 4 }}>
                  {t('requirements.detail2.preflightNoAgentHint')}
                </div>
              )}
            </div>
          </div>

          {/* Coding-stage section — cyan accent rail mirrors the developer
              stage on ModelSelect and the GIT accent on the developer
              preflight modal. */}
          <div className="preflight-section">
            <div className="preflight-section-label">{t('requirements.detail2.immediateCodingSection')}</div>

            <div className="modal-field">
              <ModelSelect
                value={codingModel}
                onChange={setCodingModel}
                stage="developer"
                working={submitting}
                label={t('schedules.modal.codingModelLabel')}
              />
              <small style={{ color: 'var(--color-text-muted)', marginTop: 4, display: 'block' }}>
                {t('schedules.modal.modelHint')}
              </small>
            </div>

            <div className="modal-field">
              <label>{t('requirements.detail2.preflightExecEnvLabel')}</label>
              <div className="preflight-field-card preflight-field-card--git">
                <span className="preflight-field-chip" aria-hidden="true">ENV</span>
                <ExecEnvSelect
                  className="form-input preflight-field-input"
                  servers={agentServers}
                  value={codingAgentServerId}
                  onChange={setCodingAgentServerId}
                  disabled={submitting}
                  title={
                    agentServers.length === 0
                      ? t('requirements.detail2.preflightAgentEmptyTitle')
                      : ''
                  }
                  localOptionLabel={t('requirements.detail2.preflightLocalExec')}
                />
              </div>
              {agentServers.length === 0 && (
                <div style={{ fontSize: 12, color: 'var(--color-text-muted)', marginTop: 4 }}>
                  {t('requirements.detail2.preflightNoAgentHint')}
                </div>
              )}
            </div>

            <div className="modal-field">
              <label>{t('requirements.detail2.preflightBaseBranchLabel')}</label>
              <div className="preflight-field-card preflight-field-card--git">
                <span className="preflight-field-chip" aria-hidden="true">BASE</span>
                <input
                  className="form-input preflight-field-input"
                  value={baseBranch}
                  onChange={e => setBaseBranch(e.target.value)}
                  placeholder="main"
                  disabled={submitting}
                />
              </div>
            </div>
            <div className="modal-field">
              <label>{t('requirements.detail2.preflightNewBranchLabel')}</label>
              <div className="preflight-field-card preflight-field-card--git">
                <span className="preflight-field-chip" aria-hidden="true">NEW</span>
                <input
                  className="form-input preflight-field-input"
                  value={branchName}
                  onChange={e => setBranchName(e.target.value)}
                  placeholder={`feat/${requirementId.replace(/^req_/, '')}`}
                  disabled={submitting}
                />
              </div>
            </div>

            <label className={`preflight-toggle ${splitTasks ? 'is-checked' : ''}`}>
              <input
                type="checkbox"
                checked={splitTasks}
                onChange={e => setSplitTasks(e.target.checked)}
                disabled={submitting}
              />
              <div className="preflight-toggle-body">
                <div className="preflight-toggle-title">
                  <IconPin size={14} className="icon-mr" />
                  {t('requirements.detail2.preflightSplitTasks')}
                </div>
                <div className="preflight-toggle-desc">
                  {t('requirements.detail2.preflightSplitTasksHint')}
                </div>
              </div>
            </label>

            {/* Dev-mode picker — mirrors the developer preflight modal's
                radio group inside a block-form toggle card. */}
            <div className="preflight-toggle" style={{ display: 'block' }}>
              <div className="preflight-toggle-body">
                <div className="preflight-toggle-title">{t('requirements.detail2.preflightDevModeTitle')}</div>
                <div className="preflight-toggle-desc" style={{ marginBottom: 8 }}>
                  {t('requirements.detail2.preflightDevModeHint')}
                </div>
                <div style={{ display: 'flex', gap: 16, flexWrap: 'wrap' }}>
                  <label style={{ display: 'inline-flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}>
                    <input
                      type="radio"
                      name="immediateDevMode"
                      value="design"
                      checked={devMode === 'design'}
                      onChange={() => setDevMode('design')}
                      disabled={submitting}
                    />
                    {t('requirements.detail2.preflightDevModeDesign')}
                  </label>
                  <label style={{ display: 'inline-flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}>
                    <input
                      type="radio"
                      name="immediateDevMode"
                      value="session"
                      checked={devMode === 'session'}
                      onChange={() => setDevMode('session')}
                      disabled={submitting}
                    />
                    {t('requirements.detail2.preflightDevModeSession')}
                  </label>
                </div>
              </div>
            </div>
          </div>

          {errorMsg && (
            <div className="modal-risk-panel" style={{ marginTop: 12 }}>
              <div className="modal-risk-title">{t('schedules.modal.failTitle')}</div>
              <div className="modal-risk-desc">{errorMsg}</div>
            </div>
          )}
        </div>

        <div className="preflight-launch">
          <button className="btn-launch" onClick={handleSubmit} disabled={submitting}>
            <IconRocket size={14} className="btn-icon" />
            {submitting
              ? t('schedules.modal.submitting')
              : t('requirements.detail2.immediateLaunchBtn')}
          </button>
          <button className="btn-cancel" onClick={onClose} disabled={submitting}>
            {t('requirements.detail2.btnCancel')}
          </button>
        </div>
      </div>
    </div>
  );
}

export default DesignCodingImmediateModal;