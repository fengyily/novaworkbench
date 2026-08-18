import { useState, useEffect } from 'react';
import { databaseApi, type DatabaseInfo, type MigrateResult } from '../api/client';

const DRIVER_LABELS: Record<string, string> = {
  sqlite: 'SQLite（本地文件）',
  mysql: 'MySQL',
  postgres: 'PostgreSQL',
};

const SOURCE_LABELS: Record<string, string> = {
  env: '环境变量（NOVA_DB_DRIVER / NOVA_DB_DSN）',
  file: '设置页保存（~/.novaworkbench/dbconfig.json）',
  default: '默认（未配置）',
};

export default function SettingsDatabase() {
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
      .catch(err => setError(err instanceof Error ? err.message : String(err)))
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
      setNotice(`✅ 连接成功，服务端版本：${res.version}`);
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
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
      setNotice('✅ 配置已保存。重启后端服务后生效。可先在下方执行"从 SQLite 迁移数据"。');
      const fresh = await databaseApi.get();
      setInfo(fresh);
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const handleMigrate = async () => {
    if (!confirm('将把当前 SQLite 数据库的全部数据复制到已配置的目标库（已存在的行会跳过）。继续吗？')) return;
    setMigrating(true);
    setError('');
    setNotice('');
    setMigrateResult(null);
    try {
      const res = await databaseApi.migrate();
      setMigrateResult(res);
      setNotice('✅ 迁移完成。重启后端服务后切换到新数据库。');
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setMigrating(false);
    }
  };

  if (loading) return <div className="settings-empty">加载中...</div>;

  return (
    <div className="settings-section">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">数据库</h3>
          <p className="settings-section-desc">
            NovaWorkbench 默认使用本地 SQLite 文件，无需任何配置。需要多人共享或集中管理数据时，可切换为 MySQL / PostgreSQL：先在下方测试并保存连接，再把现有数据一键迁移过去，最后重启后端生效。
          </p>
        </div>
      </div>

      {info && (
        <div className="form-group">
          <label>当前数据库</label>
          <div className="form-hint" style={{ fontSize: 14, lineHeight: 1.9 }}>
            <div>驱动：{DRIVER_LABELS[info.driver] || info.driver}</div>
            <div>来源：{SOURCE_LABELS[info.source] || info.source}</div>
            {info.driver === 'sqlite'
              ? <div>文件：{info.sqlite_path}</div>
              : <div>连接：{info.dsn_masked}</div>}
          </div>
        </div>
      )}

      {envManaged && (
        <div className="form-hint" style={{ marginBottom: 16 }}>
          当前数据库由环境变量指定，下方表单不可用。如需修改请调整 NOVA_DB_DRIVER / NOVA_DB_DSN 后重启。
        </div>
      )}

      {error && <div className="form-error">{error}</div>}
      {notice && <div className="form-hint" style={{ marginBottom: 12 }}>{notice}</div>}

      <fieldset disabled={envManaged} style={{ border: 'none', padding: 0, margin: 0 }}>
        <div className="form-group">
          <label>目标数据库类型</label>
          <select className="form-input" value={driver} onChange={e => setDriver(e.target.value)}>
            <option value="mysql">MySQL</option>
            <option value="postgres">PostgreSQL</option>
          </select>
        </div>

        <div className="form-group">
          <label>主机 / 端口</label>
          <div style={{ display: 'flex', gap: 8 }}>
            <input
              className="form-input"
              style={{ flex: 2 }}
              type="text"
              placeholder="如 127.0.0.1"
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
          <label>用户名 / 密码</label>
          <div style={{ display: 'flex', gap: 8 }}>
            <input
              className="form-input"
              style={{ flex: 1 }}
              type="text"
              placeholder="用户名"
              value={user}
              onChange={e => setUser(e.target.value)}
              autoComplete="off"
            />
            <input
              className="form-input"
              style={{ flex: 1 }}
              type="password"
              placeholder="密码"
              value={password}
              onChange={e => setPassword(e.target.value)}
              autoComplete="new-password"
            />
          </div>
        </div>

        <div className="form-group">
          <label>数据库名</label>
          <input
            className="form-input"
            type="text"
            placeholder="novaworkbench"
            value={dbname}
            onChange={e => setDbname(e.target.value)}
            autoComplete="off"
          />
          <small className="form-hint">数据库需提前创建好（建表由本服务自动完成）。MySQL 建议 utf8mb4 字符集。</small>
        </div>

        <div className="form-actions">
          <button className="btn btn-secondary" onClick={handleTest} disabled={testing || saving || migrating}>
            {testing ? '测试中...' : '🔌 测试连接'}
          </button>
          <button className="btn btn-primary" onClick={handleSave} disabled={testing || saving || migrating}>
            {saving ? '保存中...' : '💾 保存配置'}
          </button>
          {info?.driver === 'sqlite' && (
            <button className="btn btn-secondary" onClick={handleMigrate} disabled={testing || saving || migrating}>
              {migrating ? '迁移中...' : '🚚 从 SQLite 迁移数据'}
            </button>
          )}
        </div>
      </fieldset>

      {migrateResult && (
        <div className="form-group" style={{ marginTop: 16 }}>
          <label>迁移结果（目标：{DRIVER_LABELS[migrateResult.target_driver] || migrateResult.target_driver}）</label>
          <table style={{ width: '100%', fontSize: 13, borderCollapse: 'collapse' }}>
            <thead>
              <tr style={{ textAlign: 'left' }}>
                <th style={{ padding: '4px 8px' }}>表</th>
                <th style={{ padding: '4px 8px' }}>已写入</th>
                <th style={{ padding: '4px 8px' }}>已跳过（已存在）</th>
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
