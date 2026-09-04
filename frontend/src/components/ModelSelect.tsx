import { useEffect, useState, type CSSProperties } from 'react';
import { claudeApi, DefaultModelLabel, type ClaudeConfigItem } from '../api/client';

// ModelSelect is the per-stage model picker for the wizard pipeline.
//
// Visual: a compact horizontal "card" with a 4px stage-colored accent rail
// on the left, a stage chip (分析 / 方案 / 开发), and two selects — "配置"
// (which claude config to pick from) and "模型" (the model id). The
// horizontal layout keeps the picker from breaking across multiple lines
// when several ModelSelects sit side by side in a toolbar; the outer flex
// container handles wrapping as a whole. The accent rail + stage chip make
// it scannable which stage each picker belongs to even when three are
// visible at once.
//
// Options come from ALL claude configs (active + inactive); the user picks
// a config (defaults to the active one) then a model from that config's
// list. The empty value renders as "默认模型" which the backend interprets
// as "use the role's configured model" (role override → config default →
// CLI default). A stored value that is no longer in the currently selected
// config's list is preserved as a disabled option so it is never silently
// dropped.
//
// Note: runtime still uses the active config's ANTHROPIC_BASE_URL /
// ANTHROPIC_AUTH_TOKEN, so picking a model from a non-active config is
// fine only when the active gateway actually serves that model id. The
// wizard pipeline surfaces an error if the CLI rejects the model.
//
// When defaultModelName is provided the "默认模型" option shows the model
// that will actually be used (角色模型 > 生效配置默认模型), so the user
// sees the real model name before the stage even runs — not just the
// "默认模型" label.
//
// When `working` is true the card swaps its neutral border for an amber
// one and the accent rail turns amber + pulses, telegraphing that Claude
// is currently running for this stage and the picker is locked.
export type ModelStage = 'analyst' | 'architect' | 'developer';

interface Props {
  // Current selection — the DISPLAY value the backend persists (may be the
  // "默认模型" literal from DefaultModelLabel, which is mapped to "" here).
  value: string;
  onChange: (model: string) => void;
  disabled?: boolean;
  // Optional human label; when `stage` is provided it wins and renders the
  // standard 3-letter chip instead. Kept for callers that haven't been
  // migrated to the stage prop yet.
  label?: string;
  title?: string;
  // Actual model id that the empty "默认模型" selection maps to for this
  // stage (role default >> active claude config default). Shown next to
  // "默认模型".
  defaultModelName?: string;
  // Claude working status for this stage; truthy paints the card amber
  // and pulses the accent rail. Falsy leaves the card in its neutral
  // state.
  working?: boolean;
  // Which wizard stage this picker belongs to. Drives the stage chip and
  // accent rail color so the user can tell 分析师 / 方案 / 开发 apart at a
  // glance. When omitted the card uses a neutral rail and the optional
  // `label` prop shows verbatim.
  stage?: ModelStage;
  style?: CSSProperties;
}

const STAGE_META: Record<ModelStage, { chip: string; chipShort: string }> = {
  analyst:   { chip: '分析', chipShort: '析' },
  architect: { chip: '方案', chipShort: '方' },
  developer: { chip: '开发', chipShort: '开' },
};

export default function ModelSelect({
  value,
  onChange,
  disabled,
  label,
  title,
  defaultModelName,
  working,
  stage,
  style,
}: Props) {
  const [configs, setConfigs] = useState<ClaudeConfigItem[]>([]);
  // The config the user has currently picked in the "type" dropdown.
  // Falls back to "" = "no specific config" once we know there are zero
  // configs.
  const [selectedConfigId, setSelectedConfigId] = useState<string>('');

  useEffect(() => {
    claudeApi.list()
      .then(res => {
        const list = res ?? [];
        setConfigs(list);
        const active = list.find(c => c.is_active);
        setSelectedConfigId(active?.id ?? list[0]?.id ?? '');
      })
      .catch(() => {
        setConfigs([]);
        setSelectedConfigId('');
      });
  }, []);

  // Normalize the persisted "默认模型" sentinel to the dropdown's empty
  // value; anything else is a concrete model id kept verbatim.
  const normalized = !value || value === DefaultModelLabel ? '' : value;

  // Resolved "type" / "default label" view-models.
  const activeConfig = configs.find(c => c.is_active);
  const currentConfig = configs.find(c => c.id === selectedConfigId);
  const cfgModels = (currentConfig?.models ?? []).map(m => m.model);
  // True when the role's saved/draft model is not in the currently
  // picked config's list (e.g. user picked a non-active config and the
  // saved model belongs to the active one). Render a disabled option so
  // the value isn't lost. Only meaningful when we actually have a config
  // selected AND that config has a non-empty models list — otherwise we
  // don't know what's "in list" vs not.
  const outOfList =
    normalized !== '' &&
    !!selectedConfigId &&
    cfgModels.length > 0 &&
    !cfgModels.includes(normalized);

  // "默认模型" alone, or "默认模型（<实际模型名>）". Active config's
  // default_model is what the empty selection resolves to when role has
  // no override.
  const defaultModelNameResolved =
    defaultModelName ?? activeConfig?.default_model ?? '';
  const defaultLabel = defaultModelNameResolved
    ? `默认模型（${defaultModelNameResolved}）`
    : '默认模型';

  // When there are no configs at all (404 / 500 / empty DB), fall back to
  // the legacy single-dropdown layout so the picker still renders
  // something sensible. The model select will only contain "默认模型"
  // + the persisted current value (if any).
  const hasConfigs = configs.length > 0;

  const stageClass = stage ? ` model-select-card--${stage}` : '';
  const workClass = working ? ' is-work' : '';
  const cardClasses = `model-select-card${stageClass}${workClass}`;

  // Stage chip text — the 3-letter stage name wins over the legacy label
  // prop. When neither is set we hide the chip entirely (neutral card).
  const chipText = stage ? STAGE_META[stage].chip : (label ?? '');

  return (
    <div className="model-select" style={style} title={title}>
      <div className={cardClasses} data-stage={stage ?? 'neutral'}>
        {/* Stage accent rail + chip anchor the card to its wizard stage. */}
        {stage && <span className="model-select-rail" aria-hidden="true" />}
        {chipText && (
          <span className="model-select-chip" title={label ?? chipText}>
            {chipText}
          </span>
        )}
        {/* "Type" dropdown: only meaningful when at least one config
            exists. Falls back to the legacy single-select layout (no
            type row) when the DB has no configs yet. */}
        {hasConfigs && (
          <select
            className="form-input model-select-input model-select-type"
            value={selectedConfigId}
            disabled={disabled}
            onChange={e => setSelectedConfigId(e.target.value)}
            title="先选择 Claude 配置，再选该配置下的模型"
            aria-label="配置"
          >
            <option value="">默认（不指定）</option>
            {configs.map(c => (
              <option key={c.id} value={c.id}>
                {c.name}{c.is_active ? '（当前生效）' : ''}
              </option>
            ))}
          </select>
        )}
        <select
          className="form-input model-select-input model-select-model"
          value={outOfList ? `__legacy:${normalized}` : normalized}
          disabled={disabled}
          onChange={e => {
            const v = e.target.value;
            onChange(v.startsWith('__legacy:') ? v.slice(9) : v);
          }}
          aria-label="模型"
        >
          <option value="">{defaultLabel}</option>
          {cfgModels.map(m => <option key={m} value={m}>{m}</option>)}
          {outOfList && (
            <option value={`__legacy:${normalized}`} disabled>
              当前：{normalized}（不在当前配置列表中）
            </option>
          )}
        </select>
      </div>
    </div>
  );
}