import { useState, useEffect } from 'react';
import { rolesApi, type Role } from '../api/client';
import './SettingsRoles.css';

// Suggested model ids shown as a datalist; users may type any value.
const MODEL_SUGGESTIONS = [
  'claude-opus-5',
  'claude-sonnet-5',
  'claude-haiku-4-5',
  'sonnet',
  'opus',
  'haiku',
];

export default function SettingsRoles() {
  const [roles, setRoles] = useState<Role[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  // Per-role working copy (editable buffer, not yet saved).
  const [drafts, setDrafts] = useState<Record<string, { system_prompt: string; model: string }>>({});
  const [savingId, setSavingId] = useState('');
  const [resettingId, setResettingId] = useState('');

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
  }, []);

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
      const updated = await rolesApi.update(r.id, d);
      setRoles(prev => prev.map(x => (x.id === updated.id ? updated : x)));
      setDrafts(prev => ({ ...prev, [r.id]: { system_prompt: updated.system_prompt, model: updated.model } }));
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
            模型留空则使用 claude CLI 默认模型。
          </p>
        </div>
      </div>

      {error && <div className="form-error">{error}</div>}

      <datalist id="role-model-suggestions">
        {MODEL_SUGGESTIONS.map(m => <option key={m} value={m} />)}
      </datalist>

      {roles.map(r => {
        const d = draft(r);
        const dirty = isDirty(r);
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
                <input
                  className="form-input role-model-input"
                  list="role-model-suggestions"
                  placeholder="留空 = CLI 默认"
                  value={d.model}
                  onChange={e => update(r.id, { model: e.target.value })}
                />
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
