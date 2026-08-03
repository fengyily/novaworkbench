import { useState, useEffect, useRef, useCallback } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { requirementsApi, projectsApi, API_BASE, statusLabels, type Requirement, type Project } from '../api/client';
import DeepRefineChat from '../components/DeepRefineChat';
import DocRefineChat from '../components/DocRefineChat';
import CodingChat from '../components/CodingChat';
import MarkdownViewer from '../components/MarkdownViewer';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import './RequirementDetail.css';

interface DesignData {
  overview?: string;
  files?: string[];
  steps?: string[];
  model_changes?: string;
  risks?: string[];
  plan_markdown?: string; // plan-mode output (raw markdown, not the legacy JSON schema)
}

// Two-role stage-gate lifecycle. Each gate is completed by a manual action.
// draft → analyzing → designing → designed → developing → done
type Stage = 'analyst' | 'architect' | 'developer' | 'done';

function stageFor(status: string): Stage {
  switch (status) {
    case 'draft':
    case 'analyzing':
      return 'analyst';
    case 'designing':
      return 'architect';
    case 'designed':
    case 'developing':
      return 'developer';
    case 'done':
      return 'done';
    default:
      return 'analyst';
  }
}

export default function RequirementDetail() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [req, setReq] = useState<Requirement | null>(null);
  const [project, setProject] = useState<Project | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState('');
  const [codingLines, setCodingLines] = useState<{ type: string; content: string }[]>([]);
  const [coding, setCoding] = useState(false);
  const codingRef = useRef<HTMLDivElement>(null);
  const esRef = useRef<EventSource | null>(null);
  const extraDescRef = useRef('');

  // Branch modal state
  const [showBranchModal, setShowBranchModal] = useState(false);
  const [branchName, setBranchName] = useState('');
  const [baseBranch, setBaseBranch] = useState('');
  const [availableBranches, setAvailableBranches] = useState<string[]>([]);

  // Streaming design state (architect phase)
  const [designLines, setDesignLines] = useState<{ type: string; content: string }[]>([]);
  const [designing, setDesigning] = useState(false);
  const designRef = useRef<HTMLDivElement>(null);

  // Fullscreen Markdown viewer for design plan
  const [showDesignFullscreen, setShowDesignFullscreen] = useState(false);

  // Edit modal state
  const [showEditModal, setShowEditModal] = useState(false);
  const [editTitle, setEditTitle] = useState('');
  const [editDesc, setEditDesc] = useState('');
  const [editPriority, setEditPriority] = useState('medium');
  const [editSprint, setEditSprint] = useState('');

  const handleDelete = async () => {
    if (!req) return;
    if (!window.confirm(`确认删除需求「${req.title}」？此操作不可恢复。`)) return;
    await requirementsApi.delete(req.id);
    navigate(`/projects/${req.project_id}`);
  };

  useEffect(() => {
    if (!id) return;
    requirementsApi.get(id).then(r => {
      setReq(r);
      projectsApi.get(r.project_id).then(setProject).catch(() => {});
    }).catch(() => {}).finally(() => setLoading(false));
  }, [id]);

  const refresh = useCallback(async () => {
    if (!id) return;
    const updated = await requirementsApi.get(id);
    setReq(updated);
  }, [id]);

  const parseDesign = (raw: string): DesignData => {
    try { return JSON.parse(raw); } catch { return { plan_markdown: raw }; }
  };

  // ── Status gate transitions ────────────────────────────────────────────────
  const transition = async (newStatus: string, label: string) => {
    if (!id) return;
    setBusy(label);
    try {
      await requirementsApi.updateStatus(id, newStatus);
      await refresh();
    } catch (err: any) {
      alert('状态流转失败: ' + err.message);
    } finally {
      setBusy('');
    }
  };

  // ── Architect phase: stream design generation via SSE ─────────────────────
  const runArchitectDesign = async () => {
    if (!id) return;
    setDesigning(true);
    setDesignLines([]);

    try {
      const res = await fetch(`${API_BASE}/api/wizard/architect-design`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ requirement_id: id }),
      });

      const reader = res.body?.getReader();
      if (!reader) throw new Error('No response body');
      const decoder = new TextDecoder();
      let buffer = '';

      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        const parts = buffer.split('\n\n');
        buffer = parts.pop() ?? '';
        for (const part of parts) {
          const dataLine = part.startsWith('data: ') ? part.slice(6) : part;
          if (!dataLine.trim()) continue;
          try {
            const evt = JSON.parse(dataLine);
            if (evt.type === 'done') {
              setDesigning(false);
              await refresh();
              return;
            }
            if (evt.type === 'error') {
              setDesignLines(prev => [...prev, { type: 'error', content: '❌ ' + evt.content }]);
            } else {
              setDesignLines(prev => [...prev, { type: evt.type, content: evt.content ?? '' }]);
            }
          } catch { /* skip malformed */ }
        }
      }
    } catch (err: any) {
      setDesignLines(prev => [...prev, { type: 'error', content: '❌ ' + err.message }]);
    } finally {
      setDesigning(false);
    }
  };

  useEffect(() => {
    if (designRef.current) designRef.current.scrollTop = designRef.current.scrollHeight;
  }, [designLines]);

  // ── Edit requirement (title/description/priority/sprint) ───────────────────
  const openEdit = () => {
    if (!req) return;
    setEditTitle(req.title);
    setEditDesc(req.description);
    setEditPriority(req.priority);
    setEditSprint(req.sprint);
    setShowEditModal(true);
  };

  const saveEdit = async () => {
    if (!id) return;
    setBusy('保存');
    try {
      await requirementsApi.update(id, {
        title: editTitle,
        description: editDesc,
        priority: editPriority,
        sprint: editSprint,
      });
      setShowEditModal(false);
      await refresh();
    } catch (err: any) {
      alert('保存失败: ' + err.message);
    } finally {
      setBusy('');
    }
  };

  // ── Developer phase: job streaming ────────────────────────────────────────
  const streamJob = useCallback((jobId: string) => {
    if (esRef.current) esRef.current.close();
    const es = new EventSource(`${API_BASE}/api/wizard/jobs/${jobId}/stream`);
    esRef.current = es;
    setCoding(true);

    es.onmessage = (e) => {
      try {
        const evt = JSON.parse(e.data);
        if (evt.type === 'job_done') {
          es.close();
          esRef.current = null;
          setCoding(false);
          if (id && (evt.status === 'done' || evt.exit_code === 0)) {
            requirementsApi.updateStatus(id, 'developing').then(() => refresh());
            localStorage.setItem(`coding_job_${id}`, `done:${jobId}`);
          } else {
            localStorage.removeItem(`coding_job_${id}`);
          }
          return;
        }
        setCodingLines(prev => [...prev, { type: evt.type, content: evt.content ?? '' }]);
      } catch { /* skip malformed */ }
    };

    es.onerror = () => {
      es.close();
      esRef.current = null;
      setCoding(false);
    };
  }, [id, refresh]);

  const doStartCoding = async (bName: string, bBase: string) => {
    if (!req || !project || !id) return;
    setCoding(true);
    setCodingLines([]);

    const design = parseDesign(req.design_docs);
    const baseDesc = (design.plan_markdown
      ? `## 技术方案\n${design.plan_markdown}`
      : [
          design.overview ? `## 技术方案\n${design.overview}` : '',
          design.steps?.length ? `## 实现步骤\n${design.steps.map((s, i) => `${i + 1}. ${s}`).join('\n')}` : '',
          design.files?.length ? `## 涉及文件\n${design.files.map(f => `- ${f}`).join('\n')}` : '',
          design.model_changes && design.model_changes !== '无' ? `## 数据模型变更\n${design.model_changes}` : '',
        ].filter(Boolean).join('\n\n')) || req.description;

    let desc = baseDesc;
    if (extraDescRef.current) {
      desc = baseDesc + `\n\n## 追加调整\n${extraDescRef.current}`;
      extraDescRef.current = '';
    }

    try {
      // Mark developer phase as in-progress before launching the job.
      await requirementsApi.updateStatus(id, 'developing').catch(() => {});
      await refresh();

      const res = await fetch(`${API_BASE}/api/wizard/start-coding`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          project_path: project.local_path,
          requirement_title: req.title,
          requirement_desc: desc,
          requirement_id: req.id,
          branch_name: bName,
          base_branch: bBase,
        }),
      });
      const json = await res.json();
      const jobId = json.data?.job_id;
      if (!jobId) throw new Error('未获取到任务 ID');
      localStorage.setItem(`coding_job_${id}`, jobId);
      streamJob(jobId);
    } catch (err: any) {
      setCodingLines([{ type: 'error', content: '❌ ' + err.message }]);
      setCoding(false);
    }
  };

  const openBranchModal = (extraDesc = '') => {
    if (!req || !project) return;
    extraDescRef.current = extraDesc;
    const defaultBranch = `feat/${req.id}`;
    const defaultBase = project.default_branch || 'main';
    setBranchName(defaultBranch);
    setBaseBranch(defaultBase);
    setShowBranchModal(true);
    fetch(`${API_BASE}/api/fs/git-branches?path=${encodeURIComponent(project.local_path)}`)
      .then(r => r.json())
      .then(json => {
        if (json.success && Array.isArray(json.data?.branches)) {
          setAvailableBranches(json.data.branches);
        }
      })
      .catch(() => {});
  };

  const confirmBranchAndStart = () => {
    setShowBranchModal(false);
    doStartCoding(branchName, baseBranch);
  };

  // Restore active coding job when returning to this page
  useEffect(() => {
    if (!id) return;
    const saved = localStorage.getItem(`coding_job_${id}`);
    if (!saved) return;

    const isDone = saved.startsWith('done:');
    const savedJobId = isDone ? saved.slice(5) : saved;

    fetch(`${API_BASE}/api/wizard/jobs/${savedJobId}`)
      .then(r => r.json())
      .then(json => {
        if (!json.success) return;
        const { status, log } = json.data as { status: string; log: { type: string; content: string }[] };
        if (!log || log.length === 0) return;
        setCodingLines(log);
        if (status === 'running') streamJob(savedJobId);
      })
      .catch(() => {});
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id]);

  useEffect(() => {
    return () => { if (esRef.current) esRef.current.close(); };
  }, []);

  useEffect(() => {
    if (codingRef.current) codingRef.current.scrollTop = codingRef.current.scrollHeight;
  }, [codingLines]);

  if (loading) return <div className="detail-loading">⏳ 加载中...</div>;
  if (!req) return <div className="detail-error">❌ 需求未找到</div>;

  const design = parseDesign(req.design_docs);
  const hasDesign = !!(design.overview || (design.steps && design.steps.length > 0) || design.plan_markdown);
  const stage = stageFor(req.status);

  const STEPS = [
    { key: 'analyst', label: '需求分析师', icon: '🔍', doneStatus: 'designing' },
    { key: 'architect', label: '架构师', icon: '📐', doneStatus: 'designed' },
    { key: 'developer', label: '开发者', icon: '🚀', doneStatus: 'done' },
  ] as const;
  const stageIndex = stage === 'done' ? 3 : STEPS.findIndex(s => s.key === stage);

  return (
    <div className="req-detail">
      {/* Branch modal */}
      {showBranchModal && (
        <div className="modal-overlay" onClick={() => setShowBranchModal(false)}>
          <div className="modal-box" onClick={e => e.stopPropagation()}>
            <h3>🌿 选择开发分支</h3>
            <p style={{ fontSize: 13, color: 'var(--color-text-muted)', marginBottom: 16 }}>
              Claude 将在指定分支上进行代码修改
            </p>
            <div className="modal-field">
              <label>基础分支（从哪里签出）</label>
              <select className="input" value={baseBranch} onChange={e => setBaseBranch(e.target.value)}>
                {availableBranches.length === 0 && <option value={baseBranch}>{baseBranch}</option>}
                {availableBranches.map(b => <option key={b} value={b}>{b}</option>)}
              </select>
            </div>
            <div className="modal-field">
              <label>新分支名</label>
              <input className="input" list="branch-suggestions" value={branchName}
                onChange={e => setBranchName(e.target.value)} placeholder={`feat/${req.id}`} />
              <datalist id="branch-suggestions">
                {availableBranches.map(b => <option key={b} value={b} />)}
              </datalist>
            </div>
            <div className="modal-actions">
              <button className="btn btn-primary" onClick={confirmBranchAndStart}>🚀 确认，开始开发</button>
              <button className="btn" onClick={() => setShowBranchModal(false)}>取消</button>
            </div>
          </div>
        </div>
      )}

      {/* Edit modal */}
      {showEditModal && req && (
        <div className="modal-overlay" onClick={() => setShowEditModal(false)}>
          <div className="modal-box" onClick={e => e.stopPropagation()}>
            <h3>✏️ 编辑需求</h3>
            <div className="modal-field">
              <label>标题</label>
              <input className="input" value={editTitle} onChange={e => setEditTitle(e.target.value)} />
            </div>
            <div className="modal-field">
              <label>描述</label>
              <textarea className="input" rows={6} value={editDesc} onChange={e => setEditDesc(e.target.value)} style={{ resize: 'vertical' }} />
            </div>
            <div style={{ display: 'flex', gap: 12 }}>
              <div className="modal-field" style={{ flex: 1 }}>
                <label>优先级</label>
                <select className="input" value={editPriority} onChange={e => setEditPriority(e.target.value)}>
                  <option value="low">low</option>
                  <option value="medium">medium</option>
                  <option value="high">high</option>
                  <option value="critical">critical</option>
                </select>
              </div>
              <div className="modal-field" style={{ flex: 1 }}>
                <label>Sprint</label>
                <input className="input" value={editSprint} onChange={e => setEditSprint(e.target.value)} />
              </div>
            </div>
            <div className="modal-actions">
              <button className="btn btn-primary" onClick={saveEdit} disabled={!!busy}>
                {busy === '保存' ? '⏳ 保存中...' : '💾 保存'}
              </button>
              <button className="btn" onClick={() => setShowEditModal(false)}>取消</button>
            </div>
          </div>
        </div>
      )}

      {/* Fullscreen Markdown viewer for design plan */}
      {showDesignFullscreen && design.plan_markdown && (
        <MarkdownViewer
          title="📐 技术实现方案"
          content={design.plan_markdown}
          onClose={() => setShowDesignFullscreen(false)}
        />
      )}

      {/* Header */}
      <div className="detail-header">
        <button className="btn" onClick={() => navigate(`/projects/${req.project_id}`)}>← 项目</button>
        <div className="detail-id">{req.id}</div>
        <div style={{ marginLeft: 'auto', display: 'flex', gap: 8 }}>
          <button className="btn btn-sm" onClick={openEdit}>✏️ 编辑</button>
          <button className="btn btn-sm btn-danger" onClick={handleDelete}>🗑️ 删除</button>
        </div>
      </div>

      <h1>{req.title}</h1>

      <div className="detail-meta">
        <span className={`status-tag status-${req.status}`}>{statusLabels[req.status] || req.status}</span>
        <span className={`priority-tag ${req.priority}`}>{req.priority.toUpperCase()}</span>
        {req.sprint && <span className="sprint-tag">Sprint: {req.sprint}</span>}
        {project && <span className="project-tag">📁 {project.name}</span>}
      </div>

      {req.description && (
        <div className="detail-desc"><p>{req.description}</p></div>
      )}

      {/* Stage stepper */}
      <div className="stage-stepper">
        {STEPS.map((s, i) => {
          const isDone = stageIndex > i || (stage === 'done');
          const isActive = stageIndex === i;
          return (
            <div key={s.key} className={`stage-step${isActive ? ' active' : ''}${isDone ? ' done' : ''}`}>
              <span className="stage-num">{isDone ? '✅' : s.icon}</span>
              <span className="stage-label">{s.label}</span>
              {i < STEPS.length - 1 && <span className="stage-sep">→</span>}
            </div>
          );
        })}
      </div>

      {/* ── Analyst stage ── */}
      {/* While analyzing, DeepRefineChat is itself the section (own card + header),
          so we render it standalone — no outer "需求分析师" card around it, which
          would otherwise create a card-in-card with two overlapping 🔍 headers. */}
      {/* ── Analyst stage ── */}
      {/* While analyzing, DeepRefineChat is itself the section (own card + header),
          so we render it standalone — no outer "需求分析师" card around it, which
          would otherwise create a card-in-card with two overlapping 🔍 headers. */}
      {req.status === 'analyzing' && (
        <DeepRefineChat
          reqId={req.id}
          projectPath={project?.local_path || ''}
          requirementTitle={req.title}
          currentAnalysis={req.acceptance_criteria}
          onGenerateDesign={() => transition('designing', '生成技术方案').then(() => runArchitectDesign())}
          onReset={() => setReq(prev => prev ? { ...prev, status: 'draft' } : prev)}
        />
      )}

      {req.status === 'draft' && (
        <div className="detail-section analysis-section">
          <div className="section-header"><h3>🔍 需求分析师</h3></div>
          <div className="tab-empty">
            <p>需求已创建。由需求分析师结合项目情况完善需求。</p>
            <button className="btn btn-primary" onClick={() => transition('analyzing', '开始分析')} disabled={!!busy}>
              {busy === '开始分析' ? '⏳ ...' : '🤖 开始需求分析'}
            </button>
          </div>
        </div>
      )}

      {/* ── Architect stage ── */}
      {(stage === 'architect' || req.status === 'designed' || stage === 'developer' || stage === 'done') && (
        <div className="detail-section design-section">
          <div className="section-header">
            <h3>📐 架构师</h3>
            {req.status === 'designing' && hasDesign && (
              <button className="btn btn-sm" onClick={runArchitectDesign} disabled={designing}>🔄 重新生成</button>
            )}
          </div>

          {(designing || designLines.length > 0) && (
            <div className="coding-panel" ref={designRef} style={{ marginBottom: 16 }}>
              {designLines.map((line, i) => (
                <div key={i} className={`coding-line coding-line-${line.type}`}>{line.content}</div>
              ))}
              {designing && <div className="coding-line coding-line-tool_call">⏳ Claude 正在 plan 模式下制定技术方案...</div>}
            </div>
          )}

          {req.status === 'designing' && !hasDesign && !designing && (
            <div className="tab-empty">
              <p>需求分析已完成。架构师将在 <strong>plan 模式</strong>下探索项目代码，制定具体可执行的技术实现方案（Markdown）。</p>
              <button className="btn btn-primary" onClick={() => runArchitectDesign()} disabled={!!busy || designing}>
                {busy === '生成技术方案' ? '⏳ ...' : '📐 开始制定技术方案'}
              </button>
            </div>
          )}

          {hasDesign && (
            <>
              {design.plan_markdown ? (
                <>
                  <div className="analysis-summary"><ReactMarkdown remarkPlugins={[remarkGfm]}>{design.plan_markdown}</ReactMarkdown></div>
              <div style={{ display: 'flex', gap: 8, marginTop: 12, flexWrap: 'wrap' }}>
                        <button className="btn" onClick={() => setShowDesignFullscreen(true)}>📐 全屏查看方案</button>
                      </div>
                </>
              ) : (
                <>
                  {design.overview && <div className="analysis-summary">{design.overview}</div>}
                  {design.files && design.files.length > 0 && (
                    <div className="analysis-block">
                      <h4>📄 涉及文件</h4>
                      <ul>{design.files.map((f, i) => <li key={i}><code>{f}</code></li>)}</ul>
                    </div>
                  )}
                  {design.steps && design.steps.length > 0 && (
                    <div className="analysis-block">
                      <h4>🔢 实现步骤</h4>
                      <ol>{design.steps.map((s, i) => <li key={i}>{s}</li>)}</ol>
                    </div>
                  )}
                  {design.model_changes && design.model_changes !== '无' && (
                    <div className="analysis-block"><h4>🗄️ 数据模型变更</h4><p>{design.model_changes}</p></div>
                  )}
                  {design.risks && design.risks.length > 0 && (
                    <div className="analysis-block">
                      <h4>⚠️ 实现风险</h4>
                      <ul>{design.risks.map((r, i) => <li key={i} className="risk-item">{r}</li>)}</ul>
                    </div>
                  )}
                </>
              )}
            </>
          )}

          {req.status === 'designing' && hasDesign && (
            <>
              <div style={{ display: 'flex', gap: 8, marginTop: 12, flexWrap: 'wrap' }}>
                <button className="btn btn-primary" onClick={() => transition('designed', '方案完成')} disabled={!!busy}>
                  {busy === '方案完成' ? '⏳ ...' : '✅ 方案完成'}
                </button>
              </div>
              <DocRefineChat
                reqId={req.id}
                projectPath={project?.local_path || ''}
                docType="design"
                currentDoc={req.design_docs}
                onApplied={(newDoc) => setReq(prev => prev ? { ...prev, design_docs: newDoc } : prev)}
              />
            </>
          )}
        </div>
      )}

      {/* ── Developer stage ── */}
      {(stage === 'developer' || stage === 'done') && hasDesign && (
        <div className="detail-section">
          <div className="section-header"><h3>🚀 开发者</h3></div>

          {req.status === 'designed' && codingLines.length === 0 && !coding && (
            <div className="tab-empty">
              <p>方案已完成。开发者（Claude Code）将根据技术方案进行开发。</p>
              <p style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>项目路径：<code>{project?.local_path}</code></p>
              <button className="btn btn-primary" onClick={() => openBranchModal()}>🚀 开始开发</button>
            </div>
          )}

          {(codingLines.length > 0 || coding) && (
            <div className="coding-panel" ref={codingRef}>
              {codingLines.map((line, i) => (
                <div key={i} className={`coding-line coding-line-${line.type}`}>{line.content}</div>
              ))}
              {coding && <div className="coding-line coding-line-tool_call">⏳ Claude 正在工作...</div>}
            </div>
          )}

          {req.status === 'developing' && !coding && codingLines.length > 0 && (
            <>
              <div style={{ display: 'flex', gap: 8, marginTop: 12, flexWrap: 'wrap' }}>
                <button className="btn btn-primary" onClick={() => transition('done', '开发完成')} disabled={!!busy}>
                  {busy === '开发完成' ? '⏳ ...' : '✅ 开发完成'}
                </button>
                <button className="btn" onClick={() => openBranchModal()}>🔄 重新运行</button>
              </div>
              <CodingChat
                reqId={req.id}
                projectPath={project?.local_path || ''}
                requirementTitle={req.title}
                onStartCoding={(d) => openBranchModal(d)}
              />
            </>
          )}

          {req.status === 'done' && (
            <div className="tab-empty"><p>✅ 开发已完成。</p></div>
          )}
        </div>
      )}
    </div>
  );
}
