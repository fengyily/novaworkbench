import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { testingApi, type TestEnvironment, type TestPlan, type TestRun } from '../../api/client';
import { errorMessage } from '../../utils/errMsg';
import TestRunDetail from './TestRunDetail';

interface Props {
  projectId: string;
  env: TestEnvironment;
  canManage: boolean;
  subscribe: (jobId: string, title: string, onDone?: () => void) => void;
}

const STATUS_CLASS: Record<string, string> = {
  running: 'status-running',
  passed: 'status-succeeded',
  failed: 'status-failed',
  canceled: 'status-pending',
  error: 'status-error',
};

/**
 * TestRunList launches batch runs and shows their history.
 *
 * The run scope mirrors the backend's RunScope: "all" walks every generated
 * plan in the environment, "plans" narrows to a selection, "files" takes an
 * explicit list. Healer is opt-in per run because it spawns another agent
 * turn and a second full execution.
 */
export default function TestRunList({ projectId, env, canManage, subscribe }: Props) {
  const { t } = useTranslation();

  const [runs, setRuns] = useState<TestRun[]>([]);
  const [plans, setPlans] = useState<TestPlan[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [expandedId, setExpandedId] = useState('');

  const [startOpen, setStartOpen] = useState(false);
  const [scopeMode, setScopeMode] = useState<'all' | 'plans' | 'files'>('all');
  const [selectedPlanIds, setSelectedPlanIds] = useState<string[]>([]);
  const [specFiles, setSpecFiles] = useState('');
  const [healer, setHealer] = useState(false);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const [runList, planList] = await Promise.all([
        testingApi.listRuns(projectId, env.id),
        testingApi.listPlans(projectId, env.id),
      ]);
      setRuns(runList ?? []);
      setPlans(planList ?? []);
      setError('');
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setLoading(false);
    }
  }, [projectId, env.id]);

  useEffect(() => {
    void load();
  }, [load]);

  const togglePlan = (id: string) => {
    setSelectedPlanIds((prev) => (prev.includes(id) ? prev.filter((x) => x !== id) : [...prev, id]));
  };

  const doStart = async () => {
    setBusy(true);
    setError('');
    try {
      const scope =
        scopeMode === 'plans'
          ? { plan_ids: selectedPlanIds }
          : scopeMode === 'files'
            ? { spec_files: specFiles.split('\n').map((s) => s.trim()).filter(Boolean) }
            : { all: true };
      const res = await testingApi.startRun(env.id, { scope, healer });
      setStartOpen(false);
      setSelectedPlanIds([]);
      setSpecFiles('');
      setHealer(false);
      subscribe(res.job_id, t('testing.runTitle'), () => {
        void load();
        setExpandedId(res.run_id);
      });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const doCancel = async (run: TestRun) => {
    setBusy(true);
    setError('');
    try {
      await testingApi.cancelRun(run.id);
      await load();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const doDelete = async (run: TestRun) => {
    if (!window.confirm(t('testing.runDeleteConfirm'))) return;
    setBusy(true);
    setError('');
    try {
      await testingApi.deleteRun(run.id);
      if (expandedId === run.id) setExpandedId('');
      await load();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const envReady = env.init_status === 'ready';
  const generatedPlans = plans.filter((p) => (p.spec_files?.length ?? 0) > 0);

  return (
    <div className="tp-section">
      <div className="tp-section-head">
        <span>{t('testing.runTitle')}</span>
        {canManage && envReady && (
          <button className="btn btn-sm btn-primary" onClick={() => setStartOpen((v) => !v)}>
            {t('testing.runStart')}
          </button>
        )}
      </div>
      <div className="tp-section-body">
        {error && <div className="tp-error">{error}</div>}

        {startOpen && canManage && (
          <div style={{ marginBottom: 12 }}>
            <div className="tp-field">
              <label>{t('testing.runScopeAll')}</label>
              <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
                <label className="tp-inline" style={{ gap: 4 }}>
                  <input type="radio" checked={scopeMode === 'all'} onChange={() => setScopeMode('all')} />
                  <span style={{ fontSize: 13 }}>{t('testing.runScopeAll')}</span>
                </label>
                <label className="tp-inline" style={{ gap: 4 }}>
                  <input type="radio" checked={scopeMode === 'plans'} onChange={() => setScopeMode('plans')} />
                  <span style={{ fontSize: 13 }}>{t('testing.runScopePlans')}</span>
                </label>
                <label className="tp-inline" style={{ gap: 4 }}>
                  <input type="radio" checked={scopeMode === 'files'} onChange={() => setScopeMode('files')} />
                  <span style={{ fontSize: 13 }}>{t('testing.runScopeFiles')}</span>
                </label>
              </div>
            </div>

            {scopeMode === 'plans' && (
              <div className="tp-field">
                {generatedPlans.length === 0 ? (
                  <p className="tp-muted">{t('testing.errNoSpecs')}</p>
                ) : (
                  generatedPlans.map((p) => (
                    <label key={p.id} className="tp-inline" style={{ gap: 4, display: 'flex' }}>
                      <input type="checkbox" checked={selectedPlanIds.includes(p.id)} onChange={() => togglePlan(p.id)} />
                      <span style={{ fontSize: 13 }}>{p.title}</span>
                    </label>
                  ))
                )}
              </div>
            )}

            {scopeMode === 'files' && (
              <div className="tp-field">
                <textarea
                  className="form-input"
                  style={{ minHeight: 70 }}
                  placeholder={'tests/a.spec.ts\ntests/b.spec.ts'}
                  value={specFiles}
                  onChange={(e) => setSpecFiles(e.target.value)}
                  spellCheck={false}
                />
              </div>
            )}

            <label className="tp-inline" style={{ gap: 4 }}>
              <input type="checkbox" checked={healer} onChange={(e) => setHealer(e.target.checked)} />
              <span style={{ fontSize: 13 }}>{t('testing.runHealer')}</span>
            </label>
            <p className="tp-hint">{t('testing.runHealerHint')}</p>

            <div className="tp-inline" style={{ marginTop: 8 }}>
              <button
                className="btn btn-sm btn-primary"
                disabled={busy || (scopeMode === 'plans' && selectedPlanIds.length === 0)
                  || (scopeMode === 'files' && !specFiles.trim())}
                onClick={doStart}
              >
                {t('testing.confirm')}
              </button>
              <button className="btn btn-sm" onClick={() => setStartOpen(false)}>
                {t('testing.cancel')}
              </button>
            </div>
          </div>
        )}

        {loading ? (
          <p className="tp-muted">{t('testing.loading')}</p>
        ) : runs.length === 0 ? (
          <p className="tp-muted">{t('testing.runEmpty')}</p>
        ) : (
          runs.map((run) => (
            <div key={run.id}>
              <div className="tp-row">
                <div style={{ cursor: 'pointer', flex: 1, minWidth: 0 }} onClick={() => setExpandedId(expandedId === run.id ? '' : run.id)}>
                  <div className="tp-row-title">
                    {new Date(run.started_at).toLocaleString()}
                    {run.healed_from ? ` · ${t('testing.runHealedFrom')} ${run.healed_from}` : ''}
                  </div>
                  <div className="tp-row-sub">
                    {t('testing.runPassed')} {run.passed} · {t('testing.runFailed')} {run.failed}
                    {run.flaky > 0 ? ` · ${t('testing.runFlaky')} ${run.flaky}` : ''}
                    {run.skipped > 0 ? ` · ${t('testing.runSkipped')} ${run.skipped}` : ''}
                    {` · ${t('testing.runDuration')} ${(run.duration_ms / 1000).toFixed(1)}s`}
                  </div>
                  {run.error && <div className="tp-row-sub" style={{ color: '#b91c1c' }}>{run.error}</div>}
                </div>
                <div className="tp-row-actions">
                  <span className={`status-badge ${STATUS_CLASS[run.status] ?? 'status-pending'}`}>
                    {run.status}
                  </span>
                  {canManage && run.status === 'running' && (
                    <button className="btn btn-sm" disabled={busy} onClick={() => doCancel(run)}>
                      {t('testing.runCancel')}
                    </button>
                  )}
                  {canManage && run.status !== 'running' && (
                    <button className="btn btn-sm btn-danger" disabled={busy} onClick={() => doDelete(run)}>
                      {t('testing.delete')}
                    </button>
                  )}
                </div>
              </div>
              {expandedId === run.id && (
                <TestRunDetail runId={run.id} canManage={canManage} onChanged={load} />
              )}
            </div>
          ))
        )}
      </div>
    </div>
  );
}
