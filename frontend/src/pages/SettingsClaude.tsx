import { useState, useEffect, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { claudeApi, type ClaudeConfigItem, type ModelEntry } from '../api/client';
import { errorMessage } from '../utils/errMsg';
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
  const { t } = useTranslation();
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
      .catch(err => setError(errorMessage(err)))
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
    if (!form.name.trim()) { setError(t('settings.claude.errNameRequired')); return; }
    if (form.default_model && !form.models.some(x => x.model === form.default_model)) {
      setError(t('settings.claude.errDefaultModelMissing')); return;
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
        showToast(t('settings.claude.toastUpdated'));
      } else {
        await claudeApi.create({
          name: form.name.trim(),
          base_url: form.base_url.trim(),
          auth_token: form.auth_token || undefined,
          models: form.models,
          default_model: form.default_model,
          currency: form.currency,
        });
        showToast(t('settings.claude.toastCreated'));
      }
      setShowModal(false);
      load();
    } catch (err: unknown) {
      setError(errorMessage(err));
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
      const modelDesc = res.applied_model ? `「${res.applied_model}」` : t('settings.claude.cliDefault');
      showToast(t('settings.claude.activateToast', { model: modelDesc }));
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setBusyId('');
    }
  };

  const handleDelete = async (c: ClaudeConfigItem) => {
    if (!confirm(t('settings.claude.deleteConfirm', { name: c.name }))) return;
    setBusyId(c.id);
    setError('');
    try {
      await claudeApi.remove(c.id);
      setConfigs(prev => prev.filter(x => x.id !== c.id));
      showToast(t('settings.claude.toastDeleted'));
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setBusyId('');
    }
  };

  if (loading) return <div className="settings-empty">{t('settings.claude.loading')}</div>;

  return (
    <div className="settings-section claude-configs">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">{t('settings.claude.title')}</h3>
          <p className="settings-section-desc">
            {t('settings.claude.descPrefix')}<b>{t('settings.claude.descBold1')}</b>{t('settings.claude.descMiddle')}
            <b>{t('settings.claude.descBold2')}</b>{t('settings.claude.descSuffix')}
          </p>
        </div>
        <button className="btn btn-primary" onClick={openCreate}>{t('settings.claude.add')}</button>
      </div>

      {error && <div className="form-error">{error}</div>}
      {toast && <div className="claude-toast">{toast}</div>}

      {configs.length === 0 && !loading && (
        <div className="settings-empty">
          <p>{t('settings.claude.empty')}</p>
        </div>
      )}

      {configs.length > 0 && (
        <table className="project-table claude-config-table">
          <thead>
            <tr>
              <th>{t('settings.claude.colName')}</th>
              <th>{t('settings.claude.colBaseUrl')}</th>
              <th>{t('settings.claude.colToken')}</th>
              <th>{t('settings.claude.colDefaultModel')}</th>
              <th>{t('settings.claude.colCurrency')}</th>
              <th>{t('settings.claude.colStatus')}</th>
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
                    ? <span className="claude-token-preview">{t('settings.claude.tokenSet', { preview: c.auth_token_preview || '****' })}</span>
                    : <span className="claude-token-unset">{t('settings.claude.tokenUnset')}</span>}
                </td>
                <td>{c.default_model || <span className="claude-token-unset">{t('settings.claude.cliDefault')}</span>}</td>
                <td>{c.currency || '—'}</td>
                <td>{c.is_active && <span className="claude-active-badge">{t('settings.claude.activeBadge')}</span>}</td>
                <td className="claude-row-actions">
                  {!c.is_active && (
                    <button
                      className="btn btn-sm btn-primary"
                      onClick={() => handleActivate(c)}
                      disabled={!!busyId}
                    >
                      {busyId === c.id ? t('settings.claude.activating') : t('settings.claude.activate')}
                    </button>
                  )}
                  <button className="btn btn-sm btn-secondary" onClick={() => openEdit(c)} disabled={!!busyId}>
                    {t('settings.claude.edit')}
                  </button>
                  <button
                    className="btn-link btn-danger-link"
                    onClick={() => handleDelete(c)}
                    disabled={!!busyId || c.is_active}
                    title={c.is_active ? t('settings.claude.cannotDeleteActive') : ''}
                  >
                    {busyId === c.id ? t('settings.claude.deleting') : t('settings.claude.delete')}
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
            <h3>{editingId ? t('settings.claude.modal.editTitle') : t('settings.claude.modal.createTitle')}</h3>

            {error && <div className="form-error">{error}</div>}

            <div className="form-group">
              <label>{t('settings.claude.modal.nameLabel')}</label>
              <input
                className="form-input"
                placeholder={t('settings.claude.modal.namePlaceholder')}
                value={form.name}
                onChange={e => setForm(f => ({ ...f, name: e.target.value }))}
              />
            </div>

            <div className="form-group">
              <label>{t('settings.claude.modal.baseUrlLabel')}</label>
              <input
                className="form-input"
                type="text"
                placeholder={t('settings.claude.modal.baseUrlPlaceholder')}
                value={form.base_url}
                onChange={e => setForm(f => ({ ...f, base_url: e.target.value }))}
                autoComplete="off"
              />
              <small className="form-hint">{t('settings.claude.modal.baseUrlHint')}</small>
            </div>

            <div className="form-group">
              <label>{t('settings.claude.modal.tokenLabel')}</label>
              <input
                className="form-input"
                type="password"
                placeholder={editingId ? t('settings.claude.modal.tokenPlaceholderEdit') : t('settings.claude.modal.tokenPlaceholderNew')}
                value={form.auth_token}
                onChange={e => setForm(f => ({ ...f, auth_token: e.target.value }))}
                autoComplete="off"
              />
              <small className="form-hint">{t('settings.claude.modal.tokenHint')}</small>
            </div>

            <div className="form-group">
              <label>{t('settings.claude.modal.modelsLabel')}</label>
              {form.models.map((m, idx) => (
                <div className="model-price-row" key={`${m.model}-${idx}`}>
                  <input
                    className="form-input model-name-input"
                    placeholder={t('settings.claude.modal.modelNamePlaceholder')}
                    value={m.model}
                    onChange={e => updateModel(idx, { ...m, model: e.target.value })}
                  />
                  <input
                    className="form-input model-price-input"
                    type="number"
                    min="0"
                    step="0.01"
                    placeholder={t('settings.claude.modal.inputPricePlaceholder')}
                    value={m.input_price}
                    onChange={e => updateModel(idx, { ...m, input_price: Number(e.target.value) || 0 })}
                  />
                  <input
                    className="form-input model-price-input"
                    type="number"
                    min="0"
                    step="0.01"
                    placeholder={t('settings.claude.modal.outputPricePlaceholder')}
                    value={m.output_price}
                    onChange={e => updateModel(idx, { ...m, output_price: Number(e.target.value) || 0 })}
                  />
                  <button type="button" className="model-chip-remove" onClick={() => removeModel(m.model)}>×</button>
                </div>
              ))}
              {form.models.length === 0 && (
                <div className="claude-token-unset" style={{ margin: '4px 0 8px' }}>{t('settings.claude.modal.noModels')}</div>
              )}
              <div className="model-input-row">
                <input
                  className="form-input"
                  placeholder={t('settings.claude.modal.modelInputPlaceholder')}
                  value={modelInput}
                  onChange={e => setModelInput(e.target.value)}
                  onKeyDown={e => { if (e.key === 'Enter') { e.preventDefault(); addModel(); } }}
                />
                <button type="button" className="btn btn-secondary" onClick={addModel}>{t('settings.claude.modal.addModel')}</button>
              </div>
              <small className="form-hint">{t('settings.claude.modal.modelsHint')}</small>
            </div>

            <div className="form-group">
              <label>{t('settings.claude.modal.currencyLabel')}</label>
              <select
                className="form-input"
                value={form.currency}
                onChange={e => setForm(f => ({ ...f, currency: e.target.value }))}
              >
                <option value="">{t('settings.claude.modal.currencyUnset')}</option>
                <option value="CNY">{t('settings.claude.modal.currencyCNY')}</option>
                <option value="USD">{t('settings.claude.modal.currencyUSD')}</option>
              </select>
              <small className="form-hint">{t('settings.claude.modal.currencyHint')}</small>
            </div>

            <div className="form-group">
              <label>{t('settings.claude.modal.defaultModelLabel')}</label>
              <select
                className="form-input"
                value={form.default_model}
                onChange={e => setForm(f => ({ ...f, default_model: e.target.value }))}
              >
                <option value="">{t('settings.claude.modal.defaultModelUnspecified')}</option>
                {form.models.map(m => <option key={m.model} value={m.model}>{m.model}</option>)}
              </select>
              <small className="form-hint">{t('settings.claude.modal.defaultModelHint')}</small>
            </div>

            <div className="form-actions stack-mobile">
              <button className="btn btn-secondary" onClick={() => setShowModal(false)}>{t('settings.claude.modal.cancel')}</button>
              <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
                {saving ? t('settings.claude.modal.saving') : t('settings.claude.modal.save')}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
