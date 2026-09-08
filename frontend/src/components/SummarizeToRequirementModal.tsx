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

import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { authedFetch, API_BASE } from '../api/client';

type Phase = 'idle' | 'running' | 'error';

interface Props {
  sourceId: string;
  sourceTitle: string;
  onClose: () => void;
  onCreated: (newId: string) => void;
}

export function SummarizeToRequirementModal({ sourceId, sourceTitle, onClose, onCreated }: Props) {
  const { t } = useTranslation();
  const [phase, setPhase] = useState<Phase>('idle');
  const [errorMsg, setErrorMsg] = useState('');

  const handleConfirm = async () => {
    setPhase('running');
    setErrorMsg('');
    try {
      const res = await authedFetch(`${API_BASE}/api/requirements/${sourceId}/promote`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: '{}',
      });
      const json = await res.json();
      if (!res.ok || !json.success) {
        // 422 NOT_CONVERGED — the discussion has not converged. Render the
        // friendly message (the prompt already encodes that semantic) and let
        // the user keep chatting before retrying.
        if (res.status === 422 || json.error?.code === 'NOT_CONVERGED') {
          setPhase('error');
          setErrorMsg(t('requirements.promote.notConverged'));
          return;
        }
        setPhase('error');
        setErrorMsg(json.error?.message || t('requirements.promote.requestFailed', { status: res.status }));
        return;
      }
      // requirementsApi.promoteFromIdea could be used instead, but this modal
      // handles the 422 case itself and consumes json.data directly.
      const newReq = json.data as { id: string };
      onCreated(newReq.id);
    } catch (err: any) {
      setPhase('error');
      setErrorMsg(err?.message || t('requirements.promote.networkError'));
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
    <div className="modal-backdrop" onClick={handleBackdropClick}>
      <div className="modal-card summarize-modal" onClick={e => e.stopPropagation()}>
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