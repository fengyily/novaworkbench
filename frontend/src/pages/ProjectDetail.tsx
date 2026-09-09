import { useState, useEffect, useRef, useCallback, useMemo } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';

import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import {
  projectsApi, runnerApi, reviewApi, platformApi, requirementsApi, knowledgeApi,
  usageApi, usageTotalInput, fmtCost, wizardApi,
  type Project, type RunStatus, type PR, type PRListResponse, type PlatformToken,
  type Requirement, type KnowledgeItem, type ReqUsage, type ProjectUsage, statusLabelKeys,
  kindLabelKeys, kindOf,
} from '../api/client';
import { tLabel } from '../i18n/label';
import { fmtDate, fmtDateTime } from '../utils/intl';
import { CreateRequirementForm } from '../components/CreateRequirementForm/CreateRequirementForm';
import ProjectWeeklyReport from './ProjectWeeklyReport';
import { IconPlug, IconRobot } from '../components/icons';
import { stripMarkdownPreview } from '../utils/preview';
import { createEventStream, type EventStream } from '../api/stream';
import './RequirementDetail.css';
import './ProjectDetail.css';
import './KnowledgePage.css';

type Tab = 'overview' | 'knowledge' | 'run' | 'requirements' | 'review' | 'weekly' | 'usage';

const priorityDots: Record<string, string> = {
  high: '🔴', medium: '🟡', low: '🟢',
};

