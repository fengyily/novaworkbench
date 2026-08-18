import { useState, useEffect } from 'react';
import { rolesApi, claudeApi, type Role } from '../api/client';
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

  // Model options from the active Claude config (drives the model dropdown).
  const [activeModels, setActiveModels] = useState<string[]>([]);

  useEffect(() => {
    rolesApi.list()
      .then(data => {
        setRoles(data ?? []);
        const map: Record<string, { system_prompt: string; model: string }> = {};
        (data ?? []).forEach(r => { map[r.id] = { system_prompt: r.system_prompt, model: r.model }; });
        setDrafts(map);
      })
      .catch(err => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setLoading(false));

    // Load the active Claude config's model list for the model dropdown.
    claudeApi.active()
      .then(res => setActiveModels(res?.models ?? []))
      .catch(() => setActiveModels([]));
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

  // Whether a role's current draft model is outside the active config list.
  // When true we append a disabled "current value" option so it is not lost.
  const modelOutOfList = (current: string) => {
    if (!current) return false;
    if (activeModels.length === 0) return false;
    return !activeModels.includes(current);
  };

  return (
    <div className="settings-roles">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">角色管理</h3>
          <p className="settings-section-desc">
            为每个角色编辑系统提示词（通过 <code>--system-prompt</code> 注入）并选择模型（通过 <code>--model</code> 注入）。
            模型下拉来自当前生效 Claude 配置的模型列表，留空则使用 claude CLI 默认模型。
            {activeModels.length === 0 && '（当前生效配置未配置模型列表，可先在「Claude 配置」中维护。）'}
          </p>
        </div>
      </div>

      {error && <div className="form-error">{error}</div>}
      {toast && <div className="role-toast">{toast}</div>}

      {roles.map(r => {
        const d = draft(r);
        const dirty = isDirty(r);
        const outOfList = modelOutOfList(d.model);
        return (
          <div className="role-card" key={r.id}>
            <div className="role-card-head">
              <div>
                <h4 className="role-name">{r.name}</h4>
                <p className="role-desc">{r.description}</p>
                <span className="role-key">key: {r.key}</span>
              </div>
              <div className="role-model-field">
                <label>模型</label>
                <select
                  className="form-input role-model-input"
                  value={outOfList ? `__legacy:${d.model}` : d.model}
                  onChange={e => {
                    const v = e.target.value;
                    update(r.id, { model: v.startsWith('__legacy:') ? v.slice(9) : v });
                  }}
                >
                  <option value="">默认（不指定）</option>
                  {activeModels.map(m => <option key={m} value={m}>{m}</option>)}
                  {outOfList && (
                    <option value={`__legacy:${d.model}`} disabled>
                      当前值：{d.model}（不在列表中）
                    </option>
                  )}
                </select>
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
