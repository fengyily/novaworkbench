// DesignPreflightFields — the shared "DESIGN stage" field block.
//
// Three surfaces can start an architect-design run and each of them needs
// the SAME set of knobs:
//
//   (1) "generate design" preflight modal  (RequirementDetail.tsx)
//   (2) "design + code now" modal          (DesignCodingImmediateModal.tsx)
//   (3) "regenerate design" preflight modal (RequirementDetail.tsx)
//
// They used to hand-roll the same five fields (model + config / reasoning
// effort / read-knowledge toggle / designer-context toggle / execution
// environment + sync mode) and drifted apart: ③ had no effort picker at
// all, ① had neither model nor environment nor effort, and ① used a plain
// `.merge-check` checkbox that — because the styling is scoped to
// `.merge-modal .merge-check` — rendered completely unstyled.
//
// Extracting the block makes the drift structurally impossible: one
// implementation, three call sites.
//
// THE FIELD ORDER IS FIXED (model → effort → knowledge → role context →
// env → sync mode). It was copied verbatim from the LaunchSpec modal's
// DESIGN section, which is the reference implementation the users already
// know; changing it would be a visual regression.
//
// The component deliberately does NOT import any CSS: `.preflight-*` and
// `.effort-bar*` live in RequirementDetail.css / index.css, loaded by the
// page. All three call sites live inside the RequirementDetail page.
//
// EFFORT SEMANTICS (three distinct values — do not conflate them):
//
//   * `effort`        — the RAW pick. null means "the user never picked
//                       one" and the field must stay OFF the wire so the
//                       backend can resolve the configured stage default.
//   * `effortValue`   — the DISPLAY value (raw pick ?? configured stage
//                       default). EffortBar requires a concrete tier.
//   * the wire value  — always `...effortPick(rawPick)`. Sending the
//                       display value would arrive as an explicit pick and
//                       bypass the backend's configured-default layer
//                       (scripts/check-effort-picks.mjs enforces this).

import { useTranslation } from 'react-i18next';
import ModelSelect from './ModelSelect';
import EffortBar, { type EffortTier } from './EffortBar';
import { ExecEnvSelect, type ExecEnvServer } from './ExecEnvSelect';
import { SyncModeSelect } from './SyncModeSelect';
import { IconArchitect, IconBook } from './icons';

export interface DesignPreflightFieldsProps {
  // ── Model + claude config ────────────────────────────────────────────
  model: string;
  onModelChange: (v: string) => void;
  configId: string;
  onConfigChange: (v: string) => void;
  // True while the design run is in flight — paints the model card amber.
  working?: boolean;

  // ── Reasoning effort ────────────────────────────────────────────────
  /**
   * Called with the newly picked tier. The RAW pick itself is NOT a prop:
   * it only matters at the wire layer (`...effortPick(rawPick)`), which
   * lives in the caller. What this component renders is the DISPLAY value.
   */
  onEffortChange: (v: EffortTier) => void;
  /** DISPLAY value = raw pick ?? configured stage default. Must be a concrete tier. */
  effortValue: EffortTier;
  effortDisabled: boolean;
  /** EffortBar's id — doubles as the radio group's `name`, so it must be unique per page. */
  effortId: string;

  // ── Toggles ─────────────────────────────────────────────────────────
  readKnowledge: boolean;
  onReadKnowledgeChange: (v: boolean) => void;
  /** Overrides the knowledge-toggle description (defaults to the shared hint). */
  readKnowledgeHint?: string;
  useRoleContext: boolean;
  onUseRoleContextChange: (v: boolean) => void;

  // ── Execution environment ───────────────────────────────────────────
  agentServers: ExecEnvServer[];
  agentServerId: string;
  onAgentServerIdChange: (v: string) => void;
  syncMode: '' | 'local';
  onSyncModeChange: (v: '' | 'local') => void;

  /** Disables every control in the block. */
  disabled: boolean;
}