// Run-status badge labels — keys are resolved at render via tLabel so the
// chip text follows the active language.
const runStatusLabelKeys: Record<string, string> = {
  stopped: 'projects.detail.runStatusStopped',
  running: 'projects.detail.runStatusRunning',
  done: 'projects.detail.runStatusDone',
  error: 'projects.detail.runStatusError',
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
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();

  const [project, setProject] = useState<Project | null>(null);
  const [loading, setLoading] = useState(true);
  const [tab, setTab] = useState<Tab>('requirements');

  // Run tab
  const [runStatus, setRunStatus] = useState<RunStatus | null>(null);
  const [logLines, setLogLines] = useState<{ type: string; content: string }[]>([]);
  const [starting, setStarting] = useState(false);
  const [stopping, setStopping] = useState(false);
  const pollTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const pollCountRef = useRef(0);
  const logPanelRef = useRef<HTMLDivElement>(null);
  // Anchor for the inline "new requirement" composer — used by the
  // "+ New requirement" button to scroll the form into view when the list is
  // long enough that the form would otherwise open below the fold.
  const createReqFormRef = useRef<HTMLDivElement>(null);

  // Overview: platform config
  const [tokens, setTokens] = useState<PlatformToken[]>([]);
  const [platformForm, setPlatformForm] = useState({ platform_type: '', platform_token_id: '' });
  const [platformSaving, setPlatformSaving] = useState(false);
  const [platformSaved, setPlatformSaved] = useState(false);

  // Overview: basic info (name / remote_url / project_type / local_path)
  const [basicEditing, setBasicEditing] = useState(false);
  const [basicDraft, setBasicDraft] = useState({
    name: '', remote_url: '', project_type: '', local_path: '',
  });
  const [basicSaving, setBasicSaving] = useState(false);
  const [basicMsg, setBasicMsg] = useState<{ ok: boolean; text: string } | null>(null);

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
  // Requirements tab pagination — first page by default.
  const [reqPage, setReqPage] = useState(1);

  // Requirement ids currently running a wizard job (across the whole
  // backend process). Populated by polling GET /api/wizard/active-jobs
  // every 5s. The renderRequirementRows helper checks this set per row
  // and appends a small amber breathing dot next to the status badge
  // whenever the requirement has an in-flight job — covers coding /
  // design / apply / analyst without requiring the backend to add a
  // `coding_job_id` column to the requirements table. List page only:
  // detail page has its own richer aggregation in RequirementDetail.
  const [activeReqIds, setActiveReqIds] = useState<Set<string>>(new Set());

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
  // can never land past the new last page.
  useEffect(() => { setReqPage(1); }, [reqs]);

  // 5s poll of /api/wizard/active-jobs. Drives the small amber breathing
  // dot rendered next to each requirement's status badge in
  // renderRequirementRows. Independent of `tab` so the dot stays accurate
  // even when the user has the requirements tab collapsed on overview
  // (re-entering the tab shows up-to-date state without a refresh). The
  // `cancelled` flag guards against a late tick leaking into a different
  // project (we'd otherwise see the previous project's requirements get
  // a stale dot set after navigation).
  useEffect(() => {
    if (!id) return;
    let cancelled = false;
    const tick = async () => {
      try {
        const { jobs } = await wizardApi.listActiveJobs();
        if (cancelled) return;
        setActiveReqIds(new Set(jobs.map((j) => j.requirement_id).filter(Boolean)));
      } catch {
        /* transient network blip — keep previous set until the next tick */
      }
    };
    tick();
    const handle = setInterval(tick, 5000);
    return () => { cancelled = true; clearInterval(handle); };
  }, [id]);

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
      setLogLines(prev => [...prev, { type: 'error', content: t('projects.detail.runStopFailPrefix') + (err instanceof Error ? err.message : String(err)) }]);
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

  // ── Overview: basic info edit (name / remote_url / project_type / local_path) ─
  const handleSaveBasic = async () => {
    if (!id) return;
    setBasicSaving(true);
    setBasicMsg(null);
    try {
      const updated = await projectsApi.updateBasicInfo(id, {
        name: basicDraft.name.trim() ? basicDraft.name : undefined,
        remote_url: basicDraft.remote_url,
        project_type: basicDraft.project_type,
        local_path: basicDraft.local_path.trim() ? basicDraft.local_path : undefined,
      });
      setProject(updated);
      setBasicEditing(false);
      setBasicMsg({ ok: true, text: t('projects.detail.basicSaved') });
    } catch (e: unknown) {
      setBasicMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
    } finally {
      setBasicSaving(false);
    }
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
      setDescMsg({ ok: true, text: t('projects.detail.basicSaved') });
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
      setDescMsg({ ok: true, text: t('projects.detail.descRegenerated') });
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
          setReviewLines(prev => [...prev, { type: 'error', content: t('projects.detail.reviewSseDrop') }]);
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
  }, [id, reviewingPR, extraRequirements, t]);

  const handleSubmitComment = async () => {
    if (!id || !commentBody || lastReviewedPRRef.current === 0) return;
    setSubmitting(true);
    setSubmitMsg('');
    try {
      await reviewApi.submitComment(id, lastReviewedPRRef.current, commentBody);
      setSubmitMsg(t('projects.detail.reviewSubmitOk'));
    } catch (err: unknown) {
      setSubmitMsg(t('projects.detail.reviewErrPrefix') + (err instanceof Error ? err.message : String(err)));
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
  // The "Agent server" column mirrors the cross-project RequirementsList so
  // users can see at a glance which remote execution target the requirement
  // was developed on (or local if it ran locally). Stays consistent with the
  // `agent-server-tag` styling on the global list page.
  const renderRequirementRows = (items: Requirement[]) => items.map(req => (
    <tr
      key={req.id}
      style={{ cursor: 'pointer' }}
      onClick={() => navigate(`/requirements/${req.id}`)}
    >
      <td data-label={t('projects.detail.colId')} style={{ color: 'var(--color-text-muted)', fontFamily: 'var(--font-mono)', fontSize: 12 }}>{req.id}</td>
      <td data-label={t('projects.detail.colType')}><span className={`kind-badge kind-${kindOf(req)}`}>{tLabel(t, kindLabelKeys as Record<string, string>, kindOf(req))}</span></td>
      <td data-label={t('projects.detail.colTitle')} className="pr-title">{req.title}</td>
      <td data-label={t('projects.detail.colPriority')}><span style={{ fontSize: 12, whiteSpace: 'nowrap' }}>{priorityDots[req.priority] ?? '⚪'} {req.priority}</span></td>
      <td data-label={t('projects.detail.colStatus')}>
        <span className={`status-badge status-${req.status}`}>{tLabel(t, statusLabelKeys as Record<string, string>, req.status)}</span>
        {/* Breathing dot for any wizard job in flight on this requirement
            (analyst/design/apply/coding). Set is populated by the 5s poll
            of /api/wizard/active-jobs above. aria-label + title so screen
            readers + hover explain the indicator (the dot has no text). */}
        {activeReqIds.has(req.id) && (
          <span
            className="claude-pulse-dot is-work"
            aria-label={t('projects.detail.claudePulseAria')}
            title={t('projects.detail.claudePulseTitle')}
          />
        )}
      </td>
      {/* Agent server column: which remote execution target the requirement
          was developed on. Empty = local. The joined name comes from the
          backend's LEFT JOIN; a deleted server falls back to local so stale
          ids never render as raw hex. Identical to the column on the
          cross-project RequirementsList. */}
      <td data-label={t('projects.detail.colAgentServer')}>
        {req.agent_server_name ? (
          <span className="agent-server-tag" title={req.agent_server_name}>
            🖥️ {req.agent_server_name}
          </span>
        ) : (
          <span style={{ color: 'var(--color-text-muted)' }}>{t('projects.detail.agentServerLocal')}</span>
        )}
      </td>
      <td data-label={t('projects.detail.colTokens')} style={{ fontFamily: 'var(--font-mono)', fontSize: 12, whiteSpace: 'nowrap' }}>
        {(() => {
          const u = reqUsageMap.get(req.id);
          if (!u) return '—';
          return `${usageTotalInput(u).toLocaleString()} / ${u.output_tokens.toLocaleString()}`;
        })()}
      </td>
      <td data-label={t('projects.detail.colCost')} style={{ fontFamily: 'var(--font-mono)', fontSize: 12, whiteSpace: 'nowrap' }}>
        {(() => {
          const u = reqUsageMap.get(req.id);
          if (!u) return '—';
          return fmtCost(u.costs);
        })()}
      </td>
      <td data-label={t('projects.detail.colCreatedAt')} style={{ color: 'var(--color-text-muted)', fontSize: 12 }}>
        {req.created_at ? fmtDate(req.created_at) : '—'}
      </td>
      <td data-label={t('projects.detail.colUpdatedAt')} style={{ color: 'var(--color-text-muted)', fontSize: 12 }}>
        {req.updated_at ? fmtDate(req.updated_at) : '—'}
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
  // "View full" button on every card opens the full Markdown-rendered modal.
  const renderKnowledgeGroup = (labelKey: string, items: KnowledgeItem[]) => {
    if (!items.length) return null;
    const label = t(labelKey);
    return (
      <div className="detail-section" key={labelKey}>
        <h3 style={{ marginBottom: 12 }}>{label}{t('projects.detail.knowledgeCountSuffix', { n: items.length })}</h3>
        {items.map(k => (
          <div key={k.id} className="kb-card">
            <div className="kb-card-header">
              <span className={`kb-type-badge cat-${k.category}`}>{k.category || 'general'}</span>
              <span className="kb-card-title">{k.title}</span>
            </div>
            <div className="kb-card-content kb-clamp">{stripMarkdownPreview(k.content)}</div>
            <button className="kb-view-full" onClick={() => setKnowledgeModal(k)}>{t('projects.detail.knowledgeViewFull')}</button>
            <div className="kb-card-meta">
              <span className="kb-source">{t('projects.detail.knowledgeSource', { type: k.source_type })}</span>
              {k.source_ref && <span className="kb-ref">{k.source_ref}</span>}
              <span className="kb-date">{new Date(k.created_at).toLocaleDateString()}</span>
            </div>
          </div>
        ))}
      </div>
    );
  };

  if (loading) return <div className="detail-loading">{t('projects.detail.loading')}</div>;
  if (!project) return <div className="detail-loading">{t('projects.detail.notFound')}</div>;

  const selectedToken = tokens.find(t => t.id === platformForm.platform_token_id);

  return (
    <div className="project-detail">
      {/* Top header: breadcrumb-style back link, then the project's name +
          type tag, then its filesystem path as muted metadata. Keeping the
          back link small + unbordered stops it from competing visually with
          the title (the old bordered button felt heavier than the title
          underneath it). */}
      <div className="detail-header">
        <button className="back-link" onClick={() => navigate('/projects')}>
          <span className="back-arrow" aria-hidden="true">‹</span>
          <span>{t('projects.detail.backLink')}</span>
        </button>
      </div>

      <div className="detail-title-row">
        <h1 className="detail-title">{project.name}</h1>
        <span className="detail-type-tag">{project.project_type || 'Unknown'}</span>
        {project.remote_url && (
          <a
            className="detail-remote-link"
            href={project.remote_url}
            target="_blank"
            rel="noreferrer"
            title={project.remote_url}
          >
            {t('projects.detail.repoLink')}
          </a>
        )}
      </div>
      <p className="detail-path"><code>{project.local_path}</code></p>

      <div className="detail-tabs">
        <button className={`tab-btn${tab === 'overview' ? ' active' : ''}`} onClick={() => setTab('overview')}>{t('projects.detail.tabs.overview')}</button>
        <button className={`tab-btn${tab === 'knowledge' ? ' active' : ''}`} onClick={() => setTab('knowledge')}>{t('projects.detail.tabs.knowledge')}</button>
        <button className={`tab-btn${tab === 'run' ? ' active' : ''}`} onClick={() => setTab('run')}>
          {t('projects.detail.tabs.run')}{isRunning ? t('projects.detail.activeSuffix') : ''}
        </button>
        <button className={`tab-btn${tab === 'requirements' ? ' active' : ''}`} onClick={() => { setTab('requirements'); setReqPage(1); }}>
          {t('projects.detail.tabs.requirements')}
        </button>
        <button className={`tab-btn${tab === 'review' ? ' active' : ''}`} onClick={() => setTab('review')}>
          {t('projects.detail.tabs.review')}{reviewingPR ? t('projects.detail.activeSuffix') : ''}
        </button>
        <button className={`tab-btn${tab === 'usage' ? ' active' : ''}`} onClick={() => setTab('usage')}>
          {t('projects.detail.tabs.usage')}
        </button>
        <button className={`tab-btn${tab === 'weekly' ? ' active' : ''}`} onClick={() => setTab('weekly')}>
          {t('projects.detail.tabs.weekly')}
        </button>
      </div>

      {/* ── Overview ── */}
      {tab === 'overview' && (
        <div className="tab-content">
          <div className="detail-section">
            <div className="section-header" style={{ marginBottom: 12 }}>
              <span style={{ fontWeight: 600, fontSize: 14 }}>{t('projects.detail.basicTitle')}</span>
              {!basicEditing && (
                <button
                  className="btn btn-sm"
                  onClick={() => {
                    setBasicDraft({
                      name: project.name,
                      remote_url: project.remote_url ?? '',
                      project_type: project.project_type ?? '',
                      local_path: project.local_path,
                    });
                    setBasicEditing(true);
                    setBasicMsg(null);
                  }}
                >
                  {t('projects.detail.editBtn')}
                </button>
              )}
            </div>

            {basicEditing ? (
              <div style={{ display: 'grid', gap: 12 }}>
                <div className="form-group" style={{ margin: 0 }}>
                  <label style={{ fontSize: 12, fontWeight: 600, color: 'var(--color-text-secondary)', display: 'block', marginBottom: 4 }}>
                    {t('projects.detail.basicName')}
                  </label>
                  <input
                    className="form-input"
                    value={basicDraft.name}
                    onChange={e => setBasicDraft(d => ({ ...d, name: e.target.value }))}
                    placeholder={t('projects.detail.basicNamePlaceholder')}
                    autoFocus
                  />
                </div>
                <div className="form-group" style={{ margin: 0 }}>
                  <label style={{ fontSize: 12, fontWeight: 600, color: 'var(--color-text-secondary)', display: 'block', marginBottom: 4 }}>
                    {t('projects.detail.basicRemoteUrl')}
                  </label>
                  <input
                    className="form-input"
                    value={basicDraft.remote_url}
                    onChange={e => setBasicDraft(d => ({ ...d, remote_url: e.target.value }))}
                    placeholder={t('projects.detail.basicRemoteUrlPlaceholder')}
                  />
                </div>
                <div className="form-group" style={{ margin: 0 }}>
                  <label style={{ fontSize: 12, fontWeight: 600, color: 'var(--color-text-secondary)', display: 'block', marginBottom: 4 }}>
                    {t('projects.detail.basicType')}
                  </label>
                  <select
                    className="form-input"
                    value={basicDraft.project_type}
                    onChange={e => setBasicDraft(d => ({ ...d, project_type: e.target.value }))}
                  >
                    <option value="">{t('projects.detail.basicTypeAuto')}</option>
                    <option value="Go">Go</option>
                    <option value="Node.js">Node.js</option>
                    <option value="Python">Python</option>
                    <option value="Rust">Rust</option>
                    <option value="Java/Maven">Java/Maven</option>
                    <option value="Java/Gradle">Java/Gradle</option>
                    <option value="Unknown">Unknown</option>
                  </select>
                </div>
                <div className="form-group" style={{ margin: 0 }}>
                  <label style={{ fontSize: 12, fontWeight: 600, color: 'var(--color-text-secondary)', display: 'block', marginBottom: 4 }}>
                    {t('projects.detail.basicLocalPath')}
                  </label>
                  <input
                    className="form-input"
                    value={basicDraft.local_path}
                    onChange={e => setBasicDraft(d => ({ ...d, local_path: e.target.value }))}
                    placeholder={t('projects.detail.basicLocalPathPlaceholder')}
                  />
                </div>
                <div style={{ display: 'flex', gap: 8 }}>
                  <button className="btn btn-primary btn-sm" onClick={handleSaveBasic} disabled={basicSaving}>
                    {basicSaving ? t('projects.detail.basicSaving') : t('projects.detail.basicSave')}
                  </button>
                  <button
                    className="btn btn-sm"
                    onClick={() => { setBasicEditing(false); setBasicMsg(null); }}
                    disabled={basicSaving}
                  >
                    {t('projects.detail.basicCancel')}
                  </button>
                </div>
              </div>
            ) : (
              <>
                <div className="info-row"><span className="info-label">{t('projects.detail.basicInfoName')}</span><span>{project.name}</span></div>
                <div className="info-row">
                  <span className="info-label">{t('projects.detail.basicInfoPath')}</span>
                  <code className="info-code">{project.local_path}</code>
                </div>
                <div className="info-row"><span className="info-label">{t('projects.detail.basicInfoType')}</span><span>{project.project_type || 'Unknown'}</span></div>
                <div className="info-row">
                  <span className="info-label">{t('projects.detail.basicInfoStatus')}</span>
                  <span className={`status-badge status-${project.status}`}>{project.status}</span>
                </div>
                {project.remote_url && (
                  <div className="info-row">
                    <span className="info-label">{t('projects.detail.basicInfoRemote')}</span>
                    <code className="info-code">{project.remote_url}</code>
                  </div>
                )}
              </>
            )}

            {basicMsg && (
              <div style={{ marginTop: 8, fontSize: 12, color: basicMsg.ok ? 'var(--color-success)' : 'var(--color-error)' }}>
                {basicMsg.ok ? '✅ ' : '❌ '}{basicMsg.text}
              </div>
            )}
          </div>

          {/* Project description (AI-generated from CLAUDE.md, manual-edit lockable) */}
          <div className="detail-section" style={{ marginTop: 16 }}>
            <div className="section-header" style={{ marginBottom: 12 }}>
              <span style={{ fontWeight: 600, fontSize: 14 }}>{t('projects.detail.descTitle')}</span>
              <span style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>
                {project.description_manual ? t('projects.detail.descManualFlag') : t('projects.detail.descAiFlag')}
              </span>
            </div>

            {descEditing ? (
              <div>
                <textarea
                  className="form-input"
                  rows={3}
                  value={descDraft}
                  onChange={e => setDescDraft(e.target.value)}
                  placeholder={t('projects.detail.descPlaceholder')}
                  autoFocus
                />
                <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
                  <button className="btn btn-primary btn-sm" onClick={handleSaveDesc} disabled={descSaving}>
                    {descSaving ? t('projects.detail.basicSaving') : t('projects.detail.basicSave')}
                  </button>
                  <button
                    className="btn btn-sm"
                    onClick={() => { setDescEditing(false); setDescMsg(null); }}
                    disabled={descSaving}
                  >
                    {t('projects.detail.basicCancel')}
                  </button>
                </div>
              </div>
            ) : (
              <div>
                <div style={{ color: 'var(--color-text-secondary)', fontSize: 13, whiteSpace: 'pre-wrap' }}>
                  {project.description || t('projects.detail.descNoDesc')}
                </div>
                <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
                  <button
                    className="btn btn-sm"
                    onClick={() => { setDescDraft(project.description); setDescEditing(true); setDescMsg(null); }}
                  >
                    {t('projects.detail.editBtn')}
                  </button>
                  <button
                    className="btn btn-sm"
                    onClick={handleRegenerateDesc}
                    disabled={regenerating}
                    title={t('projects.detail.descRegenerateTitle')}
                  >
                    {regenerating ? t('projects.detail.descRegenerating') : t('projects.detail.descRegenerate')}
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
              <span style={{ fontWeight: 600, fontSize: 14 }}>{t('projects.detail.platformTitle')}</span>
              {project.platform_type && (
                <span style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>
                  {t('projects.detail.platformCurrent', { platform: platformLabels[project.platform_type] ?? project.platform_type })}
                  {project.platform_token_id ? '' : t('projects.detail.platformUnbound')}
                </span>
              )}
            </div>

            <div className="platform-form-row">
              <div className="form-group platform-form-field">
                <label>{t('projects.detail.platformField')}</label>
                <select
                  className="form-input"
                  value={platformForm.platform_type}
                  onChange={e => setPlatformForm(f => ({ ...f, platform_type: e.target.value, platform_token_id: '' }))}
                >
                  <option value="">{t('projects.detail.platformPlaceholder')}</option>
                  <option value="github">GitHub</option>
                  <option value="gitlab">GitLab</option>
                  <option value="gitea">Gitea</option>
                </select>
              </div>

              <div className="form-group platform-form-field platform-form-field-grow">
                <label>{t('projects.detail.platformToken')}</label>
                <select
                  className="form-input"
                  value={platformForm.platform_token_id}
                  onChange={e => setPlatformForm(f => ({ ...f, platform_token_id: e.target.value }))}
                  disabled={!platformForm.platform_type}
                >
                  <option value="">{t('projects.detail.platformTokenPlaceholder')}</option>
                  {tokens
                    .filter(t => !platformForm.platform_type || t.platform === platformForm.platform_type)
                    .map(t => (
                      <option key={t.id} value={t.id}>{t.name}</option>
                    ))}
                </select>
              </div>

              <button
                className="btn btn-primary platform-form-save"
                onClick={handleSavePlatform}
                disabled={platformSaving}
              >
                {platformSaving ? t('projects.detail.platformSaving') : platformSaved ? t('projects.detail.platformSaved') : t('projects.detail.platformSave')}
              </button>
            </div>

            {tokens.length === 0 && (
              <p style={{ margin: '8px 0 0', fontSize: 12, color: 'var(--color-text-muted)' }}>
                {t('projects.detail.platformNoToken')}
                <a href="/settings" style={{ color: 'var(--color-primary)', margin: '0 4px' }}>{t('projects.detail.platformSettingsLink')}</a>
                {t('projects.detail.platformAddTail')}
              </p>
            )}
            {selectedToken && (
              <p style={{ margin: '8px 0 0', fontSize: 12, color: 'var(--color-success)' }}>
                {t('projects.detail.platformBound', { name: selectedToken.name })}
                {selectedToken.base_url ? ` (${selectedToken.base_url})` : ''}
              </p>
            )}
          </div>

          {/* Recent requirements (height-adaptive) */}
          <div className="detail-section recent-reqs-section" style={{ marginTop: 16 }} ref={recentSectionRef}>
            <div className="recent-reqs-header">
              <span style={{ fontWeight: 600, fontSize: 14 }}>{t('projects.detail.recentTitle')}</span>
              <button className="recent-reqs-more" onClick={() => { setTab('requirements'); setReqPage(1); }}>{t('projects.detail.recentViewAll')}</button>
            </div>

            {reqsLoading && <div className="tab-empty">{t('projects.detail.recentLoading')}</div>}
            {!reqsLoading && overviewReqs.length === 0 && (
              <div className="tab-empty">
                {reqs.length === 0 ? (
                  <p>{t('projects.detail.recentEmpty')}<button className="recent-reqs-link" onClick={() => { setTab('requirements'); setShowCreateReq(true); }}>{t('projects.detail.recentCreateLink')}</button></p>
                ) : (
                  <p>{t('projects.detail.recentNoActive')}</p>
                )}
              </div>
            )}
            {!reqsLoading && overviewReqs.length > 0 && (
              <div className="pr-list" style={{ marginBottom: 0 }}>
                <table className="pr-table table-cards">
                  <thead>
                    <tr>
                      <th style={{ width: 110 }}>{t('projects.detail.colId')}</th>
                      <th style={{ width: 70 }}>{t('projects.detail.colType')}</th>
                      <th>{t('projects.detail.colTitle')}</th>
                      <th style={{ width: 90 }}>{t('projects.detail.colPriority')}</th>
                      <th style={{ width: 130 }}>{t('projects.detail.colStatus')}</th>
                      <th style={{ width: 140 }}>{t('projects.detail.colAgentServer')}</th>
                      <th style={{ width: 130 }}>{t('projects.detail.colTokens')}</th>
                      <th style={{ width: 110 }}>{t('projects.detail.colCost')}</th>
                      <th style={{ width: 110 }}>{t('projects.detail.colCreatedAt')}</th>
                      <th style={{ width: 110 }}>{t('projects.detail.colUpdatedAt')}</th>
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
            <div className="tab-empty"><p>{t('projects.detail.knowledgeLoading')}</p></div>
          ) : knowledge.length === 0 ? (
            <div className="tab-empty"><p>{t('projects.detail.knowledgeEmpty')}</p></div>
          ) : (
            <>
              {renderKnowledgeGroup('projects.detail.knowledgeGroupReq', knowledge.filter(k => k.source_type === 'requirement'))}
              {renderKnowledgeGroup('projects.detail.knowledgeGroupDoc', knowledge.filter(k => k.source_type === 'document'))}
              {renderKnowledgeGroup('projects.detail.knowledgeGroupCode', knowledge.filter(k => k.source_type === 'code'))}
              {renderKnowledgeGroup('projects.detail.knowledgeGroupOther', knowledge.filter(k => !['requirement', 'document', 'code'].includes(k.source_type)))}
            </>
          )}
        </div>
      )}

      {/* ── Run ── */}
      {tab === 'run' && (
        <div className="tab-content">
          <div className="run-control-bar">
            {runStatus?.compose_file
              ? <span className="compose-detect">{t('projects.detail.runDetected', { file: runStatus.compose_file })}</span>
              : <span className="compose-detect compose-missing">{t('projects.detail.runMissing')}</span>}
            <span className={`run-status-badge run-status-${runStatus?.status ?? 'stopped'}`}>
              {tLabel(t, runStatusLabelKeys, runStatus?.status ?? 'stopped')}
            </span>
            {!isRunning
              ? <button className="btn btn-primary" onClick={handleStart} disabled={starting || stopping}>{starting ? t('projects.detail.runStarting') : t('projects.detail.runStart')}</button>
              : <button className="btn btn-danger" onClick={handleStop} disabled={stopping}>{stopping ? t('projects.detail.runStopping') : t('projects.detail.runStop')}</button>}
          </div>
          {(logLines.length > 0 || isRunning) && (
            <div className="coding-panel" ref={logPanelRef}>
              {logLines.map((line, i) => (
                <div key={i} className={`coding-line coding-line-${line.type}`}>{line.content}</div>
              ))}
              {isRunning && <div className="coding-line coding-line-tool_call">{t('projects.detail.runRunningLine')}</div>}
            </div>
          )}
          {!isRunning && logLines.length === 0 && (
            <div className="tab-empty"><p>{t('projects.detail.runEmpty')}</p></div>
          )}
        </div>
      )}

      {/* ── Requirements ── */}
      {tab === 'requirements' && (
        <div className="tab-content">
          <div className="run-control-bar">
            <span className="compose-detect">{t('projects.detail.reqCount', { n: reqs.length })}</span>
            <button
              className={`btn btn-primary btn-sm${showCreateReq ? ' is-open' : ''}`}
              onClick={() => {
                setShowCreateReq(true);
                // Defer the scroll one frame so the form is mounted
                // before we try to measure it — otherwise
                // scrollIntoView runs against the still-empty anchor
                // and the page never moves.
                requestAnimationFrame(() => {
                  createReqFormRef.current?.scrollIntoView({ behavior: 'smooth', block: 'start' });
                  createReqFormRef.current?.querySelector('textarea')?.focus();
                });
              }}
              aria-expanded={showCreateReq}
              aria-controls="new-requirement-form"
            >
              {showCreateReq ? t('projects.detail.reqCollapseBtn') : t('projects.detail.reqNewBtn')}
            </button>
          </div>

          {/* The composer is mounted ABOVE the requirements list so the
              user sees it appear right under the button they just
              clicked. Previously it rendered at the bottom of the tab
              content — with paginated lists the form lived past the
              fold and the click felt like nothing happened. */}
          {showCreateReq && id && (
            <div ref={createReqFormRef} id="new-requirement-form" className="create-req-anchor">
              <CreateRequirementForm
                projectId={id}
                onClose={() => setShowCreateReq(false)}
                onCreated={async (created: Requirement) => {
                  setShowCreateReq(false);
                  // Skip analysis → auto-enter the design stage: navigate to
                  // the detail page and pass the autoStartDesign intent flag
                  // so RequirementDetail auto-triggers architect-design in a
                  // one-shot, replacing the manual "Generate design" click.
                  if (created.skip_design) {
                    // Skip design → code directly: navigate to the detail
                    // page and pass the autoStartCoding intent flag so
                    // RequirementDetail opens the branch-picker modal and
                    // jumps straight into coding.
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
            </div>
          )}

          {reqsLoading && <div className="tab-empty">{t('projects.detail.reqLoading')}</div>}
          {reqsError && <div className="tab-empty" style={{ color: 'var(--color-error)' }}>{reqsError}</div>}

          {!reqsLoading && !reqsError && reqs.length === 0 && (
            <div className="tab-empty"><p>{t('projects.detail.reqEmpty')}</p></div>
          )}

          {!reqsLoading && !reqsError && reqs.length > 0 && (
            <div className="pr-list">
              <table className="pr-table table-cards">
                <thead>
                  <tr>
                    <th style={{ width: 110 }}>{t('projects.detail.colId')}</th>
                    <th style={{ width: 70 }}>{t('projects.detail.colType')}</th>
                    <th>{t('projects.detail.colTitle')}</th>
                    <th style={{ width: 90 }}>{t('projects.detail.colPriority')}</th>
                    <th style={{ width: 130 }}>{t('projects.detail.colStatus')}</th>
                    <th style={{ width: 140 }}>{t('projects.detail.colAgentServer')}</th>
                    <th style={{ width: 130 }}>{t('projects.detail.colTokens')}</th>
                      <th style={{ width: 110 }}>{t('projects.detail.colCost')}</th>
                    <th style={{ width: 110 }}>{t('projects.detail.colCreatedAt')}</th>
                    <th style={{ width: 110 }}>{t('projects.detail.colUpdatedAt')}</th>
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
                {t('projects.detail.paginationInfo', { total: reqs.length, cur: curReqPage, totalPages: totalReqPages })}
              </span>
              <button className="btn btn-sm" disabled={curReqPage <= 1} onClick={() => setReqPage(curReqPage - 1)}>
                {t('projects.detail.paginationPrev')}
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
                {t('projects.detail.paginationNext')}
              </button>
            </div>
          )}

        </div>
      )}

      {/* ── Review ── */}
      {tab === 'review' && (
        <div className="tab-content">
          {/* Not configured */}
          {!prsLoading && prData && !prData.configured && (
            <div className="review-unconfigured">
              <div className="review-unconfigured-icon"><IconPlug size={28} /></div>
              <p>{t('projects.detail.reviewUnconfiguredTitle')}</p>
              <p>
                {t('projects.detail.reviewUnconfiguredHint')}
                <a href="/settings" style={{ color: 'var(--color-primary)', margin: '0 4px' }}>{t('projects.detail.platformSettingsLink')}</a>
                {t('projects.detail.reviewUnconfiguredHint2')}
              </p>
            </div>
          )}

          {prsLoading && <div className="tab-empty">{t('projects.detail.reviewLoading')}</div>}
          {prsError && <div className="tab-empty" style={{ color: 'var(--color-error)' }}>{prsError}</div>}

          {!prsLoading && prData && (prData.configured || prData.prs.length > 0) && (
            <>
              {prData.prs.length === 0 && (
                <div className="tab-empty"><p>{t('projects.detail.reviewNoPRs')}</p></div>
              )}
              {prData.prs.length > 0 && (
                <>
                  <div className="review-requirements-bar">
                    <label className="review-requirements-label">{t('projects.detail.reviewExtraLabel')}</label>
                    <input
                      className="form-input review-requirements-input"
                      placeholder={t('projects.detail.reviewExtraPlaceholder')}
                      value={extraRequirements}
                      onChange={e => setExtraRequirements(e.target.value)}
                      disabled={!!reviewingPR}
                    />
                  </div>
                  <div className="pr-list">
                  <table className="pr-table table-cards">
                    <thead>
                      <tr>
                        <th style={{ width: 52 }}>{t('projects.detail.reviewColNumber')}</th>
                        <th>{t('projects.detail.reviewColTitle')}</th>
                        <th style={{ width: 260 }}>{t('projects.detail.reviewColBranch')}</th>
                        <th style={{ width: 100 }}>{t('projects.detail.reviewColAuthor')}</th>
                        <th style={{ width: 90 }}>{t('projects.detail.reviewColUpdatedAt')}</th>
                        <th style={{ width: 120 }}>{t('projects.detail.reviewColActions')}</th>
                      </tr>
                    </thead>
                    <tbody>
                      {prData.prs.map(pr => (
                        <tr key={pr.number} className={reviewingPR?.number === pr.number ? 'pr-row-active' : ''}>
                          <td data-label="#" style={{ color: 'var(--color-text-muted)', fontFamily: 'var(--font-mono)', fontSize: 12 }}>#{pr.number}</td>
                          <td data-label={t('projects.detail.reviewColTitle')} className="pr-title">
                            <a href={pr.html_url} target="_blank" rel="noreferrer" style={{ color: 'var(--color-primary)' }}>
                              {pr.title}
                            </a>
                          </td>
                          <td data-label={t('projects.detail.reviewColBranch')}><code className="pr-branch">{pr.head_branch}</code> ← <code className="pr-branch">{pr.base_branch}</code></td>
                          <td data-label={t('projects.detail.reviewColAuthor')}>{pr.author}</td>
                          <td data-label={t('projects.detail.reviewColUpdatedAt')}>{pr.updated_at ? fmtDate(pr.updated_at) : '—'}</td>
                          <td data-label={t('projects.detail.reviewColActions')}>
                            <button
                              className="btn btn-primary btn-sm"
                              onClick={() => handleReviewWithTracking(pr)}
                              disabled={!!reviewingPR}
                            >
                              {reviewingPR?.number === pr.number ? t('projects.detail.reviewBtnActive') : t('projects.detail.reviewBtn')}
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
                  {t('projects.detail.reviewHeader')}
                  {reviewingPR ? `：#${reviewingPR.number} ${reviewingPR.title}` : ''}
                  {reviewDone ? t('projects.detail.reviewHeaderDone') : ''}
                </span>
                {reviewDone && (
                  <button className="btn btn-secondary btn-sm" onClick={() => {
                    setReviewLines([]); setReviewDone(false); setCommentBody(''); setSubmitMsg('');
                  }}>
                    {t('projects.detail.reviewDoneBtn')}
                  </button>
                )}
              </div>
              <div className="coding-panel" ref={reviewPanelRef} style={{ maxHeight: 360 }}>
                {reviewLines.map((line, i) => (
                  <div key={i} className={`coding-line coding-line-${line.type}`}>{line.content}</div>
                ))}
                {reviewingPR && !reviewDone && (
                  <div className="coding-line coding-line-tool_call">{t('projects.detail.reviewProgress')}</div>
                )}
              </div>
            </div>
          )}

          {/* Comment editor — shown after review completes */}
          {reviewDone && lastReviewedPRRef.current > 0 && (
            <div className="review-comment-wrap">
              <div className="review-panel-header">
                <span>{t('projects.detail.reviewCommentHeader', { n: lastReviewedPRRef.current })}</span>
              </div>
              <div className="review-model-line">
                <IconRobot size={13} /> {t('projects.detail.reviewModelLine', { model: reviewModel || t('projects.detail.reviewDefaultModel') })}
              </div>
              <textarea
                className="review-comment-editor"
                value={commentBody}
                onChange={e => setCommentBody(e.target.value)}
                rows={12}
                placeholder={t('projects.detail.reviewEditorPlaceholder')}
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
                  {submitting ? t('projects.detail.reviewSubmitBusy') : t('projects.detail.reviewSubmit')}
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
            <div className="tab-empty"><p>{t('projects.detail.usageLoading')}</p></div>
          ) : !projectUsage ? (
            <div className="tab-empty"><p>{t('projects.detail.usageEmpty')}</p></div>
          ) : (
            <>
              {/* Project total (excludes review rows) */}
              <div className="detail-section" style={{ marginBottom: 16 }}>
                <div className="section-header" style={{ marginBottom: 12 }}>
                  <span style={{ fontWeight: 600, fontSize: 14 }}>{t('projects.detail.usageTitle')}</span>
                  <span style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>{t('projects.detail.usageExcludeReview')}</span>
                </div>
                <div style={{ display: 'flex', gap: 24, flexWrap: 'wrap' }}>
                  <div>
                    <div style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>{t('projects.detail.usageInput')}</div>
                    <div style={{ fontFamily: 'var(--font-mono)', fontSize: 22, fontWeight: 600 }}>
                      {usageTotalInput(projectUsage.total).toLocaleString()}
                    </div>
                  </div>
                  <div>
                    <div style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>{t('projects.detail.usageOutput')}</div>
                    <div style={{ fontFamily: 'var(--font-mono)', fontSize: 22, fontWeight: 600, color: 'var(--color-primary)' }}>
                      {projectUsage.total.output_tokens.toLocaleString()}
                    </div>
                  </div>
                  <div>
                    <div style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>{t('projects.detail.usageCacheRead')}</div>
                    <div style={{ fontFamily: 'var(--font-mono)', fontSize: 16 }}>
                      {projectUsage.total.cache_read_tokens.toLocaleString()}
                    </div>
                  </div>
                  <div>
                    <div style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>{t('projects.detail.usageCacheCreation')}</div>
                    <div style={{ fontFamily: 'var(--font-mono)', fontSize: 16 }}>
                      {projectUsage.total.cache_creation_tokens.toLocaleString()}
                    </div>
                  </div>
                  <div>
                    <div style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>{t('projects.detail.usageCost')}</div>
                    <div style={{ fontFamily: 'var(--font-mono)', fontSize: 22, fontWeight: 600, color: '#10B981' }}>
                      {fmtCost(projectUsage.total.costs)}
                    </div>
                  </div>
                </div>
              </div>

              {/* Per-requirement breakdown */}
              <div className="detail-section" style={{ marginBottom: 16 }}>
                <div className="section-header" style={{ marginBottom: 12 }}>
                  <span style={{ fontWeight: 600, fontSize: 14 }}>{t('projects.detail.usageByReq')}</span>
                </div>
                {projectUsage.by_requirement.length === 0 ? (
                  <div className="tab-empty"><p>{t('projects.detail.usageByReqEmpty')}</p></div>
                ) : (
                  <div className="pr-list">
                    <table className="pr-table table-cards">
                      <thead>
                        <tr>
                          <th>{t('projects.detail.usageColReqId')}</th>
                          <th style={{ width: 120 }}>{t('projects.detail.usageInput')}</th>
                          <th style={{ width: 120 }}>{t('projects.detail.usageOutput')}</th>
                          <th style={{ width: 120 }}>{t('projects.detail.usageCacheRead')}</th>
                          <th style={{ width: 120 }}>{t('projects.detail.usageCacheCreation')}</th>
                          <th style={{ width: 140 }}>{t('projects.detail.usageCost')}</th>
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
                  <span style={{ fontWeight: 600, fontSize: 14 }}>{t('projects.detail.usageByModel')}</span>
                  <span style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>{t('projects.detail.usageByModelHint')}</span>
                </div>
                {projectUsage.by_model.length === 0 ? (
                  <div className="tab-empty"><p>{t('projects.detail.usageByModelEmpty')}</p></div>
                ) : (
                  <div className="pr-list">
                    <table className="pr-table table-cards">
                      <thead>
                        <tr>
                          <th>{t('projects.detail.usageByModel')}</th>
                          <th style={{ width: 120 }}>{t('projects.detail.usageInput')}</th>
                          <th style={{ width: 120 }}>{t('projects.detail.usageOutput')}</th>
                          <th style={{ width: 120 }}>{t('projects.detail.usageCacheRead')}</th>
                          <th style={{ width: 120 }}>{t('projects.detail.usageCacheCreation')}</th>
                          <th style={{ width: 150 }}>{t('projects.detail.usageCost')}</th>
                        </tr>
                      </thead>
                      <tbody>
                        {projectUsage.by_model.map(mu => (
                          <tr key={mu.model}>
                            <td><code className="pr-branch">{mu.model || t('projects.detail.usageByModelUnknown')}</code></td>
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
                  <span style={{ fontWeight: 600, fontSize: 14 }}>{t('projects.detail.usageByDay')}</span>
                </div>
                {projectUsage.by_day.length === 0 ? (
                  <div className="tab-empty"><p>{t('projects.detail.usageByDayEmpty')}</p></div>
                ) : (
                  <div className="pr-list">
                    <table className="pr-table table-cards">
                      <thead>
                        <tr>
                          <th>{t('projects.detail.usageByDay')}</th>
                          <th style={{ width: 120 }}>{t('projects.detail.usageInput')}</th>
                          <th style={{ width: 120 }}>{t('projects.detail.usageOutput')}</th>
                          <th style={{ width: 120 }}>{t('projects.detail.usageCacheRead')}</th>
                          <th style={{ width: 120 }}>{t('projects.detail.usageCacheCreation')}</th>
                          <th style={{ width: 150 }}>{t('projects.detail.usageCost')}</th>
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
                  <span style={{ fontWeight: 600, fontSize: 14 }}>{t('projects.detail.usageReviewTitle')}</span>
                  <span style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>{t('projects.detail.usageReviewNote')}</span>
                </div>
                {projectUsage.review.length === 0 ? (
                  <div className="tab-empty"><p>{t('projects.detail.usageReviewEmpty')}</p></div>
                ) : (
                  <div className="pr-list">
                    <table className="pr-table table-cards">
                      <thead>
                        <tr>
                          <th style={{ width: 70 }}>{t('projects.detail.usageReviewColPr')}</th>
                          <th>{t('projects.detail.reviewColTitle')}</th>
                          <th style={{ width: 160 }}>{t('projects.detail.usageReviewColBranch')}</th>
                          <th style={{ width: 110 }}>{t('projects.detail.usageInput')}</th>
                          <th style={{ width: 110 }}>{t('projects.detail.usageOutput')}</th>
                          <th style={{ width: 110 }}>{t('projects.detail.usageReviewColTime')}</th>
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
                              {rv.created_at ? fmtDateTime(rv.created_at) : '—'}
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </div>
              <small style={{ color: 'var(--color-text-muted)', fontSize: 12, display: 'block', marginTop: 8 }}>
                {t('projects.detail.usageInputFootnote')}
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
              <button className="kb-modal-close" onClick={() => setKnowledgeModal(null)}>{t('projects.detail.modalClose')}</button>
            </div>
            <div className="kb-modal-body">
              <div className="kb-markdown">
                <ReactMarkdown remarkPlugins={[remarkGfm]}>{knowledgeModal.content}</ReactMarkdown>
              </div>
            </div>
          </div>
        </div>
      )}

      {/* Mobile FAB: shown only on the requirements tab so each tab keeps
          its own primary CTA in the thumb zone. CSS (.fab) hides it on
          desktop, where the inline "+ New requirement" button is already reachable. */}
      {tab === 'requirements' && (
        <button
          className="fab fab-extended"
          aria-label={t('projects.detail.fabLabel')}
          onClick={() => setShowCreateReq(true)}
        >
          <span>＋</span>
          <span>{t('projects.detail.fabLabel')}</span>
        </button>
      )}
    </div>
  );
}

// (CreateRequirementForm moved to its own component in
// components/CreateRequirementForm/CreateRequirementForm.tsx so it can be reused
// from the cross-project RequirementsList page. The local function above was
// removed; the import at the top of this file supplies the same component.)