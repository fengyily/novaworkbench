import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { testingApi, requirementsApi, type TestEnvironment, type TestPlan } from '../../api/client';
import { errorMessage } from '../../utils/errMsg';
import MarkdownRender from '../MarkdownRender';

interface Props {
  projectId: string;
  env: TestEnvironment;
  canManage: boolean;
  /** Lifted stream subscription, shared with the rest of the tab. */
  subscribe: (jobId: string, title: string, onDone?: () => void) => void;
  onChanged: () => void;
}

const STATUS_CLASS: Record<string, string> = {
  draft: 'status-pending',
  planning: 'status-running',
  planned: 'status-succeeded',
  generating: 'status-running',
  generated: 'status-succeeded',
  failed: 'status-failed',
};

/**
 * TestPlanList owns the scenario → plan → generated-tests flow.
 *
 * The detail drawer doubles as the plan editor: a human edit is mirrored to
 * specs/<slug>.md on the backend, so it is what the generator reads on the
 * next generate — editing here is not cosmetic.
 */
export default function TestPlanList({ projectId, env, canManage, subscribe, onChanged }: Props) {
  const { t } = useTranslation();

  const [plans, setPlans] = useState<TestPlan[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [expandedId, setExpandedId] = useState('');

  const [creating, setCreating] = useState(false);
  const [title, setTitle] = useState('');
  const [scenario, setScenario] = useState('');
  const [requirementId, setRequirementId] = useState('');
  const [requirements, setRequirements] = useState<{ id: string; title: string }[]>([]);
  const [busy, setBusy] = useState(false);

  const [editingPlan, setEditingPlan] = useState<TestPlan | null>(null);
  const [draftMd, setDraftMd] = useState('');

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const list = (await testingApi.listPlans(projectId, env.id)) ?? [];
      setPlans(list);
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

  // Requirement dropdown: optional PRD context for the planner.
  useEffect(() => {
    let cancelled = false;
    requirementsApi
      .list({ project_id: projectId })
      .then((items) => {
        if (cancelled) return;
        setRequirements((items ?? []).map((r) => ({ id: r.id, title: r.title })));
      })
      .catch(() => {
        // The dropdown is optional context; a failure here must not block
        // plan creation.
      });
    return () => { cancelled = true; };
  }, [projectId]);

  const doCreate = async () => {
    setBusy(true);
    setError('');
    try {
      const res = await testingApi.createPlan(projectId, {
        title: title.trim(),
        scenario: scenario.trim(),
        env_id: env.id,
        requirement_id: requirementId || undefined,
      });
      setCreating(false);
      setTitle('');
      setScenario('');
      setRequirementId('');
      subscribe(res.job_id, t('testing.planTitle'), () => void load());
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const doGenerate = async (plan: TestPlan) => {
    setBusy(true);
    setError('');
    try {
      const res = await testingApi.startGenerate(plan.id);
      subscribe(res.job_id, `${t('testing.planTitle')} · ${plan.title}`, () => void load());
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const doSaveEdit = async () => {
    if (!editingPlan) return;
    setBusy(true);
    setError('');
    try {
      await testingApi.updatePlan(editingPlan.id, { plan_md: draftMd });
      setEditingPlan(null);
      await load();
      onChanged();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const doDelete = async (plan: TestPlan) => {
    if (!window.confirm(t('testing.planDeleteConfirm'))) return;
    setBusy(true);
    setError('');
    try {
      await testingApi.deletePlan(plan.id);
      if (expandedId === plan.id) setExpandedId('');
      await load();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const envReady = env.init_status === 'ready';

  return (
    <div className="tp-section">
      <div className="tp-section-head">
        <span>{t('testing.planTitle')}</span>
        {canManage && envReady && (
          <button className="btn btn-sm btn-primary" onClick={() => setCreating((v) => !v)}>
            {t('testing.planCreate')}
          </button>
        )}
      </div>
      <div className="tp-section-body">
        {error && <div className="tp-error">{error}</div>}

        {!envReady && (
          <p className="tp-hint">{t('testing.errEnvNotReady')}</p>
        )}

        {creating && canManage && (
          <div style={{ marginBottom: 12 }}>
            <div className="tp-field">
              <label>{t('testing.planScenario')}</label>
              <textarea
                className="form-input"
                value={scenario}
                placeholder={t('testing.planScenarioPlaceholder')}
                onChange={(e) => setScenario(e.target.value)}
              />
            </div>
            <div className="tp-field">
              <label>{t('testing.planTitleLabel')}</label>
              <input
                type="text"
                className="form-input"
                value={title}
                placeholder={t('testing.planTitlePlaceholder')}
                onChange={(e) => setTitle(e.target.value)}
              />
            </div>
            {requirements.length > 0 && (
              <div className="tp-field">
                <label>{t('testing.planRequirement')}</label>
                <select
                  className="form-input"
                  value={requirementId}
                  onChange={(e) => setRequirementId(e.target.value)}
                >
                  <option value="">—</option>
                  {requirements.map((r) => (
                    <option key={r.id} value={r.id}>{r.title}</option>
                  ))}
                </select>
                <p className="tp-hint">{t('testing.planRequirementHint')}</p>
              </div>
            )}
            <div className="tp-inline">
              <button
                className="btn btn-sm btn-primary"
                disabled={busy || !scenario.trim()}
                onClick={doCreate}
              >
                {t('testing.confirm')}
              </button>
              <button className="btn btn-sm" onClick={() => setCreating(false)}>
                {t('testing.cancel')}
              </button>
            </div>
          </div>
        )}

        {loading ? (
          <p className="tp-muted">{t('testing.loading')}</p>
        ) : plans.length === 0 ? (
          <p className="tp-muted">{t('testing.planEmpty')}</p>
        ) : (
          plans.map((plan) => (
            <div key={plan.id}>
              <div className="tp-row">
                <div
                  style={{ cursor: 'pointer', flex: 1, minWidth: 0 }}
                  onClick={() => setExpandedId(expandedId === plan.id ? '' : plan.id)}
                >
                  <div className="tp-row-title">{plan.title}</div>
                  <div className="tp-row-sub">
                    {plan.spec_files?.length
                      ? `${t('testing.planFiles')}: ${plan.spec_files.join(', ')}`
                      : t('testing.planEmptyFiles')}
                  </div>
                </div>
                <div className="tp-row-actions">
                  <span className={`status-badge ${STATUS_CLASS[plan.status] ?? 'status-pending'}`}>
                    {t(`testing.planStatus${plan.status.charAt(0).toUpperCase()}${plan.status.slice(1)}`)}
                  </span>
                  {canManage && plan.status === 'planned' && (
                    <button className="btn btn-sm btn-primary" disabled={busy} onClick={() => doGenerate(plan)}>
                      {t('testing.planGenerate')}
                    </button>
                  )}
                  {canManage && plan.status === 'generated' && (
                    <button className="btn btn-sm" disabled={busy} onClick={() => doGenerate(plan)}>
                      {t('testing.planRegenerate')}
                    </button>
                  )}
                  {canManage && (
                    <button className="btn btn-sm btn-danger" disabled={busy} onClick={() => doDelete(plan)}>
                      {t('testing.delete')}
                    </button>
                  )}
                </div>
              </div>

              {expandedId === plan.id && (
                <div className="tp-plan-body">
                  {plan.error && <div className="tp-error">{plan.error}</div>}
                  {editingPlan?.id === plan.id ? (
                    <>
                      <textarea
                        className="form-input tp-plan-editor"
                        value={draftMd}
                        onChange={(e) => setDraftMd(e.target.value)}
                        spellCheck={false}
                      />
                      <div className="tp-inline" style={{ marginTop: 8 }}>
                        <button className="btn btn-sm btn-primary" disabled={busy} onClick={doSaveEdit}>
                          {t('testing.planSave')}
                        </button>
                        <button className="btn btn-sm" onClick={() => setEditingPlan(null)}>
                          {t('testing.planCancel')}
                        </button>
                      </div>
                      <p className="tp-hint">{t('testing.planEditHint')}</p>
                    </>
                  ) : (
                    <>
                      {canManage && (
                        <button
                          className="btn btn-sm"
                          style={{ marginBottom: 8 }}
                          onClick={() => { setEditingPlan(plan); setDraftMd(plan.plan_md || ''); }}
                        >
                          {t('testing.planEdit')}
                        </button>
                      )}
                      {plan.plan_md ? (
                        <div className="md-prose">
                          <MarkdownRender content={plan.plan_md} />
                        </div>
                      ) : (
                        <p className="tp-muted">{t('testing.empty')}</p>
                      )}
                      {plan.spec_files?.length > 0 && (
                        <ul className="tp-file-list">
                          {plan.spec_files.map((f) => <li key={f}>{f}</li>)}
                        </ul>
                      )}
                    </>
                  )}
                </div>
              )}
            </div>
          ))
        )}
      </div>
    </div>
  );
}
