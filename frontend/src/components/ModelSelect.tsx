import { useEffect, useState, type CSSProperties } from 'react';
import { claudeApi, DefaultModelLabel } from '../api/client';

// ModelSelect is a per-stage model picker for the wizard pipeline. Options come
// from the active claude config's model list (claudeApi.active()); the empty
// value renders as "默认模型" which the backend interprets as "use the role's
// configured model" (role override → config default → CLI default). A stored
// value that is no longer in the list is preserved as a disabled option so it
// is never silently dropped.
//
// When defaultModelName is provided the "默认模型" option shows the model that
// will actually be used (角色模型 > 生效配置默认模型), so the user sees the
// real model name before the stage even runs — not just the "默认模型" label.
//
// When `working` is a boolean the component also renders the stage's Claude
// status indicator (green pulsing dot + "工作中" / gray dot + "空闲"). When it
// is undefined the status row is hidden (used by callers that only need the
// dropdown).
interface Props {
  // Current selection — the DISPLAY value the backend persists (may be the
  // "默认模型" literal from DefaultModelLabel, which is mapped to "" here).
  value: string;
  onChange: (model: string) => void;
  disabled?: boolean;
  label?: string;
  title?: string;
  // Actual model id that the empty "默认模型" selection maps to for this stage
  // (role default >> active claude config default). Shown next to "默认模型".
  defaultModelName?: string;
  // Claude working status for this stage; undefined hides the status indicator.
  working?: boolean;
  style?: CSSProperties;
}

export default function ModelSelect({ value, onChange, disabled, label, title, defaultModelName, working, style }: Props) {
  const [models, setModels] = useState<string[]>([]);
  useEffect(() => {
    claudeApi.active()
      .then(res => setModels(res?.models ?? []))
      .catch(() => setModels([]));
  }, []);

  // Normalize the persisted "默认模型" sentinel to the dropdown's empty value;
  // anything else is a concrete model id kept verbatim.
  const normalized = !value || value === DefaultModelLabel ? '' : value;
  const outOfList = normalized !== '' && models.length > 0 && !models.includes(normalized);
  const hasStatus = typeof working === 'boolean';
  // Resolved default label: "默认模型" alone, or "默认模型（<实际模型名>）".
  const defaultLabel = defaultModelName
    ? `默认模型（${defaultModelName}）`
    : '默认模型';

  return (
    <div className="model-select" style={style} title={title}>
      {label && <span className="model-select-label">{label}</span>}
      {hasStatus && <span className={`model-status-dot${working ? ' is-work' : ''}`} />}
      <select
        className="form-input model-select-input"
        value={outOfList ? `__legacy:${normalized}` : normalized}
        disabled={disabled}
        onChange={e => {
          const v = e.target.value;
          onChange(v.startsWith('__legacy:') ? v.slice(9) : v);
        }}
      >
        <option value="">{defaultLabel}</option>
        {models.map(m => <option key={m} value={m}>{m}</option>)}
        {outOfList && (
          <option value={`__legacy:${normalized}`} disabled>
            当前：{normalized}（不在列表中）
          </option>
        )}
      </select>
      {hasStatus && <span className="model-status-text">{working ? '工作中' : '空闲'}</span>}
    </div>
  );
}