import { useState, useEffect, useRef, useCallback, type ReactNode } from 'react';
import { useParams, useNavigate, useLocation } from 'react-router-dom';
import { requirementsApi, projectsApi, API_BASE, authedFetch, statusLabels, mergeApi, usageApi, usageTotalInput, stepLabels, type Requirement, type Project, type MergeState, type RequirementUsage } from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import DeepRefineChat from '../components/DeepRefineChat';
import DocRefineChat from '../components/DocRefineChat';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { exportDesignPdf } from '../utils/exportDesignPdf';
import { appendLogLine, coalesceLogLines } from '../utils/logLines';
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
    case 'archived':
      // An archived requirement is conceptually "done"; render the done-stage
      // layout plus an archive banner, instead of falling back to analyst.
      return 'done';
    default:
      return 'analyst';
  }
}

// Renders JobStore log lines into a dark coding panel. Consecutive "message"
// lines (Claude's assistant text, streamed token-by-token as separate LogLines)
// are joined back into one markdown string and rendered via ReactMarkdown so
// ```code blocks``` become real <pre> with a distinct background instead of
// plain text that blends into the panel. Joining also avoids spurious
// mid-line breaks: token deltas often split a single source line across two
// LogLines, and rendering each in its own block div would wrap them apart.
// tool_call / tool_result / phase / error / done lines render as before.
function CodingLines({ lines }: { lines: { type: string; content: string }[] }) {
  const nodes: ReactNode[] = [];
  let i = 0;
  let key = 0;
  while (i < lines.length) {
    const line = lines[i];
    // "message" lines are streamed token-by-token; group consecutive ones and
    // render through ReactMarkdown so code blocks/headings/lists become real
    // elements instead of blending into plain text.
    if (line.type === 'message') {
      const group: string[] = [];
      while (i < lines.length && lines[i].type === 'message') {
        group.push(lines[i].content);
        i++;
      }
      nodes.push(
        <div key={key++} className="coding-line coding-line-message coding-message-md">
          <ReactMarkdown remarkPlugins={[remarkGfm]}>{group.join('')}</ReactMarkdown>
        </div>
      );
    } else if (line.type === 'result') {
      // The "result" line is the dev-complete summary emitted as a single
      // LogLine (e.g. "全部完成。下面是实现总结。…" + Markdown). Render it
      // through ReactMarkdown too — otherwise the summary's headings/code
      // blocks/lists show as a garbled wall of plain text.
      nodes.push(
        <div key={key++} className="coding-line coding-line-message coding-message-md">
          <ReactMarkdown remarkPlugins={[remarkGfm]}>{line.content}</ReactMarkdown>
        </div>
      );
      i++;
    } else {
      nodes.push(
        <div key={key++} className={`coding-line coding-line-${line.type}`}>{line.content}</div>
      );
      i++;
    }
  }
  return <>{nodes}</>;
}

