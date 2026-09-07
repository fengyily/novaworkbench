import { useState, useEffect, useCallback } from 'react';
import { claudeApi, type ClaudeConfigItem, type ModelEntry } from '../api/client';
import './SettingsClaude.css';

interface ConfigForm {
  name: string;
  base_url: string;
  auth_token: string;
  models: ModelEntry[];
  default_model: string;
  currency: string;
}

const emptyForm: ConfigForm = {
  name: '',
  base_url: '',
  auth_token: '',
  models: [],
  default_model: '',
  currency: '',
};

export default function SettingsClaude() {
  const [configs, setConfigs] = useState<ClaudeConfigItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [toast, setToast] = useState('');

  // Edit/create modal state.
  const [showModal, setShowModal] = useState(false);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [form, setForm] = useState<ConfigForm>(emptyForm);
  const [modelInput, setModelInput] = useState('');
  const [busyId, setBusyId] = useState<string>('');

  const load = useCallback(() => {
    setLoading(true);
    claudeApi.list()
      .then(data => setConfigs(data ?? []))
      .catch(err => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => { load(); }, [load]);

  const showToast = (msg: string) => {
    setToast(msg);
    window.setTimeout(() => setToast(''), 4000);
  };

  const openCreate = () => {
    setEditingId(null);
    setForm(emptyForm);
    setModelInput('');
    setError('');
    setShowModal(true);
  };

  const openEdit = (c: ClaudeConfigItem) => {
    setEditingId(c.id);
    setForm({
      name: c.name,
      base_url: c.base_url,
      auth_token: '',
      models: [...(c.models ?? [])],
      default_model: c.default_model ?? '',
      currency: c.currency ?? '',
    });
    setModelInput('');
    setError('');
    setShowModal(true);
  };

  const addModel = () => {
    const m = modelInput.trim();
    if (!m) return;
    if (form.models.some(x => x.model === m)) { setModelInput(''); return; }
    setForm(f => ({ ...f, models: [...f.models, { model: m, input_price: 0, output_price: 0 }] }));
    setModelInput('');
  };

  const updateModel = (idx: number, entry: ModelEntry) => {
    setForm(f => {
      const models = [...f.models];
      models[idx] = entry;
      return { ...f, models };
    });
  };

  const removeModel = (m: string) => {
    setForm(f => {
      const models = f.models.filter(x => x.model !== m);
      // If the removed model was the default, drop the default too.
      const default_model = f.default_model === m ? '' : f.default_model;
      return { ...f, models, default_model };
    });
  };

  const handleSave = async () => {
    if (!form.name.trim()) { setError('名称不能为空'); return; }
    if (form.default_model && !form.models.some(x => x.model === form.default_model)) {
      setError('默认模型必须在模型列表中'); return;
    }
    setSaving(true);
    setError('');
    try {
      if (editingId) {
        await claudeApi.update(editingId, {
          name: form.name.trim(),
          base_url: form.base_url.trim(),
          auth_token: form.auth_token || undefined,
          models: form.models,
          default_model: form.default_model,
          currency: form.currency,
        });
        showToast('配置已更新');
      } else {
        await claudeApi.create({
          name: form.name.trim(),
          base_url: form.base_url.trim(),
          auth_token: form.auth_token || undefined,
          models: form.models,
          default_model: form.default_model,
          currency: form.currency,
        });
        showToast('配置已创建');
      }
      setShowModal(false);
      load();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const handleActivate = async (c: ClaudeConfigItem) => {
    setBusyId(c.id);
    setError('');
    try {
      const res = await claudeApi.activate(c.id);
      setConfigs(res.configs ?? []);
      const modelDesc = res.applied_model ? `「${res.applied_model}」` : 'CLI 默认';
      showToast(`已切换为生效配置，各角色模型已重置为 ${modelDesc}`);
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusyId('');
    }
  };

  const handleDelete = async (c: ClaudeConfigItem) => {
    if (!confirm(`确定删除配置「${c.name}」吗？`)) return;
    setBusyId(c.id);
    setError('');
    try {
      await claudeApi.remove(c.id);
      setConfigs(prev => prev.filter(x => x.id !== c.id));
      showToast('配置已删除');
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusyId('');
    }
  };

  if (loading) return <div className="settings-empty">加载中...</div>;

  return (
    <div className="settings-section claude-configs">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">Claude CLI 配置</h3>
          <p className="settings-section-desc">
            管理多套 Claude 配置（名称 + Base URL + Auth Token + 模型列表）。
            切换生效配置后，<b>新发起的 AI 任务立即使用新配置</b>（进行中的任务不受影响），
            同时<b>所有角色的模型将重置为该配置的默认模型</b>。
          </p>
        </div>
        <button className="btn btn-primary" onClick={openCreate}>+ 添加配置</button>
      </div>

      {error && <div className="form-error">{error}</div>}
      {toast && <div className="claude-toast">{toast}</div>}

      {configs.length === 0 && !loading && (
        <div className="settings-empty">
          <p>暂无配置。点击「添加配置」开始。</p>
        </div>
      )}

      {configs.length > 0 && (
        <table className="project-table claude-config-table">
          <thead>
            <tr>
              <th>名称</th>
              <th>Base URL</th>
              <th>Token</th>
              <th>默认模型</th>
              <th>币种</th>
              <th>状态</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {configs.map(c => (
              <tr key={c.id} className={c.is_active ? 'claude-row-active' : ''}>
                <td className="project-name">{c.name}</td>
                <td className="path-cell">{c.base_url || '—'}</td>
                <td>
                  {c.auth_token_set
                    ? <span className="claude-token-preview">已设置（{c.auth_token_preview || '****'}）</span>
                    : <span className="claude-token-unset">未设置</span>}
                </td>
                <td>{c.default_model || <span className="claude-token-unset">CLI 默认</span>}</td>
                <td>{c.currency || '—'}</td>
                <td>{c.is_active && <span className="claude-active-badge">当前生效</span>}</td>
                <td className="claude-row-actions">
                  {!c.is_active && (
                    <button
                      className="btn btn-sm btn-primary"
                      onClick={() => handleActivate(c)}
                      disabled={!!busyId}
                    >
                      {busyId === c.id ? '切换中...' : '设为生效'}
                    </button>
                  )}
                  <button className="btn btn-sm btn-secondary" onClick={() => openEdit(c)} disabled={!!busyId}>
                    编辑
                  </button>
                  <button
                    className="btn-link btn-danger-link"
                    onClick={() => handleDelete(c)}
                    disabled={!!busyId || c.is_active}
                    title={c.is_active ? '不能删除当前生效的配置' : ''}
                  >
                    {busyId === c.id ? '删除中...' : '删除'}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {showModal && (
        <div className="modal-overlay" onClick={() => setShowModal(false)}>
          <div className="modal-box claude-config-modal" onClick={e => e.stopPropagation()}>
            <h3>{editingId ? '编辑配置' : '添加配置'}</h3>

            {error && <div className="form-error">{error}</div>}

            <div className="form-group">
              <label>名称</label>
              <input
                className="form-input"
                placeholder="如：官方 API / 自建网关 / 代理"
                value={form.name}
                onChange={e => setForm(f => ({ ...f, name: e.target.value }))}
              />
            </div>

            <div className="form-group">
              <label>ANTHROPIC_BASE_URL</label>
              <input
                className="form-input"
                type="text"
                placeholder="如 https://api.anthropic.com（留空使用默认）"
                value={form.base_url}
                onChange={e => setForm(f => ({ ...f, base_url: e.target.value }))}
                autoComplete="off"
              />
              <small className="form-hint">指向自建/第三方 Anthropic 兼容网关时填写。</small>
            </div>

            <div className="form-group">
              <label>ANTHROPIC_AUTH_TOKEN</label>
              <input
                className="form-input"
                type="password"
                placeholder={editingId ? '留空保持不变' : '输入 Auth Token（可选）'}
                value={form.auth_token}
                onChange={e => setForm(f => ({ ...f, auth_token: e.target.value }))}
                autoComplete="off"
              />
              <small className="form-hint">留空保存时不会修改已存在的 Token。</small>
            </div>

            <div className="form-group">
              <label>模型列表（单价：¥ 或 $ / 百万 tokens）</label>
              {form.models.map((m, idx) => (
                <div className="model-price-row" key={`${m.model}-${idx}`}>
                  <input
                    className="form-input model-name-input"
                    placeholder="模型名，如 claude-sonnet-4-5"
                    value={m.model}
                    onChange={e => updateModel(idx, { ...m, model: e.target.value })}
                  />
                  <input
                    className="form-input model-price-input"
                    type="number"
                    min="0"
                    step="0.01"
                    placeholder="输入价"
                    value={m.input_price}
                    onChange={e => updateModel(idx, { ...m, input_price: Number(e.target.value) || 0 })}
                  />
                  <input
                    className="form-input model-price-input"
                    type="number"
                    min="0"
                    step="0.01"
                    placeholder="输出价"
                    value={m.output_price}
                    onChange={e => updateModel(idx, { ...m, output_price: Number(e.target.value) || 0 })}
                  />
                  <button type="button" className="model-chip-remove" onClick={() => removeModel(m.model)}>×</button>
                </div>
              ))}
              {form.models.length === 0 && (
                <div className="claude-token-unset" style={{ margin: '4px 0 8px' }}>尚未添加模型</div>
              )}
              <div className="model-input-row">
                <input
                  className="form-input"
                  placeholder="输入模型名，如 claude-sonnet-4-5"
                  value={modelInput}
                  onChange={e => setModelInput(e.target.value)}
                  onKeyDown={e => { if (e.key === 'Enter') { e.preventDefault(); addModel(); } }}
                />
                <button type="button" className="btn btn-secondary" onClick={addModel}>添加</button>
              </div>
              <small className="form-hint">
                该配置可用的模型（角色将从这里下拉选择）；单价留 0 表示该模型不参与成本核算，
                成本按「每百万 tokens」计算，改价后历史用量立即按新价重算。
              </small>
            </div>

            <div className="form-group">
              <label>计价币种</label>
              <select
                className="form-input"
                value={form.currency}
                onChange={e => setForm(f => ({ ...f, currency: e.target.value }))}
              >
                <option value="">未设置</option>
                <option value="CNY">CNY（人民币 ¥）</option>
                <option value="USD">USD（美元 $）</option>
              </select>
              <small className="form-hint">不同平台（配置）的价格与币种可能不同，成本统计按币种分组展示。</small>
            </div>

            <div className="form-group">
              <label>默认模型</label>
              <select
                className="form-input"
                value={form.default_model}
                onChange={e => setForm(f => ({ ...f, default_model: e.target.value }))}
              >
                <option value="">不指定（CLI 默认）</option>
                {form.models.map(m => <option key={m.model} value={m.model}>{m.model}</option>)}
              </select>
              <small className="form-hint">切换为生效配置时，所有角色的模型会重置为此项。</small>
            </div>

            <div className="form-actions stack-mobile">
              <button className="btn btn-secondary" onClick={() => setShowModal(false)}>取消</button>
              <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
                {saving ? '保存中...' : '💾 保存'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
