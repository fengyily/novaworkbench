import { useState, useEffect } from 'react';
import { rolesApi, claudeApi, type Role, type ClaudeConfigItem } from '../api/client';
import './SettingsRoles.css';

export default function SettingsRoles() {
  const [roles, setRoles] = useState<Role[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [toast, setToast] = useState('');
  // Per-role working copy (editable buffer, not yet saved).
  const [drafts, setDrafts] = useState<Record<string, { system_prompt: string; model: string }>>({});
  const [savingId, setSavingId] = useState('');
  const [resettingId, setResettingId] = useState('');

  // All Claude configs (active + inactive) — drives both the "type" (config)
  // and the "model" dropdowns. The active config is the default per-role pick.
  const [configs, setConfigs] = useState<ClaudeConfigItem[]>([]);
  // The config id each role currently has selected in its dropdown. Empty
  // string means "no config selected" → falls through to the CLI default model.
  const [selectedConfigId, setSelectedConfigId] = useState<Record<string, string>>({});

  useEffect(() => {
    let cancelled = false;
    setLoading(true);

    // Fetch roles and configs in parallel — we need both before we can
    // backfill each role's selected config (active by default).
    Promise.all([
      rolesApi.list().catch(err => { setError(err instanceof Error ? err.message : String(err)); return [] as Role[]; }),
      claudeApi.list().catch(() => [] as ClaudeConfigItem[]),
    ])
      .then(([roleList, configList]) => {
        if (cancelled) return;
        const rs = roleList ?? [];
        setRoles(rs);
        const draftMap: Record<string, { system_prompt: string; model: string }> = {};
        rs.forEach(rr => { draftMap[rr.id] = { system_prompt: rr.system_prompt, model: rr.model }; });
        setDrafts(draftMap);

        const cfgs = configList ?? [];
        setConfigs(cfgs);
        const active = cfgs.find(c => c.is_active);
        const defaultId = active?.id ?? cfgs[0]?.id ?? '';
        const sel: Record<string, string> = {};
        rs.forEach(rr => { sel[rr.id] = defaultId; });
        setSelectedConfigId(sel);
      })
      .finally(() => { if (!cancelled) setLoading(false); });

    return () => { cancelled = true; };
  }, []);

  const showToast = (msg: string) => {
    setToast(msg);
    window.setTimeout(() => setToast(''), 4000);
  };

  const draft = (r: Role) => drafts[r.id] ?? { system_prompt: r.system_prompt, model: r.model };
  const isDirty = (r: Role) => {
    const d = draft(r);
    return d.system_prompt !== r.system_prompt || d.model !== r.model;
  };

  const update = (id: string, patch: Partial<{ system_prompt: string; model: string }>) => {
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
  };

  const save = async (r: Role) => {
    setSavingId(r.id);
    setError('');
    try {
      const d = draft(r);
      const res = await rolesApi.update(r.id, d);
      const updated = res.role;
      setRoles(prev => prev.map(x => (x.id === updated.id ? updated : x)));
      setDrafts(prev => ({ ...prev, [r.id]: { system_prompt: updated.system_prompt, model: updated.model } }));
      if (res.warning) showToast(res.warning);
      else showToast('已保存');
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
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
      setDrafts(prev => ({ ...prev, [r.id]: { system_prompt: updated.system_prompt, model: updated.model } }));
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setResettingId('');
    }
  };

  if (loading) return <div className="settings-empty">加载中...</div>;

  return (
    <div className="settings-roles">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">角色管理</h3>
          <p className="settings-section-desc">
            为每个角色编辑系统提示词（通过 <code>--system-prompt</code> 注入）并选择模型（通过 <code>--model</code> 注入）。
            先选择 Claude 配置（默认当前生效配置），再从该配置的模型列表中选择模型（即使未激活配置中的模型也可保存，但运行时仍走生效配置的 Base URL/Token，请确认所选模型被当前网关支持）。留空则使用 claude CLI 默认模型。
            {configs.length === 0 && '（尚未配置任何 Claude 配置，请先在「Claude 配置」中维护。）'}
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
              </div>
              <div className="role-model-field">
                <div className="role-model-row">
                  <label>配置</label>
                  <select
                    className="form-input role-model-input"
                    value={cfgId}
                    onChange={e => pickConfig(r.id, e.target.value)}
                  >
                    <option value="">默认（不指定）</option>
                    {configs.map(c => (
                      <option key={c.id} value={c.id}>
                        {c.name}{c.is_active ? '（当前生效）' : ''}
                      </option>
                    ))}
                  </select>
                </div>
                <div className="role-model-row">
                  <label>模型</label>
                  <select
                    className="form-input role-model-input"
                    value={outOfList ? `__legacy:${d.model}` : d.model}
                    onChange={e => {
                      const v = e.target.value;
                      update(r.id, { model: v.startsWith('__legacy:') ? v.slice(9) : v });
                    }}
                    disabled={!cfgId || cfgModels.length === 0}
                  >
                    <option value="">默认（不指定）</option>
                    {cfgModels.map(m => <option key={m} value={m}>{m}</option>)}
                    {outOfList && (
                      <option value={`__legacy:${d.model}`} disabled>
                        当前值：{d.model}（不在当前配置列表中）
                      </option>
                    )}
                  </select>
                  {!cfgId && (
                    <small className="form-hint">先选择一个 Claude 配置</small>
                  )}
                  {cfgId && cfgModels.length === 0 && (
                    <small className="form-hint">该配置未配置模型列表，可先在「Claude 配置」中维护</small>
                  )}
                </div>
              </div>
            </div>

            <div className="role-prompt-field">
              <label>系统提示词</label>
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
                {resettingId === r.id ? '恢复中...' : '恢复默认提示词'}
              </button>
              <button
                className="btn btn-primary"
                onClick={() => save(r)}
                disabled={!dirty || !!savingId}
              >
                {savingId === r.id ? '保存中...' : '保存'}
              </button>
            </div>
          </div>
        );
      })}
    </div>
  );
}
