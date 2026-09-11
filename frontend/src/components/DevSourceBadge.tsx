import { useTranslation } from 'react-i18next';
import type { Requirement } from '../api/client';
import './DevSourceBadge.css';

/**
 * DevSourceBadge renders where a requirement was (or is being) developed:
 * an Agent server — with its name and the model the developer stage ran —
 * or the local NovaWorkbench host.
 *
 * Nothing is rendered when dev_source is empty: that means the coding stage
 * has never run (or the row predates the field), and showing a local-dev label there
 * would be a guess rather than a fact.
 *
 * Two densities:
 *  - compact  → list rows. Icon + short label only; the server name and model
 *    move into the native tooltip so a long server name can't blow out the
 *    column width.
 *  - default  → detail header. Server name inline, model as a secondary chip.
 *
 * Local requirements deliberately carry no server/model detail — the acceptance
 * criteria call for them to look exactly as they did before this feature.
 */
export function DevSourceBadge({
  req,
  compact = false,
}: {
  req: Pick<Requirement, 'dev_source' | 'agent_server_id' | 'agent_server_name' | 'developer_model'>;
  compact?: boolean;
}) {
  const { t } = useTranslation();
  const source = req.dev_source;
  if (source !== 'agent' && source !== 'local') return null;

  if (source === 'local') {
    return (
      <span className="dev-source-badge is-local" title={t('components.devSource.localTitle')}>
        <span className="dsb-icon">💻</span>
        <span className="dsb-label">{t('components.devSource.local')}</span>
      </span>
    );
  }

  // A deleted agent_servers row leaves the name empty — fall back to the id so
  // the user still has something to correlate against the settings page.
  // Resolved at render time so a language switch re-translates the tooltip.
  const serverName = req.agent_server_name || req.agent_server_id || t('components.devSource.unknownServer');
  const model = req.developer_model || '';
  const tip =
    t('components.devSource.agentTip', { server: serverName }) +
    '\n' + t('components.devSource.modelLine', { model: model || t('components.modelSelect.default') });

  if (compact) {
    return (
      <span className="dev-source-badge is-agent" title={tip}>
        <span className="dsb-icon">🛰️</span>
        <span className="dsb-label">Agent</span>
        <span className="dsb-server">{serverName}</span>
      </span>
    );
  }

  return (
    <span className="dev-source-badge is-agent is-wide" title={tip}>
      <span className="dsb-icon">🛰️</span>
      <span className="dsb-label">{t('components.devSource.agentLabel')}</span>
      <span className="dsb-server">{serverName}</span>
      {model && <span className="dsb-model">{model}</span>}
    </span>
  );
}

export default DevSourceBadge;