export default function RequirementDetail() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const location = useLocation();
  const [req, setReq] = useState<Requirement | null>(null);
  const [project, setProject] = useState<Project | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState('');
  const [codingLines, setCodingLines] = useState<{ type: string; content: string }[]>([]);
  const [coding, setCoding] = useState(false);
  const codingRef = useRef<HTMLDivElement>(null);
  const esRef = useRef<EventStream | null>(null);
  const extraDescRef = useRef('');
  // One-shot guard for the "auto-start design after skip-analysis creation"
  // flow. Set when the autoStartDesign navigation intent triggers the architect
  // stage so a subsequent refresh / req change doesn't re-fire it.
  const autoStartRef = useRef(false);

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
  const mergeEsRef = useRef<EventStream | null>(null);

  // Streaming design state (architect phase)
  const [designLines, setDesignLines] = useState<{ type: string; content: string }[]>([]);
  const [designing, setDesigning] = useState(false);
  // Set when the design job ended in an error status. The stream panel
  // collapses on success (the design renders standalone), but on failure we
  // keep it open so the red error line stays visible — otherwise the error
  // hides behind the "思考过程" toggle and the user has no idea the run failed.
  const [designError, setDesignError] = useState(false);
  const designRef = useRef<HTMLDivElement>(null);
  const designEsRef = useRef<EventStream | null>(null);

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
  const [editSkipAnalysis, setEditSkipAnalysis] = useState(true);

  // 追加调整 input. Resume the prior coding session (--resume coding_session_id)
  // with ONLY the user's follow-up as -p — the resumed conversation already
  // carries the requirement/design/persona, so no system prompt or project
  // context is re-injected. Output reuses codingLines/coding-panel for continuity.
  const [adjustInput, setAdjustInput] = useState('');

  // PDF export state for the technical design doc.
  const [exporting, setExporting] = useState(false);

  // Token usage for this requirement (per-step + total). Refreshed on mount
  // and after each stage completes (refresh()), so the breakdown reflects the
  // latest claude turns.
  const [usage, setUsage] = useState<RequirementUsage | null>(null);
  const [usageLoading, setUsageLoading] = useState(false);
  const loadUsage = useCallback(async () => {
    if (!id) return;
    setUsageLoading(true);
    try { setUsage(await usageApi.requirement(id)); } catch { /* ignore */ }
    finally { setUsageLoading(false); }
  }, [id]);

  // Copy a ready-to-paste resume command to the clipboard. Instead of just the
  // bare session id, we compose `cd "<project_path>" && claude --resume "<sid>"`
  // so the user can paste it straight into a shell and land in the right CWD.
  // Falls back to copying the sid alone when no project path is known.
  const copySessionId = async (sid: string): Promise<void> => {
    const path = project?.local_path;
    const cmd = path
      ? `cd "${path}" && claude --resume "${sid}"`
      : `claude --resume "${sid}"`;
    try {
      await navigator.clipboard.writeText(cmd);
    } catch {
      // Clipboard API can be unavailable (non-secure context); fall back to a
      // legacy execCommand copy off a transient textarea.
      const ta = document.createElement('textarea');
      ta.value = cmd;
      ta.style.position = 'fixed';
      ta.style.left = '-9999px';
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand('copy'); } catch { /* noop */ }
      ta.remove();
    }
  };

  // Compose the design as a Markdown string for PDF export. plan_markdown is
  // already raw Markdown; legacy JSON designs are reassembled into Markdown
  // sections mirroring how doStartCoding builds the dev-instruction payload.
  const designToMarkdown = (d: DesignData): string => {
    if (d.plan_markdown) return d.plan_markdown;
    const parts: string[] = [];
    if (d.overview) parts.push(`## 概述\n${d.overview}`);
    if (d.steps?.length) parts.push(`## 实现步骤\n${d.steps.map((s, i) => `${i + 1}. ${s}`).join('\n')}`);
    if (d.files?.length) parts.push(`## 涉及文件\n${d.files.map(f => `- ${f}`).join('\n')}`);
    if (d.model_changes && d.model_changes !== '无') parts.push(`## 数据模型变更\n${d.model_changes}`);
    if (d.risks?.length) parts.push(`## 实现风险\n${d.risks.map(r => `- ${r}`).join('\n')}`);
    return parts.join('\n\n') || req?.description || '';
  };

  const handleExportPdf = async () => {
    if (!req || !project) return;
    setExporting(true);
    try {
      await exportDesignPdf({
        title: req.title,
        meta: `${project.name} · ${req.id}`,
        markdown: designToMarkdown(parseDesign(req.design_docs)),
        filename: req.title,
      });
    } catch (err: any) {
      alert('导出 PDF 失败: ' + err.message);
    } finally {
      setExporting(false);
    }
  };

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
    loadUsage();
  }, [id, loadUsage]);

  const refresh = useCallback(async () => {
    if (!id) return;
    const updated = await requirementsApi.get(id);
    setReq(updated);
    loadUsage();
  }, [id, loadUsage]);

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

  // Archive a finished requirement into the project knowledge base (final
  // requirement + design docs become reusable AI context). Re-archiving the
  // same requirement overwrites the previous knowledge entry.
  const handleArchive = async () => {
    if (!id) return;
    setBusy('归档');
    try {
      await requirementsApi.archive(id);
      await refresh();
    } catch (err: any) {
      alert('归档失败: ' + err.message);
    } finally {
      setBusy('');
    }
  };

  // Reverse archive: status returns to "done" and the knowledge entry produced
  // by archiving is removed from the project knowledge base.
  const handleUnarchive = async () => {
    if (!id) return;
    if (!confirm('取消归档将同时移除该需求在知识库中的条目，确认继续？')) return;
    setBusy('取消归档');
    try {
      await requirementsApi.unarchive(id);
      await refresh();
    } catch (err: any) {
      alert('取消归档失败: ' + err.message);
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
    setDesigning(true);
    setDesignError(false);

    designEsRef.current = createEventStream(
      `/api/wizard/jobs/${jobId}/stream`,
      (evt) => {
        if (evt.type === 'job_done') {
          designEsRef.current?.close();
          designEsRef.current = null;
          setDesigning(false);
          // Keep the stream panel open when the job errored so the red error
          // line stays in view instead of collapsing behind the toggle.
          const failed = evt.status === 'error' || (typeof evt.exit_code === 'number' && evt.exit_code !== 0);
          setDesignError(failed);
          // Always refresh: the backend clears design_job_id on every terminal
          // path (success and error), so refreshing unblocks the UI even when
          // the job ended in an error status (which a success-only refresh
          // would skip, leaving the stale job id wedging the stage).
          refresh();
          return;
        }
        // Coalesce consecutive "模型思考中… (N tokens)" phase lines into one
        // updatable row instead of stacking one per heartbeat.
        setDesignLines(prev => appendLogLine(prev, { type: evt.type, content: evt.content ?? '' }));
      },
      () => {
        designEsRef.current = null;
        // The SSE link can drop before the final job_done frame lands (network
        // blip, proxy timeout). Poll the snapshot; if the job has finished
        // server-side, finalize + refresh; otherwise keep designing and let the
        // reconnect effect (keyed on design_job_id) or a later poll reconcile.
        pollDesignJob(jobId, 0);
      },
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refresh]);

  // Reconcile a dropped design SSE link by polling the job snapshot. If the
  // job is finished server-side, stop designing and refresh so design_docs /
  // design_job_id reflect server truth. Bounded retries so a genuinely
  // long-running job doesn't spin here forever — if still running after the
  // retries, leave designing=true and let the user refresh manually.
  const pollDesignJob = useCallback((jobId: string, attempt: number) => {
    if (attempt > 12) { // ~2 min of retries (12 * 10s)
      // Give up polling but sync with server truth: the job may have finished
      // (and cleared design_job_id) since the last check. Without this refresh
      // a stale req.design_job_id would keep designProcessActive true forever.
      setDesigning(false);
      refresh();
      return;
    }
    setTimeout(() => {
      authedFetch(`${API_BASE}/api/wizard/jobs/${jobId}`)
        .then(r => r.json())
        .then(json => {
          if (!json.success) { setDesigning(false); refresh(); return; }
          const { status, exit_code } = json.data as { status: string; exit_code: number };
          if (status === 'running') {
            pollDesignJob(jobId, attempt + 1);
          } else {
            setDesigning(false);
            setDesignError(status === 'error' || exit_code !== 0);
            refresh();
          }
        })
        .catch(() => {
          setDesigning(false);
        });
    }, 10000);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refresh]);

  const runArchitectDesign = async () => {
    if (!id) return;
    setDesigning(true);
    setDesignLines([]);
    setDesignError(false);

    try {
      const res = await authedFetch(`${API_BASE}/api/wizard/architect-design`, {
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

  // Auto-start the architect-design flow when the user just created a
  // requirement with skip_analysis (navigated here with the autoStartDesign
  // intent flag). This replaces the manual "生成技术方案" click for the
  // skip-analysis path. It runs the SAME code path as that button:
  // transition('designing') then runArchitectDesign(). One-shot guarded so a
  // later refresh / req change can't re-fire it; if a design job is already
  // running or a design already exists, we leave it to the reconnect effect.
  useEffect(() => {
    if (!req || autoStartRef.current) return;
    const auto = (location.state as { autoStartDesign?: boolean } | null)?.autoStartDesign;
    if (!auto) return;
    if (req.skip_analysis && req.status === 'draft'
        && !req.design_job_id && !req.design_docs) {
      autoStartRef.current = true;
      transition('designing', '生成技术方案').then(() => runArchitectDesign());
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [req]);

  // Reconnect to an in-flight design job when (re)entering the page — e.g.
  // after a refresh. The requirement carries design_job_id (server truth); if
  // the job is still running we resume its stream, otherwise (server restarted,
  // job evicted from the in-memory ring buffer) we drop into the idle state so
  // the start button shows again.
  useEffect(() => {
    if (!id || !req?.design_job_id) return;
    const jobId = req.design_job_id;
    authedFetch(`${API_BASE}/api/wizard/jobs/${jobId}`)
      .then(r => r.json())
      .then(json => {
        if (!json.success) { setDesigning(false); refresh(); return; }
        const { status, exit_code, log } = json.data as { status: string; exit_code: number; log: { type: string; content: string }[] };
        if (log && log.length > 0) setDesignLines(coalesceLogLines(log));
        if (status === 'running') streamDesignJob(jobId);
        else { setDesigning(false); setDesignError(status === 'error' || exit_code !== 0); refresh(); }
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
    setEditSkipAnalysis(req.skip_analysis);
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
        skip_analysis: editSkipAnalysis,
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
  // streamJob subscribes to a wizard job's SSE stream and appends events to
  // codingLines (the shared coding-panel). keepDone=true is used by 追加调整
  // rounds: on job_done it leaves the requirement status untouched (stays
  // developing/done) and only refreshes, instead of flipping to developing and
  // writing the localStorage "done" marker like the first coding pass.
  const streamJob = useCallback((jobId: string, opts?: { keepDone?: boolean }) => {
    if (esRef.current) esRef.current.close();
    setCoding(true);

    esRef.current = createEventStream(
      `/api/wizard/jobs/${jobId}/stream`,
      (evt) => {
        if (evt.type === 'job_done') {
          esRef.current?.close();
          esRef.current = null;
          setCoding(false);
          const ok = evt.status === 'done' || evt.exit_code === 0;
          if (opts?.keepDone) {
            // 追加调整: preserve current status, just refresh on success.
            if (ok) refresh();
          } else if (id && ok) {
            requirementsApi.updateStatus(id, 'developing').then(() => refresh());
            localStorage.setItem(`coding_job_${id}`, `done:${jobId}`);
          } else {
            localStorage.removeItem(`coding_job_${id}`);
          }
          return;
        }
        // Coalesce consecutive "模型思考中… (N tokens)" phase lines into one
        // updatable row instead of stacking one per heartbeat.
        setCodingLines(prev => appendLogLine(prev, { type: evt.type, content: evt.content ?? '' }));
      },
      () => {
        esRef.current = null;
        setCoding(false);
      },
    );
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

      const res = await authedFetch(`${API_BASE}/api/wizard/start-coding`, {
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
    authedFetch(`${API_BASE}/api/fs/git-branches?path=${encodeURIComponent(project.local_path)}`)
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

  // ── 追加调整: resume the prior coding session, output appends to codingLines
  // doAdjustCoding posts the follow-up message to adjust-coding (which resumes
  // coding_session_id with ONLY the user's message as -p) and streams the job
  // into the SAME codingLines panel as the first coding pass — no separate
  // output area, so the adjustment reads as a continuation of the dev log.
  const doAdjustCoding = async () => {
    if (!req || !id) return;
    const msg = adjustInput.trim();
    if (!msg) return;
    // Reuse coding state + coding-panel; do NOT clear codingLines (continuity).
    setCoding(true);
    try {
      const res = await authedFetch(`${API_BASE}/api/wizard/adjust-coding`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          requirement_id: id,
          message: msg,
        }),
      });
      const json = await res.json();
      const jobId = json.data?.job_id;
      if (!jobId) throw new Error(json.error?.message || '未获取到任务 ID');
      setAdjustInput('');
      streamJob(jobId, { keepDone: true });
    } catch (err: any) {
      setCodingLines(prev => [...prev, { type: 'error', content: '❌ ' + err.message }]);
      setCoding(false);
    }
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

  // applyMergeSignals derives conflictFiles / prLink from a full set of merge
  // log lines (a "conflict" line drives the conflict panel, a "pr_link" line
  // surfaces the create-PR link). Shared by the live job_done handler and the
  // restore-on-refresh effect below.
  const applyMergeSignals = useCallback((lines: { type: string; content: string }[]) => {
    const conflict = lines.find(l => l.type === 'conflict');
    if (conflict) {
      try {
        // content looks like "...[\"a\",\"b\"]"; pull out the JSON array.
        const m = conflict.content.match(/\[[\s\S]*\]/);
        setConflictFiles(m ? JSON.parse(m[0]) : []);
      } catch { setConflictFiles([]); }
    }
    const link = lines.find(l => l.type === 'pr_link');
    if (link) setPrLink(link.content);
  }, []);

  // streamMergeJob subscribes to a merge/push/resolve job. It collects log lines
  // into mergeLines and, on job_done, inspects the accumulated lines via
  // applyMergeSignals. The active job id is persisted to localStorage (same
  // pattern as the coding job) so a page refresh can reload the finished log
  // from the durable job_logs store.
  const streamMergeJob = useCallback((jobId: string) => {
    if (mergeEsRef.current) mergeEsRef.current.close();
    setMerging(true);
    setConflictFiles(null);
    if (id) localStorage.setItem(`merge_job_${id}`, jobId);

    let acc: { type: string; content: string }[] = [];
    mergeEsRef.current = createEventStream(
      `/api/wizard/jobs/${jobId}/stream`,
      (evt) => {
        if (evt.type === 'job_done') {
          mergeEsRef.current?.close();
          mergeEsRef.current = null;
          setMerging(false);
          const exitOk = evt.status === 'done' || evt.exit_code === 0;
          if (id) localStorage.setItem(`merge_job_${id}`, `done:${jobId}`);
          const conflict = acc.find(l => l.type === 'conflict');
          applyMergeSignals(acc);
          if (exitOk && !conflict) refreshMergeState();
          return;
        }
        const line = { type: evt.type, content: evt.content ?? '' };
        // Coalesce consecutive "模型思考中… (N tokens)" phase lines into one
        // updatable row instead of stacking one per heartbeat.
        acc = appendLogLine(acc, line);
        setMergeLines(prev => appendLogLine(prev, line));
      },
      () => {
        mergeEsRef.current = null;
        setMerging(false);
      },
    );
  }, [id, applyMergeSignals, refreshMergeState]);

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

  // cleanWorktree drops the requirement's isolated dev worktree + branch so
  // finished/abandoned parallel dev dirs don't accumulate on disk. A dirty
  // worktree is refused unless the user confirms a force cleanup.
  const cleanWorktree = async () => {
    if (!id || !mergeState?.worktree_path) return;
    if (!window.confirm('将删除该需求的隔离 worktree 目录与开发分支，确认清理？')) return;
    const run = async (force: boolean) => {
      setBusy('清理');
      try {
        await mergeApi.cleanup(id, { force });
        await refreshMergeState();
        await refresh();
      } finally {
        setBusy('');
      }
    };
    try {
      await run(false);
    } catch (err: any) {
      const msg = err?.message || '';
      if (msg.includes('WORKTREE_DIRTY')) {
        if (window.confirm('worktree 存在未提交改动，是否强制清理（将丢失这些改动）？')) {
          try {
            await run(true);
          } catch (e: any) {
            setMergeLines([{ type: 'error', content: '❌ ' + e.message }]);
          }
        }
      } else {
        setMergeLines([{ type: 'error', content: '❌ ' + msg }]);
      }
    }
  };

  // Restore the last merge / push / resolve job log when returning to this page.
  // The job id is persisted to localStorage (see streamMergeJob); GetJob replays
  // the durable job_logs snapshot, so the finished push/PR log + pr_link survive
  // a refresh. Same pattern as the coding job restore below.
  useEffect(() => {
    if (!id) return;
    const saved = localStorage.getItem(`merge_job_${id}`);
    if (!saved) return;

    const savedJobId = saved.startsWith('done:') ? saved.slice(5) : saved;

    authedFetch(`${API_BASE}/api/wizard/jobs/${savedJobId}`)
      .then(r => r.json())
      .then(json => {
        if (!json.success) {
          localStorage.removeItem(`merge_job_${id}`);
          return;
        }
        const { status, log } = json.data as { status: string; log: { type: string; content: string }[] };
        if (!log || log.length === 0) return;
        setMergeLines(coalesceLogLines(log));
        applyMergeSignals(log);
        if (status === 'running') streamMergeJob(savedJobId);
      })
      .catch(() => {});
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id]);

  // Restore active coding job when returning to this page
  useEffect(() => {
    if (!id) return;
    const saved = localStorage.getItem(`coding_job_${id}`);
    if (!saved) return;

    const isDone = saved.startsWith('done:');
    const savedJobId = isDone ? saved.slice(5) : saved;

    authedFetch(`${API_BASE}/api/wizard/jobs/${savedJobId}`)
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
        setCodingLines(coalesceLogLines(log));
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
  // Keep the panel open while the job runs OR when the last run errored, so
  // the error line stays visible instead of collapsing behind the toggle.
  const designPanelOpen = designProcessActive || designError || showDesignProcess;
  const showDesignToggle = designLines.length > 0 && !designProcessActive && !designError;

  const STEPS = [
    { key: 'analyst', label: '需求分析', icon: '🔍', doneStatus: 'designing', modelKey: 'analyst_model' as const },
    { key: 'architect', label: '方案设计', icon: '📐', doneStatus: 'designed', modelKey: 'architect_model' as const },
    { key: 'developer', label: '开发实现', icon: '🚀', doneStatus: 'done', modelKey: 'developer_model' as const },
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
                {mergeState.behind > 0 && (
                  <p className="merge-warn">⚠️ 落后主分支 {mergeState.behind} 个提交，将先合并主分支再提交 PR。</p>
                )}
                <div className="merge-hint">
                  <span>执行流程</span>
                  <span>合并主分支 → 解决冲突 → 生成 PR 摘要 → 推送并发起 PR</span>
                </div>
                {mergeState.mid_merge && (
                  <p className="merge-warn">⚠️ 当前存在未完成的合并，请先解决冲突或中止合并。</p>
                )}
                <div className="modal-field">
                  <label>提交信息</label>
                  <input className="input" value={mergeCommitMsg} onChange={e => setMergeCommitMsg(e.target.value)} />
                </div>
                {!mergeState.has_remote && (
                  <p className="merge-warn">该项目未配置 origin 远程仓库，无法推送。</p>
                )}
                <div className="modal-actions">
                  <button className="btn btn-primary" onClick={confirmMerge} disabled={!!busy || !mergeState.has_remote}>🌐 推送并发起 PR</button>
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
            {/* skip_analysis toggle — only meaningful before architect-design runs */}
            {req && (req.status === 'draft' || req.status === 'analyzing') && (
              <div className="modal-field">
                <label style={{ display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer', userSelect: 'none', fontWeight: 'normal' }}>
                  <input type="checkbox" checked={editSkipAnalysis}
                    onChange={e => setEditSkipAnalysis(e.target.checked)} style={{ width: 'auto' }} />
                  跳过需求分析，直接进入方案设计
                </label>
                <small style={{ display: 'block', marginTop: 4, color: 'var(--text-secondary, #64748B)' }}>
                  勾选后在详情页主操作变为「生成技术方案」；取消勾选则恢复「开始需求分析」入口。
                </small>
              </div>
            )}
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
        <div className="detail-desc">
          <div className="analysis-summary">
            <ReactMarkdown remarkPlugins={[remarkGfm]}>{req.description}</ReactMarkdown>
          </div>
        </div>
      )}

      {/* Token usage — per-step breakdown + total for this requirement.
          input = input_tokens + cache_creation + cache_read (billed input). */}
      <div className="detail-section usage-section">
        <div className="section-header" style={{ marginBottom: 10 }}>
          <span style={{ fontWeight: 600, fontSize: 14 }}>Token 消耗</span>
          {usageLoading && <span style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>刷新中…</span>}
        </div>
        {usage && usage.by_step.length > 0 ? (
          <>
            <table className="pr-table" style={{ marginBottom: 8 }}>
              <thead>
                <tr>
                  <th>步骤</th>
                  <th style={{ width: 110 }}>输入</th>
                  <th style={{ width: 110 }}>输出</th>
                  <th style={{ width: 110 }}>缓存读</th>
                  <th style={{ width: 110 }}>缓存建</th>
                  <th style={{ width: 60 }}>次数</th>
                </tr>
              </thead>
              <tbody>
                {usage.by_step.map(s => (
                  <tr key={s.step}>
                    <td>{s.label || stepLabels[s.step] || s.step}</td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{usageTotalInput(s).toLocaleString()}</td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{s.output_tokens.toLocaleString()}</td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{s.cache_read_tokens.toLocaleString()}</td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{s.cache_creation_tokens.toLocaleString()}</td>
                    <td style={{ fontSize: 12 }}>{s.count}</td>
                  </tr>
                ))}
                <tr style={{ borderTop: '2px solid var(--color-border)' }}>
                  <td style={{ fontWeight: 600 }}>合计</td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, fontWeight: 600 }}>{usageTotalInput(usage.total).toLocaleString()}</td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, fontWeight: 600 }}>{usage.total.output_tokens.toLocaleString()}</td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{usage.total.cache_read_tokens.toLocaleString()}</td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{usage.total.cache_creation_tokens.toLocaleString()}</td>
                  <td></td>
                </tr>
              </tbody>
            </table>
            <small style={{ color: 'var(--color-text-muted)', fontSize: 12 }}>
              输入 = 直接输入 + 缓存读 + 缓存建（均按计费输入计入）。仅统计已完成的完整调用。
            </small>
          </>
        ) : (
          <div style={{ color: 'var(--color-text-muted)', fontSize: 13 }}>
            {usageLoading ? '加载中…' : '暂无 Token 消耗记录。完成一次分析 / 方案 / 编码后将在此展示。'}
          </div>
        )}
      </div>

      {/* Stage stepper */}
      <div className="stage-stepper">
        {STEPS.map((s, i) => {
          const isDone = stageIndex > i || (stage === 'done');
          const isActive = stageIndex === i;
          const stageModel = req[s.modelKey];
          return (
            <div key={s.key} className={`stage-step${isActive ? ' active' : ''}${isDone ? ' done' : ''}`}>
              <span className="stage-num">{isDone ? '✅' : s.icon}</span>
              <span className="stage-label">{s.label}</span>
              {stageModel && (
                <span className="stage-model-tag" title={`${s.label}使用的执行模型`}>
                  🤖 {stageModel}
                </span>
              )}
              {i < STEPS.length - 1 && <span className="stage-sep">→</span>}
            </div>
          );
        })}
      </div>

      {/* Claude session ids — per-stage, for local resume/inspection. */}
      {(() => {
        const rows = [
          { stage: '需求分析', sid: req.analysis_session_id },
          { stage: '方案设计', sid: req.design_session_id },
          { stage: '开发实现', sid: req.coding_session_id },
        ].filter(r => r.sid);
        if (rows.length === 0) return null;
        return (
          <details className="session-panel">
            <summary>
              <span className="session-caret">▶</span>
              🔧 Claude 会话（{rows.length}）
            </summary>
            <div className="session-body">
              <p className="session-hint">
                点击会话 ID 或「复制」按钮即可复制完整命令 <code>cd "&lt;项目路径&gt;" &amp;&amp; claude --resume "&lt;session_id&gt;"</code>，粘贴到终端即可在该项目目录中恢复对应阶段的会话。
              </p>
              {rows.map(r => (
                <div className="session-row" key={r.stage}>
                  <span className="session-stage">{r.stage}</span>
                  <code
                    className="session-id"
                    title={r.sid}
                    onClick={() => copySessionId(r.sid)}
                  >
                    {r.sid}
                  </code>
                  <button className="btn btn-sm session-copy" onClick={() => copySessionId(r.sid)}>
                    📋 复制
                  </button>
                </div>
              ))}
            </div>
          </details>
        );
      })()}

      {/* ── Analyst stage ── */}
      {/* While analyzing, DeepRefineChat is itself the section (own card + header),
          so we render it standalone — no outer "需求分析" card around it, which
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
          <div className="section-header"><h3>🔍 需求分析</h3></div>
          <div className="tab-empty">
            {req.skip_analysis ? (
              <>
                <p>需求已创建（已跳过需求分析）。可直接进入方案设计，或先进行需求分析完善需求。</p>
                <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                  {/* Primary: go straight to architect-design. status stays draft;
                      the backend ArchitectDesign handler tolerates the missing
                      analyst session when skip_analysis is set, and UpdateDesign
                      then moves status to designing. We transition locally first
                      so the UI flips to the architect stage immediately. */}
                  <button className="btn btn-primary"
                    onClick={() => transition('designing', '生成技术方案').then(() => runArchitectDesign())}
                    disabled={!!busy}>
                    {busy === '生成技术方案' ? '⏳ ...' : '📐 生成技术方案'}
                  </button>
                  <button className="btn btn-sm"
                    onClick={() => transition('analyzing', '开始分析')} disabled={!!busy}
                    title="先进行需求分析，完善需求后再生成方案">
                    或先进行需求分析 →
                  </button>
                </div>
              </>
            ) : (
              <>
                <p>需求已创建。结合项目情况完善需求。</p>
                <button className="btn btn-primary" onClick={() => transition('analyzing', '开始分析')} disabled={!!busy}>
                  {busy === '开始分析' ? '⏳ ...' : '🤖 开始需求分析'}
                </button>
              </>
            )}
          </div>
        </div>
      )}

      {/* ── Architect stage ── */}
      {(stage === 'architect' || req.status === 'designed' || stage === 'developer' || stage === 'done') && (
        <div className="detail-section design-section">
          {/* Compact toolbar: the architect role is already shown in the
              stepper, so this section leads with a content-oriented caption
              and parks the stream toggle + regenerate action together. */}
          {(hasDesign || showDesignToggle) && (
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
              {hasDesign && (
                <button
                  className="btn btn-sm"
                  onClick={handleExportPdf}
                  disabled={exporting}
                  style={{ marginLeft: 'auto' }}
                  title="将技术方案导出为 PDF"
                >
                  {exporting ? '⏳ 导出中...' : '📄 导出 PDF'}
                </button>
              )}
            </div>
          )}

          {designPanelOpen && (
            <div className="coding-panel" ref={designRef} style={{ marginBottom: 16 }}>
              <CodingLines lines={designLines} />
              {designProcessActive && <div className="coding-line coding-line-tool_call">⏳ Claude 正在 plan 模式下制定技术方案...</div>}
            </div>
          )}

          {req.status === 'designing' && !hasDesign && !designing && !req.design_job_id && (
            <div className="tab-empty">
              <p>需求分析已完成。方案设计阶段将在 <strong>plan 模式</strong>下探索项目代码，制定具体可执行的技术实现方案（Markdown）。</p>
              <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                <button className="btn btn-primary" onClick={() => runArchitectDesign()} disabled={!!busy || designing}>
                  {busy === '生成技术方案' ? '⏳ ...' : '📐 开始制定技术方案'}
                </button>
                {/* Roll back to the analyst stage. The backend allows
                    designing → analyzing; this is the recovery path when the
                    user advanced with a failed/incomplete analysis.
                    Hidden for skip-analysis requirements since there is no
                    prior analysis session to return to. */}
                {!req.skip_analysis && (
                  <button
                    className="btn btn-sm"
                    onClick={() => transition('analyzing', '返回重新分析')}
                    disabled={!!busy}
                    title="退回到需求分析阶段继续完善对话"
                  >
                    ↩ 返回重新分析
                  </button>
                )}
              </div>
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
                  {busy === '方案完成' ? '⏳ ...' : '📐 方案完成'}
                </button>
              </div>
              <DocRefineChat
                reqId={req.id}
                projectPath={project?.local_path || ''}
                docType="design"
                currentDoc={req.design_docs}
                applyJobId={req.apply_job_id}
                onTurnDone={refresh}
              />
            </>
          )}
        </div>
      )}

      {/* ── Developer stage ── */}
      {(stage === 'developer' || stage === 'done') && hasDesign && (
        <div className="detail-section">
          <div className="section-header"><h3>🚀 开发实现</h3></div>

          {req.status === 'designed' && codingLines.length === 0 && !coding && (
            <div className="tab-empty">
              <p>方案已完成。将根据技术方案进行开发实现。</p>
              <p style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>项目路径：<code>{project?.local_path}</code></p>
              <p style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>
                将在独立 git worktree 中隔离开发（<code>{project?.local_path}.worktrees/{req.id}</code>），多需求并行互不干扰。
              </p>
              <button className="btn btn-primary" onClick={() => openBranchModal()}>🚀 开始开发</button>
            </div>
          )}

          {(codingLines.length > 0 || coding) && (
            <div className="coding-panel" ref={codingRef}>
              <CodingLines lines={codingLines} />
              {coding && <div className="coding-line coding-line-tool_call">⏳ Claude 正在工作...</div>}
            </div>
          )}

          {/* ── 追加调整 ── 续接 coding session（--resume），仅携带本指令；
              输出追加到上方 coding-panel，与首轮开发连贯。developing/done 均可。 */}
          {req.coding_session_id && (req.status === 'developing' || req.status === 'done') && !coding && (
            <div className="adjust-composer">
              <div className="adjust-composer-header">
                <span className="ac-title">🔧 追加调整</span>
                <span className="ac-tag">续接原开发会话 · 仅携带本指令</span>
              </div>
              <textarea
                className="ac-textarea"
                rows={3}
                value={adjustInput}
                onChange={e => setAdjustInput(e.target.value)}
                placeholder="描述需要追加修改的内容… Claude 将 --resume 原开发会话直接执行修改，不重读项目上下文"
                onKeyDown={e => {
                  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); doAdjustCoding(); }
                }}
              />
              <div className="adjust-composer-footer">
                <span className="ac-hint">Enter 发送 · Shift+Enter 换行</span>
                <button className="btn btn-primary" onClick={doAdjustCoding} disabled={!adjustInput.trim()}>
                  🚀 追加调整
                </button>
              </div>
            </div>
          )}

          {req.status === 'developing' && !coding && (
            <>
              {/* After a backend restart the in-memory job log is gone, but the
                  developing status is persisted in the DB — still allow the user
                  to mark done or re-run without a live coding log. */}
              {codingLines.length === 0 && (
                <p style={{ fontSize: 12, color: 'var(--color-text-muted)', marginTop: 8 }}>
                  开发任务已完成（日志因服务重启已清空）。确认代码无误后可标记开发完成，或重新开发（基于技术方案 fork 新会话）。
                </p>
              )}
              <div style={{ display: 'flex', gap: 8, marginTop: 12, flexWrap: 'wrap' }}>
                <button className="btn btn-primary" onClick={() => transition('done', '开发完成')} disabled={!!busy}>
                  {busy === '开发完成' ? '⏳ ...' : '✅ 开发完成'}
                </button>
                <button className="btn" title="从技术方案重新 fork 新会话开始开发，不携带上次开发历史" onClick={() => openBranchModal()}>🔄 重新开发</button>
              </div>

              {/* ── Merge / PR step ── */}
              <div className="merge-section">
                <div className="merge-actions">
                  <button className="btn" onClick={() => openMergeModal('local')} disabled={merging}>🔀 本地合入</button>
                  <button className="btn" onClick={() => openMergeModal('push')} disabled={merging}>🌐 推送并发起 PR</button>
                </div>

                {mergeState?.worktree_path && (
                  <div className="merge-hint" style={{ marginTop: 8, alignItems: 'center' }}>
                    <span>隔离开发目录</span>
                    <code style={{ fontSize: 12 }}>{mergeState.worktree_path}</code>
                    <button className="btn btn-sm" onClick={cleanWorktree} disabled={merging || !!busy}>🧹 清理开发环境</button>
                  </div>
                )}

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
                    <CodingLines lines={mergeLines} />
                    {merging && <div className="coding-line coding-line-tool_call">⏳ 执行中...</div>}
                  </div>
                )}

                {prLink && !merging && (
                  <a className="btn btn-primary pr-link-btn" href={prLink} target="_blank" rel="noreferrer">
                    🌐 创建 PR
                  </a>
                )}
              </div>
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
              {mergeState?.worktree_path && (
                <div className="merge-hint" style={{ marginTop: 8, alignItems: 'center' }}>
                  <span>隔离开发目录</span>
                  <code style={{ fontSize: 12 }}>{mergeState.worktree_path}</code>
                  <button className="btn btn-sm" onClick={cleanWorktree} disabled={merging || !!busy}>🧹 清理开发环境</button>
                </div>
              )}
              <div className="merge-actions" style={{ marginTop: 8 }}>
                <button className="btn btn-primary" onClick={handleArchive} disabled={!!busy}>
                  {busy === '归档' ? '⏳ ...' : '📦 归档到知识库'}
                </button>
              </div>
            </div>
          )}

          {req.status === 'archived' && (
            <div className="merge-section">
              <div className="tab-empty">
                <p>📦 已归档至项目知识库（最终需求 + 技术方案）。</p>
              </div>
              <div className="merge-actions" style={{ marginTop: 8 }}>
                <button className="btn" onClick={handleUnarchive} disabled={!!busy}>
                  {busy === '取消归档' ? '⏳ ...' : '↩ 取消归档'}
                </button>
              </div>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
