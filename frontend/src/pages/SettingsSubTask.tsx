import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { subTaskConfigApi } from '../api/client';
import { errorMessage } from '../utils/errMsg';
import { fmtTime } from '../utils/intl';

// 设置 → 子任务: the two execution knobs behind sub-task dispatch.
//   - 并发数        → how many sub-tasks ONE project may run at once. The
//                     backend gate is per project, so raising it never lets a
//                     single project starve the others.
//   - 失败自动重做   → whether a failed auto-orchestrated child is re-dispatched
//                     automatically. Default off: recovery stays a manual
//                     action (the card's 重做 / 继续 buttons) unless opted in.
//   - 最大自动重做次数 → cap for the above, so a deterministically failing child
//                     stops instead of looping.
// Saving only writes the settings rows; the orchestration tick and the
// sub-task runner re-read them on their next pass, so no restart is needed.
export default function SettingsSubTask() {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [savedAt, setSavedAt] = useState('');

  const [concurrency, setConcurrency] = useState(1);
  const [autoRetry, setAutoRetry] = useState(false);
  const [retryMax, setRetryMax] = useState(1);

  useEffect(() => {
    subTaskConfigApi.get()
      .then(cfg => {
        setConcurrency(cfg.concurrency);
        setAutoRetry(cfg.auto_retry);
        setRetryMax(cfg.retry_max);
      })
      .catch(err => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, []);

  const handleSave = async () => {
    setSaving(true);
    setError('');
    try {
      const cfg = await subTaskConfigApi.update({
        concurrency: Math.max(1, Math.floor(concurrency) || 1),
        auto_retry: autoRetry,
        retry_max: Math.max(0, Math.floor(retryMax) || 0),
      });
      setConcurrency(cfg.concurrency);
      setAutoRetry(cfg.auto_retry);
      setRetryMax(cfg.retry_max);
      setSavedAt(fmtTime(new Date()));
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  if (loading) return <div className="settings-empty">{t('settings.subtask.loading')}</div>;

  return (
    <div className="settings-section">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">{t('settings.subtask.title')}</h3>
          <p className="settings-section-desc">{t('settings.subtask.desc')}</p>
        </div>
      </div>

      {error && <div className="form-error">{error}</div>}

      <div className="form-group">
        <label>{t('settings.subtask.concurrencyLabel')}</label>
        <input
          className="form-input"
          type="number"
          min={1}
          value={concurrency}
          onChange={e => setConcurrency(Number(e.target.value))}
        />
        <small className="form-hint">{t('settings.subtask.concurrencyHint')}</small>
      </div>

      <div className="form-group">
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <input
            type="checkbox"
            id="subtask-auto-retry"
            checked={autoRetry}
            onChange={e => setAutoRetry(e.target.checked)}
          />
          <label htmlFor="subtask-auto-retry">{t('settings.subtask.autoRetryLabel')}</label>
        </div>
        <small className="form-hint">{t('settings.subtask.autoRetryHint')}</small>
      </div>

      <div className="form-group">
        <label>{t('settings.subtask.retryMaxLabel')}</label>
        <input
          className="form-input"
          type="number"
          min={0}
          value={retryMax}
          disabled={!autoRetry}
          onChange={e => setRetryMax(Number(e.target.value))}
        />
        <small className="form-hint">{t('settings.subtask.retryMaxHint')}</small>
      </div>

      <div className="form-actions stack-mobile">
        <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
          {saving ? t('settings.subtask.saving') : t('settings.subtask.save')}
        </button>
        {savedAt && !saving && <span className="form-hint">{t('settings.subtask.savedAt', { time: savedAt })}</span>}
      </div>
    </div>
  );
}
