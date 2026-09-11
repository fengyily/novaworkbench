import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { databaseApi, type DatabaseInfo, type MigrateResult } from '../api/client';
import { errorMessage } from '../utils/errMsg';

// Driver/source labels are translation KEYS resolved at render time.
const DRIVER_LABELS: Record<string, string> = {
  sqlite: 'settings.database.driver.sqlite',
  mysql: 'settings.database.driver.mysql',
  postgres: 'settings.database.driver.postgres',
};

const SOURCE_LABELS: Record<string, string> = {
  env: 'settings.database.source.env',
  file: 'settings.database.source.file',
  default: 'settings.database.source.default',
};

export default function SettingsDatabase() {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(true);
  const [info, setInfo] = useState<DatabaseInfo | null>(null);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');

  const [driver, setDriver] = useState('mysql');
  const [host, setHost] = useState('127.0.0.1');
  const [port, setPort] = useState('');
  const [user, setUser] = useState('');
  const [password, setPassword] = useState('');
  const [dbname, setDbname] = useState('novaworkbench');

  const [testing, setTesting] = useState(false);
  const [saving, setSaving] = useState(false);
  const [migrating, setMigrating] = useState(false);
  const [migrateResult, setMigrateResult] = useState<MigrateResult | null>(null);

  useEffect(() => {
    databaseApi.get()
      .then(setInfo)
      .catch(err => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, []);

  const connReq = () => ({
    driver,
    host: host.trim(),
    port: port.trim(),
    user: user.trim(),
    password,
    dbname: dbname.trim(),
  });

  const envManaged = info?.source === 'env';

  const handleTest = async () => {
    setTesting(true);
    setError('');
    setNotice('');
    try {
      const res = await databaseApi.test(connReq());
      setNotice(t('settings.database.testOk', { version: res.version }));
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setTesting(false);
    }
  };

  const handleSave = async () => {
    setSaving(true);
    setError('');
    setNotice('');
    try {
      await databaseApi.save(connReq());
      setNotice(t('settings.database.saved'));
      const fresh = await databaseApi.get();
      setInfo(fresh);
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  const handleMigrate = async () => {
    if (!confirm(t('settings.database.migrateConfirm'))) return;
    setMigrating(true);
    setError('');
    setNotice('');
    setMigrateResult(null);
    try {
      const res = await databaseApi.migrate();
      setMigrateResult(res);
      setNotice(t('settings.database.migrated'));
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setMigrating(false);
    }
  };

  if (loading) return <div className="settings-empty">{t('settings.database.loading')}</div>;

  return (
    <div className="settings-section">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">{t('settings.database.title')}</h3>
          <p className="settings-section-desc">{t('settings.database.desc')}</p>
        </div>
      </div>

      {info && (
        <div className="form-group">
          <label>{t('settings.database.currentLabel')}</label>
          <div className="form-hint" style={{ fontSize: 14, lineHeight: 1.9 }}>
            <div>{t('settings.database.driverLabel')}{t(DRIVER_LABELS[info.driver]) || info.driver}</div>
            <div>{t('settings.database.sourceLabel')}{t(SOURCE_LABELS[info.source]) || info.source}</div>
            {info.driver === 'sqlite'
              ? <div>{t('settings.database.fileLabel')}{info.sqlite_path}</div>
              : <div>{t('settings.database.connLabel')}{info.dsn_masked}</div>}
          </div>
        </div>
      )}

      {envManaged && (
        <div className="form-hint" style={{ marginBottom: 16 }}>
          {t('settings.database.envManaged')}
        </div>
      )}

      {error && <div className="form-error">{error}</div>}
      {notice && <div className="form-hint" style={{ marginBottom: 12 }}>{notice}</div>}

      <fieldset disabled={envManaged} style={{ border: 'none', padding: 0, margin: 0 }}>
        <div className="form-group">
          <label>{t('settings.database.targetLabel')}</label>
          <select className="form-input" value={driver} onChange={e => setDriver(e.target.value)}>
            <option value="mysql">MySQL</option>
            <option value="postgres">PostgreSQL</option>
          </select>
        </div>

        <div className="form-group">
          <label>{t('settings.database.hostPortLabel')}</label>
          <div style={{ display: 'flex', gap: 8 }}>
            <input
              className="form-input"
              style={{ flex: 2 }}
              type="text"
              placeholder={t('settings.database.hostPlaceholder')}
              value={host}
              onChange={e => setHost(e.target.value)}
              autoComplete="off"
            />
            <input
              className="form-input"
              style={{ flex: 1 }}
              type="text"
              placeholder={driver === 'mysql' ? '3306' : '5432'}
              value={port}
              onChange={e => setPort(e.target.value)}
              autoComplete="off"
            />
          </div>
        </div>

        <div className="form-group">
          <label>{t('settings.database.userPassLabel')}</label>
          <div style={{ display: 'flex', gap: 8 }}>
            <input
              className="form-input"
              style={{ flex: 1 }}
              type="text"
              placeholder={t('settings.database.userPlaceholder')}
              value={user}
              onChange={e => setUser(e.target.value)}
              autoComplete="off"
            />
            <input
              className="form-input"
              style={{ flex: 1 }}
              type="password"
              placeholder={t('settings.database.passwordPlaceholder')}
              value={password}
              onChange={e => setPassword(e.target.value)}
              autoComplete="new-password"
            />
          </div>
        </div>

        <div className="form-group">
          <label>{t('settings.database.dbNameLabel')}</label>
          <input
            className="form-input"
            type="text"
            placeholder="novaworkbench"
            value={dbname}
            onChange={e => setDbname(e.target.value)}
            autoComplete="off"
          />
          <small className="form-hint">{t('settings.database.dbNameHint')}</small>
        </div>

        <div className="form-actions">
          <button className="btn btn-secondary" onClick={handleTest} disabled={testing || saving || migrating}>
            {testing ? t('settings.database.testing') : t('settings.database.test')}
          </button>
          <button className="btn btn-primary" onClick={handleSave} disabled={testing || saving || migrating}>
            {saving ? t('settings.database.saving') : t('settings.database.save')}
          </button>
          {info?.driver === 'sqlite' && (
            <button className="btn btn-secondary" onClick={handleMigrate} disabled={testing || saving || migrating}>
              {migrating ? t('settings.database.migrating') : t('settings.database.migrate')}
            </button>
          )}
        </div>
      </fieldset>

      {migrateResult && (
        <div className="form-group" style={{ marginTop: 16 }}>
          <label>{t('settings.database.migrateResultLabel', { target: t(DRIVER_LABELS[migrateResult.target_driver]) || migrateResult.target_driver })}</label>
          <table style={{ width: '100%', fontSize: 13, borderCollapse: 'collapse' }}>
            <thead>
              <tr style={{ textAlign: 'left' }}>
                <th style={{ padding: '4px 8px' }}>{t('settings.database.colTable')}</th>
                <th style={{ padding: '4px 8px' }}>{t('settings.database.colInserted')}</th>
                <th style={{ padding: '4px 8px' }}>{t('settings.database.colSkipped')}</th>
              </tr>
            </thead>
            <tbody>
              {migrateResult.tables.map(t => (
                <tr key={t.table}>
                  <td style={{ padding: '4px 8px' }}>{t.table}</td>
                  <td style={{ padding: '4px 8px' }}>{t.inserted}</td>
                  <td style={{ padding: '4px 8px' }}>{t.skipped}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
