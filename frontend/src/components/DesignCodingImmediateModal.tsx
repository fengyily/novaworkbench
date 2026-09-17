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
// The shape intentionally mirrors ScheduleModal (same per-stage ModelSelect
// + ExecEnvSelect, same error panel, same submit/cancel layout) so the
// user sees a familiar two-section form. The only intentional differences
// are (a) no run_at, (b) every design-stage and coding-stage option has
// its own field (design_agent_server_id / coding_agent_server_id are kept
// distinct, matching the backend column shape) and (c) submit posts to
// the new /api/wizard/requirements/{id}/design-and-coding endpoint via
// wizardApi.startDesignAndCoding.

import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  wizardApi,
  type DesignCodingImmediateReq,
} from '../api/client';
import { ApiError } from '../api/client';
import ModelSelect from './ModelSelect';
import { ExecEnvSelect, type ExecEnvServer } from './ExecEnvSelect';

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

  const agentServerEmptyTitle =
    agentServers.length === 0
      ? t('schedules.modal.designAgentServerEmptyTitle')
      : '';

  return (
    <div className="modal-overlay" onClick={handleBackdropClick}>
      <div className="modal-box schedule-modal" onClick={e => e.stopPropagation()}>
        <div className="modal-header">
          <h3>{t('requirements.detail2.immediateDesignCodingTitle')}</h3>
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
            {t('schedules.modal.introDesignCoding')}
            {t('schedules.modal.introSuffix')}
          </p>

          {/* Design-stage section: mirrors the merged-mode design card from
              ScheduleModal. ModelSelect stage="architect" matches the
              stage chip color the user already sees on the manual design
              toolbar. */}
          <div className="modal-section-title">{t('schedules.modal.sectionDesignTitle')}</div>

          <div className="modal-field">
            <label>{t('schedules.modal.designModelLabel')}</label>
            <ModelSelect
              value={designModel}
              onChange={setDesignModel}
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

          <div className="modal-field">
            <label>{t('schedules.modal.designAgentServerLabelMerged')}</label>
            <ExecEnvSelect
              servers={agentServers}
              value={designAgentServerId}
              onChange={setDesignAgentServerId}
              disabled={submitting}
              title={agentServerEmptyTitle}
              localOptionLabel={t('schedules.modal.designAgentServerDefaultMerged')}
              style={{ minWidth: 160 }}
            />
            {agentServers.length === 0 && (
              <small style={{ color: '#64748B', marginTop: 4, display: 'block' }}>
                {t('schedules.modal.designAgentServerNoHint')}
              </small>
            )}
          </div>

          {/* Coding-stage section: mirrors the merged-mode coding card from
              ScheduleModal but uses independent state for each field so
              the two stages can pick different models / servers. */}
          <div className="modal-section-title">{t('schedules.modal.sectionCodingTitle')}</div>

          <div className="modal-field">
            <label>{t('schedules.modal.codingModelLabel')}</label>
            <ModelSelect
              value={codingModel}
              onChange={setCodingModel}
              stage="developer"
              working={submitting}
            />
            <small style={{ color: '#64748B', marginTop: 4, display: 'block' }}>
              {t('schedules.modal.modelHint')}
            </small>
          </div>

          <div className="modal-field">
            <label>{t('schedules.modal.codingAgentServerLabel')}</label>
            <ExecEnvSelect
              servers={agentServers}
              value={codingAgentServerId}
              onChange={setCodingAgentServerId}
              disabled={submitting}
              title={agentServerEmptyTitle}
              localOptionLabel={t('schedules.modal.codingAgentServerDefault')}
              style={{ minWidth: 160 }}
            />
            {agentServers.length === 0 && (
              <small style={{ color: '#64748B', marginTop: 4, display: 'block' }}>
                {t('schedules.modal.designAgentServerNoHint')}
              </small>
            )}
          </div>

          <div className="modal-field">
            <label htmlFor="immediate-branch">{t('schedules.modal.branchLabel')}</label>
            <input
              id="immediate-branch"
              className="form-input"
              value={branchName}
              onChange={e => setBranchName(e.target.value)}
              placeholder="feat/req-xxx"
              disabled={submitting}
            />
          </div>
          <div className="modal-field">
            <label htmlFor="immediate-base">{t('schedules.modal.baseBranchLabel')}</label>
            <input
              id="immediate-base"
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
            {submitting
              ? t('schedules.modal.submitting')
              : t('requirements.detail2.immediateLaunchBtn')}
          </button>
        </div>
      </div>
    </div>
  );
}

export default DesignCodingImmediateModal;
