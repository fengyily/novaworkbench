import { useState, useEffect, useRef, useCallback, useMemo, type DragEvent } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';

import {
  projectsApi, runnerApi, platformApi, requirementsApi, knowledgeApi,
  usageApi, usageTotalInput, fmtCost, wizardApi,
  type Project, type RunStatus, type PlatformToken,
  type Requirement, type KnowledgeItem, type ReqUsage, type ProjectUsage,
  kindLabelKeys, priorityLabelKeys, kindOf, API_BASE, authedFetch,
  MARK_PRESETS, parseMarks,
} from '../api/client';
import { tLabel } from '../i18n/label';
import { fmtDateTime, fmtRelative, fmtRunAtRelative } from '../utils/intl';
import { CreateRequirementForm } from '../components/CreateRequirementForm/CreateRequirementForm';
import { StatusChips } from '../components/StatusChips';
import { RequirementTreeToggle, RequirementTreeSpacer } from '../components/RequirementTreeToggle';
import PdfUploadDialog from '../components/PdfUploadDialog';
import ProjectWeeklyReport from './ProjectWeeklyReport';
import ProjectIssueMonitor from './ProjectIssueMonitor';
import ProjectCodeReview from './ProjectCodeReview';
import ProjectTesting from './ProjectTesting';
import { IconClock } from '../components/icons';
import { stripMarkdownPreview } from '../utils/preview';
import { pageWindow } from '../utils/pageWindow';
import './RequirementDetail.css';
import './ProjectDetail.css';
import './KnowledgePage.css';
import { errorMessage as errMsg } from '../utils/errMsg';
import { Breadcrumb } from '../components/Breadcrumb';

type Tab = 'overview' | 'knowledge' | 'run' | 'requirements' | 'review' | 'weekly' | 'usage' | 'issueMonitor' | 'testing';

// Columns the requirements table allows the user to sort by clicking the
// header. Each value maps to one logical comparator in `compareRequirements`
// below — adding a sortable column is a two-line change (type + comparator)
// plus an `isSortable` flag on the <th>. Columns NOT in this union (type,
// agent server, tokens, cost) intentionally don't sort because their data
// either has no natural ordering (agent server name) or no cheap client-side
// representation (cost is computed from token usage).
type ReqSortColumn = 'id' | 'title' | 'priority' | 'status' | 'created_at' | 'updated_at';

// Sort direction. First click picks the column's `defaultDir` (see
// `toggleSort`); subsequent clicks on the same column flip between asc /
// desc. Strings (not a numeric enum) so the value is trivial to drop into
// `aria-sort` and the column-header indicator.
type ReqSortDir = 'asc' | 'desc';

// Natural "first click" direction for each sortable column. Chosen so the
// most-useful order shows up immediately — high priority first (desc),
// newest first (desc), A→Z (asc), active stages first (asc). Users who want
// the opposite can click again to flip.
const REQ_SORT_DEFAULT_DIR: Record<ReqSortColumn, ReqSortDir> = {
  id: 'desc',
  title: 'asc',
  priority: 'desc',
  status: 'asc',
  created_at: 'desc',
  updated_at: 'desc',
};

// Priority is rendered as an icon-only `.priority-dot` now (text label
// was removed to give the title column more horizontal space — see the
// priority td further down). The dot colors live with the other
// priority rules in `frontend/src/pages/RequirementsList.css`.

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

// SortHeader — clickable <th> that drives the requirements-tab sort state.
// Renders the column label plus an arrow indicator (▲ / ▼) when active, and
// sets aria-sort so screen readers announce the sort direction. Inactive
// columns show a faint ↕ so the click affordance is discoverable without
// being noisy. Click anywhere on the header to trigger `onSort(col)` — the
// parent decides whether to flip direction or reset to the column default.
//
// Kept as a plain function (not a useCallback inside the component) so it
// doesn't need to be wrapped in React.memo — re-renders are cheap, and the
// parent already passes stable handlers via `toggleSort`.
function SortHeader({
  col,
  label,
  width,
  sortColumn,
  sortDir,
  onSort,
}: {
  col: ReqSortColumn;
  label: string;
  width?: number;
  sortColumn: ReqSortColumn | null;
  sortDir: ReqSortDir;
  onSort: (col: ReqSortColumn) => void;
}) {
  const active = sortColumn === col;
  // ↑ = ascending, ↓ = descending, ↕ = inactive (subtle hint that the
  // column is sortable). The arrow lives in its own span so the layout
  // doesn't reflow when the indicator changes.
  const arrow = active ? (sortDir === 'asc' ? '▲' : '▼') : '↕';
  const ariaSort = active
    ? (sortDir === 'asc' ? 'ascending' : 'descending')
    : 'none';
  return (
    <th
      style={width ? { width } : undefined}
      className={`pr-th-sortable${active ? ' is-active' : ''}`}
      aria-sort={ariaSort as 'ascending' | 'descending' | 'none'}
      onClick={() => onSort(col)}
      role="button"
      tabIndex={0}
      onKeyDown={e => {
        // Space / Enter activates the header — mirrors native <button>
        // semantics so keyboard users can sort without reaching for the
        // mouse.
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          onSort(col);
        }
      }}
    >
      <span className="pr-th-sortable-label">{label}</span>
      <span className={`pr-th-sortable-arrow${active ? ' is-active' : ''}`} aria-hidden="true">{arrow}</span>
    </th>
  );
}

// CommitPushConfigSection renders the project's commit-mode selector +
// (conditional) script textarea. Kept inline in ProjectDetail.tsx
// rather than promoted to components/ because the save logic is
// project-state-bound: project.commit_mode + project.commit_script
// round-trip through setCommitPushConfig. A standalone component
// would have lifted state for no real reuse.
//
// Layout mirrors the commitLang section above: select + optional
// textarea + per-mode hint + "保存" button. Saving is debounced via
// the parent's commitPushSaving guard — the in-component commitDraft
// state lets the user pause between mode changes without a save
// round-trip on every keystroke.
function CommitPushConfigSection({
  mode,
  script,
  saving,
  onSave,
}: {
  mode: string;
  script: string;
  saving: boolean;
  onSave: (mode: string, script: string) => Promise<void> | void;
}) {
  const { t } = useTranslation();
  const [draftMode, setDraftMode] = useState(mode);
  const [draftScript, setDraftScript] = useState(script);

  // Re-sync when the parent re-loads the project row (e.g. after a
  // scanner refresh) so the form doesn't get stuck on a stale value.
  useEffect(() => {
    setDraftMode(mode);
  }, [mode]);
  useEffect(() => {
    setDraftScript(script);
  }, [script]);

  const scriptRequired =
    draftMode === 'script_only' || draftMode === 'script_first';
  const dirty = draftMode !== mode || draftScript !== script;
  const disabled = saving || !dirty || (scriptRequired && !draftScript.trim());

  const handleSave = () => {
    void onSave(draftMode, draftScript);
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
      <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
        <select
          className="form-input"
          style={{ flex: '1 1 220px', maxWidth: 320 }}
          value={draftMode}
          onChange={e => setDraftMode(e.target.value)}
          disabled={saving}
          aria-label={t('projects.detail.commitPush.label')}
        >
          <option value="script_only">{t('projects.detail.commitPush.modeScriptOnly')}</option>
          <option value="llm_only">{t('projects.detail.commitPush.modeLLMOnly')}</option>
          <option value="script_first">{t('projects.detail.commitPush.modeScriptFirst')}</option>
          <option value="llm_first">{t('projects.detail.commitPush.modeLLMFirst')}</option>
        </select>
        <button
          type="button"
          className="btn btn-secondary"
          disabled={disabled}
          onClick={handleSave}
        >
          {saving
            ? t('projects.detail.basicSaving')
            : t('projects.detail.commitPush.save')}
        </button>
      </div>
      {scriptRequired && (
        <textarea
          className="form-input"
          rows={4}
          style={{ fontFamily: 'ui-monospace, SFMono-Regular, monospace', fontSize: 12 }}
          placeholder={t('projects.detail.commitPush.scriptPlaceholder')}
          value={draftScript}
          onChange={e => setDraftScript(e.target.value)}
          disabled={saving}
          aria-label={t('projects.detail.commitPush.scriptLabel')}
        />
      )}
      <div style={{ fontSize: 12, color: 'var(--color-text-muted)', lineHeight: 1.5 }}>
        {t(`projects.detail.commitPush.modeHint.${draftMode}`)}
        {scriptRequired && !draftScript.trim() && (
          <div style={{ marginTop: 6, color: 'var(--color-warning, #d97706)' }}>
            ⚠ {t('projects.detail.commitPush.scriptMissingWarn')}
          </div>
        )}
      </div>
    </div>
  );
}

