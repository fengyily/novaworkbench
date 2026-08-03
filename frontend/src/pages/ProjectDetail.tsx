import { useState, useEffect, useRef, useCallback } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import {
  projectsApi, runnerApi, reviewApi, platformApi, requirementsApi,
  type Project, type RunStatus, type PR, type PRListResponse, type PlatformToken,
  type Requirement, statusLabels,
} from '../api/client';
import ProjectWeeklyReport from './ProjectWeeklyReport';
import './RequirementDetail.css';
import './ProjectDetail.css';

type Tab = 'overview' | 'run' | 'requirements' | 'review' | 'weekly';

const priorityDots: Record<string, string> = {
  high: '🔴', medium: '🟡', low: '🟢',
};

const runStatusLabel: Record<string, string> = {
  stopped: '未运行', running: '运行中', done: '已停止', error: '错误',
};

const platformLabels: Record<string, string> = {
  github: 'GitHub', gitlab: 'GitLab', gitea: 'Gitea',
};

export default function ProjectDetail() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();

  const [project, setProject] = useState<Project | null>(null);
  const [loading, setLoading] = useState(true);
  const [tab, setTab] = useState<Tab>('overview');

  // Run tab
  const [runStatus, setRunStatus] = useState<RunStatus | null>(null);
  const [logLines, setLogLines] = useState<{ type: string; content: string }[]>([]);
  const [starting, setStarting] = useState(false);
  const [stopping, setStopping] = useState(false);
  const pollTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const pollCountRef = useRef(0);
  const logPanelRef = useRef<HTMLDivElement>(null);

  // Overview: platform config
  const [tokens, setTokens] = useState<PlatformToken[]>([]);
  const [platformForm, setPlatformForm] = useState({ platform_type: '', platform_token_id: '' });
  const [platformSaving, setPlatformSaving] = useState(false);
  const [platformSaved, setPlatformSaved] = useState(false);

  // Requirements (shared by the requirements tab and the overview recent list)
  const [reqs, setReqs] = useState<Requirement[]>([]);
  const [reqsLoading, setReqsLoading] = useState(false);
  const [reqsError, setReqsError] = useState('');
  const [reqsLoaded, setReqsLoaded] = useState(false);
  const [showCreateReq, setShowCreateReq] = useState(false);

  // Overview: recent requirements (height-adaptive)
  const [visibleCount, setVisibleCount] = useState(6);
  const recentSectionRef = useRef<HTMLDivElement>(null);

  // Review tab
  const [prData, setPrData] = useState<PRListResponse | null>(null);
  const [prsLoading, setPrsLoading] = useState(false);
  const [prsError, setPrsError] = useState('');
  const [reviewingPR, setReviewingPR] = useState<PR | null>(null);
  const [reviewLines, setReviewLines] = useState<{ type: string; content: string }[]>([]);
  const [reviewDone, setReviewDone] = useState(false);
  const [commentBody, setCommentBody] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [submitMsg, setSubmitMsg] = useState('');
  const [extraRequirements, setExtraRequirements] = useState('');
  const reviewEsRef = useRef<EventSource | null>(null);
  const reviewPanelRef = useRef<HTMLDivElement>(null);

  // Load project
  useEffect(() => {
    if (!id) return;
    projectsApi.get(id)
      .then(p => {
        setProject(p);
        setPlatformForm({ platform_type: p.platform_type ?? '', platform_token_id: p.platform_token_id ?? '' });
      })
      .catch(() => {})
      .finally(() => setLoading(false));
  }, [id]);

  // Load tokens for overview platform config
  useEffect(() => {
    platformApi.list().then(data => setTokens(data ?? [])).catch(() => {});
  }, []);

  // Auto-scroll panels
  useEffect(() => {
    if (logPanelRef.current) logPanelRef.current.scrollTop = logPanelRef.current.scrollHeight;
  }, [logLines]);
  useEffect(() => {
    if (reviewPanelRef.current) reviewPanelRef.current.scrollTop = reviewPanelRef.current.scrollHeight;
  }, [reviewLines]);

  // Cleanup on unmount
  useEffect(() => {
    return () => {
      if (pollTimerRef.current) clearTimeout(pollTimerRef.current);
      reviewEsRef.current?.close();
    };
  }, []);

  // Load run status on tab switch
  useEffect(() => {
    if (tab !== 'run' || !id) return;
    runnerApi.status(id).then(s => {
      setRunStatus(s);
      if (s.log?.length) { setLogLines(s.log); pollCountRef.current = s.log.length; }
      if (s.status === 'running' && s.job_id) pollJob(s.job_id, true);
    }).catch(() => {});
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tab, id]);

  // Load PR list on tab switch
  useEffect(() => {
    if (tab !== 'review' || !id) return;
    setPrsLoading(true);
    setPrsError('');
    reviewApi.listPRs(id)
      .then(setPrData)
      .catch(err => setPrsError(err instanceof Error ? err.message : String(err)))
      .finally(() => setPrsLoading(false));
  }, [tab, id]);

  // Load requirements for both the overview (recent) and the requirements tab.
  // reqsLoaded prevents duplicate requests when switching between the two tabs.
  useEffect(() => {
    if ((tab !== 'overview' && tab !== 'requirements') || !id || reqsLoaded) return;
    let active = true;
    setReqsLoading(true);
    setReqsError('');
    requirementsApi.list({ project_id: id })
      .then(data => { if (active) { setReqs(data); setReqsLoaded(true); } })
      .catch(err => { if (active) setReqsError(err instanceof Error ? err.message : String(err)); })
      .finally(() => { if (active) setReqsLoading(false); });
    return () => { active = false; };
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tab, id, reqsLoaded]);

  // Overview: adapt the visible row count to the viewport's remaining space
  // below the section's top. Measuring the section's own height would
  // self-shrink (height drives count, count drives height), so we use the
  // space left below the section top instead. The section's CSS height follows
  // the rendered row count, so few requirements → short list (no big empty
  // box), many requirements → capped by visibleCount.
  useEffect(() => {
    if (tab !== 'overview') return;
    const el = recentSectionRef.current;
    if (!el) return;
    const HEADER_H = 40, ROW_H = 44, BOTTOM_PAD = 16;
    const compute = () => {
      const top = el.getBoundingClientRect().top;
      const availH = window.innerHeight - top - BOTTOM_PAD;
      setVisibleCount(Math.max(2, Math.floor((availH - HEADER_H) / ROW_H)));
    };
    compute();
    window.addEventListener('resize', compute);
    return () => window.removeEventListener('resize', compute);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tab, reqsLoaded]);

  // ── Run tab handlers ───────────────────────────────────────────────────────

  const pollJob = useCallback((jobId: string, resume = false) => {
    if (!resume) pollCountRef.current = 0;
    const tick = async () => {
      try {
        const data = await runnerApi.getJob(jobId);
        if (data.log && data.log.length > pollCountRef.current) {
          setLogLines(prev => [...prev, ...data.log.slice(pollCountRef.current)]);
          pollCountRef.current = data.log.length;
        }
        setRunStatus(prev => prev ? { ...prev, status: data.status } : prev);
        if (data.status === 'running') { pollTimerRef.current = setTimeout(tick, 2000); }
        else { setStarting(false); setStopping(false); }
      } catch { pollTimerRef.current = setTimeout(tick, 3000); }
    };
    tick();
  }, []);

  const handleStart = async () => {
    if (!id) return;
    if (pollTimerRef.current) clearTimeout(pollTimerRef.current);
    setStarting(true); setLogLines([]); pollCountRef.current = 0;
    try {
      const { job_id } = await runnerApi.start(id);
      setRunStatus(prev => ({ ...(prev ?? { log: [], compose_file: '' }), status: 'running', job_id }));
      pollJob(job_id);
    } catch (err: unknown) {
      setLogLines([{ type: 'error', content: err instanceof Error ? err.message : String(err) }]);
      setStarting(false);
    }
  };

  const handleStop = async () => {
    if (!id) return;
    setStopping(true);
    try { await runnerApi.stop(id); }
    catch (err: unknown) {
      setLogLines(prev => [...prev, { type: 'error', content: '停止失败: ' + (err instanceof Error ? err.message : String(err)) }]);
      setStopping(false);
    }
  };

  // ── Overview: platform config ──────────────────────────────────────────────

  const handleSavePlatform = async () => {
    if (!id) return;
    setPlatformSaving(true);
    setPlatformSaved(false);
    try {
      const updated = await projectsApi.updatePlatform(id, platformForm.platform_type, platformForm.platform_token_id);
      setProject(updated);
      setPlatformSaved(true);
      setTimeout(() => setPlatformSaved(false), 2000);
    } catch { /* ignore */ } finally { setPlatformSaving(false); }
  };

  // ── Review tab handlers ────────────────────────────────────────────────────

  const handleReview = useCallback(async (pr: PR) => {
    if (!id || reviewingPR) return;
    setReviewingPR(pr);
    setReviewLines([]);
    setReviewDone(false);
    setCommentBody('');
    setSubmitMsg('');
    reviewEsRef.current?.close();

    try {
      const { job_id } = await reviewApi.startReview(id, pr.head_branch, pr.base_branch, pr.number, pr.title, extraRequirements);
      const es = new EventSource(reviewApi.streamJobUrl(id, job_id));
      reviewEsRef.current = es;

      const messageLines: string[] = [];

      es.onmessage = (e) => {
        try {
          const data = JSON.parse(e.data);
          if (data.type === 'job_done') {
            setCommentBody(messageLines.join('\n\n'));
            setReviewDone(true);
            setReviewingPR(null);
            es.close();
          } else if (data.content) {
            setReviewLines(prev => [...prev, { type: data.type, content: data.content }]);
            if (data.type === 'message') messageLines.push(data.content);
          }
        } catch { /* ignore */ }
      };

      es.onerror = () => {
        setReviewLines(prev => [...prev, { type: 'error', content: 'SSE 连接中断' }]);
        setCommentBody(messageLines.join('\n\n'));
        setReviewDone(true);
        setReviewingPR(null);
        es.close();
      };
    } catch (err: unknown) {
      setReviewLines([{ type: 'error', content: err instanceof Error ? err.message : String(err) }]);
      setReviewingPR(null);
    }
  }, [id, reviewingPR]);

  const handleSubmitComment = async () => {
    if (!id || !commentBody || lastReviewedPRRef.current === 0) return;
    setSubmitting(true);
    setSubmitMsg('');
    try {
      await reviewApi.submitComment(id, lastReviewedPRRef.current, commentBody);
      setSubmitMsg('✅ Comment 已提交');
    } catch (err: unknown) {
      setSubmitMsg('❌ ' + (err instanceof Error ? err.message : String(err)));
    } finally { setSubmitting(false); }
  };

  const lastReviewedPRRef = useRef<number>(0);

  // Patch handleReview to track PR number
  const handleReviewWithTracking = useCallback(async (pr: PR) => {
    lastReviewedPRRef.current = pr.number;
    await handleReview(pr);
  }, [handleReview]);

  const isRunning = runStatus?.status === 'running';

  // Shared table rows for the requirements list (used by both the overview
  // "recent requirements" and the requirements tab — single source of truth).
  const renderRequirementRows = (items: Requirement[]) => items.map(req => (
    <tr
      key={req.id}
      style={{ cursor: 'pointer' }}
      onClick={() => navigate(`/requirements/${req.id}`)}
    >
      <td style={{ color: 'var(--color-text-muted)', fontFamily: 'var(--font-mono)', fontSize: 12 }}>{req.id}</td>
      <td className="pr-title">{req.title}</td>
      <td><span style={{ fontSize: 12, whiteSpace: 'nowrap' }}>{priorityDots[req.priority] ?? '⚪'} {req.priority}</span></td>
      <td><span className={`status-badge status-${req.status}`}>{statusLabels[req.status] ?? req.status}</span></td>
      <td>{req.sprint ? <code className="pr-branch">{req.sprint}</code> : '—'}</td>
      <td style={{ color: 'var(--color-text-muted)', fontSize: 12 }}>
        {req.updated_at ? new Date(req.updated_at).toLocaleDateString('zh-CN') : '—'}
      </td>
    </tr>
  ));

  if (loading) return <div className="detail-loading">加载中...</div>;
  if (!project) return <div className="detail-loading">项目未找到</div>;

  const selectedToken = tokens.find(t => t.id === platformForm.platform_token_id);

  return (
    <div className="project-detail">
      <div className="detail-header">
        <button className="btn btn-secondary" onClick={() => navigate('/projects')}>← 项目列表</button>
      </div>

      <h2 className="detail-title">{project.name}</h2>

      <div className="detail-tabs">
        <button className={`tab-btn${tab === 'overview' ? ' active' : ''}`} onClick={() => setTab('overview')}>概览</button>
        <button className={`tab-btn${tab === 'run' ? ' active' : ''}`} onClick={() => setTab('run')}>
          运行{isRunning ? ' ●' : ''}
        </button>
        <button className={`tab-btn${tab === 'requirements' ? ' active' : ''}`} onClick={() => setTab('requirements')}>
          需求
        </button>
        <button className={`tab-btn${tab === 'review' ? ' active' : ''}`} onClick={() => setTab('review')}>
          代码 Review{reviewingPR ? ' ●' : ''}
        </button>
        <button className={`tab-btn${tab === 'weekly' ? ' active' : ''}`} onClick={() => setTab('weekly')}>
          周报
        </button>
      </div>

      {/* ── Overview ── */}
      {tab === 'overview' && (
        <div className="tab-content">
          <div className="detail-section">
            <div className="info-row"><span className="info-label">名称</span><span>{project.name}</span></div>
            <div className="info-row">
              <span className="info-label">路径</span>
              <code className="info-code">{project.local_path}</code>
            </div>
            <div className="info-row"><span className="info-label">类型</span><span>{project.project_type || 'Unknown'}</span></div>
            <div className="info-row">
              <span className="info-label">状态</span>
              <span className={`status-badge status-${project.status}`}>{project.status}</span>
            </div>
            {project.remote_url && (
              <div className="info-row">
                <span className="info-label">仓库</span>
                <code className="info-code">{project.remote_url}</code>
              </div>
            )}
          </div>

          {/* Platform config */}
          <div className="detail-section" style={{ marginTop: 16 }}>
            <div className="section-header" style={{ marginBottom: 12 }}>
              <span style={{ fontWeight: 600, fontSize: 14 }}>平台配置</span>
              {project.platform_type && (
                <span style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>
                  当前：{platformLabels[project.platform_type] ?? project.platform_type}
                  {project.platform_token_id ? '' : ' (未绑定 Token)'}
                </span>
              )}
            </div>

            <div style={{ display: 'flex', gap: 12, alignItems: 'flex-end', flexWrap: 'wrap' }}>
              <div className="form-group" style={{ margin: 0, flex: '0 0 160px' }}>
                <label style={{ fontSize: 12, fontWeight: 600, color: 'var(--color-text-secondary)', display: 'block', marginBottom: 4 }}>
                  平台
                </label>
                <select
                  className="form-input"
                  value={platformForm.platform_type}
                  onChange={e => setPlatformForm(f => ({ ...f, platform_type: e.target.value, platform_token_id: '' }))}
                >
                  <option value="">— 不配置 —</option>
                  <option value="github">GitHub</option>
                  <option value="gitlab">GitLab</option>
                  <option value="gitea">Gitea</option>
                </select>
              </div>

              <div className="form-group" style={{ margin: 0, flex: '1 1 200px' }}>
                <label style={{ fontSize: 12, fontWeight: 600, color: 'var(--color-text-secondary)', display: 'block', marginBottom: 4 }}>
                  Token
                </label>
                <select
                  className="form-input"
                  value={platformForm.platform_token_id}
                  onChange={e => setPlatformForm(f => ({ ...f, platform_token_id: e.target.value }))}
                  disabled={!platformForm.platform_type}
                >
                  <option value="">— 选择 Token —</option>
                  {tokens
                    .filter(t => !platformForm.platform_type || t.platform === platformForm.platform_type)
                    .map(t => (
                      <option key={t.id} value={t.id}>{t.name}</option>
                    ))}
                </select>
              </div>

              <button
                className="btn btn-primary"
                style={{ height: 38 }}
                onClick={handleSavePlatform}
                disabled={platformSaving}
              >
                {platformSaving ? '保存中...' : platformSaved ? '已保存 ✓' : '保存'}
              </button>
            </div>

            {tokens.length === 0 && (
              <p style={{ margin: '8px 0 0', fontSize: 12, color: 'var(--color-text-muted)' }}>
                还没有 Token？请先到
                <a href="/settings" style={{ color: 'var(--color-primary)', margin: '0 4px' }}>设置页</a>
                添加。
              </p>
            )}
            {selectedToken && (
              <p style={{ margin: '8px 0 0', fontSize: 12, color: 'var(--color-success)' }}>
                已绑定：{selectedToken.name}
                {selectedToken.base_url ? ` (${selectedToken.base_url})` : ''}
              </p>
            )}
          </div>

          {/* Recent requirements (height-adaptive) */}
          <div className="detail-section recent-reqs-section" style={{ marginTop: 16 }} ref={recentSectionRef}>
            <div className="recent-reqs-header">
              <span style={{ fontWeight: 600, fontSize: 14 }}>最近需求</span>
              <button className="recent-reqs-more" onClick={() => setTab('requirements')}>查看全部 →</button>
            </div>

            {reqsLoading && <div className="tab-empty">⏳ 加载中...</div>}
            {!reqsLoading && reqs.length === 0 && (
              <div className="tab-empty">
                <p>该项目暂无需求，<button className="recent-reqs-link" onClick={() => { setTab('requirements'); setShowCreateReq(true); }}>去创建 →</button></p>
              </div>
            )}
            {!reqsLoading && reqs.length > 0 && (
              <div className="pr-list" style={{ marginBottom: 0 }}>
                <table className="pr-table">
                  <thead>
                    <tr>
                      <th style={{ width: 110 }}>ID</th>
                      <th>标题</th>
                      <th style={{ width: 90 }}>优先级</th>
                      <th style={{ width: 130 }}>状态</th>
                      <th style={{ width: 110 }}>Sprint</th>
                      <th style={{ width: 110 }}>更新时间</th>
                    </tr>
                  </thead>
                  <tbody>
                    {renderRequirementRows(reqs.slice(0, Math.min(reqs.length, visibleCount)))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </div>
      )}

      {/* ── Run ── */}
      {tab === 'run' && (
        <div className="tab-content">
          <div className="run-control-bar">
            {runStatus?.compose_file
              ? <span className="compose-detect">已检测到 {runStatus.compose_file}</span>
              : <span className="compose-detect compose-missing">未检测到 docker-compose 文件</span>}
            <span className={`run-status-badge run-status-${runStatus?.status ?? 'stopped'}`}>
              {runStatusLabel[runStatus?.status ?? 'stopped']}
            </span>
            {!isRunning
              ? <button className="btn btn-primary" onClick={handleStart} disabled={starting || stopping}>{starting ? '启动中...' : '启动'}</button>
              : <button className="btn btn-danger" onClick={handleStop} disabled={stopping}>{stopping ? '停止中...' : '停止'}</button>}
          </div>
          {(logLines.length > 0 || isRunning) && (
            <div className="coding-panel" ref={logPanelRef}>
              {logLines.map((line, i) => (
                <div key={i} className={`coding-line coding-line-${line.type}`}>{line.content}</div>
              ))}
              {isRunning && <div className="coding-line coding-line-tool_call">● 运行中...</div>}
            </div>
          )}
          {!isRunning && logLines.length === 0 && (
            <div className="tab-empty"><p>点击启动按钮运行 docker compose up --build</p></div>
          )}
        </div>
      )}

      {/* ── Requirements ── */}
      {tab === 'requirements' && (
        <div className="tab-content">
          <div className="run-control-bar">
            <span className="compose-detect">本项目关联的需求（{reqs.length}）</span>
            <button className="btn btn-primary btn-sm" onClick={() => setShowCreateReq(true)}>
              + 新需求
            </button>
          </div>

          {reqsLoading && <div className="tab-empty">⏳ 加载中...</div>}
          {reqsError && <div className="tab-empty" style={{ color: 'var(--color-error)' }}>{reqsError}</div>}

          {!reqsLoading && !reqsError && reqs.length === 0 && (
            <div className="tab-empty"><p>该项目暂无需求，点「+ 新需求」创建一个吧。</p></div>
          )}

          {!reqsLoading && !reqsError && reqs.length > 0 && (
            <div className="pr-list">
              <table className="pr-table">
                <thead>
                  <tr>
                    <th style={{ width: 110 }}>ID</th>
                    <th>标题</th>
                    <th style={{ width: 90 }}>优先级</th>
                    <th style={{ width: 130 }}>状态</th>
                    <th style={{ width: 110 }}>Sprint</th>
                    <th style={{ width: 110 }}>更新时间</th>
                  </tr>
                </thead>
                <tbody>
                  {renderRequirementRows(reqs)}
                </tbody>
              </table>
            </div>
          )}

          {showCreateReq && id && (
            <CreateRequirementDialog
              projectId={id}
              onClose={() => setShowCreateReq(false)}
              onCreated={async () => {
                setShowCreateReq(false);
                const data = await requirementsApi.list({ project_id: id });
                setReqs(data);
              }}
            />
          )}
        </div>
      )}

      {/* ── Review ── */}
      {tab === 'review' && (
        <div className="tab-content">
          {/* Not configured */}
          {!prsLoading && prData && !prData.configured && (
            <div className="review-unconfigured">
              <div className="review-unconfigured-icon">🔌</div>
              <p>项目未配置平台 Token，无法拉取 PR 列表。</p>
              <p>
                请先到
                <a href="/settings" style={{ color: 'var(--color-primary)', margin: '0 4px' }}>设置页</a>
                添加 Token，再到「概览」tab 绑定到本项目。
              </p>
            </div>
          )}

          {prsLoading && <div className="tab-empty">拉取 PR 列表中...</div>}
          {prsError && <div className="tab-empty" style={{ color: 'var(--color-error)' }}>{prsError}</div>}

          {!prsLoading && prData && (prData.configured || prData.prs.length > 0) && (
            <>
              {prData.prs.length === 0 && (
                <div className="tab-empty"><p>暂无 Open 状态的 PR</p></div>
              )}
              {prData.prs.length > 0 && (
                <>
                  <div className="review-requirements-bar">
                    <label className="review-requirements-label">额外审查要求</label>
                    <input
                      className="form-input review-requirements-input"
                      placeholder="如：重点关注安全性、检查 SQL 注入风险、确认错误处理是否完善..."
                      value={extraRequirements}
                      onChange={e => setExtraRequirements(e.target.value)}
                      disabled={!!reviewingPR}
                    />
                  </div>
                  <div className="pr-list">
                  <table className="pr-table">
                    <thead>
                      <tr>
                        <th style={{ width: 52 }}>#</th>
                        <th>标题</th>
                        <th style={{ width: 260 }}>分支</th>
                        <th style={{ width: 100 }}>作者</th>
                        <th style={{ width: 90 }}>更新时间</th>
                        <th style={{ width: 120 }}></th>
                      </tr>
                    </thead>
                    <tbody>
                      {prData.prs.map(pr => (
                        <tr key={pr.number} className={reviewingPR?.number === pr.number ? 'pr-row-active' : ''}>
                          <td style={{ color: 'var(--color-text-muted)', fontFamily: 'var(--font-mono)', fontSize: 12 }}>#{pr.number}</td>
                          <td className="pr-title">
                            <a href={pr.html_url} target="_blank" rel="noreferrer" style={{ color: 'var(--color-primary)' }}>
                              {pr.title}
                            </a>
                          </td>
                          <td><code className="pr-branch">{pr.head_branch}</code> ← <code className="pr-branch">{pr.base_branch}</code></td>
                          <td>{pr.author}</td>
                          <td>{pr.updated_at ? new Date(pr.updated_at).toLocaleDateString('zh-CN') : '—'}</td>
                          <td>
                            <button
                              className="btn btn-primary btn-sm"
                              onClick={() => handleReviewWithTracking(pr)}
                              disabled={!!reviewingPR}
                            >
                              {reviewingPR?.number === pr.number ? '审查中...' : 'Claude Review'}
                            </button>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
                </>
              )}
            </>
          )}

          {/* Stream output */}
          {(reviewLines.length > 0 || reviewingPR) && (
            <div className="review-panel-wrap">
              <div className="review-panel-header">
                <span>
                  Review 输出
                  {reviewingPR ? `：#${reviewingPR.number} ${reviewingPR.title}` : ''}
                  {reviewDone ? ' — 已完成' : ''}
                </span>
                {reviewDone && (
                  <button className="btn btn-secondary btn-sm" onClick={() => {
                    setReviewLines([]); setReviewDone(false); setCommentBody(''); setSubmitMsg('');
                  }}>
                    清除
                  </button>
                )}
              </div>
              <div className="coding-panel" ref={reviewPanelRef} style={{ maxHeight: 360 }}>
                {reviewLines.map((line, i) => (
                  <div key={i} className={`coding-line coding-line-${line.type}`}>{line.content}</div>
                ))}
                {reviewingPR && !reviewDone && (
                  <div className="coding-line coding-line-tool_call">● Claude 正在审查代码...</div>
                )}
              </div>
            </div>
          )}

          {/* Comment editor — shown after review completes */}
          {reviewDone && lastReviewedPRRef.current > 0 && (
            <div className="review-comment-wrap">
              <div className="review-panel-header">
                <span>PR Comment 草稿 — #{lastReviewedPRRef.current}</span>
              </div>
              <textarea
                className="review-comment-editor"
                value={commentBody}
                onChange={e => setCommentBody(e.target.value)}
                rows={12}
                placeholder="Review 内容将自动填入，可在此修改后提交..."
              />
              <div className="review-comment-actions">
                {submitMsg && (
                  <span className={submitMsg.startsWith('✅') ? 'submit-ok' : 'submit-err'}>{submitMsg}</span>
                )}
                <button
                  className="btn btn-primary"
                  onClick={handleSubmitComment}
                  disabled={submitting || !commentBody}
                >
                  {submitting ? '提交中...' : '一键提交 Comment'}
                </button>
              </div>
            </div>
          )}
        </div>
      )}

      {/* ── Weekly report ── */}
      {tab === 'weekly' && id && (
        <div className="tab-content">
          <ProjectWeeklyReport projectId={id} />
        </div>
      )}
    </div>
  );
}

// Inline "新需求" create dialog (moved from the old requirement kanban page).
function CreateRequirementDialog({ projectId, onClose, onCreated }: {
  projectId: string;
  onClose: () => void;
  onCreated: () => void;
}) {
  const [description, setDescription] = useState('');
  const [priority, setPriority] = useState('medium');
  const [sprint, setSprint] = useState('');
  const [saving, setSaving] = useState(false);

  const handleSubmit = async () => {
    if (!description) return;
    setSaving(true);
    try {
      await requirementsApi.create({ project_id: projectId, description, priority, sprint });
      onCreated();
    } catch (err: any) {
      alert(err.message);
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal" onClick={e => e.stopPropagation()} style={{ maxWidth: 520 }}>
        <h3>新需求</h3>
        <div className="form-group">
          <label>需求内容 (必填)</label>
          <textarea value={description} onChange={e => setDescription(e.target.value)} className="form-input" rows={5}
            placeholder="描述你想要实现的功能。例如：报表支持导出为 Excel 格式，目前只支持 PDF，用户需要 Excel 导出以便在本地编辑..." autoFocus />
        </div>
        <div className="form-row">
          <div className="form-group" style={{ flex: 1 }}>
            <label>优先级</label>
            <select value={priority} onChange={e => setPriority(e.target.value)} className="form-input">
              <option value="high">🔴 High</option>
              <option value="medium">🟡 Medium</option>
              <option value="low">🟢 Low</option>
            </select>
          </div>
          <div className="form-group" style={{ flex: 1 }}>
            <label>Sprint (可选)</label>
            <input type="text" value={sprint} onChange={e => setSprint(e.target.value)} className="form-input"
              placeholder="2026-W30" />
          </div>
        </div>
        <div className="form-actions">
          <button className="btn" onClick={onClose}>取消</button>
          <button className="btn btn-primary" onClick={handleSubmit} disabled={saving || !description}>
            {saving ? '创建中...（AI 正在提炼标题）' : '创建'}
          </button>
        </div>
      </div>
    </div>
  );
}
