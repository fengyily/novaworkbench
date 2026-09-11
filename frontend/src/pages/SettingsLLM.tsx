import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { llmApi } from '../api/client';
import { errorMessage } from '../utils/errMsg';
import { fmtTime } from '../utils/intl';

export default function SettingsLLM() {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [savedAt, setSavedAt] = useState('');

  const [keySet, setKeySet] = useState(false);
  const [keyPreview, setKeyPreview] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [baseURL, setBaseURL] = useState('');
  const [model, setModel] = useState('');

  useEffect(() => {
    llmApi.get()
      .then(cfg => {
        setKeySet(cfg.api_key_set);
        setKeyPreview(cfg.api_key_preview);
        setBaseURL(cfg.base_url || '');
        setModel(cfg.model || '');
      })
      .catch(err => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, []);

  const handleSave = async () => {
    setSaving(true);
    setError('');
    try {
      // Only send the api key if the user typed a new one; leaving it blank
      // keeps the existing secret server-side.
      const cfg = await llmApi.update({
        base_url: baseURL.trim(),
        api_key: apiKey || undefined,
        model: model.trim(),
      });
      setKeySet(cfg.api_key_set);
      setKeyPreview(cfg.api_key_preview);
      setApiKey('');
      setSavedAt(fmtTime(new Date()));
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  const handleClearKey = async () => {
    if (!confirm(t('settings.llm.clearConfirm'))) return;
    setSaving(true);
    setError('');
    try {
      const cfg = await llmApi.update({
        base_url: baseURL.trim(),
        model: model.trim(),
        clear_api_key: true,
      });
      setKeySet(cfg.api_key_set);
      setKeyPreview(cfg.api_key_preview);
      setApiKey('');
      setSavedAt(fmtTime(new Date()));
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  if (loading) return <div className="settings-empty">{t('settings.llm.loading')}</div>;

  return (
    <div className="settings-section">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">{t('settings.llm.title')}</h3>
          <p className="settings-section-desc">{t('settings.llm.desc')}</p>
        </div>
      </div>

      {error && <div className="form-error">{error}</div>}

      <div className="form-group">
        <label>{t('settings.llm.keyLabel')}</label>
        <input
          className="form-input"
          type="password"
          placeholder={keySet ? t('settings.llm.keyPlaceholderSet', { preview: keyPreview }) : t('settings.llm.keyPlaceholderNew')}
          value={apiKey}
          onChange={e => setApiKey(e.target.value)}
          autoComplete="off"
        />
        <small className="form-hint">
          {keySet
            ? t('settings.llm.keyHintSet', { preview: keyPreview })
            : t('settings.llm.keyHintUnset')}
        </small>
      </div>

      <div className="form-group">
        <label>{t('settings.llm.baseUrlLabel')}</label>
        <input
          className="form-input"
          type="text"
          placeholder={t('settings.llm.baseUrlPlaceholder')}
          value={baseURL}
          onChange={e => setBaseURL(e.target.value)}
          autoComplete="off"
        />
        <small className="form-hint">
          {t('settings.llm.baseUrlHint')}
        </small>
      </div>

      <div className="form-group">
        <label>{t('settings.llm.modelLabel')}</label>
        <input
          className="form-input"
          type="text"
          placeholder={t('settings.llm.modelPlaceholder')}
          value={model}
          onChange={e => setModel(e.target.value)}
          autoComplete="off"
        />
        <small className="form-hint">
          {t('settings.llm.modelHint')}
        </small>
      </div>

      <div className="form-actions stack-mobile">
        <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
          {saving ? t('settings.llm.saving') : t('settings.llm.save')}
        </button>
        {savedAt && !saving && <span className="form-hint">{t('settings.llm.savedAt', { time: savedAt })}</span>}
        {keySet && (
          <button className="btn btn-secondary" onClick={handleClearKey} disabled={saving}>
            {t('settings.llm.clearKey')}
          </button>
        )}
      </div>
    </div>
  );
}