export function DesignPreflightFields({
  model,
  onModelChange,
  configId,
  onConfigChange,
  working = false,
  onEffortChange,
  effortValue,
  effortDisabled,
  effortId,
  readKnowledge,
  onReadKnowledgeChange,
  readKnowledgeHint,
  useRoleContext,
  onUseRoleContextChange,
  agentServers,
  agentServerId,
  onAgentServerIdChange,
  syncMode,
  onSyncModeChange,
  disabled,
}: DesignPreflightFieldsProps) {
  const { t } = useTranslation();

  return (
    <>
      <div className="modal-field">
        {/* ModelSelect renders its own stage chip + rail, so it stands in
            for the field card without an extra wrap. Controlled on
            configId so the picked claude config travels with the request
            body — uncontrolled mode would self-fetch the active config
            and lose it. */}
        <ModelSelect
          value={model}
          onChange={onModelChange}
          stage="architect"
          working={working}
          label={t('schedules.modal.designModelLabel')}
          configId={configId || undefined}
          onConfigChange={onConfigChange}
        />
        <small style={{ color: 'var(--color-text-muted)', marginTop: 4, display: 'block' }}>
          {t('schedules.modal.modelHint')}
        </small>
      </div>

      {/* Reasoning-effort picker — sits right below the model picker, the
          other per-stage knob that decides how the architect run thinks. */}
      <div className="modal-field">
        <EffortBar
          id={effortId}
          label={t('components.effortBar.ariaLabel')}
          value={effortValue}
          onChange={onEffortChange}
          disabled={effortDisabled}
        />
      </div>

      {/* Read-knowledge toggle — the knowledge pre-read runs once before
          the design stage so the architect persona has the context loaded. */}
      <label className={`preflight-toggle ${readKnowledge ? 'is-checked' : ''}`}>
        <input
          type="checkbox"
          checked={readKnowledge}
          onChange={e => onReadKnowledgeChange(e.target.checked)}
          disabled={disabled}
        />
        <div className="preflight-toggle-body">
          <div className="preflight-toggle-title">
            <IconBook size={14} className="icon-mr" />
            {t('requirements.detail2.preflightReadKnowledge')}
          </div>
          <div className="preflight-toggle-desc">
            {readKnowledgeHint ?? t('requirements.detail2.preflightReadKnowledgeHint')}
          </div>
        </div>
      </label>

      {/* Designer-context toggle — whether this design run carries the
          architect role prompt. Local and Agent-server runs share this
          single switch. */}
      <label className={`preflight-toggle ${useRoleContext ? 'is-checked' : ''}`}>
        <input
          type="checkbox"
          checked={useRoleContext}
          onChange={e => onUseRoleContextChange(e.target.checked)}
          disabled={disabled}
        />
        <div className="preflight-toggle-body">
          <div className="preflight-toggle-title">
            <IconArchitect size={14} className="icon-mr" />
            {t('requirements.detail2.designRoleContextLabel')}
          </div>
          <div className="preflight-toggle-desc">
            {t('requirements.detail2.designRoleContextHint')}
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
            value={agentServerId}
            onChange={onAgentServerIdChange}
            disabled={disabled}
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

      {/* Sync-mode picker — only renders when an Agent server is picked
          (SyncModeSelect returns null otherwise, so there is never an
          orphaned half-card). */}
      <div className="modal-field">
        <label>{t('requirements.detail2.syncModeLabel')}</label>
        <SyncModeSelect
          enabled={!!agentServerId}
          value={syncMode}
          onChange={onSyncModeChange}
          disabled={disabled}
        />
        <small style={{ color: 'var(--color-text-muted)', marginTop: 4, display: 'block' }}>
          {syncMode === 'local'
            ? t('requirements.detail2.syncModeLocalHint')
            : t('requirements.detail2.syncModeRemoteHint')}
        </small>
      </div>
    </>
  );
}

export default DesignPreflightFields;
