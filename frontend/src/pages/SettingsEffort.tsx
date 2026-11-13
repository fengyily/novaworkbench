import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { settingsApi } from '../api/client';
import { errorMessage } from '../utils/errMsg';
import { fmtTime } from '../utils/intl';
import EffortBar, { type EffortTier } from '../components/EffortBar';
import { normalizeEffortTier, type EffortStage } from '../utils/effort';
import { setEffortDefaults } from '../hooks/useEffortDefaults';

// 设置 → 推理档位: the reasoning-depth policy for every claude run.
//
// The backend reads this policy on EVERY run, so a change takes effect on
// the next run — no restart. That is why the page says so explicitly: the
// whole point of the four per-stage defaults is that they can be tuned
// without redeploying.
//
// The master switch is the escape hatch. When it is off, NO run sends
// `--effort` at all and the CLI keeps its own default. That covers two
// real situations: a claude build that predates the flag, and a
// third-party gateway that rejects the extra parameter. The four pickers
// below are greyed out while it is off — showing an active-looking
// control whose value would have no effect would be a lie.
//
// The per-stage defaults are the product's own numbers: 方案设计 high,
// 开发实现 medium, 提交推送 low. The fourth row (其他任务 — analysis /
// Review / Q&A / Wiki / weekly report) ships at high, which is the CLI's
// own default level, so an install that never visits this page behaves
// exactly as it did before --effort existed.

const STAGES: EffortStage[] = ['design', 'coding', 'commit', 'other'];

export default function SettingsEffort() {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [savedAt, setSavedAt] = useState('');

  const [enabled, setEnabled] = useState(true);
  const [tiers, setTiers] = useState<Record<EffortStage, EffortTier>>({
    design: 'high',
    coding: 'medium',
    commit: 'low',
    other: 'high',
  });

  useEffect(() => {
    settingsApi
      .getEffort()
      .then((cfg) => {
        setEnabled(cfg.enabled);
        setTiers({
          design: (normalizeEffortTier(cfg.design) || 'high') as EffortTier,
          coding: (normalizeEffortTier(cfg.coding) || 'medium') as EffortTier,
          commit: (normalizeEffortTier(cfg.commit) || 'low') as EffortTier,
          other: (normalizeEffortTier(cfg.other) || 'high') as EffortTier,
        });
      })
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, []);

  const handleSave = async () => {
    setSaving(true);
    setError('');
    try {
      const cfg = await settingsApi.updateEffort({
        enabled,
        design: tiers.design,
        coding: tiers.coding,
        commit: tiers.commit,
        other: tiers.other,
      });
      // Keep the module-level cache in sync so the pickers elsewhere on the
      // page (and the next navigation) show the values that were just saved
      // instead of the ones they loaded before the save.
      setEffortDefaults(cfg);
      setEnabled(cfg.enabled);
      setTiers({
        design: (normalizeEffortTier(cfg.design) || 'high') as EffortTier,
        coding: (normalizeEffortTier(cfg.coding) || 'medium') as EffortTier,
        commit: (normalizeEffortTier(cfg.commit) || 'low') as EffortTier,
        other: (normalizeEffortTier(cfg.other) || 'high') as EffortTier,
      });
      setSavedAt(fmtTime(new Date()));
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  if (loading) return <div className="settings-empty">{t('settings.effort.loading')}</div>;

  return (
    <div className="settings-section">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">{t('settings.effort.title')}</h3>
          <p className="settings-section-desc">{t('settings.effort.desc')}</p>
        </div>
      </div>

      {error && <div className="form-error">{error}</div>}

      <div className="form-group">
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <input
            type="checkbox"
            id="effort-enabled"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
          />
          <label htmlFor="effort-enabled">{t('settings.effort.enabledLabel')}</label>
        </div>
        <small className="form-hint">{t('settings.effort.enabledHint')}</small>
      </div>

      {STAGES.map((stage) => (
        <div className="form-group" key={stage}>
          <EffortBar
            id={`effort-${stage}`}
            label={t(`settings.effort.stages.${stage}Label`)}
            value={tiers[stage]}
            disabled={!enabled || saving}
            onChange={(tier) => setTiers((prev) => ({ ...prev, [stage]: tier }))}
          />
          <small className="form-hint">{t(`settings.effort.stages.${stage}Hint`)}</small>
        </div>
      ))}

      <div className="form-actions stack-mobile">
        <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
          {saving ? t('settings.effort.saving') : t('settings.effort.save')}
        </button>
        {savedAt && !saving && (
          <span className="form-hint">{t('settings.effort.savedAt', { time: savedAt })}</span>
        )}
      </div>
    </div>
  );
}
