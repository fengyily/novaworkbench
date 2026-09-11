import { useEffect, useState, type CSSProperties } from 'react';
import { useTranslation } from 'react-i18next';
import { claudeApi, DefaultModelLabel, type ClaudeConfigItem } from '../api/client';

// ModelSelect is the per-stage model picker for the wizard pipeline.
//
// Visual: a compact horizontal "card" with a 4px stage-colored accent rail
// on the left, a stage chip (analysis / design / dev), and two selects —
// "config" (which claude config to pick from) and "model" (the model id). The
// horizontal layout keeps the picker from breaking across multiple lines
// when several ModelSelects sit side by side in a toolbar; the outer flex
// container handles wrapping as a whole. The accent rail + stage chip make
// it scannable which stage each picker belongs to even when three are
// visible at once.
//
// Options come from ALL claude configs (active + inactive); the user picks
// a config (defaults to the active one) then a model from that config's
// list. The empty value renders as the DefaultModelLabel literal, which the
// backend interprets as "use the role's configured model" (role override →
// config default → CLI default). A stored value that is no longer in the
// currently selected config's list is preserved as a disabled option so it
// is never silently dropped.
//
// Note: runtime still uses the active config's ANTHROPIC_BASE_URL /
// ANTHROPIC_AUTH_TOKEN, so picking a model from a non-active config is
// fine only when the active gateway actually serves that model id. The
// wizard pipeline surfaces an error if the CLI rejects the model.
//
// When defaultModelName is provided the default-model option shows the
// model that will actually be used (role model > active config default), so
// the user sees the real model name before the stage even runs.
//
// When `working` is true the card swaps its neutral border for an amber
// one and the accent rail turns amber + pulses, telegraphing that Claude
// is currently running for this stage and the picker is locked.
export type ModelStage = 'analyst' | 'architect' | 'developer';

interface Props {
  // Current selection — the DISPLAY value the backend persists (may be the
  // DefaultModelLabel literal from the backend, which is mapped to "" here).
  value: string;
  onChange: (model: string) => void;
  disabled?: boolean;
  // Optional human label; when `stage` is provided it wins and renders the
  // standard 3-letter chip instead. Kept for callers that haven't been
  // migrated to the stage prop yet.
  label?: string;
  title?: string;
  // Actual model id that the empty default-model selection maps to for this
  // stage (role default >> active claude config default). Shown next to
  // default option.
  defaultModelName?: string;
  // Claude working status for this stage; truthy paints the card amber
  // and pulses the accent rail. Falsy leaves the card in its neutral
  // state.
  working?: boolean;
  // Which wizard stage this picker belongs to. Drives the stage chip and
  // accent rail color so the user can tell the three stages apart at a
  // glance. When omitted the card uses a neutral rail and the optional
  // `label` prop shows verbatim.
  stage?: ModelStage;
  style?: CSSProperties;
  // Controlled claude_configs row id from the config dropdown. Optional —
  // when omitted the component manages its own state (legacy behavior,
  // kept so existing call sites don't need to be migrated at the same
  // time). When provided the parent owns the state and is responsible
  // for sending `claude_config_id` alongside `model` in the request body.
  configId?: string;
  onConfigChange?: (configId: string) => void;
}

