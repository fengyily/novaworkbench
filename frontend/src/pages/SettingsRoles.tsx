import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { rolesApi, claudeApi, type Role, type ClaudeConfigItem } from '../api/client';
import { errorMessage } from '../utils/errMsg';
import './SettingsRoles.css';

export default function SettingsRoles() {
  const { t } = useTranslation();
  const [roles, setRoles] = useState<Role[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [toast, setToast] = useState('');
  // Per-role working copy (editable buffer, not yet saved). The config id
  // persists alongside the model so the role's chosen base URL / auth
  // token / model all travel together (req_0f2a842cd5096c52).
  const [drafts, setDrafts] = useState<Record<string, { system_prompt: string; model: string; claude_config_id: string }>>({});
  const [savingId, setSavingId] = useState('');
  const [resettingId, setResettingId] = useState('');

  // All Claude configs (default + inactive) — drives both the "type" (config)
  // and the "model" dropdowns. The default config is the per-role fallback
  // (legacy "no binding" → role uses the global default config).
  const [configs, setConfigs] = useState<ClaudeConfigItem[]>([]);
  // The config id each role currently has selected in its dropdown. Empty
  // string means "default (no specific config)" → falls through to the
  // global default config at runtime.
  const [selectedConfigId, setSelectedConfigId] = useState<Record<string, string>>({});

  useEffect(() => {
    let cancelled = false;
    setLoading(true);

    // Fetch roles and configs in parallel — we need both before we can
    // backfill each role's selected config (default by default).
    Promise.all([
      rolesApi.list().catch(err => { setError(errorMessage(err)); return [] as Role[]; }),
      claudeApi.list().catch(() => [] as ClaudeConfigItem[]),
    ])
      .then(([roleList, configList]) => {
        if (cancelled) return;
        const rs = roleList ?? [];
        setRoles(rs);
        const draftMap: Record<string, { system_prompt: string; model: string; claude_config_id: string }> = {};
        rs.forEach(rr => {
          draftMap[rr.id] = {
            system_prompt: rr.system_prompt,
            model: rr.model,
            // Honor the saved binding when present; otherwise the UI defaults
            // to the global default config below.
            claude_config_id: rr.claude_config_id ?? '',
          };
        });
        setDrafts(draftMap);

        const cfgs = configList ?? [];
        setConfigs(cfgs);
        const defaultCfg = cfgs.find(c => c.is_active);
        const defaultId = defaultCfg?.id ?? cfgs[0]?.id ?? '';
        const sel: Record<string, string> = {};
        rs.forEach(rr => {
          // Pre-select the role's saved binding when it's still around;
          // otherwise show the global default config in the dropdown so the
          // user understands which gateway the empty binding resolves to.
          if (rr.claude_config_id && cfgs.some(c => c.id === rr.claude_config_id)) {
            sel[rr.id] = rr.claude_config_id;
          } else {
            sel[rr.id] = defaultId;
          }
        });
        setSelectedConfigId(sel);
      })
      .finally(() => { if (!cancelled) setLoading(false); });

    return () => { cancelled = true; };
  }, []);

  const showToast = (msg: string) => {
    setToast(msg);
    window.setTimeout(() => setToast(''), 4000);
  };

  const draft = (r: Role) =>
    drafts[r.id] ?? {
      system_prompt: r.system_prompt,
      model: r.model,
      claude_config_id: r.claude_config_id ?? '',
    };
  const isDirty = (r: Role) => {
    const d = draft(r);
    return (
      d.system_prompt !== r.system_prompt ||
      d.model !== r.model ||
      d.claude_config_id !== (r.claude_config_id ?? '')
    );
  };

  const update = (
    id: string,
    patch: Partial<{ system_prompt: string; model: string; claude_config_id: string }>,
  ) => {
    setDrafts(prev => ({ ...prev, [id]: { ...prev[id], ...patch } }));
  };

  const pickConfig = (roleId: string, configId: string) => {
    setSelectedConfigId(prev => ({ ...prev, [roleId]: configId }));
    // If the role's current draft model isn't in the newly picked config's
    // list, clear it so the UI doesn't show a stale "current value" chip.
    const cfg = configs.find(c => c.id === configId);
    if (!cfg) return;
    const d = drafts[roleId];
    if (!d || !d.model) return;
    const inList = (cfg.models ?? []).some(m => m.model === d.model);
    if (!inList) {
      setDrafts(prev => ({ ...prev, [roleId]: { ...prev[roleId], model: '' } }));
    }
    // Persist the picked config as the role's binding so the saved role
    // runs against that config's gateway. The default entry (empty) clears
    // binding so the role falls back to the global default config.
    update(roleId, { claude_config_id: configId });
  };

  const save = async (r: Role) => {
    setSavingId(r.id);
    setError('');
    try {
      const d = draft(r);
      const res = await rolesApi.update(r.id, d);
      const updated = res.role;
      setRoles(prev => prev.map(x => (x.id === updated.id ? updated : x)));
      setDrafts(prev => ({
        ...prev,
        [r.id]: {
          system_prompt: updated.system_prompt,
          model: updated.model,
          claude_config_id: updated.claude_config_id ?? '',
        },
      }));
      if (res.warning) showToast(res.warning);
      else showToast(t('settings.roles.saved'));
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setSavingId('');
    }
  };

  const reset = async (r: Role) => {
    setResettingId(r.id);
    setError('');
    try {
      const updated = await rolesApi.reset(r.id);
      setRoles(prev => prev.map(x => (x.id === updated.id ? updated : x)));
      // Reset restores the built-in system prompt + model, but the user's
      // binding choice persists (claude_config_id is part of the user's
      // tunnel-routing config, not the persona). Empty it out too so the
      // role returns to the global default gateway.
      setDrafts(prev => ({
        ...prev,
        [r.id]: {
          system_prompt: updated.system_prompt,
          model: updated.model,
          claude_config_id: '',
        },
      }));
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setResettingId('');
    }
  };

  if (loading) return <div className="settings-empty">{t('settings.roles.loading')}</div>;

  return (
    <div className="settings-roles">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">{t('settings.roles.title')}</h3>
          <p className="settings-section-desc">
            {t('settings.roles.desc')}
            {configs.length === 0 && t('settings.roles.descNoConfigs')}
          </p>
        </div>
      </div>

      {error && <div className="form-error">{error}</div>}
      {toast && <div className="role-toast">{toast}</div>}

      {roles.map(r => {
        const d = draft(r);
        const dirty = isDirty(r);
        const cfgId = selectedConfigId[r.id] ?? '';
        const cfg = configs.find(c => c.id === cfgId);
        const cfgModels = (cfg?.models ?? []).map(m => m.model);
        // True when the role's saved/draft model is not in the currently
        // picked config's list (e.g. user picked a non-active config and the
        // saved model belongs to the active one). Render a disabled option so
        // the value isn't lost.
        const outOfList = !!d.model && cfgModels.length > 0 && !cfgModels.includes(d.model);
        return (
          <div className="role-card" key={r.id}>
            <div className="role-card-head">
              <div>
                <h4 className="role-name">{r.name}</h4>
                <p className="role-desc">{r.description}</p>
                <span className="role-key">key: {r.key}</span>
                <span className="role-binding">
                  {(() => {
                    const boundId = d.claude_config_id ?? '';
                    if (!boundId) {
                      return <span className="role-binding-default">{t('settings.roles.bindingDefault')}</span>;
                    }
                    const boundCfg = configs.find(c => c.id === boundId);
                    const boundName = boundCfg?.name ?? t('settings.roles.bindingDeleted');
                    return (
                      <span className="role-binding-bound">
                        {t('settings.roles.bindingPrefix')}{boundName}{boundCfg?.is_active ? t('settings.roles.activeSuffix') : ''}
                      </span>
                    );
                  })()}
                </span>
              </div>
              <div className="role-model-field">
                <div className="role-model-row">
                  <label>{t('settings.roles.configLabel')}</label>
                  <select
                    className="form-input role-model-input"
                    value={cfgId}
                    onChange={e => pickConfig(r.id, e.target.value)}
                  >
                    <option value="">{t('settings.roles.configUnspecified')}</option>
                    {configs.map(c => (
                      <option key={c.id} value={c.id}>
                        {c.name}{c.is_active ? t('settings.roles.activeSuffix') : ''}
                      </option>
                    ))}
                  </select>
                </div>
                <div className="role-model-row">
                  <label>{t('settings.roles.modelLabel')}</label>
                  <select
                    className="form-input role-model-input"
                    value={outOfList ? `__legacy:${d.model}` : d.model}
                    onChange={e => {
                      const v = e.target.value;
                      update(r.id, { model: v.startsWith('__legacy:') ? v.slice(9) : v });
                    }}
                    disabled={!cfgId || cfgModels.length === 0}
                  >
                    <option value="">{t('settings.roles.configUnspecified')}</option>
                    {cfgModels.map(m => <option key={m} value={m}>{m}</option>)}
                    {outOfList && (
                      <option value={`__legacy:${d.model}`} disabled>
                        {t('settings.roles.outOfList', { model: d.model })}
                      </option>
                    )}
                  </select>
                  {!cfgId && (
                    <small className="form-hint">{t('settings.roles.needConfigHint')}</small>
                  )}
                  {cfgId && cfgModels.length === 0 && (
                    <small className="form-hint">{t('settings.roles.noModelsHint')}</small>
                  )}
                </div>
              </div>
            </div>

            <div className="role-prompt-field">
              <label>{t('settings.roles.promptLabel')}</label>
              <textarea
                className="form-input role-prompt-input"
                rows={10}
                value={d.system_prompt}
                onChange={e => update(r.id, { system_prompt: e.target.value })}
              />
            </div>

            <div className="role-card-actions">
              <button
                className="btn btn-secondary"
                onClick={() => reset(r)}
                disabled={!!resettingId}
              >
                {resettingId === r.id ? t('settings.roles.resetting') : t('settings.roles.resetPrompt')}
              </button>
              <button
                className="btn btn-primary"
                onClick={() => save(r)}
                disabled={!dirty || !!savingId}
              >
                {savingId === r.id ? t('settings.roles.saving') : t('settings.roles.save')}
              </button>
            </div>
          </div>
        );
      })}
    </div>
  );
}
