import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import MarkdownRender from '../components/MarkdownRender';
import { createEventStream, type EventStream } from '../api/stream';
import {
  crossRequirementsApi,
  type CrossRequirementChild,
  type CrossRequirementDetail as Detail,
  type CrossRequirementPhase,
  type CrossSplitCandidate,
} from '../api/client';
import { errorMessage } from '../utils/errMsg';
import { crossPhaseBadgeClass, crossStatusBadgeClass } from '../utils/crossStatus';
import './CrossRequirements.css';

// 跨项目需求详情页。
//
// 一页要回答四个问题：
//
//   1. 整体方案是什么？（跨仓库的职责边界 + 接口契约）
//   2. 每个仓库各自要做什么？（项目进度矩阵）
//   3. 谁卡在谁前面？（跨项目依赖 + 阻塞标记）
//   4. 联调到哪一步了？（阶段看板 + 验证报告）
//
// 三个 AI 作业（整体方案 / 按项目拆分 / 阶段验证）都用 SSE 回显，产出分别落库：
// 整体方案落 cross_requirements.design_docs，验证报告落阶段行 —— 后端重启后
// 仍然可回看。

/** design_docs 存的是 JSON 数组；这里取出 Markdown 正文。 */
function designMarkdown(raw: string): string {
  const trimmed = (raw || '').trim();
  if (!trimmed) return '';
  try {
    const arr = JSON.parse(trimmed);
    if (Array.isArray(arr)) {
      return arr.map(s => (typeof s === 'string' ? s : '')).filter(Boolean).join('\n\n');
    }
  } catch {
    // A legacy / hand-written value that is not JSON is rendered verbatim.
  }
  return trimmed;
}

/** job log line kinds the SSE panel renders. */
interface JobLine {
  type: string;
  content: string;
}