// Stage chips hold translation KEYS (resolved on render) — a literal here
// would freeze the chip in whatever language was active at import time.
const STAGE_META: Record<ModelStage, { chipKey: string; chipShortKey: string }> = {
  analyst:   { chipKey: 'components.modelSelect.stageAnalyst', chipShortKey: 'components.modelSelect.stageAnalystShort' },
  architect: { chipKey: 'components.modelSelect.stageArchitect', chipShortKey: 'components.modelSelect.stageArchitectShort' },
  developer: { chipKey: 'components.modelSelect.stageDeveloper', chipShortKey: 'components.modelSelect.stageDeveloperShort' },
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
  configId,
  onConfigChange,
}: Props) {
  const { t: tStrict } = useTranslation();
  // `tStrict` is keyed against the resource tree; keys built dynamically
  // (from STAGE_META maps) need the loose overload below.
  const t = tStrict as unknown as (key: string, opts?: Record<string, unknown>) => string;
  const [configs, setConfigs] = useState<ClaudeConfigItem[]>([]);
  // Internal fallback state for the "type" dropdown. When `configId` prop is
  // provided we defer to it (controlled mode); otherwise we self-manage
  // (legacy mode, defaulting to the active config on mount). The bug fix
  // for "wrong BASE URL when coding with a picked model" requires lifting this
  // state up so the parent can send `claude_config_id` in the request body —
  // but we keep
  // the uncontrolled fallback so old call sites stay valid.
  const [internalConfigId, setInternalConfigId] = useState<string>('');

  useEffect(() => {
    claudeApi.list()
      .then(res => {
        const list = res ?? [];
        setConfigs(list);
        const active = list.find(c => c.is_active);
        const initial = active?.id ?? list[0]?.id ?? '';
        // Only seed the internal state when uncontrolled. Controlled mode
        // is the parent's responsibility — overwriting it here would fight
        // with the parent's chosen value.
        if (configId === undefined) {
          setInternalConfigId(initial);
        }
      })
      .catch(() => {
        setConfigs([]);
        if (configId === undefined) {
          setInternalConfigId('');
        }
      });
    // configId is intentionally not in the dep array — we only seed once
    // on mount; controlled updates flow through the prop directly.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Resolved "type" id: controlled prop wins, fallback to internal state.
  const selectedConfigId = configId ?? internalConfigId;
  const setSelectedConfigId = (id: string) => {
    if (onConfigChange) {
      onConfigChange(id);
    } else {
      setInternalConfigId(id);
    }
  };

  // Normalize the persisted DefaultModelLabel sentinel to the dropdown's empty
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

  // "Default model" alone, or "Default model (<resolved id>)". Active config's
  // default_model is what the empty selection resolves to when role has
  // no override.
  const defaultModelNameResolved =
    defaultModelName ?? activeConfig?.default_model ?? '';
  const defaultLabel = defaultModelNameResolved
    ? t('components.modelSelect.defaultWithModel', { model: defaultModelNameResolved })
    : t('components.modelSelect.default');

  // When there are no configs at all (404 / 500 / empty DB), fall back to
  // the legacy single-dropdown layout so the picker still renders
  // something sensible. The model select will only contain the default option
  // + the persisted current value (if any).
  const hasConfigs = configs.length > 0;

  const stageClass = stage ? ` model-select-card--${stage}` : '';
  const workClass = working ? ' is-work' : '';
  const cardClasses = `model-select-card${stageClass}${workClass}`;

  // Stage chip text — the 3-letter stage name wins over the legacy label
  // prop. When neither is set we hide the chip entirely (neutral card).
  const chipText = stage ? t(STAGE_META[stage].chipKey) : (label ?? '');

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
            title={t('components.modelSelect.configTitle')}
            aria-label={t('components.modelSelect.configAria')}
          >
            <option value="">{t('components.modelSelect.configDefault')}</option>
            {configs.map(c => (
              <option key={c.id} value={c.id}>
                {c.name}{c.is_active ? t('components.modelSelect.activeSuffix') : ''}
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
          aria-label={t('components.modelSelect.modelAria')}
        >
          <option value="">{defaultLabel}</option>
          {cfgModels.map(m => <option key={m} value={m}>{m}</option>)}
          {outOfList && (
            <option value={`__legacy:${normalized}`} disabled>
              {t('components.modelSelect.outOfList', { model: normalized })}
            </option>
          )}
        </select>
        {/* Hint when a config is picked but exposes no models. Keeps the
            dropdown usable (still renders the default option) while telling
            the user there's nothing to choose from in the active selection. */}
        {selectedConfigId && currentConfig && cfgModels.length === 0 && (
          <span className="model-select-hint" role="note">
            {t('components.modelSelect.noModels')}
          </span>
        )}
      </div>
    </div>
  );
}