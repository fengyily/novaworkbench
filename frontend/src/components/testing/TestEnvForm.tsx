import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  testingApi,
  type CreateTestEnvReq,
  type TestEnvironment,
} from '../../api/client';
import { errorMessage } from '../../utils/errMsg';

interface Props {
  projectId: string;
  /** Present when editing; absent when creating. */
  env: TestEnvironment | null;
  onClose: () => void;
  onSaved: () => void;
  onDeleted: () => void;
  /** Called with the init job id once the user starts initialization. */
  onStartInit: (jobId: string, envId: string) => void;
}

const BROWSERS = ['chromium', 'firefox', 'webkit'] as const;

const DEFAULT_SEED = `import { test, expect } from '@playwright/test';

// 供 planner agent 探索应用用的种子测试。替换为真实登录流程，
// 生成的用例就能直接带上已认证的会话。
test('seed', async ({ page }) => {
  await page.goto('/');
  await expect(page.locator('body')).toBeVisible();
});
`;

/**
 * TestEnvForm creates or edits one Playwright environment.
 *
 * In edit mode it doubles as the place init is triggered: initialization is
 * a property of the environment, so it belongs on the environment's own form
 * rather than in a separate dialog.
 */
export default function TestEnvForm({ projectId, env, onClose, onSaved, onDeleted, onStartInit }: Props) {
  const { t } = useTranslation();
  const editing = env !== null;

  const [name, setName] = useState('');
  const [baseUrl, setBaseUrl] = useState('');
  const [seedTest, setSeedTest] = useState(DEFAULT_SEED);
  const [browsers, setBrowsers] = useState<string[]>(['chromium']);
  const [envVars, setEnvVars] = useState('');
  const [workspaceOverride, setWorkspaceOverride] = useState('');
  const [retentionRuns, setRetentionRuns] = useState(20);
  const [purge, setPurge] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  useEffect(() => {
    if (!env) return;
    setName(env.name);
    setBaseUrl(env.base_url);
    setSeedTest(env.seed_test || DEFAULT_SEED);
    setBrowsers(env.browsers ? env.browsers.split(',') : ['chromium']);
    setWorkspaceOverride(env.workspace_override);
    setRetentionRuns(env.retention_runs || 20);
    // Env var VALUES are never returned by the API, so the textarea starts
    // empty with a hint rather than showing a placeholder of the real values.
    setEnvVars('');
  }, [env]);

  /** Parse "KEY=VALUE" lines; blank lines and # comments are ignored. */
  const parseEnvVars = (raw: string): Record<string, string> => {
    const out: Record<string, string> = {};
    for (const line of raw.split('\n')) {
      const trimmed = line.trim();
      if (!trimmed || trimmed.startsWith('#')) continue;
      const idx = trimmed.indexOf('=');
      if (idx <= 0) continue;
      const k = trimmed.slice(0, idx).trim();
      const v = trimmed.slice(idx + 1).trim();
      if (k) out[k] = v;
    }
    return out;
  };

  const toggleBrowser = (b: string) => {
    setBrowsers((prev) =>
      prev.includes(b) ? (prev.length > 1 ? prev.filter((x) => x !== b) : prev) : [...prev, b],
    );
  };

  const doSave = async () => {
    setBusy(true);
    setError('');
    try {
      const body: CreateTestEnvReq = {
        name: name.trim(),
        base_url: baseUrl.trim(),
        seed_test: seedTest,
        browsers: browsers.join(','),
        workspace_override: workspaceOverride.trim(),
        retention_runs: retentionRuns,
      };
      // Only send env vars when the user typed something — an empty box must
      // not be read as "clear all of them" on an unrelated edit.
      if (envVars.trim()) body.env_vars = parseEnvVars(envVars);

      if (editing) {
        await testingApi.updateEnv(env!.id, body);
      } else {
        await testingApi.createEnv(projectId, body);
      }
      onSaved();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const doDelete = async () => {
    if (!env) return;
    if (!window.confirm(t('testing.envDeleteConfirm'))) return;
    setBusy(true);
    setError('');
    try {
      await testingApi.deleteEnv(env.id, purge);
      onDeleted();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  const doInit = async () => {
    if (!env) return;
    setBusy(true);
    setError('');
    try {
      const res = await testingApi.startEnvInit(env.id);
      onStartInit(res.job_id, env.id);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const initStatus = env?.init_status ?? 'pending';
  const canInit = !env || initStatus !== 'initializing';

  return (
    <div className="tp-section">
      <div className="tp-section-head">
        <span>{editing ? t('testing.envEdit') : t('testing.envCreate')}</span>
        <button className="btn btn-sm" onClick={onClose}>{t('testing.close')}</button>
      </div>
      <div className="tp-section-body">
        {error && <div className="tp-error">{error}</div>}

        <div className="tp-field">
          <label>{t('testing.envName')}</label>
          <input
            type="text"
            className="form-input"
            value={name}
            placeholder={t('testing.envNamePlaceholder')}
            onChange={(e) => setName(e.target.value)}
          />
        </div>

        <div className="tp-field">
          <label>{t('testing.envBaseUrl')}</label>
          <input
            type="text"
            className="form-input"
            value={baseUrl}
            placeholder={t('testing.envBaseUrlPlaceholder')}
            onChange={(e) => setBaseUrl(e.target.value)}
          />
          <p className="tp-hint">{t('testing.envBaseUrlHint')}</p>
        </div>

        <div className="tp-field">
          <label>{t('testing.envBrowsers')}</label>
          <div className="tp-inline">
            {BROWSERS.map((b) => (
              <label key={b} className="tp-inline" style={{ gap: 4 }}>
                <input type="checkbox" checked={browsers.includes(b)} onChange={() => toggleBrowser(b)} />
                <span style={{ fontSize: 13 }}>{b}</span>
              </label>
            ))}
          </div>
        </div>

        <div className="tp-field">
          <label>{t('testing.envSeed')}</label>
          <textarea
            className="form-input"
            value={seedTest}
            onChange={(e) => setSeedTest(e.target.value)}
            spellCheck={false}
          />
          <p className="tp-hint">{t('testing.envSeedHint')}</p>
        </div>

        <div className="tp-field">
          <label>
            {t('testing.envEnvVars')}
            {editing && env?.env_vars_set ? ` (${t('testing.envSeedConfigured')})` : ''}
          </label>
          <textarea
            className="form-input"
            style={{ minHeight: 70 }}
            value={envVars}
            onChange={(e) => setEnvVars(e.target.value)}
            placeholder="KEY=VALUE"
            spellCheck={false}
          />
          <p className="tp-hint">
            {t('testing.envEnvVarsHint')}
            {editing && env?.env_vars_set ? ` — ${t('testing.envVarsUnchanged')}` : ''}
          </p>
        </div>

        <div className="tp-field">
          <label>{t('testing.envWorkspace')}</label>
          <input
            type="text"
            className="form-input"
            value={workspaceOverride}
            onChange={(e) => setWorkspaceOverride(e.target.value)}
          />
          <p className="tp-hint">{t('testing.envWorkspaceHint')}</p>
        </div>

        <div className="tp-field">
          <label>{t('testing.envRetention')}</label>
          <input
            type="number"
            min={1}
            max={500}
            className="form-input"
            style={{ width: 120 }}
            value={retentionRuns}
            onChange={(e) => setRetentionRuns(Number(e.target.value) || 20)}
          />
        </div>

        <div className="tp-inline">
          <button className="btn btn-sm btn-primary" disabled={busy || !name.trim() || !baseUrl.trim()} onClick={doSave}>
            {t('testing.save')}
          </button>
          {editing && (
            <>
              <button className="btn btn-sm" disabled={busy || !canInit} onClick={doInit}>
                {initStatus === 'ready' ? t('testing.envReinit') : t('testing.envInit')}
              </button>
              <label className="tp-inline" style={{ gap: 4 }}>
                <input type="checkbox" checked={purge} onChange={(e) => setPurge(e.target.checked)} />
                <span style={{ fontSize: 12 }}>{t('testing.envPurge')}</span>
              </label>
              <button className="btn btn-sm btn-danger" disabled={busy} onClick={doDelete}>
                {t('testing.delete')}
              </button>
            </>
          )}
        </div>
        {editing && <p className="tp-hint">{t('testing.initHint')}</p>}
        {editing && env?.init_status === 'failed' && env.init_error && (
          <p className="tp-hint" style={{ color: '#b91c1c' }}>{env.init_error}</p>
        )}
      </div>
    </div>
  );
}
