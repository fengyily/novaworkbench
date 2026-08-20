import { useState, useEffect, useRef, useCallback, useMemo } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import {
  projectsApi, runnerApi, reviewApi, platformApi, requirementsApi, knowledgeApi,
  usageApi, usageTotalInput, fmtCost,
  type Project, type RunStatus, type PR, type PRListResponse, type PlatformToken,
  type Requirement, type KnowledgeItem, type ReqUsage, type ProjectUsage, statusLabels,
} from '../api/client';
import ProjectWeeklyReport from './ProjectWeeklyReport';
import { stripMarkdownPreview } from '../utils/preview';
import { createEventStream, type EventStream } from '../api/stream';
import './RequirementDetail.css';
import './ProjectDetail.css';
import './KnowledgePage.css';

type Tab = 'overview' | 'knowledge' | 'run' | 'requirements' | 'review' | 'weekly' | 'usage';

const priorityDots: Record<string, string> = {
  high: '🔴', medium: '🟡', low: '🟢',
};

const runStatusLabel: Record<string, string> = {
  stopped: '未运行', running: '运行中', done: '已停止', error: '错误',
};

const platformLabels: Record<string, string> = {
  github: 'GitHub', gitlab: 'GitLab', gitea: 'Gitea',
};

// Requirements tab pagination: 15 rows per page, first page shown by default.
const REQ_PAGE_SIZE = 15;

