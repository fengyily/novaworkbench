// SummarizeToRequirementModal — turns an idea/issue into a requirement.
//
// Fired from the idea/issue detail page: it hands the whole discussion
// (description + accumulated acceptance criteria + analyst transcript) to the
// LLM in one shot to be summarized into a buildable requirement. The prompt
// lets the model answer with an empty markdown body when the discussion has
// not converged; the service surfaces that as 422 NOT_CONVERGED, which this
// modal renders as a friendly "keep chatting first" message.
//
// Three phases: idle (confirm) → running (spinner) → done (navigates away on
// success / falls back on failure). Navigation is the onCreated callback's
// job — the parent receives the new requirement id and routes to the detail
// page.

import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  requirementsApi,
  agentServersApi,
  type AgentServer,
  type PromoteDesignConfig,
} from '../api/client';
import ModelSelect from './ModelSelect';
import ExecEnvSelect, { type ExecEnvServer } from './ExecEnvSelect';

type Phase = 'idle' | 'running' | 'error';

interface Props {
  sourceId: string;
  sourceTitle: string;
  onClose: () => void;
  onCreated: (
    newId: string,
    opts?: { designJobId?: string; launchError?: string },
  ) => void;
}

export function SummarizeToRequirementModal({ sourceId, sourceTitle, onClose, onCreated }: Props) {
  const { t } = useTranslation();
  const [phase, setPhase] = useState<Phase>('idle');
  const [errorMsg, setErrorMsg] = useState('');
  // Optional "同时生成技术方案" stage. Default off — the legacy behavior
  // (just create the requirement row) stays the default. When enabled, the
  // promote POST forwards the picked design-stage overrides and the backend
  // kicks off the architect stage immediately; the dispatched JobStore job
  // id lands on Requirement.design_job_id and is forwarded to onCreated so
  // the detail page can attach the SSE stream right away.
  const [autoDesign, setAutoDesign] = useState(false);
  const [designConfig, setDesignConfig] = useState<PromoteDesignConfig>({});
  // Agent servers filtered to ready status — mirrors the design-toolbar
  // integration in RequirementDetail (settings tab is the source of truth,
  // we silently degrade on list failure so the modal never blocks on a
  // transient settings-tab hiccup).
  const [agentServers, setAgentServers] = useState<ExecEnvServer[]>([]);
  useEffect(() => {
    let cancelled = false;
    agentServersApi
      .list()
      .then((rows: AgentServer[] | null | undefined) => {
        if (cancelled) return;
        const ready = (rows ?? []).filter((s) => s.status === 'ready');
        // Project down to the structural shape ExecEnvSelect consumes — its
        // contract is {id, name, host}; passing the full AgentServer would
        // still type-check (per ExecEnvSelect's doc comment) but the slim
        // projection keeps the modal's footprint explicit.
        setAgentServers(ready.map((s) => ({ id: s.id, name: s.name, host: s.host })));
      })
      .catch(() => {
        /* settings tab is the source of truth — silently ignore */
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const handleConfirm = async () => {
    setPhase('running');
    setErrorMsg('');
    try {
      // api.post<Requirement> wraps the response envelope; on non-success it
      // throws ApiError whose `code` is the backend's error.code. We forward
      // the optional design-stage config only when autoDesign is checked so
      // un-ticked submissions stay byte-for-byte equivalent to the legacy
      // empty-body POST (the backend's "no design" branch is preserved).
      const item = await requirementsApi.promoteFromIdea(
        sourceId,
        autoDesign ? { design: designConfig } : undefined,
      );
      // design_job_id is `string` on the Requirement row (empty when no job
      // was dispatched) — collapse the empty-string sentinel to undefined so
      // downstream consumers can use a single truthy check. launch_error is
      // already optional and only set when the wizard dispatch failed.
      onCreated(item.id, {
        designJobId: item.design_job_id || undefined,
        launchError: item.launch_error,
      });
    } catch (err: any) {
      // 422 NOT_CONVERGED — the discussion has not converged. Render the
      // friendly message (the prompt already encodes that semantic) and let
      // the user keep chatting before retrying. ApiError exposes `code` as
      // a top-level field, populated from the backend's error.code.
      const code = err?.code;
      if (code === 'NOT_CONVERGED') {
        setPhase('error');
        setErrorMsg(t('requirements.promote.notConverged'));
        return;
      }
      setPhase('error');
      setErrorMsg(
        err?.message || t('requirements.promote.requestFailed', { status: err?.status ?? '?' }),
      );
    }
  };

  // Click-outside-to-dismiss — but ONLY when the LLM call isn't running.
  // Closing mid-flight would lose the in-progress summary state and force
  // the user to re-run the summarize from scratch (and pay for another LLM
  // round). The header × and Cancel buttons already gate on phase ===
  // 'running' below; the backdrop handler must match.
  const handleBackdropClick = () => {
    if (phase !== 'running') onClose();
  };

  return (
    <div className="modal-overlay" onClick={handleBackdropClick}>
      <div className="modal-box summarize-modal" onClick={e => e.stopPropagation()}>
        <div className="modal-header">
          <h3>{t('requirements.promote.title')}</h3>
          <button className="btn btn-sm" onClick={onClose} disabled={phase === 'running'}>×</button>
        </div>

        <div className="modal-body">
          {phase === 'idle' && (
            <>
              <p>
                {t('requirements.promote.introPrefix')}<strong>{sourceTitle || t('requirements.promote.sourceFallback')}</strong>{t('requirements.promote.introSuffix')}
              </p>
              <ul className="modal-hint">
                <li>{t('requirements.promote.bullet1Prefix')}<code>kind = requirement</code>{t('requirements.promote.bullet1Middle')}<code>draft</code>{t('requirements.promote.bullet1Suffix')}</li>
                <li>{t('requirements.promote.bullet2Prefix')}<strong>{t('requirements.promote.bullet2Bold')}</strong>{t('requirements.promote.bullet2Suffix')}</li>
                <li>{t('requirements.promote.bullet3')}</li>
              </ul>
              {/*
                Optional "同时生成技术方案" toggle. Default off — keeps legacy
                "just create the row" behavior as the default. When enabled,
                forwards the picked design-stage overrides to the backend and
                surfaces the dispatched JobStore job id to onCreated so the
                detail page can attach the SSE stream immediately.
              */}
              <label className="summarize-auto-design">
                <input
                  type="checkbox"
                  checked={autoDesign}
                  onChange={e => setAutoDesign(e.target.checked)}
                />
                <span>{t('requirements.promote.autoDesign')}</span>
              </label>
              {autoDesign && (
                <div className="summarize-design-stage">
                  <ModelSelect
                    stage="architect"
                    value={designConfig.design_model ?? ''}
                    onChange={m =>
                      setDesignConfig(c => ({
                        ...c,
                        design_model: m || undefined,
                      }))
                    }
                    configId={designConfig.design_claude_config_id ?? ''}
                    onConfigChange={id =>
                      setDesignConfig(c => ({
                        ...c,
                        design_claude_config_id: id || undefined,
                      }))
                    }
                  />
                  <ExecEnvSelect
                    servers={agentServers}
                    value={designConfig.design_agent_server_id ?? ''}
                    onChange={id =>
                      setDesignConfig(c => ({
                        ...c,
                        design_agent_server_id: id || undefined,
                      }))
                    }
                  />
                </div>
              )}
            </>
          )}

          {phase === 'running' && (
            <div className="modal-running">
              <div className="spinner" />
              <p>{t('requirements.promote.running')}</p>
              <small>{t('requirements.promote.runningHint')}</small>
            </div>
          )}

          {phase === 'error' && (
            <div className="modal-error" role="alert">
              <p>{errorMsg}</p>
            </div>
          )}
        </div>

        <div className="modal-actions">
          <button className="btn" onClick={onClose} disabled={phase === 'running'}>
            {phase === 'error' ? t('common.actions.close') : t('common.actions.cancel')}
          </button>
          {phase === 'error' ? (
            <button className="btn btn-primary" onClick={() => setPhase('idle')}>{t('requirements.promote.retry')}</button>
          ) : (
            <button
              className="btn btn-primary"
              onClick={handleConfirm}
              disabled={phase === 'running'}
            >
              {phase === 'running' ? t('requirements.promote.runningCta') : t('requirements.promote.startCta')}
            </button>
          )}
        </div>
      </div>
    </div>
  );
}