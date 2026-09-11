import { useTranslation } from 'react-i18next';
import './DevSourceBadge.css';

/**
 * ExecEnvBadge renders WHERE a piece of work runs — the local NovaWorkbench
 * host, or a named Agent Server. It is the shared display kernel for every
 * "execution environment" surface in the app: the architect-design toolbar
 * (which Agent Server the plan was generated on) and each sub-task card (which
 * Agent Server the child develops on). DevSourceBadge keeps its own richer
 * dev-stage rendering (model chip, dev_source gating), but this badge covers
 * the simpler "server or local" cases uniformly so their look stays identical.
 *
 * serverId '' / undefined → 💻 本地; a non-empty id → 🛰️ Agent Server「name」.
 * serverName falls back to serverId when the join produced no name (deleted
 * server row) so the user still has something to correlate against settings.
 *
 * hideLocal skips rendering entirely for the local case — used where a "💻 本地"
 * chip would be noise (e.g. next to a selector that already defaults to local).
 */
export function ExecEnvBadge({
  serverId,
  serverName,
  compact = false,
  hideLocal = false,
}: {
  serverId?: string;
  serverName?: string;
  compact?: boolean;
  hideLocal?: boolean;
}) {
  const { t } = useTranslation();
  const id = (serverId || '').trim();

  if (!id) {
    if (hideLocal) return null;
    return (
      <span className="dev-source-badge is-local" title={t('components.execEnv.localTitle')}>
        <span className="dsb-icon">💻</span>
        <span className="dsb-label">{t('components.execEnv.local')}</span>
      </span>
    );
  }

  const name = serverName || id || t('components.devSource.unknownServer');
  const tip = t('components.execEnv.agentTip', { server: name });
  return (
    <span className={`dev-source-badge is-agent${compact ? '' : ' is-wide'}`} title={tip}>
      <span className="dsb-icon">🛰️</span>
      {!compact && <span className="dsb-label">{t('components.execEnv.agentLabel')}</span>}
      <span className="dsb-server">{name}</span>
    </span>
  );
}

export default ExecEnvBadge;