export default function ProjectDetail() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();

  const [project, setProject] = useState<Project | null>(null);
  const [loading, setLoading] = useState(true);
  // `?tab=review` lets the Review detail page return to the Review tab.
  const [tab, setTab] = useState<Tab>(() =>
    new URLSearchParams(window.location.search).get('tab') === 'review' ? 'review' : 'requirements');

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

  // Overview: basic info (name / remote_url / default_branch / project_type / local_path)
  const [basicEditing, setBasicEditing] = useState(false);
  const [basicDraft, setBasicDraft] = useState({
    name: '', remote_url: '', default_branch: '', project_type: '', local_path: '',
  });
  const [basicSaving, setBasicSaving] = useState(false);
  const [basicMsg, setBasicMsg] = useState<{ ok: boolean; text: string } | null>(null);
  // Branch candidates for the default-branch datalist — populated when the
  // user enters edit mode and project.local_path points at a git checkout.
  // Mirrors RequirementDetail.tsx's fetchGitBranches pattern: a best-effort
  // GET /api/fs/git-branches?path=<local_path> so the user can pick from a
  // typed list instead of guessing the main branch name.
  const [availableBranches, setAvailableBranches] = useState<string[]>([]);

  // Fetch the project's branch list whenever the user opens the basic-info
  // editor and the project has a local path. Errors are swallowed — the
  // datalist simply stays empty, the user can still type any branch name
  // manually, and the wizard pipeline falls back to "main" when blank.
  useEffect(() => {
    if (!basicEditing) {
      setAvailableBranches([]);
      return;
    }
    const localPath = project?.local_path;
    if (!localPath) {
      setAvailableBranches([]);
      return;
    }
    let cancelled = false;
    authedFetch(`${API_BASE}/api/fs/git-branches?path=${encodeURIComponent(localPath)}`)
      .then(r => r.json())
      .then(json => {
        if (cancelled) return;
        if (json?.success && Array.isArray(json.data?.branches)) {
          setAvailableBranches(json.data.branches);
        } else {
          setAvailableBranches([]);
        }
      })
      .catch(() => { if (!cancelled) setAvailableBranches([]); });
    return () => { cancelled = true; };
  }, [basicEditing, project?.local_path]);

  // Overview: project description (AI-generated, manually editable)
  const [descEditing, setDescEditing] = useState(false);
  const [descDraft, setDescDraft] = useState('');
  const [descSaving, setDescSaving] = useState(false);
  const [regenerating, setRegenerating] = useState(false);
  const [descMsg, setDescMsg] = useState<{ ok: boolean; text: string } | null>(null);

  // Overview: commit / PR language override (pins wizard-generated commit
  // messages, push summaries, and PR titles/bodies). Empty string = auto
  // (fall back to commit_lang detected from git history).
  const [commitLangSaving, setCommitLangSaving] = useState(false);
  const [commitLangMsg, setCommitLangMsg] = useState<{ ok: boolean; text: string } | null>(null);

  // Manual repo sync (POST /api/projects/{id}/sync). syncMsg doubles as the
  // transient toast that auto-clears after a few seconds.
  const [syncBusy, setSyncBusy] = useState(false);
  const [syncMsg, setSyncMsg] = useState<{ ok: boolean; text: string } | null>(null);

  // Overview: commit-push strategy (only-script / only-LLM / script-first /
  // LLM-first). Drives which path handler.mergeGenerate takes on
  // /merge/local and /merge/push when the user leaves the commit_message
  // field empty. Mirrors the commitLang state shape so the save
  // pattern is identical.
  const [commitPushSaving, setCommitPushSaving] = useState(false);
  const [commitPushMsg, setCommitPushMsg] = useState<{ ok: boolean; text: string } | null>(null);

  // Requirements (shared by the requirements tab and the overview recent list)
  const [reqs, setReqs] = useState<Requirement[]>([]);
  const [reqsLoading, setReqsLoading] = useState(false);
  const [reqsError, setReqsError] = useState('');
  const [reqsLoaded, setReqsLoaded] = useState(false);
  const [showCreateReq, setShowCreateReq] = useState(false);
  // Master/child hierarchy on the requirements tab. This table is a
  // separate implementation from the cross-project RequirementsList, so
  // the expander / drag-to-nest / attach-picker had to be wired up here
  // independently — the project page previously showed only a passive
  // "子需求" chip on rows that already had a parent_id.
  const [reqExpanded, setReqExpanded] = useState<Set<string>>(new Set());
  // One-shot auto-expand: first time the loaded requirements include any
  // master that already has a child, expand it. The ref makes the
  // auto-expand idempotent so the user's manual toggles survive reloads.
  const didReqInitialExpand = useRef(false);
  useEffect(() => {
    if (didReqInitialExpand.current) return;
    const withKids = reqs.filter(r => (r.child_count ?? 0) > 0).map(r => r.id);
    if (withKids.length === 0) return;
    setReqExpanded(new Set(withKids));
    didReqInitialExpand.current = true;
  }, [reqs]);
  const [reqDragId, setReqDragId] = useState<string | null>(null);
  const [reqDropId, setReqDropId] = useState<string | null>(null);
  const [reqAddChildFor, setReqAddChildFor] = useState<string | null>(null);
  // Batch import — drop in / pick N .md files, each becomes a requirement
  // (title = first heading or first non-empty line; body = the rest).
  // Optional parent picker attaches all created rows beneath an existing
  // master in one round-trip. Strict 1-level nesting still applies.
  const [showBatchImport, setShowBatchImport] = useState(false);
  const [batchItems, setBatchItems] = useState<{ key: string; title: string; description: string; bytes: number }[]>([]);
  const [batchParentId, setBatchParentId] = useState('');
  const [batchBusy, setBatchBusy] = useState(false);
  const [batchError, setBatchError] = useState('');
  const batchInputRef = useRef<HTMLInputElement>(null);
  // Requirements tab pagination — first page by default.
  const [reqPage, setReqPage] = useState(1);
  // Requirements-tab filter + sort (only affects the requirements tab; the
  // overview's recent list keeps its own narrower `status !== 'done'` filter).
  // Default = 'active' so first-time visitors see only in-flight work (the
  // same slice the overview shows); a long backlog of done/archived rows
  // would otherwise bury everything below the fold. Users can flip to
  // 'all' / 'done' / 'archived' via the chip group above the table.
  const [reqStatusFilter, setReqStatusFilter] = useState<'all' | 'active' | 'done' | 'archived'>('active');
  // Text search applied to title + description (case-insensitive contains).
  // Empty string disables the search filter. Lives next to the status chips
  // so users can quickly narrow down to a specific bug / feature without
  // scrolling the full backlog.
  const [reqSearch, setReqSearch] = useState('');
  // Column-driven sort. `reqSortColumn === null` keeps the backend order
  // authoritative (done-last + created_at DESC) — same default as before.
  // Clicking a sortable column header sets `reqSortColumn` and seeds
  // `reqSortDir` with that column's natural direction; a second click on
  // the same header flips the direction. See `toggleSort` for the rule.
  const [reqSortColumn, setReqSortColumn] = useState<ReqSortColumn | null>(null);
  const [reqSortDir, setReqSortDir] = useState<ReqSortDir>('desc');

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

  // Review tab — the whole surface (PR mode + branch mode + history) lives in
  // <ProjectCodeReview>, which owns its own state and SSE lifecycle.

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

  // Cleanup on unmount
  useEffect(() => {
    return () => {
      if (pollTimerRef.current) clearTimeout(pollTimerRef.current);
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

  // Knowledge base for this project. Loaded on demand when the knowledge tab
  // is opened; the tab groups entries by source_type (requirement / document /
  // code / other). The full-content reading view lives at /knowledge/view/:id
  // (KnowledgeViewPage) — we open it in a new tab from each card, mirroring
  // KnowledgePage's behavior, so users never lose the project context behind a
  // modal.
  const [knowledge, setKnowledge] = useState<KnowledgeItem[]>([]);
  const [knowledgeLoading, setKnowledgeLoading] = useState(false);
  // PDF 上传弹窗状态:挂在项目级知识库里,上传后直接刷新当前列表,
  // 不必跳走也不必切 tab。
  const [showPdfUpload, setShowPdfUpload] = useState(false);
  const refreshKnowledge = useCallback(() => {
    if (!id) return;
    knowledgeApi.list({ project_id: id, limit: 200 })
      .then(res => setKnowledge(res.items ?? []))
      .catch(() => {});
  }, [id]);
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

  // Same reset when the user changes the filter, search, or sort — otherwise
  // a page-3 selection can survive a filter that only has 1 page of results
  // and silently render an empty tbody.
  useEffect(() => { setReqPage(1); }, [reqStatusFilter, reqSearch, reqSortColumn, reqSortDir]);

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

  // ── Overview: basic info edit (name / remote_url / default_branch / project_type / local_path) ─
  const handleSaveBasic = async () => {
    if (!id) return;
    setBasicSaving(true);
    setBasicMsg(null);
    try {
      const updated = await projectsApi.updateBasicInfo(id, {
        name: basicDraft.name.trim() ? basicDraft.name : undefined,
        remote_url: basicDraft.remote_url,
        // default_branch is sent as-is (including "") so the user can clear
        // it back to "use the platform default" by deleting the field.
        default_branch: basicDraft.default_branch,
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

  // ── Overview: commit / PR language override ───────────────────────────────
  // Pinned language used by the wizard pipeline when generating commit
  // messages, push summaries, and PR titles/bodies. value="" → revert to
  // auto (the value auto-detected from git history by the scanner).
  const handleCommitLangChange = async (value: string) => {
    if (!id || commitLangSaving) return;
    const normalized = value ?? '';
    setCommitLangSaving(true);
    setCommitLangMsg(null);
    try {
      const updated = await projectsApi.setCommitLangOverride(id, normalized);
      setProject(updated);
      setCommitLangMsg({
        ok: true,
        text: t('projects.detail.commitLang.saved'),
      });
    } catch (e: unknown) {
      setCommitLangMsg({
        ok: false,
        text: t('projects.detail.commitLang.saveFailed', {
          msg: e instanceof Error ? e.message : String(e),
        }),
      });
    } finally {
      setCommitLangSaving(false);
    }
  };

  // Manual repo sync — pulls origin/<default_branch> and refreshes the
  // sync badge. Reuses the same backend path as the wizard's architect
  // prologue (EnsureClonedAndSynced), but is non-blocking (no hard gates
  // on dirty tree / detached HEAD — fetch only touches remote refs).
  const handleSync = useCallback(async () => {
    if (!project || syncBusy) return;
    setSyncBusy(true);
    setSyncMsg(null);
    try {
      const updated = await projectsApi.syncNow(project.id);
      setProject(updated);
      const branch = updated.default_branch || 'main';
      const sha = (updated.last_synced_commit || '').slice(0, 7);
      const ok = updated.sync_status === 'ok';
      if (ok && !sha) {
        setSyncMsg({
          ok: true,
          text: t('projects.detail.syncNowOkCloned'),
        });
      } else if (ok) {
        setSyncMsg({
          ok: true,
          text: t('projects.detail.syncNowOk', { branch, sha }),
        });
      } else if (updated.sync_status === 'idle' && !updated.remote_url) {
        setSyncMsg({
          ok: false,
          text: t('projects.detail.syncNowSkipped'),
        });
      } else {
        setSyncMsg({
          ok: false,
          text: t('projects.detail.syncNowError', { err: t('projects.detail.syncStatusError') }),
        });
      }
    } catch (e) {
      setSyncMsg({ ok: false, text: t('projects.detail.syncNowError', { err: errMsg(e) }) });
    } finally {
      setSyncBusy(false);
      setTimeout(() => setSyncMsg(null), 4000);
    }
  }, [project, syncBusy, t]);

  // ── Overview: commit-push strategy ────────────────────────────────────────
  // When the user submits /merge/local or /merge/push WITHOUT a manual
  // commit_message, the backend dispatches handler.mergeGenerate using
  // the project's commit_mode + commit_script. Single save call covers
  // both the mode select and the optional shell script — keeps the
  // round-trip to one PUT regardless of which input changed.
  const handleCommitPushSave = async (
    nextMode: string,
    nextScript: string
  ) => {
    if (!id || commitPushSaving) return;
    setCommitPushSaving(true);
    setCommitPushMsg(null);
    try {
      const updated = await projectsApi.setCommitPushConfig(
        id,
        nextMode,
        nextScript
      );
      setProject(updated);
      setCommitPushMsg({
        ok: true,
        text: t('projects.detail.commitPush.saved'),
      });
    } catch (e: unknown) {
      setCommitPushMsg({
        ok: false,
        text: t('projects.detail.commitPush.saveFailed', {
          msg: e instanceof Error ? e.message : String(e),
        }),
      });
    } finally {
      setCommitPushSaving(false);
    }
  };

  const isRunning = runStatus?.status === 'running';

  // Shared table rows for the requirements list (used by both the overview
  // "recent requirements" and the requirements tab — single source of truth).
  // The "Agent server" column mirrors the cross-project RequirementsList so
  // users can see at a glance which remote execution target the requirement
  // was developed on (or local if it ran locally). Stays consistent with the
  // `agent-server-tag` styling on the global list page.
  // `items` is either flat Requirement[] (the overview's short recent list,
  // which stays hierarchy-free) or {req, depth} pairs from the requirements
  // tab's tree. Plain Requirements render at depth 0 with no expander, so
  // the overview table is unchanged by the hierarchy work.
  const renderRequirementRows = (
    items: (Requirement | { req: Requirement; depth: number; last?: boolean; open?: boolean })[],
    opts?: { hierarchy?: boolean },
  ) => items.map(item => {
    const isRow = 'req' in item;
    const req = isRow ? item.req : (item as Requirement);
    const depth = isRow ? item.depth : 0;
    const last = isRow ? !!item.last : false;
    const open = isRow ? !!item.open : false;
    const showTree = !!opts?.hierarchy;
    return (
    <tr
      key={req.id}
      className={`req-tree-row${depth > 0 ? ' req-tree-row-child' : ''}${showTree && open ? ' req-tree-row-open' : ''}${depth > 0 && last ? ' req-tree-row-last' : ''}${reqDragId === req.id ? ' dragging' : ''}${reqDropId === req.id ? ' drop-target' : ''}`}
      style={{ cursor: 'pointer' }}
      draggable
      onDragStart={e => onReqDragStart(e, req)}
      onDragEnd={() => { setReqDragId(null); setReqDropId(null); }}
      onDragOver={e => onReqDragOver(e, req)}
      onDragLeave={() => { if (reqDropId === req.id) setReqDropId(null); }}
      onDrop={e => onReqDrop(e, req)}
      onClick={() => navigate(`/requirements/${req.id}`)}
    >
      {/* Child rows leave the gutter empty on purpose — the directory-tree
          connector is drawn on this cell by CSS. */}
      <td className="req-tree-col">{depth === 0 && showTree && renderReqTreeToggle(req, depth)}</td>
      <td data-label={t('projects.detail.colId')} style={{ color: 'var(--color-text-muted)', fontFamily: 'var(--font-mono)', fontSize: 12 }}>{req.id}</td>
      <td data-label={t('projects.detail.colType')}>
        {/* Priority dot folded INTO the Type cell, ahead of the kind-badge.
            Why: the dedicated priority column was eating ~90px even when
            there was nothing to say (an em-dash, or a single 6px circle),
            and the screenshot showed two adjacent cells that competed for
            space. Putting the dot beside the badge uses the cell width we
            were already spending on the badge anyway. The dot's color is the
            only signal that mattered — High / Medium / Low already has
            palette semantics in `.priority-{high|medium|low}`.

            The 8px slot is reserved even when there's no priority, so the
            badge's left edge is identical on every row (see .req-type-cell
            + .priority-dot-slot in index.css). */}
        <span className="req-type-cell">
          {req.priority ? (
            <span
              className={`priority-dot priority-${req.priority}`}
              title={tLabel(t, priorityLabelKeys, req.priority)}
              aria-label={tLabel(t, priorityLabelKeys, req.priority)}
            />
          ) : (
            <span className="priority-dot-slot" aria-hidden />
          )}
          <span className={`kind-badge kind-${kindOf(req)}`}>{tLabel(t, kindLabelKeys as Record<string, string>, kindOf(req))}</span>
        </span>
      </td>
      <td data-label={t('projects.detail.colTitle')} className={`pr-title${depth > 0 ? ' req-tree-child-title' : ''}`}>
        {/* Preset marks (重要 / 跟进 / 阻塞 / 风险) render INLINE with
            the title (chip strip BEFORE the schedule clock + title text)
            so marked requirements stay on a single line — same order as
            the global RequirementsList. Unmarked rows render nothing (no
            extra height), and unknown codes are skipped so a stale value
            written before a preset is renamed can't crash the row. Colors
            come from MARK_PRESETS in api/client.ts (single source of
            truth shared with the detail-page editor). */}
        <div className="pr-title-text">
          {(() => {
            const rowMarks = parseMarks(req.marks);
            if (rowMarks.length === 0) return null;
            return (
              <div className="pr-title-marks">
                {rowMarks.map(code => {
                  const p = MARK_PRESETS.find(x => x.code === code);
                  if (!p) return null;
                  const label = t(`requirements.detail2.marksPreset.${p.code}`);
                  return (
                    <span
                      key={code}
                      className="pr-title-mark"
                      style={{ color: p.color, background: p.bg, borderColor: p.color }}
                      title={label}
                    >
                      <span aria-hidden>{p.icon}</span>
                      <span>{label}</span>
                    </span>
                  );
                })}
              </div>
            );
          })()}
          {/* Pending-scheduled chip — unified with the cross-project list:
              clock renders BEFORE the title text so both surfaces read
              identically. Mirrors scheduleClock() in RequirementsList.tsx
              and reuses the shared .schedule-chip indigo pill — the
              affordance is unmissable next to the title text, where the
              bare 13px outline icon was visually swallowed by the
              surrounding status badges. Gated on scheduled_run_at from
              the backend List endpoint (same helper the calendar uses). */}
          {req.scheduled_run_at && (
            <span
              className="schedule-chip"
              title={t('requirements.list.scheduledTooltip', {
                time: fmtDateTime(req.scheduled_run_at),
                rel: fmtRunAtRelative(req.scheduled_run_at),
              })}
              aria-label={t('requirements.list.scheduledAria')}
            >
              <IconClock size={12} className="schedule-chip-icon" />
              {fmtRunAtRelative(req.scheduled_run_at)}
            </span>
          )}
          <span className="pr-title-text-label">{req.title}</span>
          {req.parent_id && (
            <span className="req-tag-chip pr-title-tag" style={{ background: 'var(--color-hover)', color: 'var(--color-text-secondary)' }}>{t('requirements.tree.childBadge')}</span>
          )}
          {/* Child count on a master — without it a collapsed master is
              visually identical to a plain row apart from the expander. */}
          {showTree && depth === 0 && reqHasChildren(req) && (
            <span className="req-tag-chip pr-title-tag">{t('requirements.tree.childCount', { n: reqChildrenByParent.get(req.id)!.length })}</span>
          )}
        </div>
        {/* Tag chips: rendered below the title as a compact strip (max 3
            visible, then +N overflow). Empty / undefined = no chips row,
            so legacy rows without the tags column stay on the original
            one-line title layout. Force-closed rows also surface a small
            "中途关闭" badge with the reason as the hover tooltip so the
            user can tell at a glance why a done row looks short. */}
        {(() => {
          const list: string[] = (() => {
            const raw = (req.tags || '').trim();
            if (!raw || raw === '[]') return [];
            try {
              const parsed = JSON.parse(raw);
              return Array.isArray(parsed) ? parsed.filter((x): x is string => typeof x === 'string') : [];
            } catch { return []; }
          })();
          if (list.length === 0 && !req.closed_at) return null;
          const visible = list.slice(0, 3);
          const overflow = list.length - visible.length;
          return (
            <div className="pr-title-tags">
              {req.closed_at && (
                <span
                  className="req-tag-chip pr-title-tag pr-title-tag-closed"
                  title={req.closed_reason || t('requirements.detail2.closedBadge')}
                >
                  {t('requirements.detail2.closedBadge')}
                </span>
              )}
              {visible.map(tag => (
                <span key={tag} className="req-tag-chip pr-title-tag" title={tag}>{tag}</span>
              ))}
              {overflow > 0 && (
                <span className="req-tag-chip pr-title-tag pr-title-tag-more" title={list.slice(3).join(', ')}>+{overflow}</span>
              )}
            </div>
          );
        })()}
      </td>
      <td data-label={t('projects.detail.colStatus')}>
        <StatusChips req={req} />
        {/* Breathing dot for any wizard job in flight on this requirement
            (analyst/design/apply/coding). Set is populated by the 5s poll
            of /api/wizard/active-jobs above. aria-label + title so screen
            readers + hover explain the indicator (the dot has no text).
            Rendered next to the status chip group; both are visible at once
            because pulse indicates an active job, chip indicates stored status. */}
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
      <td
        data-label={t('projects.detail.colCreatedAt')}
        style={{
          color: 'var(--color-text-muted)',
          fontSize: 12,
          fontFamily: 'var(--font-mono)',
          fontVariantNumeric: 'tabular-nums',
          whiteSpace: 'nowrap',
        }}
        title={req.created_at ? fmtRelative(req.created_at) : undefined}
      >
        {req.created_at ? fmtDateTime(req.created_at) : '—'}
      </td>
      <td
        data-label={t('projects.detail.colUpdatedAt')}
        style={{
          color: 'var(--color-text-muted)',
          fontSize: 12,
          fontFamily: 'var(--font-mono)',
          fontVariantNumeric: 'tabular-nums',
          whiteSpace: 'nowrap',
        }}
        title={req.updated_at ? fmtRelative(req.updated_at) : undefined}
      >
        {req.updated_at ? fmtDateTime(req.updated_at) : '—'}
      </td>
    </tr>
    );
  });

  // Overview shows only tasks that have not finished development ('done').
  // The requirements tab keeps showing everything (done sorts to the end).
  const overviewReqs = reqs.filter(r => r.status !== 'done');

  // Requirements tab — client-side filter + search applied BEFORE the sort
  // and pagination. Two independent predicates are combined here so the sort
  // step below only ever sees the final user-visible slice.
  //   • Status chip: 'all' intentionally excludes 'archived' (consistent with
  //     the backend's List() default, which hides archived unless the caller
  //     explicitly opts in via status='archived'); 'archived' is its own
  //     bucket so it never bleeds into other views.
  //   • Search box: case-insensitive substring match against the title OR
  //     description. Whitespace-trimmed so a stray space at the end doesn't
  //     accidentally exclude every row.
  // Parent-anchored two-phase filter: the status + search predicates above
  // run only on top-level rows (no parent_id). Any child whose parent_id is
  // in the phase-1 keep set is pulled in unconditionally, regardless of its
  // own status — this is what keeps a sub-requirement visible beneath its
  // master row no matter what the master/child status split looks like.
  // The tree assembler (reqTreeRows) is left untouched: its existing
  // "promote to top-level when parent is not in the set" rule stays, it just
  // no longer gets falsely triggered by status mismatches.
  const filteredReqs = useMemo(() => {
    const term = reqSearch.trim().toLowerCase();
    const passes = (r: Requirement) => {
      switch (reqStatusFilter) {
        case 'all':       if (r.status === 'archived') return false; break;
        case 'active':    if (!['draft', 'analyzing', 'designing', 'designed', 'developing'].includes(r.status)) return false; break;
        case 'done':      if (r.status !== 'done') return false; break;
        case 'archived':  if (r.status !== 'archived') return false; break;
      }
      // Search predicate (empty term = pass)
      if (!term) return true;
      const title = (r.title || '').toLowerCase();
      const desc = (r.description || '').toLowerCase();
      return title.includes(term) || desc.includes(term);
    };
    // Phase 1: top-level rows pass through the predicates.
    const masters = reqs.filter(r => !r.parent_id && passes(r));
    // Phase 2: children whose parent_id is in the phase-1 keep set follow
    // unconditionally, regardless of their own status or search match.
    const keep = new Set(masters.map(r => r.id));
    return [...masters, ...reqs.filter(r => r.parent_id && keep.has(r.parent_id))];
  }, [reqs, reqStatusFilter, reqSearch]);

  // Comparator for one sortable column. Returns negative if `a` should
  // appear before `b` in ASC order; the caller flips the sign for DESC.
  // Pure functions so the sort step is trivially testable.
  const compareRequirements = (a: Requirement, b: Requirement, col: ReqSortColumn): number => {
    switch (col) {
      case 'id':
        // `req_xxxxxxxx` is hex; lexicographic compare matches numeric order
        // because the ids are zero-padded to a fixed width.
        return (a.id || '').localeCompare(b.id || '');
      case 'title':
        return (a.title || '').localeCompare(b.title || '', undefined, { sensitivity: 'base' });
      case 'priority': {
        // Custom order so high→low stays meaningful in both directions.
        const order: Record<string, number> = { high: 0, medium: 1, low: 2 };
        return (order[a.priority] ?? 99) - (order[b.priority] ?? 99);
      }
      case 'status': {
        // Lifecycle order: draft→analyzing→designing→designed→developing→done→archived.
        // Ascending shows "what's in flight" first; descending is rarely useful
        // but still consistent.
        const order: Record<string, number> = {
          draft: 0, analyzing: 1, designing: 2, designed: 3, developing: 4,
          done: 5, archived: 6,
        };
        return (order[a.status] ?? 99) - (order[b.status] ?? 99);
      }
      case 'created_at':
      case 'updated_at':
        // ISO-8601 strings sort correctly lexicographically — same approach as
        // the earlier dropdown sort, kept here for consistency.
        return (a[col] || '').localeCompare(b[col] || '');
    }
  };

  // Sort the filtered slice. `reqSortColumn === null` is a pass-through —
  // the backend already returns done-last + created_at DESC, and we want
  // that order to stay authoritative when no column has been clicked yet.
  // When a column is active, the sort uses `compareRequirements` and applies
  // `reqSortDir` (flip the sign for DESC). The list is shallow-copied first
  // so we never mutate `filteredReqs` (it'd poison the next render's
  // reference equality).
  // Special case: the "已完成" quick filter only contains status='done' rows,
  // and the user expects them ordered by completion time descending — newest
  // completions first. The backend List() endpoint already returns the right
  // order when called with status=done, but ProjectDetail fetches without a
  // status filter and trims client-side, so we re-sort here to match the
  // cross-project RequirementsList behavior.
  const sortedReqs = useMemo(() => {
    if (reqSortColumn === null) {
      if (reqStatusFilter === 'done') {
        return [...filteredReqs].sort((a, b) => {
          // ISO-8601 strings sort correctly lexicographically; missing
          // completed_at sinks to the bottom.
          const aTs = a.completed_at || '';
          const bTs = b.completed_at || '';
          if (aTs === bTs) return (b.id || '').localeCompare(a.id || '');
          return bTs.localeCompare(aTs);
        });
      }
      return filteredReqs;
    }
    const sign = reqSortDir === 'asc' ? 1 : -1;
    return [...filteredReqs].sort((a, b) => {
      const cmp = compareRequirements(a, b, reqSortColumn);
      if (cmp !== 0) return sign * cmp;
      // Stable tie-breaker: fall back to backend order (created_at DESC).
      // Keeps the active sort's primary key unambiguous and matches what
      // the user saw before clicking.
      return (b.created_at || '').localeCompare(a.created_at || '');
    });
  }, [filteredReqs, reqSortColumn, reqSortDir, reqStatusFilter]);

  // Header-click handler. Same column flips direction; new column resets to
  // that column's natural default (REQ_SORT_DEFAULT_DIR). The page reset is
  // handled by the useEffect above.
  const toggleSort = (col: ReqSortColumn) => {
    if (reqSortColumn === col) {
      setReqSortDir(d => (d === 'asc' ? 'desc' : 'asc'));
    } else {
      setReqSortColumn(col);
      setReqSortDir(REQ_SORT_DEFAULT_DIR[col]);
    }
  };

  // ── Master/child hierarchy ────────────────────────────────────────
  // The tree is assembled over the FULL sorted/filtered list, never the
  // page slice: with 15-row pagination a parent and its child routinely
  // land on different pages, and nesting inside the slice would silently
  // promote the child to a top-level row. A child whose parent isn't in
  // the current result set (filtered out / archived) is promoted the same
  // way — it must never vanish. Pagination then runs over the tree rows,
  // so expanding a master reflows pages the way a real tree table does.
  const { reqTreeRows, reqChildrenByParent } = useMemo(() => {
    const ids = new Set(sortedReqs.map(r => r.id));
    const byParent = new Map<string, Requirement[]>();
    const top: Requirement[] = [];
    for (const r of sortedReqs) {
      if (r.parent_id && ids.has(r.parent_id)) {
        const arr = byParent.get(r.parent_id);
        if (arr) arr.push(r);
        else byParent.set(r.parent_id, [r]);
      } else {
        top.push(r);
      }
    }
    const rows: { req: Requirement; depth: number; last?: boolean; open?: boolean }[] = [];
    for (const r of top) {
      const kids = byParent.get(r.id);
      // `last` is computed from the full kids list (not the page slice),
      // so a child that paginates onto the next page still shows the
      // guide line on this page — it terminates on the next page.
      rows.push({ req: r, depth: 0, open: !!kids && reqExpanded.has(r.id) });
      if (kids && reqExpanded.has(r.id)) {
        for (let i = 0; i < kids.length; i++) {
          rows.push({ req: kids[i], depth: 1, last: i === kids.length - 1 });
        }
      }
    }
    return { reqTreeRows: rows, reqChildrenByParent: byParent };
  }, [sortedReqs, reqExpanded]);

  const reqHasChildren = (r: Requirement) => (reqChildrenByParent.get(r.id)?.length ?? 0) > 0;
  const toggleReqExpanded = (rid: string) =>
    setReqExpanded(prev => {
      const next = new Set(prev);
      if (next.has(rid)) next.delete(rid);
      else next.add(rid);
      return next;
    });

  // Attach-picker candidates. Mirrors the backend's SetParent rules exactly
  // so the picker can never offer something the server would reject.
  const reqChildCandidates = useMemo(() => {
    if (!reqAddChildFor) return [] as Requirement[];
    const parent = reqs.find(r => r.id === reqAddChildFor);
    if (!parent) return [] as Requirement[];
    return reqs.filter(
      c =>
        c.id !== parent.id &&
        c.project_id === parent.project_id &&
        !c.parent_id &&
        c.status !== 'archived' &&
        !reqHasChildren(c) &&
        (c.child_count ?? 0) === 0,
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [reqAddChildFor, reqs, reqChildrenByParent]);

  useEffect(() => {
    if (!reqAddChildFor) return;
    const onDown = (e: globalThis.MouseEvent) => {
      if ((e.target as HTMLElement | null)?.closest('.req-tree-cell')) return;
      setReqAddChildFor(null);
    };
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setReqAddChildFor(null); };
    document.addEventListener('mousedown', onDown);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('mousedown', onDown);
      document.removeEventListener('keydown', onKey);
    };
  }, [reqAddChildFor]);

  const reqReparent = async (child: Requirement, parentId: string) => {
    try {
      await requirementsApi.setParent(child.id, parentId);
      if (parentId) setReqExpanded(prev => new Set(prev).add(parentId));
      setReqs(await requirementsApi.list({ project_id: id }));
    } catch (err: any) {
      const raw: string = err?.message || String(err);
      let msg = t('requirements.tree.parentFailed', { msg: raw });
      if (raw.includes('different project')) msg = t('requirements.tree.errCrossProject');
      else if (raw.includes('already a child')) msg = t('requirements.tree.errTargetIsChild');
      else if (raw.includes('has child requirements')) msg = t('requirements.tree.errSelfHasChildren');
      // eslint-disable-next-line no-alert
      window.alert(msg);
    } finally {
      setReqDragId(null);
      setReqDropId(null);
    }
  };

  // ── Batch import .md ────────────────────────────────────────
  // Parse one md: prefer a leading markdown heading ("# Title") for the
  // title; fall back to the first non-empty line. Body = everything after
  // the title line (or whole file when there's no title), so an empty file
  // still imports as a titled stub rather than failing silently.
  const parseMdFile = async (file: File): Promise<{ key: string; title: string; description: string; bytes: number }> => {
    const text = await file.text();
    const lines = text.split(/\r?\n/);
    let title = '';
    let titleIdx = -1;
    for (let i = 0; i < lines.length; i++) {
      const m = lines[i].match(/^#+\s+(.+?)\s*$/);
      if (m) { title = m[1].trim(); titleIdx = i; break; }
    }
    if (!title) {
      for (let i = 0; i < lines.length; i++) {
        const t = lines[i].trim();
        if (t) { title = t.replace(/^#+\s*/, '').slice(0, 200); titleIdx = i; break; }
      }
    }
    if (!title) title = file.name.replace(/\.md$/i, '');
    const description = (titleIdx >= 0 ? lines.slice(titleIdx + 1).join('\n') : text).trim();
    return { key: `${file.name}|${file.size}|${file.lastModified}`, title, description, bytes: file.size };
  };

  const onBatchPick = async (files: FileList | File[]) => {
    const md = Array.from(files).filter(f => /\.md$/i.test(f.name) || f.type === 'text/markdown');
    if (md.length === 0) {
      setBatchError('没有 .md 文件');
      return;
    }
    const items = await Promise.all(md.map(parseMdFile));
    setBatchItems(prev => {
      // De-dupe on (name, size, mtime) so dropping the same folder twice
      // doesn't double-create the requirements.
      const seen = new Set(prev.map(p => p.key));
      const fresh = items.filter(it => !seen.has(it.key));
      return [...prev, ...fresh];
    });
    setBatchError('');
  };

  const submitBatch = async () => {
    if (!id || batchItems.length === 0) return;
    setBatchBusy(true);
    setBatchError('');
    const created: string[] = [];
    const failed: { name: string; reason: string }[] = [];
    for (const item of batchItems) {
      try {
        const r = await requirementsApi.create({
          project_id: id,
          // skip_organize=true + title-on-first-line → the backend's
          // fallbackTitle extracts the same line we computed, so the
          // title the user sees in the parser list is the title that
          // actually lands in the requirements table.
          description: item.title + '\n\n' + item.description,
          kind: 'requirement',
          skip_organize: true,
        });
        created.push(r.id);
      } catch (err: any) {
        failed.push({ name: item.title || item.key, reason: err?.message || String(err) });
      }
    }
    if (batchParentId) {
      for (const cid of created) {
        try {
          await requirementsApi.setParent(cid, batchParentId);
        } catch (err: any) {
          failed.push({ name: cid, reason: 'setParent: ' + (err?.message || String(err)) });
        }
      }
    }
    setReqs(await requirementsApi.list({ project_id: id }));
    if (batchParentId) setReqExpanded(prev => new Set(prev).add(batchParentId));
    setBatchBusy(false);
    if (failed.length === 0) {
      setShowBatchImport(false);
      setBatchItems([]);
      setBatchParentId('');
    } else {
      setBatchError(
        `${created.length} 已创建，${failed.length} 失败：\n` +
          failed.slice(0, 6).map(f => `• ${f.name}: ${f.reason}`).join('\n'),
      );
    }
  };

  const reqDragged = reqDragId ? reqs.find(r => r.id === reqDragId) : undefined;
  const reqCanDrop = (target: Requirement) =>
    !!reqDragged &&
    reqDragged.id !== target.id &&
    reqDragged.project_id === target.project_id &&
    !target.parent_id &&
    reqDragged.parent_id !== target.id &&
    !reqHasChildren(reqDragged) &&
    (reqDragged.child_count ?? 0) === 0;

  const onReqDragStart = (e: DragEvent, r: Requirement) => {
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', r.id);
    setReqDragId(r.id);
  };
  const onReqDragOver = (e: DragEvent, target: Requirement) => {
    if (!reqCanDrop(target)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    if (reqDropId !== target.id) setReqDropId(target.id);
  };
  const onReqDrop = (e: DragEvent, target: Requirement) => {
    if (!reqCanDrop(target) || !reqDragged) return;
    e.preventDefault();
    void reqReparent(reqDragged, target.id);
  };

  const renderReqTreeToggle = (r: Requirement, depth: number) => {
    if (depth > 0) return null;
    const toggle = reqHasChildren(r) ? (
      <RequirementTreeToggle
        expanded={reqExpanded.has(r.id)}
        onToggle={() => toggleReqExpanded(r.id)}
        controlsId={`pr-req-children-${r.id}`}
      />
    ) : r.status === 'archived' ? (
      <RequirementTreeSpacer />
    ) : (
      // No children yet — same `+` glyph, but it ATTACHES a sub-requirement
      // instead of folding. Without it this column is a blank spacer and the
      // hierarchy has no discoverable entry point but drag-and-drop.
      <RequirementTreeToggle
        expanded={false}
        mode="add"
        onAdd={() => setReqAddChildFor(cur => (cur === r.id ? null : r.id))}
        controlsId={`pr-req-add-${r.id}`}
      />
    );
    return (
      <div className="req-tree-cell">
        {toggle}
        {reqAddChildFor === r.id && (
          <div
            className="req-tree-popover"
            role="dialog"
            aria-label={t('requirements.tree.addChildHeading', { parent: r.title || t('requirements.list.noTitle') })}
            onClick={e => e.stopPropagation()}
          >
            <div className="req-tree-popover-title">
              {t('requirements.tree.addChildHeading', { parent: r.title || t('requirements.list.noTitle') })}
            </div>
            {reqChildCandidates.length === 0 ? (
              <div className="req-tree-popover-empty">
                <div>{t('requirements.tree.addChildEmpty')}</div>
                <div className="req-tree-popover-hint">{t('requirements.tree.addChildEmptyHint')}</div>
              </div>
            ) : (
              <ul className="req-tree-popover-list">
                {reqChildCandidates.map(c => (
                  <li key={c.id}>
                    <button
                      type="button"
                      className="req-tree-popover-item"
                      onClick={() => { setReqAddChildFor(null); void reqReparent(c, r.id); }}
                    >
                      <span className="req-tree-popover-item-title">{c.title || t('requirements.list.noTitle')}</span>
                      <StatusChips req={c} />
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}
      </div>
    );
  };

  // Requirements tab pagination. The page is clamped to the (possibly shrunken)
  // list so a stale reqPage never escapes the array; the reset effect below
  // normally snaps back to page 1 after a list refresh.
  const totalReqPages = Math.max(1, Math.ceil(reqTreeRows.length / REQ_PAGE_SIZE));
  const curReqPage = Math.min(reqPage, totalReqPages);
  const pagedReqRows = reqTreeRows.slice((curReqPage - 1) * REQ_PAGE_SIZE, curReqPage * REQ_PAGE_SIZE);

  // Render a group of knowledge entries under a labeled section. Reuses the
  // kb-card styles from KnowledgePage.css. The content preview is stripped of
  // markdown noise (stripMarkdownPreview) and clamped to a fixed number of
  // lines via CSS so cards stay uniform regardless of entry length; the entire
  // card is wrapped in an <a target="_blank"> that opens the dedicated reading
  // view (/knowledge/view/:id) in a new tab — same pattern as KnowledgePage.
  // The "View full" button uses preventDefault + window.open to avoid double
  // navigation when the user clicks the button specifically.
  const renderKnowledgeGroup = (labelKey: string, items: KnowledgeItem[]) => {
    if (!items.length) return null;
    const label = t(labelKey);
    return (
      <div className="detail-section" key={labelKey}>
        <h3 style={{ marginBottom: 12 }}>{label}{t('projects.detail.knowledgeCountSuffix', { n: items.length })}</h3>
        {items.map(k => (
          <a
            key={k.id}
            className="kb-card-link"
            href={`/knowledge/view/${k.id}`}
            target="_blank"
            rel="noopener noreferrer"
          >
            <div className="kb-card">
              <div className="kb-card-header">
                <span className={`kb-type-badge cat-${k.category}`}>{k.category || 'general'}</span>
                <span className="kb-card-title">{k.title}</span>
              </div>
              <div className="kb-card-content kb-clamp">{stripMarkdownPreview(k.content)}</div>
              <span
                className="kb-view-full"
                role="button"
                onClick={(e) => {
                  e.preventDefault();
                  e.stopPropagation();
                  window.open(`/knowledge/view/${k.id}`, '_blank', 'noopener,noreferrer');
                }}
              >{t('projects.detail.knowledgeViewFull')}</span>
              <div className="kb-card-meta">
                <span className="kb-source">{t('projects.detail.knowledgeSource', { type: k.source_type })}</span>
                {k.source_ref && <span className="kb-ref">{k.source_ref}</span>}
                <span className="kb-date">{new Date(k.created_at).toLocaleDateString()}</span>
              </div>
            </div>
          </a>
        ))}
      </div>
    );
  };

  if (loading) return <div className="detail-loading">{t('projects.detail.loading')}</div>;
  if (!project) return <div className="detail-loading">{t('projects.detail.notFound')}</div>;

  const selectedToken = tokens.find(t => t.id === platformForm.platform_token_id);

  return (
    <div className="project-detail">
      {/* Top header: breadcrumb trail, then the project's name + type tag,
          then its filesystem path as muted metadata. */}
      <div className="detail-header">
        <Breadcrumb items={[
          { label: t('projects.breadcrumb.list'), to: '/projects' },
          { label: project.name, title: project.name },
        ]} />
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
          {t('projects.detail.tabs.review')}
        </button>
        <button className={`tab-btn${tab === 'usage' ? ' active' : ''}`} onClick={() => setTab('usage')}>
          {t('projects.detail.tabs.usage')}
        </button>
        <button className={`tab-btn${tab === 'weekly' ? ' active' : ''}`} onClick={() => setTab('weekly')}>
          {t('projects.detail.tabs.weekly')}
        </button>
        <button className={`tab-btn${tab === 'issueMonitor' ? ' active' : ''}`} onClick={() => setTab('issueMonitor')}>
          {t('settings.issueMonitor.title')}
        </button>
        <button className={`tab-btn${tab === 'testing' ? ' active' : ''}`} onClick={() => setTab('testing')}>
          {t('projects.detail.tabs.testing')}
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
                      default_branch: project.default_branch ?? '',
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
                    {t('projects.detail.basicDefaultBranch')}
                  </label>
                  {/* datalist + free-text input: lets the user pick from the
                      project's known branches OR type any other ref. A blank
                      value means "use the platform default" — the backend
                      accepts "" and the wizard pipeline falls back to "main". */}
                  <input
                    className="form-input"
                    list="project-default-branch-options"
                    value={basicDraft.default_branch}
                    onChange={e => setBasicDraft(d => ({ ...d, default_branch: e.target.value }))}
                    placeholder={t('projects.detail.basicDefaultBranchPlaceholder')}
                  />
                  <datalist id="project-default-branch-options">
                    {availableBranches.map((b) => (
                      <option key={b} value={b} />
                    ))}
                  </datalist>
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
                {project.default_branch && (
                  <div className="info-row">
                    <span className="info-label">{t('projects.detail.basicInfoDefaultBranch')}</span>
                    <code className="info-code">{project.default_branch}</code>
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

          {/* Repo sync badge — mirrors projects.last_synced_at /
              last_synced_commit / sync_status stamped by the backend the
              moment it clones or fetches origin before architect-design runs.
              Renders inline (no modal, no toast) so the user can see at a
              glance whether the local checkout reflects upstream. The
              sync_status colour follows the existing CSS variables
              (--color-success / --color-error / --color-text-muted). */}
          <div className="detail-section" style={{ marginTop: 16 }}>
            <div className="section-header" style={{ marginBottom: 12, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
              <span style={{ fontWeight: 600, fontSize: 14 }}>{t('projects.detail.syncTitle')}</span>
              <button
                className="btn btn-secondary"
                onClick={handleSync}
                disabled={syncBusy || !project.remote_url}
                title={!project.remote_url ? t('projects.detail.syncNowSkipped') : ''}
                style={{ fontSize: 12, padding: '4px 12px' }}
              >
                {syncBusy ? t('projects.detail.syncNowBusy') : `🔄 ${t('projects.detail.syncNowBtn')}`}
              </button>
            </div>
            {(() => {
              const status = project.sync_status;
              const lastAt = project.last_synced_at;
              const sha = (project.last_synced_commit || '').slice(0, 7);
              if (status === 'ok' && lastAt) {
                return (
                  <div style={{ fontSize: 13, color: 'var(--color-success)' }}>
                    ✅ {t('projects.detail.syncStatusOK')}
                    {' · '}
                    {t('projects.detail.syncLastTime', { time: fmtDateTime(lastAt) })}
                    {sha && (
                      <>
                        {' '}
                        {t('projects.detail.syncShortSHA', { sha })}
                      </>
                    )}
                  </div>
                );
              }
              if (status === 'error') {
                return (
                  <div style={{ fontSize: 13, color: 'var(--color-error)' }}>
                    ❌ {t('projects.detail.syncStatusError')}
                    {lastAt && (
                      <>
                        {' · '}
                        {t('projects.detail.syncLastTime', { time: fmtDateTime(lastAt) })}
                      </>
                    )}
                  </div>
                );
              }
              // idle / undefined — project never went through the wizard or
              // has no remote configured. Don't show SHA/time.
              return (
                <div style={{ fontSize: 13, color: 'var(--color-text-muted)' }}>
                  ℹ️ {t('projects.detail.syncStatusIdle')}
                </div>
              );
            })()}
            {syncMsg && (
              <div style={{ marginTop: 8, fontSize: 12, color: syncMsg.ok ? 'var(--color-success)' : 'var(--color-error)' }}>
                {syncMsg.ok ? '✅ ' : '❌ '}{syncMsg.text}
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

          {/* Commit / PR language override — pins the language used by the
              wizard pipeline for commit messages, push summaries, and PR
              titles/bodies. Empty value = auto (use the git-history detected
              value). The active source (auto vs manual override) is surfaced
              inline so the user can tell which one is currently in effect. */}
          <div className="detail-section" style={{ marginTop: 16 }}>
            <div className="section-header" style={{ marginBottom: 12 }}>
              <span style={{ fontWeight: 600, fontSize: 14 }}>{t('projects.detail.commitLang.label')}</span>
              <span
                data-testid="commit-lang-source"
                style={{
                  fontSize: 12,
                  color: project.commit_lang_source === 'override'
                    ? 'var(--color-primary)'
                    : 'var(--color-text-muted)',
                  fontWeight: project.commit_lang_source === 'override' ? 600 : 400,
                }}
              >
                {project.commit_lang_source === 'override'
                  ? t('projects.detail.commitLang.overrideBadge')
                  : t('projects.detail.commitLang.sourceAuto')}
              </span>
            </div>
            <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
              <select
                className="form-input"
                style={{ flex: '1 1 200px', maxWidth: 280 }}
                value={project.commit_lang_override ?? ''}
                onChange={e => handleCommitLangChange(e.target.value)}
                disabled={commitLangSaving}
                aria-label={t('projects.detail.commitLang.label')}
              >
                <option value="">{t('projects.detail.commitLang.auto')}</option>
                <option value="zh">{t('projects.detail.commitLang.zh')}</option>
                <option value="en">{t('projects.detail.commitLang.en')}</option>
              </select>
              {commitLangSaving && (
                <span style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>
                  {t('projects.detail.basicSaving')}
                </span>
              )}
            </div>
            <div style={{ marginTop: 8, fontSize: 12, color: 'var(--color-text-muted)', lineHeight: 1.5 }}>
              {t('projects.detail.commitLang.hint')}
              <div style={{ marginTop: 6 }}>{t('projects.detail.aiContextHint')}</div>
              {project.commit_lang && project.commit_lang !== 'auto' && project.commit_lang_source !== 'override' && (
                <span style={{ marginLeft: 6 }}>
                  {t('projects.detail.commitLang.detectedLabel')}
                  <code style={{ background: 'var(--color-bg-2, #f1f5f9)', padding: '1px 6px', borderRadius: 4 }}>
                    {project.commit_lang}
                  </code>
                </span>
              )}
              {!project.commit_lang && (
                <span style={{ marginLeft: 6, fontStyle: 'italic' }}>
                  {t('projects.detail.commitLang.noDetect')}
                </span>
              )}
            </div>
            {commitLangMsg && (
              <div style={{ marginTop: 8, fontSize: 12, color: commitLangMsg.ok ? 'var(--color-success)' : 'var(--color-error)' }}>
                {commitLangMsg.ok ? '✅ ' : '❌ '}{commitLangMsg.text}
              </div>
            )}
          </div>

          {/* Commit / push strategy — choose which path the merge pipeline
              takes to source the commit message when the user leaves the
              field empty in the merge modal. The four strategies are
              documented inline; the script textarea only appears when the
              chosen mode requires it. Default = llm_only (legacy). */}
          <div className="detail-section" style={{ marginTop: 16 }}>
            <div className="section-header" style={{ marginBottom: 12 }}>
              <span style={{ fontWeight: 600, fontSize: 14 }}>{t('projects.detail.commitPush.label')}</span>
              <span style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>
                {t('projects.detail.commitPush.hint')}
              </span>
            </div>
            <div style={{ marginTop: 8, fontSize: 12, color: 'var(--color-text-muted)', lineHeight: 1.5 }}>
              {t('projects.detail.aiContextHint')}
            </div>
            <CommitPushConfigSection
              mode={project.commit_mode ?? 'llm_only'}
              script={project.commit_script ?? ''}
              saving={commitPushSaving}
              onSave={handleCommitPushSave}
            />
            {commitPushMsg && (
              <div style={{ marginTop: 8, fontSize: 12, color: commitPushMsg.ok ? 'var(--color-success)' : 'var(--color-error)' }}>
                {commitPushMsg.ok ? '✅ ' : '❌ '}{commitPushMsg.text}
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
            <div style={{ marginTop: 8, fontSize: 12, color: 'var(--color-text-muted)', lineHeight: 1.5 }}>
              {t('projects.detail.aiContextHint')}
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
                      <th className="req-tree-col" aria-label={t('requirements.tree.expand')} />
                      <th style={{ width: 110 }}>{t('projects.detail.colId')}</th>
                      {/* Type cell now hosts the priority dot ahead of the
                          kind-badge — the dot doesn't need its own column.
                          Widened 70→96 so the dot + badge stays on one line,
                          then 96→112 once we wrap the pair in a nowrap flex
                          row (.req-type-cell) so the wiki pill (12 chars) fits
                          without falling back to overflow ellipsis. */}
                      <th style={{ width: 112 }}>{t('projects.detail.colType')}</th>
                      <th>{t('projects.detail.colTitle')}</th>
                      <th style={{ width: 150 }}>{t('projects.detail.colStatus')}</th>
                      <th style={{ width: 140 }}>{t('projects.detail.colAgentServer')}</th>
                      <th style={{ width: 130 }}>{t('projects.detail.colTokens')}</th>
                      <th style={{ width: 110 }}>{t('projects.detail.colCost')}</th>
                      <th style={{ width: 144 }}>{t('projects.detail.colCreatedAt')}</th>
                      <th style={{ width: 144 }}>{t('projects.detail.colUpdatedAt')}</th>
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
          <div className="run-control-bar">
            <span className="compose-detect">
              {t('projects.detail.knowledgeCount', { n: knowledge.length })}
            </span>
            <button
              className="btn btn-primary btn-sm"
              onClick={() => setShowPdfUpload(true)}
              title={t('knowledge.uploadPdfTitle')}
            >
              📄 {t('knowledge.uploadPdf')}
            </button>
          </div>
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
          {showPdfUpload && id && (
            <PdfUploadDialog
              projectId={id}
              onClose={() => setShowPdfUpload(false)}
              onSuccess={() => {
                setShowPdfUpload(false);
                refreshKnowledge();
              }}
            />
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
            <button
              type="button"
              className={`btn btn-sm${showBatchImport ? ' btn-primary is-open' : ''}`}
              onClick={() => setShowBatchImport(v => !v)}
              aria-expanded={showBatchImport}
            >
              {showBatchImport ? t('projects.detail.reqBatchCollapse', { defaultValue: '关闭批量导入' }) : t('projects.detail.reqBatchOpen', { defaultValue: '批量导入 md' })}
            </button>
          </div>

          {/* Status filter (chip group) + text search. Mounted under the
              run-control-bar and ABOVE the composer so the controls stay
              visible even when the composer is collapsed.

              Status is rendered as a chip group instead of a <select>:
              every status is one tap away (no menu open / arrow-key dance),
              the active chip is obvious at a glance, and the group is
              keyboard-friendly via the implicit button focus ring. The
              chip pattern matches the cross-project RequirementsList so
              the two pages read as one family — `.project-req-chip` /
              `.project-req-chip.active` mirror `.req-kind-chip` semantics.

              Search is a single text box that filters on title OR
              description (case-insensitive contains). Reuses the global
              `.search-input` pill style from index.css so the affordance
              looks consistent with the search input on the cross-project
              list. The trailing × clear button shows up only when there's
              text — common UX pattern, matches RequirementsList. */}

          {/* Counter sits on the right side of the run-control-bar so users
              can see "本项目关联的需求（X）" even before they touch the
              filter. The shown/total counter (under the search box) shows
              the post-filter count, which is the more useful number once
              they've started narrowing things down. */}
          <div className="project-req-filter-bar">
            <div className="project-req-filter-chips" role="tablist" aria-label={t('projects.detail.reqFilterStatusLabel')}>
              <button
                type="button"
                role="tab"
                aria-selected={reqStatusFilter === 'active'}
                className={`project-req-chip project-req-chip-active${reqStatusFilter === 'active' ? ' is-active' : ''}`}
                onClick={() => setReqStatusFilter('active')}
              >
                {t('projects.detail.reqFilterActive')}
              </button>
              <button
                type="button"
                role="tab"
                aria-selected={reqStatusFilter === 'all'}
                className={`project-req-chip project-req-chip-all${reqStatusFilter === 'all' ? ' is-active' : ''}`}
                onClick={() => setReqStatusFilter('all')}
              >
                {t('projects.detail.reqFilterAll')}
              </button>
              <button
                type="button"
                role="tab"
                aria-selected={reqStatusFilter === 'done'}
                className={`project-req-chip project-req-chip-done${reqStatusFilter === 'done' ? ' is-active' : ''}`}
                onClick={() => setReqStatusFilter('done')}
              >
                {t('projects.detail.reqFilterDone')}
              </button>
              <button
                type="button"
                role="tab"
                aria-selected={reqStatusFilter === 'archived'}
                className={`project-req-chip project-req-chip-archived${reqStatusFilter === 'archived' ? ' is-active' : ''}`}
                onClick={() => setReqStatusFilter('archived')}
              >
                {t('projects.detail.reqFilterArchived')}
              </button>
            </div>
            <div className="search-input project-req-search">
              <span className="search-input-icon" aria-hidden="true">🔍</span>
              <input
                type="text"
                value={reqSearch}
                onChange={e => setReqSearch(e.target.value)}
                placeholder={t('projects.detail.reqSearchPlaceholder')}
                aria-label={t('projects.detail.reqSearchAriaLabel')}
              />
              {reqSearch && (
                <button
                  type="button"
                  className="search-input-clear"
                  aria-label={t('projects.detail.reqSearchClear')}
                  onClick={() => setReqSearch('')}
                >×</button>
              )}
            </div>
            <span className="project-req-filter-count">
              {t('projects.detail.reqCountFiltered', { shown: sortedReqs.length, total: reqs.length })}
            </span>
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
                  // Launch plan attached at creation time — the backend
                  // already dispatched (or tried to). Surface the dispatch
                  // result, then route to the detail page WITHOUT the
                  // autoStartDesign/autoStartCoding intent flags, because
                  // the job is already running (or already on the schedule)
                  // and re-prompting would mislead.
                  if (created.launch_error) {
                    // eslint-disable-next-line no-alert
                    window.alert(t('components.createRequirement.launchPlan.launchFailed', { reason: created.launch_error }));
                  }
                  if (created.launch_mode === 'immediate' || created.launch_mode === 'scheduled') {
                    navigate(`/requirements/${created.id}`);
                    return;
                  }
                  // No launch plan → original behavior, line-for-line.
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

          {showBatchImport && id && (
            <div className="pr-batch-import">
              <div className="pr-batch-import-row">
                <label className="pr-batch-import-label">
                  {t('projects.detail.reqBatchParentLabel', { defaultValue: '挂到父需求下（可选）' })}
                </label>
                <select
                  value={batchParentId}
                  onChange={e => setBatchParentId(e.target.value)}
                  style={{ minWidth: 240 }}
                >
                  <option value="">{t('projects.detail.reqBatchParentNone', { defaultValue: '无（顶层）' })}</option>
                  {reqs
                    .filter(r => !r.parent_id && r.status !== 'archived')
                    .map(r => (
                      <option key={r.id} value={r.id}>{r.title || r.id}</option>
                    ))}
                </select>
              </div>
              <div
                className="pr-batch-import-zone"
                onClick={() => batchInputRef.current?.click()}
                onDragOver={e => { e.preventDefault(); e.dataTransfer.dropEffect = 'copy'; }}
                onDrop={e => {
                  e.preventDefault();
                  if (e.dataTransfer.files.length > 0) void onBatchPick(e.dataTransfer.files);
                }}
              >
                <input
                  ref={batchInputRef}
                  type="file"
                  accept=".md"
                  multiple
                  style={{ display: 'none' }}
                  onChange={e => {
                    if (e.target.files) void onBatchPick(e.target.files);
                    e.target.value = '';
                  }}
                />
                <div className="pr-batch-import-zone-hint">
                  {t('projects.detail.reqBatchDropHint', { defaultValue: '点击或拖入 .md 文件，每条成为一个需求（标题 = 第一行）' })}
                </div>
              </div>
              {batchItems.length > 0 && (
                <div className="pr-batch-import-list">
                  <div className="pr-batch-import-list-head">
                    {t('projects.detail.reqBatchPreview', { defaultValue: '已解析 {{n}} 条', n: batchItems.length })}
                    <button
                      type="button"
                      className="btn btn-sm"
                      onClick={() => setBatchItems([])}
                      style={{ marginLeft: 8 }}
                    >
                      {t('projects.detail.reqBatchClear', { defaultValue: '清空' })}
                    </button>
                  </div>
                  <ul>
                    {batchItems.map(it => (
                      <li key={it.key}>
                        <span className="pr-batch-import-item-title">{it.title}</span>
                        <span className="pr-batch-import-item-meta">{(it.bytes / 1024).toFixed(1)} KB · {it.description.length} 字</span>
                      </li>
                    ))}
                  </ul>
                </div>
              )}
              {batchError && <pre className="pr-batch-import-err">{batchError}</pre>}
              <div className="pr-batch-import-actions">
                <button
                  type="button"
                  className="btn btn-primary"
                  disabled={batchBusy || batchItems.length === 0}
                  onClick={() => void submitBatch()}
                >
                  {batchBusy
                    ? t('projects.detail.reqBatchBusy', { defaultValue: '创建中…' })
                    : t('projects.detail.reqBatchSubmit', { count: batchItems.length, defaultValue: `创建 ${batchItems.length} 条` })}
                </button>
                <button
                  type="button"
                  className="btn"
                  onClick={() => { setShowBatchImport(false); setBatchItems([]); setBatchError(''); }}
                  disabled={batchBusy}
                >
                  {t('projects.detail.reqBatchCancel', { defaultValue: '取消' })}
                </button>
              </div>
            </div>
          )}

          {reqsLoading && <div className="tab-empty">{t('projects.detail.reqLoading')}</div>}
          {reqsError && <div className="tab-empty" style={{ color: 'var(--color-error)' }}>{reqsError}</div>}

          {!reqsLoading && !reqsError && reqs.length === 0 && (
            <div className="tab-empty"><p>{t('projects.detail.reqEmpty')}</p></div>
          )}

          {/* The project has requirements but the active filter / search
              shows none (e.g. user picked 'archived' but the project has
              zero archived rows, or typed a keyword that doesn't match).
              Reusing `reqEmpty` would lie ("项目暂无需求") since the
              project does have rows — a dedicated "filtered" copy keeps
              the hint accurate and nudges the user toward the controls. */}
          {!reqsLoading && !reqsError && reqs.length > 0 && sortedReqs.length === 0 && (
            <div className="tab-empty"><p>{t('projects.detail.reqEmptyFiltered')}</p></div>
          )}

          {!reqsLoading && !reqsError && sortedReqs.length > 0 && (
            <div className="pr-list">
              <table className="pr-table table-cards">
                <thead>
                  <tr>
                    {/* Sortable headers use the <SortHeader> component (defined
                        near the top of the file): clickable, shows the ↑ / ↓
                        arrow on the active column, sets aria-sort for screen
                        readers. Non-sortable columns (Type / Agent server /
                        Tokens / Cost) stay plain <th>. */}
                    <th className="req-tree-col" aria-label={t('requirements.tree.expand')} />
                    <SortHeader
                      col="id"
                      width={110}
                      label={t('projects.detail.colId')}
                      sortColumn={reqSortColumn}
                      sortDir={reqSortDir}
                      onSort={toggleSort}
                    />
                    {/* Type cell now hosts the priority dot ahead of the
                        kind-badge — see the overview recent table header
                        for the rationale. Widened 70→96 then 96→112 once we
                        wrapped the pair in a nowrap flex row (.req-type-cell)
                        so the wiki pill (12 chars) fits without falling back
                        to overflow ellipsis. */}
                    <th style={{ width: 112 }}>{t('projects.detail.colType')}</th>
                    <SortHeader
                      col="title"
                      label={t('projects.detail.colTitle')}
                      sortColumn={reqSortColumn}
                      sortDir={reqSortDir}
                      onSort={toggleSort}
                    />
                    {/* Priority column removed — the dot moved into the
                        Type cell above. Status widened 130→150 so a single
                        chip group with two stacked badges (e.g. "草稿 +
                        跳过方案") doesn't get forced onto a second row. */}
                    <SortHeader
                      col="status"
                      width={150}
                      label={t('projects.detail.colStatus')}
                      sortColumn={reqSortColumn}
                      sortDir={reqSortDir}
                      onSort={toggleSort}
                    />
                    <th style={{ width: 140 }}>{t('projects.detail.colAgentServer')}</th>
                    <th style={{ width: 130 }}>{t('projects.detail.colTokens')}</th>
                    <th style={{ width: 110 }}>{t('projects.detail.colCost')}</th>
                    <SortHeader
                      col="created_at"
                      width={144}
                      label={t('projects.detail.colCreatedAt')}
                      sortColumn={reqSortColumn}
                      sortDir={reqSortDir}
                      onSort={toggleSort}
                    />
                    <SortHeader
                      col="updated_at"
                      width={144}
                      label={t('projects.detail.colUpdatedAt')}
                      sortColumn={reqSortColumn}
                      sortDir={reqSortDir}
                      onSort={toggleSort}
                    />
                  </tr>
                </thead>
                <tbody>
                  {renderRequirementRows(pagedReqRows, { hierarchy: true })}
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
              {pageWindow(totalReqPages, curReqPage).map((p, i) =>
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
      {tab === 'review' && id && (
        <ProjectCodeReview projectId={id} project={project} />
      )}

      {/* ── Weekly report ── */}
      {tab === 'weekly' && id && (
        <div className="tab-content">
          <ProjectWeeklyReport projectId={id} />
        </div>
      )}

      {/* ── Issue monitor (v0.5.x GitHub auto-import) ── */}
      {tab === 'issueMonitor' && id && (
        <div className="tab-content">
          <ProjectIssueMonitor projectId={id} />
        </div>
      )}

      {/* ── 自动化测试（Playwright Test Agents）── */}
      {tab === 'testing' && id && (
        <ProjectTesting projectId={id} project={project} />
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