export default function CrossRequirementDetailPage() {
  const { id } = useParams();
  const navigate = useNavigate();
  const { t } = useTranslation();

  const [detail, setDetail] = useState<Detail | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [toast, setToast] = useState<string | null>(null);

  // design editing
  const [editingDesign, setEditingDesign] = useState(false);
  const [designDraft, setDesignDraft] = useState('');

  // split confirmation
  const [candidates, setCandidates] = useState<CrossSplitCandidate[]>([]);
  const [confirming, setConfirming] = useState(false);

  // phase creation
  const [addingPhase, setAddingPhase] = useState(false);
  const [phaseName, setPhaseName] = useState('');
  const [phaseMembers, setPhaseMembers] = useState<string[]>([]);

  // job panel
  const [jobLines, setJobLines] = useState<JobLine[]>([]);
  const [jobRunning, setJobRunning] = useState(false);
  const esRef = useRef<EventStream | null>(null);

  const xreqId = id || '';

  const load = useCallback(() => {
    if (!xreqId) return;
    setLoading(true);
    setError(null);
    crossRequirementsApi.get(xreqId)
      .then(setDetail)
      .catch(err => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, [xreqId]);

  useEffect(() => { load(); }, [load]);

  useEffect(() => () => { esRef.current?.close(); esRef.current = null; }, []);

  useEffect(() => {
    if (!toast) return;
    const timer = setTimeout(() => setToast(null), 4000);
    return () => clearTimeout(timer);
  }, [toast]);

  const statusLabel = useCallback((v: string) => t(`crossRequirements.status.${v}` as never), [t]);
  const phaseStatusLabel = useCallback((v: string) => t(`crossRequirements.phaseStatus.${v}` as never), [t]);

  const children = detail?.children || [];
  const childrenByProject = useMemo(() => {
    const map = new Map<string, CrossRequirementChild[]>();
    (detail?.children || []).forEach(c => {
      const list = map.get(c.project_id) || [];
      list.push(c);
      map.set(c.project_id, list);
    });
    return map;
  }, [detail]);

  const titleById = useMemo(() => {
    const map = new Map<string, string>();
    (detail?.children || []).forEach(c => map.set(c.id, c.title));
    return map;
  }, [detail]);

  // ── jobs ────────────────────────────────────────────────────────────────

  const appendLine = useCallback((line: JobLine) => {
    setJobLines(prev => [...prev, line]);
  }, []);

  const runJob = useCallback(
    (start: () => Promise<{ job_id: string }>, streamUrl: (jobId: string) => string,
      onDone: (result: any) => void) => {
      setJobRunning(true);
      setJobLines([]);
      start()
        .then(({ job_id }) => {
          esRef.current = createEventStream(
            streamUrl(job_id),
            (data) => {
              if (data.type === 'job_done') {
                esRef.current?.close();
                esRef.current = null;
                setJobRunning(false);
                if (data.status === 'done') {
                  onDone(data.result || {});
                  load();
                } else {
                  appendLine({
                    type: 'error',
                    content: data.error_message || t('crossRequirements.job.failed'),
                  });
                }
              } else if (data.content) {
                appendLine({ type: data.type || 'message', content: String(data.content) });
              }
            },
            () => {
              setJobRunning(false);
              appendLine({ type: 'error', content: t('crossRequirements.job.failed') });
            },
          );
        })
        .catch(err => {
          setJobRunning(false);
          appendLine({ type: 'error', content: errorMessage(err) });
        });
    },
    [appendLine, load, t],
  );

  const startDesign = () => {
    runJob(
      () => crossRequirementsApi.startDesign(xreqId),
      jobId => crossRequirementsApi.streamDesignUrl(xreqId, jobId),
      () => setToast(t('crossRequirements.detail.designSaved')),
    );
  };

  const startSplit = () => {
    runJob(
      () => crossRequirementsApi.startSplit(xreqId),
      jobId => crossRequirementsApi.streamSplitUrl(xreqId, jobId),
      (result) => {
        const list = Array.isArray(result?.candidates) ? result.candidates : [];
        setCandidates(list);
        setConfirming(true);
      },
    );
  };

  const startVerify = (phaseId: string) => {
    runJob(
      () => crossRequirementsApi.startVerify(xreqId, phaseId),
      jobId => crossRequirementsApi.streamVerifyUrl(xreqId, phaseId, jobId),
      () => setToast(t('crossRequirements.detail.verdictSaved')),
    );
  };

  // ── actions ─────────────────────────────────────────────────────────────

  const saveDesign = async () => {
    try {
      await crossRequirementsApi.saveDesign(xreqId, designDraft);
      setEditingDesign(false);
      setToast(t('crossRequirements.detail.designSaved'));
      load();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const confirmSplit = async () => {
    try {
      await crossRequirementsApi.confirmSplit(xreqId, candidates);
      setConfirming(false);
      setCandidates([]);
      setToast(t('crossRequirements.split.confirmed', { count: candidates.length }));
      load();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const updateCandidateProject = (index: number, projectId: string) => {
    setCandidates(prev => prev.map((c, i) => (i === index ? { ...c, project_id: projectId } : c)));
  };

  const createPhase = async () => {
    if (!phaseName.trim()) return;
    try {
      await crossRequirementsApi.createPhase(xreqId, {
        name: phaseName.trim(),
        requirement_ids: phaseMembers,
      });
      setAddingPhase(false);
      setPhaseName('');
      setPhaseMembers([]);
      load();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const setPhaseStatus = async (phase: CrossRequirementPhase, status: string) => {
    try {
      await crossRequirementsApi.updatePhase(xreqId, phase.id, { status });
      load();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const removePhase = async (phase: CrossRequirementPhase) => {
    try {
      await crossRequirementsApi.deletePhase(xreqId, phase.id);
      setToast(t('crossRequirements.detail.phaseDeleted'));
      load();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  if (loading) {
    return <div className="xreq-empty">{t('crossRequirements.loading')}</div>;
  }
  if (error && !detail) {
    return (
      <div className="xreq-empty">
        <p className="xreq-error">{error}</p>
        <button type="button" className="btn" onClick={load}>{t('crossRequirements.retry')}</button>
      </div>
    );
  }
  if (!detail) return null;

  const design = designMarkdown(detail.design_docs);
  const progress = detail.progress;
  const projectRows = detail.projects || [];

  return (
    <div className="xreq-page">
      <div className="xreq-head">
        <div className="xreq-head-main">
          <button type="button" className="btn btn-sm xreq-back" onClick={() => navigate('/cross-requirements')}>
            ← {t('crossRequirements.detail.back')}
          </button>
          <h1 className="xreq-title">{detail.title}</h1>
          <span className={`status-badge ${crossStatusBadgeClass(detail.status)}`}>
            {statusLabel(detail.status)}
          </span>
        </div>
        <div className="xreq-actions">
          <button
            type="button"
            className="btn"
            onClick={startDesign}
            disabled={jobRunning || projectRows.length === 0}
          >
            {design
              ? t('crossRequirements.detail.regenerateDesign')
              : t('crossRequirements.detail.generateDesign')}
          </button>
          <button
            type="button"
            className="btn btn-primary"
            onClick={startSplit}
            disabled={jobRunning || !design}
          >
            {t('crossRequirements.detail.split')}
          </button>
        </div>
      </div>

      {error && <div className="xreq-error">{error}</div>}
      {toast && <div className="xreq-toast">{toast}</div>}
      {projectRows.length === 0 && (
        <div className="xreq-hint">{t('crossRequirements.noProjects')}</div>
      )}

      {/* ── overview ─────────────────────────────────────────────────── */}
      <section className="xreq-section">
        <div className="xreq-meta">
          <span>{t('crossRequirements.detail.status')}：{statusLabel(detail.status)}</span>
          <span>{t('crossRequirements.detail.priority')}：{t(`crossRequirements.priority.${detail.priority}` as never)}</span>
          <span>{t('crossRequirements.projectsCount', { count: projectRows.length })}</span>
        </div>
        <div className="xreq-progress">
          <div className="xreq-progress-track">
            <div className="xreq-progress-fill" style={{ width: `${progress?.percent ?? 0}%` }} />
          </div>
          <span className="xreq-progress-text">
            {progress?.percent ?? 0}% ·{' '}
            {t('crossRequirements.childrenDone', { done: progress?.done ?? 0, total: progress?.total ?? 0 })}
          </span>
        </div>
      </section>

      {/* ── participating projects ───────────────────────────────────── */}
      <section className="xreq-section">
        <h2 className="xreq-section-title">{t('crossRequirements.detail.projects')}</h2>
        {projectRows.length === 0 ? (
          <div className="xreq-hint">{t('crossRequirements.detail.projectsEmpty')}</div>
        ) : (
          <table className="xreq-table">
            <thead>
              <tr>
                <th>{t('crossRequirements.projectLabel')}</th>
                <th>{t('crossRequirements.roleLabel')}</th>
                <th>{t('crossRequirements.detail.matrix')}</th>
                <th>{t('crossRequirements.detail.progress')}</th>
              </tr>
            </thead>
            <tbody>
              {projectRows.map(p => (
                <tr key={p.project_id}>
                  <td>{p.project_name || p.project_id}</td>
                  <td>{p.role || '-'}</td>
                  <td>{p.total}</td>
                  <td>{p.done}/{p.total}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      {/* ── overall design ───────────────────────────────────────────── */}
      <section className="xreq-section">
        <div className="xreq-section-head">
          <h2 className="xreq-section-title">{t('crossRequirements.detail.design')}</h2>
          {design && !editingDesign && (
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => { setDesignDraft(design); setEditingDesign(true); }}
            >
              {t('crossRequirements.detail.editDesign')}
            </button>
          )}
        </div>
        {editingDesign ? (
          <div className="xreq-editor">
            <p className="xreq-field-hint">{t('crossRequirements.detail.editDesignHint')}</p>
            <textarea
              className="form-input"
              rows={18}
              value={designDraft}
              onChange={e => setDesignDraft(e.target.value)}
            />
            <div className="xreq-modal-actions">
              <button type="button" className="btn" onClick={() => setEditingDesign(false)}>
                {t('crossRequirements.common.cancel')}
              </button>
              <button type="button" className="btn btn-primary" onClick={saveDesign}>
                {t('crossRequirements.detail.saveDesign')}
              </button>
            </div>
          </div>
        ) : design ? (
          <div className="md-prose xreq-design">
            <MarkdownRender content={design} />
          </div>
        ) : (
          <div className="xreq-empty">
            <p className="xreq-empty-title">{t('crossRequirements.detail.designEmpty')}</p>
            <p className="xreq-empty-hint">{t('crossRequirements.detail.designEmptyHint')}</p>
          </div>
        )}
      </section>

      {/* ── sub-requirement matrix ───────────────────────────────────── */}
      <section className="xreq-section">
        <h2 className="xreq-section-title">{t('crossRequirements.detail.matrix')}</h2>
        {children.length === 0 ? (
          <div className="xreq-empty">
            <p className="xreq-empty-title">{t('crossRequirements.detail.matrixEmpty')}</p>
            <p className="xreq-empty-hint">{t('crossRequirements.detail.matrixEmptyHint')}</p>
          </div>
        ) : (
          <div className="xreq-matrix">
            {projectRows.map(p => {
              const cells = childrenByProject.get(p.project_id) || [];
              if (!cells.length) return null;
              return (
                <div key={p.project_id} className="xreq-matrix-row">
                  <div className="xreq-matrix-project">{p.project_name || p.project_id}</div>
                  <div className="xreq-matrix-cells">
                    {cells.map(c => {
                      const missing = c.blocked_by
                        .map(b => titleById.get(b) || b)
                        .join('、');
                      return (
                        <button
                          type="button"
                          key={c.id}
                          className="xreq-matrix-cell"
                          title={missing ? t('crossRequirements.detail.blockedBy', { titles: missing }) : c.title}
                          onClick={() => navigate(`/requirements/${c.id}`)}
                        >
                          <span className="xreq-cell-title">{c.title}</span>
                          <span className={`status-badge ${crossStatusBadgeClass(c.status)}`}>
                            {c.status}
                          </span>
                          {c.blocked_by.length > 0 && (
                            <span className="xreq-cell-blocked">⏳</span>
                          )}
                        </button>
                      );
                    })}
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </section>

      {/* ── cross-project dependencies ───────────────────────────────── */}
      <section className="xreq-section">
        <h2 className="xreq-section-title">{t('crossRequirements.detail.dependencies')}</h2>
        {(detail.dependencies || []).length === 0 ? (
          <div className="xreq-hint">{t('crossRequirements.detail.dependenciesEmpty')}</div>
        ) : (
          <ul className="xreq-deps">
            {detail.dependencies.map(dep => (
              <li key={`${dep.requirement_id}>${dep.depends_on_id}`} className="xreq-dep">
                <span className="xreq-dep-text">
                  {dep.requirement_title} ← {dep.depends_on_title}
                </span>
                {dep.cross_project && (
                  <span className="xreq-chip">{t('crossRequirements.detail.crossProject')}</span>
                )}
                {!dep.met && <span className="xreq-chip xreq-chip-warn">⏳</span>}
              </li>
            ))}
          </ul>
        )}
      </section>

      {/* ── phases ───────────────────────────────────────────────────── */}
      <section className="xreq-section">
        <div className="xreq-section-head">
          <h2 className="xreq-section-title">{t('crossRequirements.detail.phases')}</h2>
          <button type="button" className="btn btn-sm" onClick={() => setAddingPhase(v => !v)}>
            {t('crossRequirements.detail.addPhase')}
          </button>
        </div>

        {addingPhase && (
          <div className="xreq-editor">
            <div className="xreq-field">
              <span className="xreq-field-label">{t('crossRequirements.detail.phaseName')}</span>
              <input
                className="form-input"
                value={phaseName}
                placeholder={t('crossRequirements.detail.phaseNamePlaceholder')}
                onChange={e => setPhaseName(e.target.value)}
              />
            </div>
            <div className="xreq-field">
              <span className="xreq-field-label">{t('crossRequirements.detail.phaseMembers')}</span>
              <div className="xreq-picker">
                {children.map(c => (
                  <label key={c.id} className="xreq-option">
                    <input
                      type="checkbox"
                      checked={phaseMembers.includes(c.id)}
                      onChange={() => setPhaseMembers(prev => (
                        prev.includes(c.id) ? prev.filter(x => x !== c.id) : [...prev, c.id]
                      ))}
                    />
                    <span>{c.title}</span>
                  </label>
                ))}
              </div>
            </div>
            <div className="xreq-modal-actions">
              <button type="button" className="btn" onClick={() => setAddingPhase(false)}>
                {t('crossRequirements.common.cancel')}
              </button>
              <button type="button" className="btn btn-primary" onClick={createPhase}>
                {t('crossRequirements.detail.phaseSave')}
              </button>
            </div>
          </div>
        )}

        {(detail.phases || []).length === 0 && !addingPhase ? (
          <div className="xreq-empty">
            <p className="xreq-empty-title">{t('crossRequirements.detail.phasesEmpty')}</p>
            <p className="xreq-empty-hint">{t('crossRequirements.detail.phasesEmptyHint')}</p>
          </div>
        ) : (
          <div className="xreq-phases">
            {detail.phases.map(phase => (
              <div key={phase.id} className="xreq-phase">
                <div className="xreq-phase-head">
                  <span className="xreq-phase-name">{phase.name}</span>
                  <span className={`status-badge ${crossPhaseBadgeClass(phase.status)}`}>
                    {phaseStatusLabel(phase.status)}
                  </span>
                  <button
                    type="button"
                    className="btn btn-sm"
                    onClick={() => startVerify(phase.id)}
                    disabled={jobRunning || phase.status !== 'ready'}
                  >
                    {t('crossRequirements.detail.verify')}
                  </button>
                  <button
                    type="button"
                    className="btn btn-sm"
                    onClick={() => setPhaseStatus(phase, 'passed')}
                  >
                    {t('crossRequirements.detail.verifyPassed')}
                  </button>
                  <button
                    type="button"
                    className="btn btn-sm"
                    onClick={() => setPhaseStatus(phase, 'failed')}
                  >
                    {t('crossRequirements.detail.verifyFailed')}
                  </button>
                  <button
                    type="button"
                    className="btn btn-sm"
                    onClick={() => removePhase(phase)}
                  >
                    {t('crossRequirements.detail.phaseDelete')}
                  </button>
                </div>
                {phase.items.length > 0 && (
                  <div className="xreq-phase-members">
                    {phase.items.map(item => (
                      <span key={item.id} className="xreq-chip">
                        {titleById.get(item.requirement_id) || item.requirement_id}
                      </span>
                    ))}
                  </div>
                )}
                {phase.verify_report ? (
                  <div className="md-prose xreq-report">
                    <MarkdownRender content={phase.verify_report} />
                  </div>
                ) : (
                  <div className="xreq-hint">{t('crossRequirements.detail.verifyReportEmpty')}</div>
                )}
              </div>
            ))}
          </div>
        )}
      </section>

      {/* ── job panel ────────────────────────────────────────────────── */}
      {jobLines.length > 0 && (
        <section className="xreq-section">
          <div className="xreq-job">
            {jobLines.map((line, i) => (
              <div key={i} className={`xreq-job-line xreq-job-${line.type}`}>{line.content}</div>
            ))}
          </div>
        </section>
      )}

      {/* ── split confirmation ───────────────────────────────────────── */}
      {confirming && (
        <div className="xreq-modal-overlay">
          <div className="xreq-modal xreq-modal-wide">
            <h2 className="xreq-modal-title">{t('crossRequirements.split.title')}</h2>
            {candidates.length === 0 ? (
              <div className="xreq-hint">{t('crossRequirements.split.empty')}</div>
            ) : (
              <div className="xreq-candidates">
                {candidates.map((c, i) => (
                  <div key={i} className="xreq-candidate">
                    <div className="xreq-candidate-head">
                      <span className="xreq-candidate-title">{c.title}</span>
                      <span className="xreq-candidate-order">#{c.order}</span>
                    </div>
                    <div className="xreq-field">
                      <span className="xreq-field-label">{t('crossRequirements.split.project')}</span>
                      <select
                        className="form-input"
                        value={c.project_id}
                        onChange={e => updateCandidateProject(i, e.target.value)}
                      >
                        <option value="">{'-'}</option>
                        {projectRows.map(p => (
                          <option key={p.project_id} value={p.project_id}>
                            {p.project_name || p.project_id}
                          </option>
                        ))}
                      </select>
                    </div>
                    {c.depends_on.length > 0 && (
                      <div className="xreq-hint">
                        {t('crossRequirements.split.dependsOn')}：
                        {c.depends_on.map(d => titleById.get(String(d)) || `#${d}`).join('、')}
                      </div>
                    )}
                  </div>
                ))}
              </div>
            )}
            <div className="xreq-modal-actions">
              <button type="button" className="btn" onClick={() => setConfirming(false)}>
                {t('crossRequirements.common.cancel')}
              </button>
              <button
                type="button"
                className="btn btn-primary"
                onClick={confirmSplit}
                disabled={candidates.length === 0}
              >
                {t('crossRequirements.split.confirm')}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
