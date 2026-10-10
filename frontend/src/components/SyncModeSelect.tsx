import { useTranslation } from 'react-i18next';

/**
 * SyncModeSelect is the shared "代码同步方式" dropdown for Agent-server runs.
 * It only renders when `enabled` is true — the caller wires `enabled` to
 * the same `agentServerId` state that gates the adjacent ExecEnvSelect so
 * the picker disappears the moment the user flips back to 本地执行.
 *
 * Two options:
 *   value=""     → 远程 Git 仓库同步 (origin clone / push)
 *   value="local" → 本地仓库同步 (git bundle over SFTP)
 *
 * The "remote" wording is intentionally collapsed to "" because the wire
 * format on LaunchSpec.sync_mode / CodingPreflight.syncMode / etc. only
 * differentiates between "" and "local" — see the client.ts types. Picking
 * "" sends `sync_mode: "remote"` semantics on the wire but the server
 * normalizes it to "" via model.NormalizeSyncMode.
 *
 * Mirrors ExecEnvSelect: the surrounding label / hint / form-field wrapper
 * is the caller's responsibility so this component can drop into the
 * architect toolbar, the coding pre-flight modal, the immediate-design-and-
 * coding modal and the schedule modal without fighting their card layouts.
 */
export function SyncModeSelect({
  value,
  onChange,
  enabled,
  id,
  className = 'form-input',
  disabled = false,
  title,
  style,
}: {
  value: '' | 'local';
  onChange: (v: '' | 'local') => void;
  enabled: boolean;
  id?: string;
  className?: string;
  disabled?: boolean;
  title?: string;
  style?: React.CSSProperties;
}) {
  const { t } = useTranslation();
  if (!enabled) return null;
  return (
    <select
      id={id}
      className={className}
      value={value}
      onChange={(e) => onChange(e.target.value as '' | 'local')}
      disabled={disabled}
      title={title}
      style={style}
    >
      <option value="">{t('requirements.detail2.syncModeRemote')}</option>
      <option value="local">{t('requirements.detail2.syncModeLocal')}</option>
    </select>
  );
}

export default SyncModeSelect;
