import { useState, useEffect, useRef, useCallback } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { requirementsApi, projectsApi, API_BASE, statusLabels, type Requirement, type Project, mergeApi, type MergeState } from '../api/client';
import DeepRefineChat from '../components/DeepRefineChat';
import DocRefineChat from '../components/DocRefineChat';
import CodingChat from '../components/CodingChat';
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

  // Merge / PR step state (post-coding 合入).
  // mergeState holds the git preview (dev/target branches, uncommitted, pr_url);
  // mergeLines streams the merge/push/resolve job; conflictFiles / prLink are
  // surfaced from job log lines of type "conflict" / "pr_link".
  const [mergeState, setMergeState] = useState<MergeState | null>(null);
  const [showMergeModal, setShowMergeModal] = useState(false);
  const [mergeMode, setMergeMode] = useState<'local' | 'push'>('local');
  const [mergeTarget, setMergeTarget] = useState('main');
  const [mergeCommitMsg, setMergeCommitMsg] = useState('');
  const [mergeDeleteBranch, setMergeDeleteBranch] = useState(false);
  const [mergeLines, setMergeLines] = useState<{ type: string; content: string }[]>([]);
  const [merging, setMerging] = useState(false);
  const [conflictFiles, setConflictFiles] = useState<string[] | null>(null);
  const [prLink, setPrLink] = useState('');
  const mergeEsRef = useRef<EventSource | null>(null);

  // Streaming design state (architect phase)
  const [designLines, setDesignLines] = useState<{ type: string; content: string }[]>([]);
  const [designing, setDesigning] = useState(false);
  const designRef = useRef<HTMLDivElement>(null);
  const designEsRef = useRef<EventSource | null>(null);

  // Collapsible "思考过程" toggle for the architect design stream.
  // While the design job is actively running the panel stays open; once it
  // finishes the panel collapses and a toggle lets the user re-expand it.
  const [showDesignProcess, setShowDesignProcess] = useState(false);

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

  // ── Architect phase: async design generation via JobStore ─────────────────
  // The architect-design endpoint creates a background job and returns its id
  // immediately (same pattern as start-coding). We stream the job's log lines
  // over SSE; on job_done we refresh so design_docs (now persisted server-side)
  // renders. The active job id is persisted on the requirement as design_job_id,
  // so a page refresh reconnects to the running job instead of re-launching it
  // or re-showing the "开始制定技术方案" button.
  const streamDesignJob = useCallback((jobId: string) => {
    if (designEsRef.current) designEsRef.current.close();
    const es = new EventSource(`${API_BASE}/api/wizard/jobs/${jobId}/stream`);
    designEsRef.current = es;
    setDesigning(true);

    es.onmessage = (e) => {
      try {
        const evt = JSON.parse(e.data);
        if (evt.type === 'job_done') {
          es.close();
          designEsRef.current = null;
          setDesigning(false);
          if (evt.status === 'done' || evt.exit_code === 0) {
            // design_docs persisted server-side; refresh to render it.
            refresh();
          }
          return;
        }
        setDesignLines(prev => [...prev, { type: evt.type, content: evt.content ?? '' }]);
      } catch { /* skip malformed */ }
    };
    es.onerror = () => {
      es.close();
      designEsRef.current = null;
      setDesigning(false);
    };
  }, [refresh]);

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
      const json = await res.json();
      const jobId = json.data?.job_id;
      if (!jobId) throw new Error(json.error?.message || '未获取到任务 ID');
      streamDesignJob(jobId);
    } catch (err: any) {
      setDesignLines([{ type: 'error', content: '❌ ' + err.message }]);
      setDesigning(false);
    }
  };

  // Reconnect to an in-flight design job when (re)entering the page — e.g.
  // after a refresh. The requirement carries design_job_id (server truth); if
  // the job is still running we resume its stream, otherwise (server restarted,
  // job evicted from the in-memory ring buffer) we drop into the idle state so
  // the start button shows again.
  useEffect(() => {
    if (!id || !req?.design_job_id) return;
    const jobId = req.design_job_id;
    fetch(`${API_BASE}/api/wizard/jobs/${jobId}`)
      .then(r => r.json())
      .then(json => {
        if (!json.success) { setDesigning(false); return; }
        const { status, log } = json.data as { status: string; log: { type: string; content: string }[] };
        if (log && log.length > 0) setDesignLines(log);
        if (status === 'running') streamDesignJob(jobId);
        else setDesigning(false);
      })
      .catch(() => setDesigning(false));
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id, req?.design_job_id]);

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

  // ── Merge / PR step (post-coding 合入) ──────────────────────────────────────
  // Loads the git preview (dev branch = current HEAD, target branch, uncommitted
  // changes, pr_url). Called when entering the merge section and after each job
  // completes so the UI reflects the real on-disk repo state (the merge job may
  // have switched branches or left a mid-merge).
  const refreshMergeState = useCallback(async () => {
    if (!id) return;
    try {
      const st = await mergeApi.state(id);
      setMergeState(st);
    } catch { /* not a git repo or not ready → keep null */ }
  }, [id]);

  // streamMergeJob subscribes to a merge/push/resolve job. It collects log lines
  // into mergeLines and, on job_done, inspects the accumulated lines: a "conflict"
  // line drives the conflict panel, a "pr_link" line surfaces the create-PR link.
  const streamMergeJob = useCallback((jobId: string) => {
    if (mergeEsRef.current) mergeEsRef.current.close();
    const es = new EventSource(`${API_BASE}/api/wizard/jobs/${jobId}/stream`);
    mergeEsRef.current = es;
    setMerging(true);
    setConflictFiles(null);

    let acc: { type: string; content: string }[] = [];
    es.onmessage = (e) => {
      try {
        const evt = JSON.parse(e.data);
        if (evt.type === 'job_done') {
          es.close();
          mergeEsRef.current = null;
          setMerging(false);
          const exitOk = evt.status === 'done' || evt.exit_code === 0;
          // Resolve conflict / pr_link signals from the accumulated log.
          const conflict = acc.find(l => l.type === 'conflict');
          if (conflict) {
            try {
              // content looks like "...[\"a\",\"b\"]"; pull out the JSON array.
              const m = conflict.content.match(/\[[\s\S]*\]/);
              setConflictFiles(m ? JSON.parse(m[0]) : []);
            } catch { setConflictFiles([]); }
          }
          const link = acc.find(l => l.type === 'pr_link');
          if (link) setPrLink(link.content);
          if (exitOk && !conflict) refreshMergeState();
          return;
        }
        const line = { type: evt.type, content: evt.content ?? '' };
        acc = [...acc, line];
        setMergeLines(prev => [...prev, line]);
      } catch { /* skip malformed */ }
    };
    es.onerror = () => {
      es.close();
      mergeEsRef.current = null;
      setMerging(false);
    };
  }, [refreshMergeState]);

  const openMergeModal = async (mode: 'local' | 'push') => {
    if (!req || !id) return;
    setMergeMode(mode);
    setMergeLines([]);
    setConflictFiles(null);
    setPrLink('');
    setMergeCommitMsg(req.title);
    setMergeDeleteBranch(false);
    setShowMergeModal(true);
    await refreshMergeState();
  };

  const confirmMerge = async () => {
    if (!req || !id) return;
    setShowMergeModal(false);
    setMerging(true);
    setMergeLines([]);
    setConflictFiles(null);
    setPrLink('');
    try {
      const body = mergeMode === 'local'
        ? { target_branch: mergeTarget, commit_message: mergeCommitMsg, delete_branch: mergeDeleteBranch }
        : { commit_message: mergeCommitMsg };
      const { job_id } = mergeMode === 'local'
        ? await mergeApi.local(id, body as any)
        : await mergeApi.push(id, body as any);
      streamMergeJob(job_id);
    } catch (err: any) {
      setMergeLines([{ type: 'error', content: '❌ ' + err.message }]);
      setMerging(false);
    }
  };

  const doMergeAction = async (action: 'abort' | 'continue' | 'resolve') => {
    if (!id) return;
    setMergeLines([]);
    setConflictFiles(null);
    try {
      if (action === 'abort') {
        await mergeApi.abort(id);
        await refreshMergeState();
        setMerging(false);
        return;
      }
      const { job_id } = action === 'continue' ? await mergeApi.cont(id) : await mergeApi.resolve(id);
      streamMergeJob(job_id);
    } catch (err: any) {
      setMergeLines([{ type: 'error', content: '❌ ' + err.message }]);
      setMerging(false);
    }
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
        if (!json.success) {
          // Job is neither in memory nor persisted (e.g. backend restarted
          // mid-run before the log could be saved). Drop the stale pointer so
          // we don't keep retrying a dead job.
          localStorage.removeItem(`coding_job_${id}`);
          return;
        }
        const { status, log } = json.data as { status: string; log: { type: string; content: string }[] };
        if (!log || log.length === 0) return;
        setCodingLines(log);
        if (status === 'running') streamJob(savedJobId);
      })
      .catch(() => {});
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id]);

  useEffect(() => {
    return () => {
      if (esRef.current) esRef.current.close();
      if (designEsRef.current) designEsRef.current.close();
      if (mergeEsRef.current) mergeEsRef.current.close();
    };
  }, []);

  useEffect(() => {
    if (codingRef.current) codingRef.current.scrollTop = codingRef.current.scrollHeight;
  }, [codingLines]);

  // Load merge state when entering the developer/done stage (the merge step
  // only makes sense once coding has produced a dev branch to merge from).
  useEffect(() => {
    if (!id || !req) return;
    if (req.status === 'developing' || req.status === 'done') refreshMergeState();
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id, req?.status, refreshMergeState]);

  // Keep the modal's target-branch selector in sync with the loaded state.
  useEffect(() => {
    if (mergeState) setMergeTarget(mergeState.target_branch);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mergeState?.target_branch]);

  if (loading) return <div className="detail-loading">⏳ 加载中...</div>;
  if (!req) return <div className="detail-error">❌ 需求未找到</div>;

  const design = parseDesign(req.design_docs);
  const hasDesign = !!(design.overview || (design.steps && design.steps.length > 0) || design.plan_markdown);
  const stage = stageFor(req.status);
  // Design (architect) stream state. While the job runs the panel stays open;
  // once finished it collapses behind the "思考过程" toggle.
  const designProcessActive = designing || !!req.design_job_id;
  const designPanelOpen = designProcessActive || showDesignProcess;
  const showDesignToggle = designLines.length > 0 && !designProcessActive;

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

      {/* Merge / PR modal */}
      {showMergeModal && mergeState && (
        <div className="modal-overlay" onClick={() => !merging && setShowMergeModal(false)}>
          <div className="modal-box merge-modal" onClick={e => e.stopPropagation()}>
            <h3>{mergeMode === 'local' ? '🔀 本地合入' : '🌐 推送并发起 PR'}</h3>
            {mergeMode === 'local' ? (
              <>
                <div className="modal-field">
                  <label>目标分支（合入到哪）</label>
                  <select className="input" value={mergeTarget} onChange={e => setMergeTarget(e.target.value)}>
                    {availableBranches.length === 0 && <option value={mergeTarget}>{mergeTarget}</option>}
                    {availableBranches.map(b => <option key={b} value={b}>{b}</option>)}
                  </select>
                </div>
                <div className="modal-field">
                  <label>开发分支</label>
                  <input className="input" value={mergeState.dev_branch} disabled />
                </div>
                <div className="merge-hint">
                  <span>领先目标 {mergeState.ahead} · 落后 {mergeState.behind}</span>
                  <span>未提交文件 {mergeState.uncommitted_count}</span>
                </div>
                {mergeState.uncommitted_count > 0 && (
                  <details className="merge-files">
                    <summary>查看未提交文件</summary>
                    <ul>{mergeState.uncommitted_files.map((f, i) => <li key={i}><code>{f}</code></li>)}</ul>
                  </details>
                )}
                <div className="modal-field">
                  <label>提交信息</label>
                  <input className="input" value={mergeCommitMsg} onChange={e => setMergeCommitMsg(e.target.value)} />
                </div>
                <label className="merge-check">
                  <input type="checkbox" checked={mergeDeleteBranch} onChange={e => setMergeDeleteBranch(e.target.checked)} />
                  合并后删除开发分支
                </label>
                <div className="modal-actions">
                  <button className="btn btn-primary" onClick={confirmMerge} disabled={!!busy}>🔀 确认合入</button>
                  <button className="btn" onClick={() => setShowMergeModal(false)} disabled={!!busy}>取消</button>
                </div>
              </>
            ) : (
              <>
                <div className="modal-field">
                  <label>推送分支</label>
                  <input className="input" value={mergeState.dev_branch} disabled />
                </div>
                <div className="merge-hint">
                  <span>远程仓库</span>
                  <code>{mergeState.remote_url || '（未配置）'}</code>
                </div>
                <div className="modal-field">
                  <label>提交信息</label>
                  <input className="input" value={mergeCommitMsg} onChange={e => setMergeCommitMsg(e.target.value)} />
                </div>
                {!mergeState.has_remote && (
                  <p className="merge-warn">该项目未配置 origin 远程仓库，无法推送。</p>
                )}
                <div className="modal-actions">
                  <button className="btn btn-primary" onClick={confirmMerge} disabled={!!busy || !mergeState.has_remote}>🌐 确认推送</button>
                  <button className="btn" onClick={() => setShowMergeModal(false)} disabled={!!busy}>取消</button>
                </div>
              </>
            )}
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
          analysisJobId={req.analysis_job_id}
          onTurnDone={refresh}
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
          {/* Compact toolbar: the architect role is already shown in the
              stepper, so this section leads with a content-oriented caption
              and parks the stream toggle + regenerate action together. */}
          {(showDesignToggle || (req.status === 'designing' && hasDesign)) && (
            <div className="design-toolbar">
              {showDesignToggle && (
                <button
                  className="btn btn-sm process-toggle"
                  onClick={() => setShowDesignProcess(v => !v)}
                  aria-expanded={designPanelOpen}
                >
                  {designPanelOpen ? '▼ 收起思考过程' : '▶ 思考过程'}
                </button>
              )}
              {req.status === 'designing' && hasDesign && (
                <button className="btn btn-sm" onClick={runArchitectDesign} disabled={designing}>🔄 重新生成</button>
              )}
            </div>
          )}

          {designPanelOpen && (
            <div className="coding-panel" ref={designRef} style={{ marginBottom: 16 }}>
              {designLines.map((line, i) => (
                <div key={i} className={`coding-line coding-line-${line.type}`}>{line.content}</div>
              ))}
              {designProcessActive && <div className="coding-line coding-line-tool_call">⏳ Claude 正在 plan 模式下制定技术方案...</div>}
            </div>
          )}

          {req.status === 'designing' && !hasDesign && !designing && !req.design_job_id && (
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
                <div className="analysis-summary"><ReactMarkdown remarkPlugins={[remarkGfm]}>{design.plan_markdown}</ReactMarkdown></div>
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

          {req.status === 'developing' && !coding && (
            <>
              {/* After a backend restart the in-memory job log is gone, but the
                  developing status is persisted in the DB — still allow the user
                  to mark done or re-run without a live coding log. */}
              {codingLines.length === 0 && (
                <p style={{ fontSize: 12, color: 'var(--color-text-muted)', marginTop: 8 }}>
                  开发任务已完成（日志因服务重启已清空）。确认代码无误后可标记开发完成，或重新运行。
                </p>
              )}
              <div style={{ display: 'flex', gap: 8, marginTop: 12, flexWrap: 'wrap' }}>
                <button className="btn btn-primary" onClick={() => transition('done', '开发完成')} disabled={!!busy}>
                  {busy === '开发完成' ? '⏳ ...' : '✅ 开发完成'}
                </button>
                <button className="btn" onClick={() => openBranchModal()}>🔄 重新运行</button>
              </div>

              {/* ── Merge / PR step ── */}
              <div className="merge-section">
                <div className="merge-actions">
                  <button className="btn" onClick={() => openMergeModal('local')} disabled={merging}>🔀 本地合入</button>
                  <button className="btn" onClick={() => openMergeModal('push')} disabled={merging}>🌐 推送并发起 PR</button>
                </div>

                {mergeState?.mid_merge && (
                  <div className="conflict-panel">
                    <p className="conflict-title">⚠️ 仓库处于合并冲突状态</p>
                    {conflictFiles && conflictFiles.length > 0 && (
                      <ul className="conflict-file-list">
                        {conflictFiles.map((f, i) => <li key={i} className="conflict-file"><code>{f}</code></li>)}
                      </ul>
                    )}
                    <div className="conflict-actions">
                      <button className="btn btn-primary" onClick={() => doMergeAction('resolve')} disabled={merging}>🤖 AI 解决冲突</button>
                      <button className="btn" onClick={() => doMergeAction('continue')} disabled={merging}>✋ 已手动解决，继续</button>
                      <button className="btn btn-danger" onClick={() => doMergeAction('abort')} disabled={merging}>↩️ 中止合并</button>
                    </div>
                  </div>
                )}

                {mergeLines.length > 0 && (
                  <div className="coding-panel merge-panel">
                    {mergeLines.map((line, i) => (
                      <div key={i} className={`coding-line coding-line-${line.type}`}>{line.content}</div>
                    ))}
                    {merging && <div className="coding-line coding-line-tool_call">⏳ 执行中...</div>}
                  </div>
                )}

                {prLink && !merging && (
                  <a className="btn btn-primary pr-link-btn" href={prLink} target="_blank" rel="noreferrer">
                    🌐 创建 PR
                  </a>
                )}
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
            <div className="merge-section">
              {prLink ? (
                <a className="btn btn-primary pr-link-btn" href={prLink} target="_blank" rel="noreferrer">🌐 查看 / 创建 PR</a>
              ) : (
                <div className="tab-empty"><p>✅ 开发已完成。</p></div>
              )}
              <div className="merge-actions">
                <button className="btn" onClick={() => openMergeModal('local')} disabled={merging}>🔀 本地合入</button>
                <button className="btn" onClick={() => openMergeModal('push')} disabled={merging}>🌐 推送并发起 PR</button>
              </div>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
