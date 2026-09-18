import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { gitSyncConfigApi } from '../api/client';
import { errorMessage } from '../utils/errMsg';
import { fmtTime } from '../utils/intl';

// 设置 → Git 同步: the single knob behind the design-stage hard sync.
//
// 同步超时（秒） → max wall-clock time the architect-design prologue is
//                   allowed to spend on its three dirty-state gates + the
//                   `git fetch origin <base>` + `merge --ff-only` chain.
//                   The backend reads this on every design run, so saving
//                   takes effect without a restart; large repos on first
//                   fetch over a slow link may need to raise this.
//
// Range is clamped server-side to [10, 600]; values outside the clamp are
// silently snapped to the nearest bound by SetGitSyncTimeout.
export default function SettingsGitSync() {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [savedAt, setSavedAt] = useState('');

  const [timeoutSeconds, setTimeoutSeconds] = useState(60);

  useEffect(() => {
    gitSyncConfigApi.get()
      .then(cfg => {
        setTimeoutSeconds(cfg.timeout_seconds);
      })
      .catch(err => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, []);

  const handleSave = async () => {
    setSaving(true);
    setError('');
    try {
      const cfg = await gitSyncConfigApi.update({
        timeout_seconds: Math.max(10, Math.min(600, Math.floor(timeoutSeconds) || 60)),
      });
      setTimeoutSeconds(cfg.timeout_seconds);
      setSavedAt(fmtTime(new Date()));
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  if (loading) return <div className="settings-empty">{t('settings.gitSync.loading')}</div>;

  return (
    <div className="settings-section">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">{t('settings.gitSync.title')}</h3>
          <p className="settings-section-desc">{t('settings.gitSync.desc')}</p>
        </div>
      </div>

      {error && <div className="form-error">{error}</div>}

      <div className="form-group">
        <label>{t('settings.gitSync.timeoutLabel')}</label>
        <input
          className="form-input"
          type="number"
          min={10}
          max={600}
          value={timeoutSeconds}
          onChange={e => setTimeoutSeconds(Number(e.target.value))}
        />
        <small className="form-hint">{t('settings.gitSync.timeoutHint')}</small>
      </div>

      <div className="form-actions stack-mobile">
        <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
          {saving ? t('settings.gitSync.saving') : t('settings.gitSync.save')}
        </button>
        {savedAt && !saving && <span className="form-hint">{t('settings.gitSync.savedAt', { time: savedAt })}</span>}
      </div>
    </div>
  );
}