// Page-number window for the pagination control — always include the first and
// last page, keep a small neighborhood around the current one, and collapse the
// gap in between into an ellipsis. Yields numbers interleaved with '…'.
function reqPageWindow(total: number, current: number): (number | '…')[] {
  if (total <= 7) return Array.from({ length: total }, (_, i) => i + 1);
  const candidates = new Set([1, total, current - 1, current, current + 1]);
  const pages = [...candidates].filter(p => p >= 1 && p <= total).sort((a, b) => a - b);
  const out: (number | '…')[] = [];
  let prev = 0;
  for (const p of pages) {
    if (prev && p - prev > 1) out.push('…');
    out.push(p);
    prev = p;
  }
  return out;
}

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

  // Overview: project description (AI-generated, manually editable)
  const [descEditing, setDescEditing] = useState(false);
  const [descDraft, setDescDraft] = useState('');
  const [descSaving, setDescSaving] = useState(false);
  const [regenerating, setRegenerating] = useState(false);
  const [descMsg, setDescMsg] = useState<{ ok: boolean; text: string } | null>(null);

  // Requirements (shared by the requirements tab and the overview recent list)
  const [reqs, setReqs] = useState<Requirement[]>([]);
  const [reqsLoading, setReqsLoading] = useState(false);
  const [reqsError, setReqsError] = useState('');
  const [reqsLoaded, setReqsLoaded] = useState(false);
  const [showCreateReq, setShowCreateReq] = useState(false);
  // Requirements tab pagination — first page by default ("默认加载第一页").
  const [reqPage, setReqPage] = useState(1);

  // Per-requirement token totals (excl review) — drives the Tokens column in
  // the requirements list + overview. Loaded alongside reqs and refetched when
  // a requirement is created so the column stays current.
  const [reqUsage, setReqUsage] = useState<ReqUsage[]>([]);
  const refreshReqUsage = useCallback((projectId: string) => {
    usageApi.byRequirement(projectId).then(setReqUsage).catch(() => {});
  }, []);
  const reqUsageMap = useMemo(() => {
    const m = new Map<string, ReqUsage>();
    for (const r of reqUsage) m.set(r.requirement_id, r);
    return m;
  }, [reqUsage]);

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
  // Effective model the reviewer role ran with for the most recent review
  // (from the job_done SSE frame). Empty until a review completes.
  const [reviewModel, setReviewModel] = useState('');
  const [commentBody, setCommentBody] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [submitMsg, setSubmitMsg] = useState('');
  const [extraRequirements, setExtraRequirements] = useState('');
  const reviewEsRef = useRef<EventStream | null>(null);
  const reviewPanelRef = useRef<HTMLDivElement>(null);

  // Token usage tab — project total (excl review), per-requirement totals,
  // and the review breakdown (recorded but not counted in the total).
  const [projectUsage, setProjectUsage] = useState<ProjectUsage | null>(null);
  const [projectUsageLoading, setProjectUsageLoading] = useState(false);
  useEffect(() => {
    if (tab !== 'usage' || !id) return;
    let active = true;
    setProjectUsageLoading(true);
    usageApi.project(id)
      .then(data => { if (active) setProjectUsage(data); })
      .catch(() => {})
      .finally(() => { if (active) setProjectUsageLoading(false); });
    return () => { active = false; };
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tab, id]);

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

  // Knowledge base for this project. Loaded on demand when the knowledge tab
  // is opened; the tab groups entries by source_type (requirement / document /
  // code / other).
  const [knowledge, setKnowledge] = useState<KnowledgeItem[]>([]);
  const [knowledgeLoading, setKnowledgeLoading] = useState(false);
  // Markdown detail modal for archived-requirement knowledge entries.
  const [knowledgeModal, setKnowledgeModal] = useState<KnowledgeItem | null>(null);
  useEffect(() => {
    if (tab !== 'knowledge' || !id) return;
    let active = true;
    setKnowledgeLoading(true);
    knowledgeApi.list({ project_id: id, limit: 200 })
      .then(res => { if (active) setKnowledge(res.items ?? []); })
      .catch(() => {})
      .finally(() => { if (active) setKnowledgeLoading(false); });
    return () => { active = false; };
  // eslint-disable-next-line react-hooks/exhaustive-deps
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
    refreshReqUsage(id);
    return () => { active = false; };
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tab, id, reqsLoaded]);

  // Snaps the requirements-tab page back to the first page whenever the list is
  // replaced (initial load, post-create refresh), so a previously selected page
  // can never land past the new last page and "默认加载第一页" holds after refresh.
  useEffect(() => { setReqPage(1); }, [reqs]);

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

  // ── Overview: project description ───────────────────────────────────────────
  const handleSaveDesc = async () => {
    if (!id) return;
    setDescSaving(true);
    setDescMsg(null);
    try {
      const updated = await projectsApi.updateDescription(id, descDraft);
      setProject(updated);
      setDescEditing(false);
      setDescMsg({ ok: true, text: '已保存' });
    } catch (e: unknown) {
      setDescMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
    } finally {
      setDescSaving(false);
    }
  };

  const handleRegenerateDesc = async () => {
    if (!id || regenerating) return;
    setRegenerating(true);
    setDescMsg(null);
    try {
      const updated = await projectsApi.regenerateDescription(id);
      setProject(updated);
      setDescMsg({ ok: true, text: '已重新生成' });
    } catch (e: unknown) {
      setDescMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
    } finally {
      setRegenerating(false);
    }
  };

  // ── Review tab handlers ────────────────────────────────────────────────────

  const handleReview = useCallback(async (pr: PR) => {
    if (!id || reviewingPR) return;
    setReviewingPR(pr);
    setReviewLines([]);
    setReviewDone(false);
    setReviewModel('');
    setCommentBody('');
    setSubmitMsg('');
    reviewEsRef.current?.close();

    try {
      const { job_id } = await reviewApi.startReview(id, pr.head_branch, pr.base_branch, pr.number, pr.title, extraRequirements);

      const messageLines: string[] = [];

      reviewEsRef.current = createEventStream(
        reviewApi.streamJobUrl(id, job_id),
        (data) => {
          if (data.type === 'job_done') {
            setCommentBody(messageLines.join('\n\n'));
            setReviewDone(true);
            setReviewModel(data.model || '');
            setReviewingPR(null);
            reviewEsRef.current?.close();
            reviewEsRef.current = null;
          } else if (data.content) {
            setReviewLines(prev => [...prev, { type: data.type, content: data.content }]);
            if (data.type === 'message') messageLines.push(data.content);
          }
        },
        () => {
          setReviewLines(prev => [...prev, { type: 'error', content: 'SSE 连接中断' }]);
          setCommentBody(messageLines.join('\n\n'));
          setReviewDone(true);
          setReviewingPR(null);
          reviewEsRef.current = null;
        },
      );
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
      <td data-label="ID" style={{ color: 'var(--color-text-muted)', fontFamily: 'var(--font-mono)', fontSize: 12 }}>{req.id}</td>
      <td data-label="标题" className="pr-title">{req.title}</td>
      <td data-label="优先级"><span style={{ fontSize: 12, whiteSpace: 'nowrap' }}>{priorityDots[req.priority] ?? '⚪'} {req.priority}</span></td>
      <td data-label="状态"><span className={`status-badge status-${req.status}`}>{statusLabels[req.status] ?? req.status}</span></td>
      <td data-label="Tokens (入/出)" style={{ fontFamily: 'var(--font-mono)', fontSize: 12, whiteSpace: 'nowrap' }}>
        {(() => {
          const u = reqUsageMap.get(req.id);
          if (!u) return '—';
          return `${usageTotalInput(u).toLocaleString()} / ${u.output_tokens.toLocaleString()}`;
        })()}
      </td>
      <td data-label="成本" style={{ fontFamily: 'var(--font-mono)', fontSize: 12, whiteSpace: 'nowrap' }}>
        {(() => {
          const u = reqUsageMap.get(req.id);
          if (!u) return '—';
          return fmtCost(u.costs);
        })()}
      </td>
      <td data-label="创建时间" style={{ color: 'var(--color-text-muted)', fontSize: 12 }}>
        {req.created_at ? new Date(req.created_at).toLocaleDateString('zh-CN') : '—'}
      </td>
      <td data-label="更新时间" style={{ color: 'var(--color-text-muted)', fontSize: 12 }}>
        {req.updated_at ? new Date(req.updated_at).toLocaleDateString('zh-CN') : '—'}
      </td>
    </tr>
  ));

  // Overview shows only tasks that have not finished development ('done').
  // The requirements tab keeps showing everything (done sorts to the end).
  const overviewReqs = reqs.filter(r => r.status !== 'done');

  // Requirements tab pagination. The page is clamped to the (possibly shrunken)
  // list so a stale reqPage never escapes the array; the reset effect below
  // normally snaps back to page 1 after a list refresh.
  const totalReqPages = Math.max(1, Math.ceil(reqs.length / REQ_PAGE_SIZE));
  const curReqPage = Math.min(reqPage, totalReqPages);
  const pagedReqs = reqs.slice((curReqPage - 1) * REQ_PAGE_SIZE, curReqPage * REQ_PAGE_SIZE);

  // Render a group of knowledge entries under a labeled section. Reuses the
  // kb-card styles from KnowledgePage.css. The content preview is stripped of
  // markdown noise (stripMarkdownPreview) and clamped to a fixed number of
  // lines via CSS so cards stay uniform regardless of entry length; a
  // "查看全文" button on every card opens the full Markdown-rendered modal.
  const renderKnowledgeGroup = (label: string, items: KnowledgeItem[]) => {
    if (!items.length) return null;
    return (
      <div className="detail-section" key={label}>
        <h3 style={{ marginBottom: 12 }}>{label}（{items.length}）</h3>
        {items.map(k => (
          <div key={k.id} className="kb-card">
            <div className="kb-card-header">
              <span className={`kb-type-badge cat-${k.category}`}>{k.category || 'general'}</span>
              <span className="kb-card-title">{k.title}</span>
            </div>
            <div className="kb-card-content kb-clamp">{stripMarkdownPreview(k.content)}</div>
            <button className="kb-view-full" onClick={() => setKnowledgeModal(k)}>查看全文 →</button>
            <div className="kb-card-meta">
              <span className="kb-source">来源: {k.source_type}</span>
              {k.source_ref && <span className="kb-ref">{k.source_ref}</span>}
              <span className="kb-date">{new Date(k.created_at).toLocaleDateString()}</span>
            </div>
          </div>
        ))}
      </div>
    );
  };

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
        <button className={`tab-btn${tab === 'knowledge' ? ' active' : ''}`} onClick={() => setTab('knowledge')}>知识库</button>
        <button className={`tab-btn${tab === 'run' ? ' active' : ''}`} onClick={() => setTab('run')}>
          运行{isRunning ? ' ●' : ''}
        </button>
        <button className={`tab-btn${tab === 'requirements' ? ' active' : ''}`} onClick={() => { setTab('requirements'); setReqPage(1); }}>
          需求
        </button>
        <button className={`tab-btn${tab === 'review' ? ' active' : ''}`} onClick={() => setTab('review')}>
          代码 Review{reviewingPR ? ' ●' : ''}
        </button>
        <button className={`tab-btn${tab === 'usage' ? ' active' : ''}`} onClick={() => setTab('usage')}>
          Token 用量
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

          {/* Project description (AI-generated from CLAUDE.md, manual-edit lockable) */}
          <div className="detail-section" style={{ marginTop: 16 }}>
            <div className="section-header" style={{ marginBottom: 12 }}>
              <span style={{ fontWeight: 600, fontSize: 14 }}>简介</span>
              <span style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>
                {project.description_manual ? '✍ 手动修改' : '🤖 AI 生成'}
              </span>
            </div>

            {descEditing ? (
              <div>
                <textarea
                  className="form-input"
                  rows={3}
                  value={descDraft}
                  onChange={e => setDescDraft(e.target.value)}
                  placeholder="项目简介，建议 120 字以内..."
                  autoFocus
                />
                <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
                  <button className="btn btn-primary btn-sm" onClick={handleSaveDesc} disabled={descSaving}>
                    {descSaving ? '保存中...' : '保存'}
                  </button>
                  <button
                    className="btn btn-sm"
                    onClick={() => { setDescEditing(false); setDescMsg(null); }}
                    disabled={descSaving}
                  >
                    取消
                  </button>
                </div>
              </div>
            ) : (
              <div>
                <div style={{ color: 'var(--color-text-secondary)', fontSize: 13, whiteSpace: 'pre-wrap' }}>
                  {project.description || '暂无简介'}
                </div>
                <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
                  <button
                    className="btn btn-sm"
                    onClick={() => { setDescDraft(project.description); setDescEditing(true); setDescMsg(null); }}
                  >
                    编辑
                  </button>
                  <button
                    className="btn btn-sm"
                    onClick={handleRegenerateDesc}
                    disabled={regenerating}
                    title="根据当前 CLAUDE.md 重新由 AI 生成（会清除手动修改标记）"
                  >
                    {regenerating ? '生成中...' : '重新生成'}
                  </button>
                </div>
              </div>
            )}

            {descMsg && (
              <div style={{ marginTop: 8, fontSize: 12, color: descMsg.ok ? 'var(--color-success)' : 'var(--color-error)' }}>
                {descMsg.ok ? '✅ ' : '❌ '}{descMsg.text}
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
              <button className="recent-reqs-more" onClick={() => { setTab('requirements'); setReqPage(1); }}>查看全部 →</button>
            </div>

            {reqsLoading && <div className="tab-empty">⏳ 加载中...</div>}
            {!reqsLoading && overviewReqs.length === 0 && (
              <div className="tab-empty">
                {reqs.length === 0 ? (
                  <p>该项目暂无需求，<button className="recent-reqs-link" onClick={() => { setTab('requirements'); setShowCreateReq(true); }}>去创建 →</button></p>
                ) : (
                  <p>暂无未完成的需求</p>
                )}
              </div>
            )}
            {!reqsLoading && overviewReqs.length > 0 && (
              <div className="pr-list" style={{ marginBottom: 0 }}>
                <table className="pr-table table-cards">
                  <thead>
                    <tr>
                      <th style={{ width: 110 }}>ID</th>
                      <th>标题</th>
                      <th style={{ width: 90 }}>优先级</th>
                      <th style={{ width: 130 }}>状态</th>
                      <th style={{ width: 130 }}>Tokens (入/出)</th>
                      <th style={{ width: 110 }}>成本</th>
                      <th style={{ width: 110 }}>创建时间</th>
                      <th style={{ width: 110 }}>更新时间</th>
                    </tr>
                  </thead>
                  <tbody>
                    {renderRequirementRows(overviewReqs.slice(0, Math.min(overviewReqs.length, visibleCount)))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </div>
      )}

      {/* ── Knowledge base ── */}
      {tab === 'knowledge' && (
        <div className="tab-content">
          {knowledgeLoading ? (
            <div className="tab-empty"><p>加载中...</p></div>
          ) : knowledge.length === 0 ? (
            <div className="tab-empty"><p>暂无知识库数据。可先扫描项目或归档需求以沉淀知识。</p></div>
          ) : (
            <>
              {renderKnowledgeGroup('需求归档', knowledge.filter(k => k.source_type === 'requirement'))}
              {renderKnowledgeGroup('项目文档', knowledge.filter(k => k.source_type === 'document'))}
              {renderKnowledgeGroup('项目结构', knowledge.filter(k => k.source_type === 'code'))}
              {renderKnowledgeGroup('其他', knowledge.filter(k => !['requirement', 'document', 'code'].includes(k.source_type)))}
            </>
          )}
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
              <table className="pr-table table-cards">
                <thead>
                  <tr>
                    <th style={{ width: 110 }}>ID</th>
                    <th>标题</th>
                    <th style={{ width: 90 }}>优先级</th>
                    <th style={{ width: 130 }}>状态</th>
                    <th style={{ width: 130 }}>Tokens (入/出)</th>
                      <th style={{ width: 110 }}>成本</th>
                    <th style={{ width: 110 }}>创建时间</th>
                    <th style={{ width: 110 }}>更新时间</th>
                  </tr>
                </thead>
                <tbody>
                  {renderRequirementRows(pagedReqs)}
                </tbody>
              </table>
            </div>
          )}

          {/* Pagination — 15 rows per page; the summary shows the true total
              (count of all requirements, not just the current page). */}
          {!reqsLoading && !reqsError && totalReqPages > 1 && (
            <div className="pagination">
              <span className="pagination-info">
                共 {reqs.length} 条 · 第 {curReqPage} / {totalReqPages} 页
              </span>
              <button className="btn btn-sm" disabled={curReqPage <= 1} onClick={() => setReqPage(curReqPage - 1)}>
                ‹ 上一页
              </button>
              {reqPageWindow(totalReqPages, curReqPage).map((p, i) =>
                p === '…' ? (
                  <span key={`e${i}`} className="pagination-ellipsis">…</span>
                ) : (
                  <button
                    key={p}
                    className={`btn btn-sm${p === curReqPage ? ' active' : ''}`}
                    onClick={() => setReqPage(p)}
                  >
                    {p}
                  </button>
                ),
              )}
              <button className="btn btn-sm" disabled={curReqPage >= totalReqPages} onClick={() => setReqPage(curReqPage + 1)}>
                下一页 ›
              </button>
            </div>
          )}

          {showCreateReq && id && (
            <CreateRequirementForm
              projectId={id}
              onClose={() => setShowCreateReq(false)}
              onCreated={async (created: Requirement) => {
                setShowCreateReq(false);
                // 跳过需求分析 → 自动进入方案设计阶段：导航到详情页并传递
                // autoStartDesign 意图标记，由 RequirementDetail 一次性触发 architect-design，
                // 替代用户手动点击「生成技术方案」。
                if (created.skip_design) {
                  // 跳过设计 → 直接开发：导航到详情页并传递 autoStartCoding 意图标记，
                  // 由 RequirementDetail 自动唤起分支选择弹窗，直接进入开发流程。
                  navigate(`/requirements/${created.id}`, { state: { autoStartCoding: true } });
                  return;
                }
                if (created.skip_analysis) {
                  navigate(`/requirements/${created.id}`, { state: { autoStartDesign: true } });
                  return;
                }
                const data = await requirementsApi.list({ project_id: id });
                setReqs(data);
                refreshReqUsage(id);
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
                  <table className="pr-table table-cards">
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
                          <td data-label="#" style={{ color: 'var(--color-text-muted)', fontFamily: 'var(--font-mono)', fontSize: 12 }}>#{pr.number}</td>
                          <td data-label="标题" className="pr-title">
                            <a href={pr.html_url} target="_blank" rel="noreferrer" style={{ color: 'var(--color-primary)' }}>
                              {pr.title}
                            </a>
                          </td>
                          <td data-label="分支"><code className="pr-branch">{pr.head_branch}</code> ← <code className="pr-branch">{pr.base_branch}</code></td>
                          <td data-label="作者">{pr.author}</td>
                          <td data-label="更新时间">{pr.updated_at ? new Date(pr.updated_at).toLocaleDateString('zh-CN') : '—'}</td>
                          <td data-label="操作">
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
              <div className="review-model-line">
                🤖 本次 review 使用模型：{reviewModel || '默认模型'}
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

      {/* ── Token usage ── */}
      {tab === 'usage' && id && (
        <div className="tab-content">
          {projectUsageLoading ? (
            <div className="tab-empty"><p>加载中…</p></div>
          ) : !projectUsage ? (
            <div className="tab-empty"><p>暂无数据</p></div>
          ) : (
            <>
              {/* Project total (excludes review rows) */}
              <div className="detail-section" style={{ marginBottom: 16 }}>
                <div className="section-header" style={{ marginBottom: 12 }}>
                  <span style={{ fontWeight: 600, fontSize: 14 }}>项目 Token 消耗</span>
                  <span style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>不含代码审查</span>
                </div>
                <div style={{ display: 'flex', gap: 24, flexWrap: 'wrap' }}>
                  <div>
                    <div style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>输入</div>
                    <div style={{ fontFamily: 'var(--font-mono)', fontSize: 22, fontWeight: 600 }}>
                      {usageTotalInput(projectUsage.total).toLocaleString()}
                    </div>
                  </div>
                  <div>
                    <div style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>输出</div>
                    <div style={{ fontFamily: 'var(--font-mono)', fontSize: 22, fontWeight: 600, color: 'var(--color-primary)' }}>
                      {projectUsage.total.output_tokens.toLocaleString()}
                    </div>
                  </div>
                  <div>
                    <div style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>缓存读</div>
                    <div style={{ fontFamily: 'var(--font-mono)', fontSize: 16 }}>
                      {projectUsage.total.cache_read_tokens.toLocaleString()}
                    </div>
                  </div>
                  <div>
                    <div style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>缓存建</div>
                    <div style={{ fontFamily: 'var(--font-mono)', fontSize: 16 }}>
                      {projectUsage.total.cache_creation_tokens.toLocaleString()}
                    </div>
                  </div>
                  <div>
                    <div style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>费用</div>
                    <div style={{ fontFamily: 'var(--font-mono)', fontSize: 22, fontWeight: 600, color: '#10B981' }}>
                      {fmtCost(projectUsage.total.costs)}
                    </div>
                  </div>
                </div>
              </div>

              {/* Per-requirement breakdown */}
              <div className="detail-section" style={{ marginBottom: 16 }}>
                <div className="section-header" style={{ marginBottom: 12 }}>
                  <span style={{ fontWeight: 600, fontSize: 14 }}>按需求</span>
                </div>
                {projectUsage.by_requirement.length === 0 ? (
                  <div className="tab-empty"><p>暂无需求 Token 消耗</p></div>
                ) : (
                  <div className="pr-list">
                    <table className="pr-table table-cards">
                      <thead>
                        <tr>
                          <th>需求 ID</th>
                          <th style={{ width: 120 }}>输入</th>
                          <th style={{ width: 120 }}>输出</th>
                          <th style={{ width: 120 }}>缓存读</th>
                          <th style={{ width: 120 }}>缓存建</th>
                          <th style={{ width: 140 }}>成本</th>
                        </tr>
                      </thead>
                      <tbody>
                        {projectUsage.by_requirement.map(r => {
                          const matched = reqs.find(q => q.id === r.requirement_id);
                          return (
                            <tr key={r.requirement_id} style={{ cursor: 'pointer' }} onClick={() => navigate(`/requirements/${r.requirement_id}`)}>
                              <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>
                                {matched?.title || r.requirement_id}
                              </td>
                              <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{usageTotalInput(r).toLocaleString()}</td>
                              <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{r.output_tokens.toLocaleString()}</td>
                              <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{r.cache_read_tokens.toLocaleString()}</td>
                              <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{r.cache_creation_tokens.toLocaleString()}</td>
                              <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{fmtCost(r.costs)}</td>
                            </tr>
                          );
                        })}
                      </tbody>
                    </table>
                  </div>
                )}
              </div>

              {/* Per-model breakdown */}
              <div className="detail-section" style={{ marginBottom: 16 }}>
                <div className="section-header" style={{ marginBottom: 12 }}>
                  <span style={{ fontWeight: 600, fontSize: 14 }}>按模型</span>
                  <span style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>成本按配置币种分组（同模型跨平台默认不作合并）</span>
                </div>
                {projectUsage.by_model.length === 0 ? (
                  <div className="tab-empty"><p>暂无按模型统计</p></div>
                ) : (
                  <div className="pr-list">
                    <table className="pr-table table-cards">
                      <thead>
                        <tr>
                          <th>模型</th>
                          <th style={{ width: 120 }}>输入</th>
                          <th style={{ width: 120 }}>输出</th>
                          <th style={{ width: 120 }}>缓存读</th>
                          <th style={{ width: 120 }}>缓存建</th>
                          <th style={{ width: 150 }}>成本</th>
                        </tr>
                      </thead>
                      <tbody>
                        {projectUsage.by_model.map(mu => (
                          <tr key={mu.model}>
                            <td><code className="pr-branch">{mu.model || '未知模型'}</code></td>
                            <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{usageTotalInput(mu).toLocaleString()}</td>
                            <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{mu.output_tokens.toLocaleString()}</td>
                            <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{mu.cache_read_tokens.toLocaleString()}</td>
                            <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{mu.cache_creation_tokens.toLocaleString()}</td>
                            <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{fmtCost(mu.costs)}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </div>

              {/* Per-day breakdown */}
              <div className="detail-section" style={{ marginBottom: 16 }}>
                <div className="section-header" style={{ marginBottom: 12 }}>
                  <span style={{ fontWeight: 600, fontSize: 14 }}>按日</span>
                </div>
                {projectUsage.by_day.length === 0 ? (
                  <div className="tab-empty"><p>暂无按日统计</p></div>
                ) : (
                  <div className="pr-list">
                    <table className="pr-table table-cards">
                      <thead>
                        <tr>
                          <th>日期</th>
                          <th style={{ width: 120 }}>输入</th>
                          <th style={{ width: 120 }}>输出</th>
                          <th style={{ width: 120 }}>缓存读</th>
                          <th style={{ width: 120 }}>缓存建</th>
                          <th style={{ width: 150 }}>成本</th>
                        </tr>
                      </thead>
                      <tbody>
                        {projectUsage.by_day.map(d => (
                          <tr key={d.date}>
                            <td><code className="pr-branch">{d.date}</code></td>
                            <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{usageTotalInput(d).toLocaleString()}</td>
                            <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{d.output_tokens.toLocaleString()}</td>
                            <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{d.cache_read_tokens.toLocaleString()}</td>
                            <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{d.cache_creation_tokens.toLocaleString()}</td>
                            <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{fmtCost(d.costs)}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </div>

              {/* Review breakdown (recorded, NOT counted in the total above) */}
              <div className="detail-section">
                <div className="section-header" style={{ marginBottom: 12 }}>
                  <span style={{ fontWeight: 600, fontSize: 14 }}>代码审查消耗</span>
                  <span style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>不计入项目总额</span>
                </div>
                {projectUsage.review.length === 0 ? (
                  <div className="tab-empty"><p>暂无代码审查记录</p></div>
                ) : (
                  <div className="pr-list">
                    <table className="pr-table table-cards">
                      <thead>
                        <tr>
                          <th style={{ width: 70 }}>PR</th>
                          <th>标题</th>
                          <th style={{ width: 160 }}>分支</th>
                          <th style={{ width: 110 }}>输入</th>
                          <th style={{ width: 110 }}>输出</th>
                          <th style={{ width: 110 }}>时间</th>
                        </tr>
                      </thead>
                      <tbody>
                        {projectUsage.review.map(rv => (
                          <tr key={rv.id}>
                            <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>
                              {rv.pr_number ? `#${rv.pr_number}` : '—'}
                            </td>
                            <td className="pr-title">{rv.pr_title || '—'}</td>
                            <td>{rv.branch ? <code className="pr-branch">{rv.branch}</code> : '—'}</td>
                            <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{rv.input_tokens.toLocaleString()}</td>
                            <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{rv.output_tokens.toLocaleString()}</td>
                            <td style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>
                              {rv.created_at ? new Date(rv.created_at).toLocaleString('zh-CN') : '—'}
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </div>
              <small style={{ color: 'var(--color-text-muted)', fontSize: 12, display: 'block', marginTop: 8 }}>
                输入 = 直接输入 + 缓存读 + 缓存建。仅统计已完成的完整调用。
              </small>
            </>
          )}
        </div>
      )}

      {/* ── Knowledge detail modal (Markdown rendering) ── */}
      {knowledgeModal && (
        <div className="kb-modal-overlay modal-fullscreen-overlay" onClick={() => setKnowledgeModal(null)}>
          <div className="kb-modal modal-fullscreen" onClick={e => e.stopPropagation()}>
            <div className="kb-modal-header">
              <h2>{knowledgeModal.title}</h2>
              <button className="kb-modal-close" onClick={() => setKnowledgeModal(null)}>×</button>
            </div>
            <div className="kb-modal-body">
              <div className="kb-markdown">
                <ReactMarkdown remarkPlugins={[remarkGfm]}>{knowledgeModal.content}</ReactMarkdown>
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

// Inline "新需求" create form — expanded in-place under the requirements tab
// (replaces the old modal). After the user submits, the backend formats the
// raw content into Markdown and distills a title via the LLM, so the button
// label reflects that AI step.
function CreateRequirementForm({ projectId, onClose, onCreated }: {
  projectId: string;
  onClose: () => void;
  onCreated: (req: Requirement) => void;
}) {
  const [description, setDescription] = useState('');
  const [priority, setPriority] = useState('medium');
  // 开发流程：full（标准：分析→设计→开发）| skip-analysis（默认：跳过分析→设计→开发）| direct（直接开发：跳过分析与设计）
  const [flow, setFlow] = useState<'full' | 'skip-analysis' | 'direct'>('skip-analysis');
  const [saving, setSaving] = useState(false);

  const handleSubmit = async () => {
    if (!description) return;
    setSaving(true);
    try {
      const skipAnalysis = flow !== 'full';
      const skipDesign = flow === 'direct';
      const created = await requirementsApi.create({ project_id: projectId, description, priority, skip_analysis: skipAnalysis, skip_design: skipDesign });
      onCreated(created);
    } catch (err: any) {
      alert(err.message);
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="create-req-form">
      <div className="create-req-form-header">
        <h3>新需求</h3>
        <button className="btn btn-secondary btn-sm" onClick={onClose} disabled={saving}>收起</button>
      </div>
      <div className="form-group">
        <label>需求内容 (必填)</label>
        <textarea value={description} onChange={e => setDescription(e.target.value)} className="form-input" rows={6}
          placeholder="用自然语言描述你想要实现的功能。例如：报表支持导出为 Excel 格式，目前只支持 PDF，用户需要 Excel 导出以便在本地编辑；要能选择导出的列..." autoFocus />
        <small className="form-hint">
          提交后由 AI 整理为结构化 Markdown（背景 / 目标 / 功能要点 / 验收标准）并提炼标题，可在详情页继续编辑。
        </small>
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
      </div>
      <div className="form-group">
        <label>开发流程</label>
        <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
          <label style={{ display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer', userSelect: 'none' }}>
            <input type="radio" name="dev-flow" checked={flow === 'skip-analysis'} onChange={() => setFlow('skip-analysis')}
              style={{ width: 'auto' }} />
            <span>跳过分析（直接方案设计 → 开发）</span>
          </label>
          <label style={{ display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer', userSelect: 'none' }}>
            <input type="radio" name="dev-flow" checked={flow === 'direct'} onChange={() => setFlow('direct')}
              style={{ width: 'auto' }} />
            <span>直接开发（跳过分析与设计）</span>
          </label>
          <label style={{ display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer', userSelect: 'none' }}>
            <input type="radio" name="dev-flow" checked={flow === 'full'} onChange={() => setFlow('full')}
              style={{ width: 'auto' }} />
            <span>标准流程（分析 → 设计 → 开发）</span>
          </label>
        </div>
        <small style={{ display: 'block', marginTop: 4, color: 'var(--text-secondary, #64748B)' }}>
          小改动可选「直接开发」，跳过分析与设计阶段，创建后直接在详情页进入开发实现。
        </small>
      </div>
      <div className="form-actions">
        <button className="btn" onClick={onClose} disabled={saving}>取消</button>
        <button className="btn btn-primary" onClick={handleSubmit} disabled={saving || !description}>
          {saving ? '创建中...（AI 整理中）' : '创建'}
        </button>
      </div>
    </div>
  );
}
