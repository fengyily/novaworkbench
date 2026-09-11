import { useTranslation } from 'react-i18next';
import type { AgentServer } from '../api/client';

/**
 * ExecEnvSelect is the shared "execution environment" dropdown: 本地执行 plus
 * one option per ready Agent Server. It renders only the <select> element so
 * callers keep full control of the surrounding label / card markup (the
 * design toolbar, the coding pre-flight modal, the sub-task composer and the
 * scheduling modal each wrap it differently). Centralizing the option list
 * keeps the "本地 + name (host)" wording identical everywhere.
 *
 * `servers` should already be filtered to ready servers by the caller (the
 * backend refuses non-ready targets, so listing them would only mislead).
 * `value` '' means 本地; any other value is an agent_servers.id.
 */
export function ExecEnvSelect({
  servers,
  value,
  onChange,
  disabled = false,
  title,
  className = 'form-input',
  style,
  localOptionLabel,
}: {
  servers: AgentServer[];
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  title?: string;
  className?: string;
  style?: React.CSSProperties;
  localOptionLabel?: string;
}) {
  const { t } = useTranslation();
  return (
    <select
      className={className}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      disabled={disabled}
      title={title}
      style={style}
    >
      <option value="">{localOptionLabel ?? t('components.execEnv.localOption')}</option>
      {servers.map((s) => (
        <option key={s.id} value={s.id}>
          {s.name} ({s.host})
        </option>
      ))}
    </select>
  );
}

export default ExecEnvSelect;
