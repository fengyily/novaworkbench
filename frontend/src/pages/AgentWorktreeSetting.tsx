import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { agentWorktreeApi, type AgentWorktreeConfig } from '../api/client';
import { errorMessage } from '../utils/errMsg';

// Remote work root for Agent Servers (scratch clones + per-requirement
// worktrees). Saved globally; each run compares it with what the remote worker
// reports and relaunches the worker with the new env when they differ, so no
// manual reinstall is needed.
export default function AgentWorktreeSetting() {
  const { t } = useTranslation();
  const [cfg, setCfg] = useState<AgentWorktreeConfig | null>(null);
  const [value, setValue] = useState('');
  const [saving, setSaving] = useState(false);
  const [msg, setMsg] = useState('');
  const [error, setError] = useState('');

  useEffect(() => {
    agentWorktreeApi.get()
      .then(c => { setCfg(c); setValue(c.worktree_root); })
      .catch(err => setError(errorMessage(err)));
  }, []);

  const save = async () => {
    setSaving(true);
    setError('');
    setMsg('');
    try {
      const c = await agentWorktreeApi.update({ worktree_root: value.trim() });
      setCfg(c);
      setValue(c.worktree_root);
      setMsg(t('settings.agentWorktree.saved'));
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  if (!cfg) return error ? <div className="form-error">❌ {error}</div> : null;

  return (
    <div className="form-group" style={{ marginBottom: 16 }}>
      <label>{t('settings.agentWorktree.label')}</label>
      <div style={{ display: 'flex', gap: 8 }}>
        <input
          className="form-input"
          value={value}
          placeholder={cfg.default}
          onChange={e => setValue(e.target.value)}
        />
        <button className="btn btn-primary" onClick={save} disabled={saving}>
          {t('settings.agentWorktree.save')}
        </button>
      </div>
      <p className="settings-section-desc">{t('settings.agentWorktree.hint', { def: cfg.default })}</p>
      {cfg.env_override && (
        <p className="settings-section-desc">
          {t('settings.agentWorktree.envOverride', { path: cfg.effective })}
        </p>
      )}
      {msg && <p className="settings-section-desc">✓ {msg}</p>}
      {error && <div className="form-error">❌ {error}</div>}
    </div>
  );
}
