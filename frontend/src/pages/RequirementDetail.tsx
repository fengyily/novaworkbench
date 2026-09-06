import { useState, useEffect, useRef, useCallback, Fragment, type ReactNode, type CSSProperties } from 'react';
import { useParams, useNavigate, useLocation, Link } from 'react-router-dom';
import { requirementsApi, projectsApi, API_BASE, authedFetch, statusLabels, mergeApi, usageApi, usageTotalInput, fmtCost, stepLabels, rolesApi, claudeApi, wizardApi, agentServersApi, type AgentServer, type Requirement, type Project, type MergeState, type RequirementUsage, type UsageRow, kindLabels, kindOf, STAGE_VISIBILITY, type Kind, type CostItem } from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import DeepRefineChat from '../components/DeepRefineChat';
import DocRefineChat from '../components/DocRefineChat';
import ModelSelect from '../components/ModelSelect';
import AtMentionTextarea from '../components/AtMentionTextarea';
import SubTaskPanel from '../components/SubTaskPanel';
import { SummarizeToRequirementModal } from '../components/SummarizeToRequirementModal';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { exportDesignPdf } from '../utils/exportDesignPdf';
import { appendLogLine, coalesceLogLines, type LogLine, type UsageInfo, parseUsageSnapshots } from '../utils/logLines';
import { buildPhaseGroups, formatDuration, useTick } from '../utils/phaseGroups';
import { ContextUsageBar } from '../components/ContextUsageBar';
import { SessionContextStrip } from '../components/SessionContextStrip';
import './RequirementDetail.css';
import { FullscreenButton } from '../components/FullscreenButton';
import { useFullscreen } from '../utils/useFullscreen';

interface DesignData {
  overview?: string;
  files?: string[];
  steps?: string[];
  model_changes?: string;
  risks?: string[];
  plan_markdown?: string; // plan-mode output (raw markdown, not the legacy JSON schema)
}

// Decide whether the stored design document is long enough to warrant the
// "default collapsed / click to expand" treatment. The current rule: more than
// 12 newline-separated lines OR more than 1200 characters is "long". The
// check tolerates both the plan-mode markdown payload and the legacy JSON
// schema by sniffing the first character (`{` → JSON, otherwise treat the
// whole thing as markdown).
//
// Kept outside the component so it isn't recreated on every render — this is
// called from a useEffect that fires on req.id / req.design_docs changes.
function isLongDesignDoc(raw: string): boolean {
  if (!raw || !raw.trim()) return false;
  let body = raw;
  if (raw.trimStart().startsWith('{')) {
    try {
      const obj = JSON.parse(raw) as Partial<DesignData>;
      if (obj.plan_markdown) {
        body = obj.plan_markdown;
      } else {
        body = [
          obj.overview ?? '',
          ...(obj.files ?? []),
          ...(obj.steps ?? []),
          obj.model_changes ?? '',
          ...(obj.risks ?? []),
        ].join('\n');
      }
    } catch {
      // Fall through and treat raw as markdown.
    }
  }
  const lines = body.split('\n').length;
  const chars = body.length;
  return lines > 12 || chars > 1200;
}

// Two-role stage-gate lifecycle. Each gate is completed by a manual action.
// draft → analyzing → designing → designed → developing → done
type Stage = 'analyst' | 'architect' | 'developer' | 'done';

// Per-step emoji + accent color for the mobile token receipt. Color is the
// `--accent` custom property the receipt card uses for its left stripe and
// proportion-bar segment, so the visual identity of each stage is consistent
// across the hero bar and the individual cards. Defaults to a neutral slate
// for steps we don't have an opinion on (e.g. requirement_create).
const STAGE_VISUALS: Record<string, { icon: string; accent: string }> = {
  requirement_create: { icon: '🗂️', accent: '#94A3B8' },
  analyst_chat:       { icon: '🔍', accent: '#4F46E5' },
  architect_design:   { icon: '📐', accent: '#7C3AED' },
  refine_doc:         { icon: '✏️', accent: '#7C3AED' },
  apply_doc:          { icon: '🪄', accent: '#7C3AED' },
  coding:             { icon: '🚀', accent: '#0E7490' },
  developer_chat:     { icon: '💬', accent: '#0E7490' },
  adjust_coding:      { icon: '🛠️', accent: '#0E7490' },
  continue_coding:    { icon: '🔁', accent: '#0E7490' },
  merge:              { icon: '🔀', accent: '#059669' },
  review:             { icon: '🧐', accent: '#D97706' },
};

// Wizard-stage display order — used to sort the receipt cards so the user
// reads them in the order the steps actually ran (analyst → architect → dev)
// instead of by model. Anything not in this list falls through at the end.
const RECEIPT_STAGE_ORDER = [
  'requirement_create',
  'analyst_chat',
  'architect_design',
  'refine_doc',
  'apply_doc',
  'coding',
  'developer_chat',
  'adjust_coding',
  'continue_coding',
  'merge',
  'review',
];

function stageFor(status: string, skipDesign?: boolean): Stage {
  switch (status) {
    case 'draft':
      // "直接开发" (skip_design) 的草稿直接落在 developer 阶段：分析师/架构师
      // 区段不再渲染，详情页从「开始开发」入口进入。
      if (skipDesign) return 'developer';
      return 'analyst';
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
// Renders the design / coding / merge job's progress panel: groups phase
// + tool_call lines into named phases (with per-phase + per-tool-call
// durations), while message / result / error / done / conflict lines render
// as before so the AI summary and conflict list keep their ReactMarkdown
// styling.
function CodingLines({ lines, working }: { lines: LogLine[]; working?: boolean }) {
  // Re-render every 500ms while working so any trailing (still-active) phase
  // shows a live duration. The counter value is unused.
  useTick(!!working);
  const nodes: ReactNode[] = [];
  let key = 0;

  // Walk through the line stream. When we hit a `phase` line, accumulate
  // until the next `phase` (or end) and render the run as a single phase
  // block. message / result / error / done / conflict pass through.
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (line.type === 'phase') {
      const phaseStart = i;
      const group: LogLine[] = [];
      while (i < lines.length && lines[i].type === 'phase') {
        group.push(lines[i]);
        i++;
      }
      while (i < lines.length && lines[i].type === 'tool_call') {
        group.push(lines[i]);
        i++;
      }
      nodes.push(
        <PhaseBlock key={key++} group={group} working={!!working} phaseStartIdx={phaseStart} lines={lines} />,
      );
      continue;
    }
    if (line.type === 'message') {
      const group: string[] = [];
      while (i < lines.length && lines[i].type === 'message') {
        group.push(lines[i].content);
        i++;
      }
      nodes.push(
        <div key={key++} className="coding-line coding-line-message coding-message-md">
          <ReactMarkdown remarkPlugins={[remarkGfm]}>{group.join('')}</ReactMarkdown>
        </div>,
      );
      continue;
    }
    if (line.type === 'result') {
      // The "result" line is the dev-complete summary emitted as a single
      // LogLine (e.g. "全部完成。下面是实现总结。…" + Markdown). Render it
      // through ReactMarkdown too — otherwise the summary's headings/code
      // blocks/lists show as a garbled wall of plain text.
      nodes.push(
        <div key={key++} className="coding-line coding-line-message coding-message-md">
          <ReactMarkdown remarkPlugins={[remarkGfm]}>{line.content}</ReactMarkdown>
        </div>,
      );
      i++;
      continue;
    }
    nodes.push(
      <div key={key++} className={`coding-line coding-line-${line.type}`}>{line.content}</div>,
    );
    i++;
  }
  return <>{nodes}</>;
}

// One phase block: header (label + elapsed) + thinking line + tool_call rows.
// `group` is the consecutive run of phase + tool_call lines. `working` flips
// the trailing phase into live-tick mode.
function PhaseBlock({
  group,
  working,
  phaseStartIdx,
  lines,
}: {
  group: LogLine[];
  working: boolean;
  phaseStartIdx: number;
  lines: LogLine[];
}) {
  const phases = buildPhaseGroups(group);
  // The first phase in `group` is always the one the user is looking at;
  // mark it active iff `working` AND the next line in the original stream
  // (if any) isn't another phase boundary.
  const lastInOriginal = phaseStartIdx + group.length >= lines.length;
  const phase = phases[0];
  if (!phase) return null;
  const active = working && lastInOriginal && phase.isActive;
  const displayMs = active ? Date.now() - phase.startedAt : phase.durationMs;
  return (
    <div className={`coding-line-phase-group${active ? ' active' : ''}`}>
      <div className="coding-line-phase-header">
        <span className="coding-line-phase-label">{phase.label}</span>
        <span className="coding-line-phase-time">{formatDuration(displayMs)}</span>
      </div>
      {phase.thinking && (
        <div className="coding-line coding-line-phase">{phase.thinking.content}</div>
      )}
      {phase.toolCalls.map((tc, j) => (
        <div key={j} className="coding-line coding-line-tool_call">
          <span>{tc.content}</span>
          {tc.durationMs != null && tc.durationMs > 0 && (
            <span className="coding-line-entry-time">
              {' · '}{formatDuration(tc.durationMs)}
            </span>
          )}
        </div>
      ))}
    </div>
  );
}

// Strips wizard "knowledge" / "knowledge_result" events out of a log snapshot,
// returning the plain lines plus the parsed knowledge payload (used when
// replaying a job after a page refresh — the knowledge lines must never reach
// the coding/design panel). The result event carries per-entry usage, so the
// last payload wins.
type KnowledgeEntry = { title: string; used?: boolean };

function extractKnowledge(log: LogLine[]) {
  const lines: LogLine[] = [];
  let items: KnowledgeEntry[] = [];
  let empty = false;
  let any = false;
  for (const l of log) {
    if (l.type === 'knowledge' || l.type === 'knowledge_result') {
      any = true;
      try {
        const kb = JSON.parse(l.content ?? '{}') as { count?: number; items?: KnowledgeEntry[] };
        if (Array.isArray(kb.items)) items = kb.items;
        empty = (kb.count ?? 0) === 0;
      } catch { /* malformed frame — ignore */ }
    } else {
      // Preserve `at` so the phase timings survive a snapshot replay after a
      // page refresh mid-stage.
      lines.push(l);
    }
  }
  return { lines, items, empty, any };
}

// The wizard's optional "读取项目知识库" step display: rendered at the top of
// the architect / developer cards when a knowledge event arrived (i.e. the user
// opted in and the backend read the project knowledge base before the stage).
// Shows the titles read, a per-entry usage verdict (after the run emits
// "knowledge_result": ✅ 已引用 / not-directly referenced — a cheap signal, not
// an exact measurement), and a link to the full knowledge page. Hidden entirely
// when the option was not used (no knowledge event).
function KnowledgeReadPanel({ items, empty, projectId }: { items: KnowledgeEntry[]; empty: boolean; projectId?: string }) {
  if (items.length === 0 && !empty) return null;
  const usedCount = items.filter(k => k.used === true).length;
  const assessed = items.length > 0 && items.some(k => k.used !== undefined);
  return (
    <div className="knowledge-read-panel">
      <div className="knowledge-read-header">
        <span>📚 已读取项目知识库</span>
        {projectId && (
          <Link className="btn btn-sm knowledge-read-link" to={`/knowledge?project_id=${projectId}`}>
            查看知识库全文 →
          </Link>
        )}
      </div>
      {empty ? (
        <p className="knowledge-read-empty">暂无相关知识，已直接进入代码分析。</p>
      ) : (
        <>
          <p className="knowledge-read-count">已读取 {items.length} 条相关知识</p>
          <div className="knowledge-read-tags">
            {items.map((k, i) => (
              <span
                key={i}
                className={`knowledge-read-tag${k.used === true ? ' tag-used' : k.used === false ? ' tag-unused' : ''}`}
                title={k.used === true ? 'Claude 在本次运行中实际读取/引用了该知识' : k.used === false ? '未观测到直接使用痕迹（不必然等于无效）' : undefined}
              >
                {k.title}
              </span>
            ))}
          </div>
          {assessed && (
            <p className="knowledge-read-usage">
              评估：{usedCount}/{items.length} 条被 Claude 实际读取/引用（未标注者不代表无效，仅供参考）
            </p>
          )}
        </>
      )}
    </div>
  );
}

export default function RequirementDetail() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const location = useLocation();
  const [req, setReq] = useState<Requirement | null>(null);
  const [project, setProject] = useState<Project | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState('');
  // Independent fullscreen controllers for the three SSE panels that live
  // here (design / coding / merge). Each panel toggles its own state via a
  // toolbar button; CSS `.is-fullscreen` swaps the panel into a fixed
  // full-viewport surface without disturbing React state or the live SSE.
  const designFs = useFullscreen();
  const codingFs = useFullscreen();
  const mergeFs = useFullscreen();
  // Live "analyst turn running" signal lifted from DeepRefineChat, so the
  // header Claude-status badge is accurate during an in-flight turn.
  const [analystWorking, setAnalystWorking] = useState(false);

  // Per-stage model selection (analyst / architect / developer). Seeded once
  // from the server-persisted stage model so each dropdown defaults to 已设置
  // 的模型; a user switch is sent with the next stage POST. These are selectable
  // BEFORE the stage starts (draft / designing-empty / designed-empty gates).
  const [analystModel, setAnalystModel] = useState('');
  const [architectModel, setArchitectModel] = useState('');
  const [developerModel, setDeveloperModel] = useState('');
  // Agent-server selector for the developer stage. Empty string = local
  // execution (the historical default); non-empty = run claude on the chosen
  // remote target. Only `ready` servers are listed — the wizard refuses to
  // start coding on a target whose dependencies haven't been verified.
  const [agentServerId, setAgentServerId] = useState('');
  const [agentServers, setAgentServers] = useState<AgentServer[]>([]);
  useEffect(() => {
    agentServersApi.list()
      .then((rows) => setAgentServers((rows ?? []).filter((s) => s.status === 'ready')))
      .catch(() => {/* settings tab is the source of truth — silently ignore */});
  }, []);
  const modelSeedRef = useRef(false);
  useEffect(() => {
    if (!req || modelSeedRef.current) return;
    modelSeedRef.current = true;
    const norm = (m?: string) => (!m || m === '默认模型' ? '' : m);
    setAnalystModel(norm(req.analyst_model ?? ''));
    setArchitectModel(norm(req.architect_model ?? ''));
    setDeveloperModel(norm(req.developer_model ?? ''));
  }, [req]);

  // Effective default model per role (角色配置模型 > 生效 Claude 配置默认模型).
  // Used so ModelSelect's "默认模型" option shows the actual model name that
  // will run for each stage before the stage starts.
  const [roleDefaultModels, setRoleDefaultModels] = useState<Record<string, string>>({});
  useEffect(() => {
    let cancelled = false;
    Promise.all([rolesApi.list(), claudeApi.active()])
      .then(([roles, active]) => {
        if (cancelled) return;
        const configDefault = active?.default_model || '';
        const map: Record<string, string> = {};
        for (const r of roles ?? []) {
          // The role's model field may be empty (no override) or the literal
          // "默认模型" sentinel — both fall back to the config default.
          const rm = r.model && r.model !== '默认模型' ? r.model : '';
          if (map[r.key] === undefined) map[r.key] = rm || configDefault;
        }
        // Stages whose role row is missing still resolve to the config default.
        for (const k of ['analyst', 'architect', 'developer']) {
          if (!map[k]) map[k] = configDefault;
        }
        setRoleDefaultModels(map);
      })
      .catch(() => {});
    return () => { cancelled = true; };
  }, []);
  const analystDefaultModel = roleDefaultModels['analyst'] ?? '';
  const architectDefaultModel = roleDefaultModels['architect'] ?? '';
  const developerDefaultModel = roleDefaultModels['developer'] ?? '';
  const [codingLines, setCodingLines] = useState<LogLine[]>([]);
  const [coding, setCoding] = useState(false);
  // Live context-usage snapshots for the three wizard sessions. All three are
  // owned here (the page) — not inside each chat/panel component — because
  // context usage is a SESSION attribute: it must survive page refresh (seed
  // from req.usage_snapshots), panel collapse (design panel folds on success),
  // and stage transitions. The top SessionContextStrip reads all three live;
  // the in-panel ContextUsageBar reads the one for its stage. Live values are
  // fed back here from DeepRefineChat / DocRefineChat via onUsage callbacks
  // (analyst + design/coding-refine) and from the design/coding SSE handlers
  // below (which setDesignUsage / setCodingUsage directly).
  const [analystUsage, setAnalystUsage] = useState<UsageInfo | undefined>(undefined);
  // Live context-usage snapshot for the coding job (start-coding / 调整 / 继续),
  // pushed by the backend's `usage` SSE event. Rendered via ContextUsageBar at
  // the top of the coding-panel. The coding stage is multi-turn (--resume
  // coding_session_id), so compressible=true — the 压缩按钮 hands off to
  // wizardApi.compressContext(step:'coding') which summarizes + clears the
  // session, mirroring CodingChat / DeepRefineChat.
  const [codingUsage, setCodingUsage] = useState<UsageInfo | undefined>(undefined);
  const [codingCompressing, setCodingCompressing] = useState(false);
  const [codingCompressedAt, setCodingCompressedAt] = useState<string | null>(null);
  const [codingSummaryModal, setCodingSummaryModal] = useState<string | null>(null);
  const codingRef = useRef<HTMLDivElement>(null);
  const esRef = useRef<EventStream | null>(null);
  const extraDescRef = useRef('');
  // One-shot guard for the "auto-start design after skip-analysis creation"
  // flow. Set when the autoStartDesign navigation intent triggers the architect
  // stage so a subsequent refresh / req change doesn't re-fire it.
  const autoStartRef = useRef(false);
  // Live sub-task count: 0 until SubTaskPanel mounts and reports its current
  // list size via the onSubTasksChange callback, then stays in sync as the
  // panel creates / finishes children. Combined with req.sub_task_count
  // (seeded by the GET response) to decide whether to hide the requirement-
  // level "追加调整" composer. The state lives here (not just on req) so a
  // newly-created child agent immediately hides the composer without waiting
  // for the next refetch.
  const [liveSubTaskCount, setLiveSubTaskCount] = useState(0);

  // Branch modal state
  const [showBranchModal, setShowBranchModal] = useState(false);
  const [branchName, setBranchName] = useState('');
  const [baseBranch, setBaseBranch] = useState('');
  const [availableBranches, setAvailableBranches] = useState<string[]>([]);

  // Optional "读取项目知识库" step (default off). The dev checkbox lives in the
  // branch modal; the design one has its own confirm modal so the user can opt
  // in right before generating the technical plan.
  const [readKnowledgeDev, setReadKnowledgeDev] = useState(false);
  // Optional "是否拆分任务" switch (default off — i.e. 默认不拆分). When
  // checked, the backend runs the developer persona's task-decomposition
  // branch + auto-dispatches sub-agents. When unchecked, the backend runs the
  // developer persona in a "direct implementation" branch (no subtask split,
  // no auto-orchestration). Reset to false each time the branch modal opens so
  // the default is preserved across coding runs.
  const [splitTasksDev, setSplitTasksDev] = useState(false);
  const [showDesignKnowledgeModal, setShowDesignKnowledgeModal] = useState(false);
  const [readKnowledgeDesign, setReadKnowledgeDesign] = useState(false);
  const designNeedsTransitionRef = useRef(false);
  // Titles read by the backend, surfaced via the SSE "knowledge" event.
  const [knowledgeItems, setKnowledgeItems] = useState<KnowledgeEntry[]>([]);
  const [knowledgeEmpty, setKnowledgeEmpty] = useState(false);

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
  const [mergeLines, setMergeLines] = useState<LogLine[]>([]);
  const [merging, setMerging] = useState(false);
  const [conflictFiles, setConflictFiles] = useState<string[] | null>(null);
  const [prLink, setPrLink] = useState('');
  const mergeEsRef = useRef<EventStream | null>(null);

  // Streaming design state (architect phase)
  const [designLines, setDesignLines] = useState<LogLine[]>([]);
  const [designing, setDesigning] = useState(false);
  // Live context-usage snapshot for the architect-design job, pushed by the
  // backend's `usage` SSE event at the end of each claude turn. Rendered in
  // the design panel header via ContextUsageBar so the user can see how full
  // the plan-mode context is getting (plan-mode exploration can chew tokens).
  const [designUsage, setDesignUsage] = useState<UsageInfo | undefined>(undefined);
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

  // Collapsible design-doc state. Long design documents default to collapsed
  // (truncated with a fade-mask + "展开全文" button); short ones render in
  // full as before. Re-evaluated whenever the requirement or its stored
  // design_docs change, and any switch collapses the view back to its
  // default so the user isn't left with a stale "expanded" state.
  const [designExpanded, setDesignExpanded] = useState(false);
  const [isLongDesign, setIsLongDesign] = useState(false);
  useEffect(() => {
    setIsLongDesign(!!req?.design_docs && isLongDesignDoc(req.design_docs));
    setDesignExpanded(false);
  }, [req?.id, req?.design_docs]);

  // Edit modal state
  const [showEditModal, setShowEditModal] = useState(false);
  const [editTitle, setEditTitle] = useState('');
  const [editDesc, setEditDesc] = useState('');
  const [editPriority, setEditPriority] = useState('medium');
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
  // Per-invocation rows for the "追加调整" steps (adjust_coding +
  // developer_chat + continue_coding). Loaded in parallel with usage() so the
  // detailed history (each turn's model / tokens / cost / time / summary) is
  // always available when the rollup is.
  const [adjustRows, setAdjustRows] = useState<UsageRow[] | null>(null);
  const loadUsage = useCallback(async () => {
    if (!id) return;
    setUsageLoading(true);
    try {
      setUsage(await usageApi.requirement(id));
    } catch { /* ignore */ }
    try {
      const [chat, dev, cont] = await Promise.all([
        usageApi.rows(id, 'adjust_coding'),
        usageApi.rows(id, 'developer_chat'),
        usageApi.rows(id, 'continue_coding'),
      ]);
      setAdjustRows([...chat, ...dev, ...cont].sort((a, b) => (a.created_at < b.created_at ? -1 : 1)));
    } catch { /* ignore */ }
    finally { setUsageLoading(false); }
  }, [id]);

  // Seed the three session-usage snapshots from the persisted
  // requirements.usage_snapshots blob. This is what makes the usage bar +
  // top strip show the real last-known fill on page load / refresh instead
  // of dropping to 0% — usage is a session attribute, so it belongs on the
  // page and survives a remount. We only seed when a value is currently
  // undefined (so a live SSE value mid-turn isn't clobbered by a stale
  // persisted snapshot from a prior turn). Re-runs on every req refresh
  // (after a turn the backend writes a fresh snapshot + the GET re-fetches,
  // so the strip picks up the new value here too).
  useEffect(() => {
    if (!req) return;
    const snaps = parseUsageSnapshots(req.usage_snapshots);
    if (snaps.analyst_chat) setAnalystUsage(prev => prev ?? snaps.analyst_chat!);
    if (snaps.architect_design) setDesignUsage(prev => prev ?? snaps.architect_design!);
    if (snaps.coding) setCodingUsage(prev => prev ?? snaps.coding!);
  }, [req]);

  // ── Coding-stage context compression ───────────────────────────────────
  // Mirrors CodingChat / DeepRefineChat: summarize the current coding
  // session (coding_session_id), persist the summary, stamp
  // coding_compressed_at, and clear the session id so the next coding turn
  // sees the summary as prepended context instead of full history. The bar's
  // 压缩按钮 is only meaningful for the multi-turn coding stage (not the
  // one-shot plan-mode design stage).
  const handleCodingCompress = useCallback(async () => {
    if (!id || codingCompressing) return;
    if (!confirm('让 Claude 总结当前开发会话并压缩上下文？\n\n该操作会清空当前会话 ID,下次开发将看到压缩摘要而不是完整历史。')) return;
    setCodingCompressing(true);
    try {
      const data = await wizardApi.compressContext(id, 'coding');
      setCodingCompressedAt(data.compressed_at ?? null);
      // Reset usage so the bar doesn't keep reporting the soon-cleared
      // session's token counts; the next turn pushes a fresh snapshot.
      setCodingUsage(undefined);
    } catch (err: any) {
      alert('压缩失败:' + (err?.message || String(err)));
    } finally {
      setCodingCompressing(false);
    }
  }, [id, codingCompressing]);

  // Lazy fetch of the persisted summary text for the modal preview.
  const handleShowCodingSummary = useCallback(async () => {
    if (!id) return;
    try {
      const data = await wizardApi.getContextSummary(id, 'coding');
      setCodingSummaryModal(data.summary || '(暂无压缩摘要)');
    } catch {
      setCodingSummaryModal('(加载摘要失败)');
    }
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
    // Boot-fetch the coding stage's compression record so the bar's "📦 已压缩"
    // badge is correct after a page refresh, before the user clicks anything.
    wizardApi.getContextSummary(id, 'coding')
      .then(data => setCodingCompressedAt(data.compressed_at ?? null))
      .catch(() => { /* silent */ });
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

  // Promote an issue/idea (only allowed when status is done/archived) into a
  // full "requirement" so the developer stage becomes reachable. One-way gate:
  // the backend's UpdateKind rejects demotions back to issue/idea.
  const handlePromoteToRequirement = async () => {
    if (!id) return;
    if (!confirm('将该条目转为「需求」，并启用方案设计与开发流程？')) return;
    setBusy('转为需求');
    try {
      await requirementsApi.updateKind(id, 'requirement');
      await refresh();
    } catch (err: any) {
      alert('转为需求失败: ' + err.message);
    } finally {
      setBusy('');
    }
  };

  // ── Summarize-to-requirement modal (kind=idea) ────────────────────────────
  // The modal lives at the page level so any CTA that triggers "总结转需求"
  // (in the draft section, the chat header, or the done/archived footer) can
  // open the same component. onCreated navigates to the brand-new requirement
  // — the user lands on its freshly-minted detail page.
  const [summarizeOpen, setSummarizeOpen] = useState(false);

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
    // Fresh design run → drop the prior run's usage snapshot so the bar
    // doesn't briefly show a stale percentage before the first turn lands.
    setDesignUsage(undefined);

    designEsRef.current = createEventStream(
      `/api/wizard/jobs/${jobId}/stream`,
      (evt) => {
        if (evt.type === 'knowledge' || evt.type === 'knowledge_result') {
          // Optional knowledge pre-read: surface the read titles instead of
          // appending the raw line to the design panel. The result event
          // carries the per-entry used flag, so it wins over the earlier one.
          try {
            const kb = JSON.parse(evt.content ?? '{}') as { count?: number; items?: KnowledgeEntry[] };
            if (Array.isArray(kb.items)) setKnowledgeItems(kb.items);
            if (kb.count !== undefined) setKnowledgeEmpty(kb.count === 0);
          } catch { /* malformed frame — ignore */ }
          return;
        }
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
        // Live context-usage snapshot emitted at the end of every claude turn
        // (mirrors DeepRefineChat / DocRefineChat). Parse into UsageInfo so the
        // design panel's ContextUsageBar can render without re-parsing; compute
        // used/pct client-side so the bar fills before the backend stamps them.
        // Handled here — NOT appended to designLines — otherwise the raw JSON
        // shows up as a garbage "coding-line-usage" row in the thinking panel.
        if (evt.type === 'usage') {
          try {
            const parsed = JSON.parse(evt.content ?? '{}') as UsageInfo;
            const used = parsed.input_tokens + parsed.cache_creation_tokens + parsed.cache_read_tokens;
            const cw = parsed.context_window || 200000;
            const pct = cw > 0 ? (used / cw) * 100 : 0;
            setDesignUsage({ ...parsed, used, pct });
          } catch { /* malformed payload — ignore */ }
          return;
        }
        // Coalesce consecutive "模型思考中… (N tokens)" phase lines into one
        // updatable row instead of stacking one per heartbeat. Use the
        // backend-stamped `at` so phase timings stay accurate.
        const at = typeof evt.at === 'number' ? evt.at : Date.now();
        setDesignLines(prev => appendLogLine(prev, { type: evt.type, content: evt.content ?? '', at }));
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

  const runArchitectDesign = async (useKnowledge: boolean) => {
    if (!id) return;
    setDesigning(true);
    setDesignLines([]);
    setDesignError(false);
    // Reset the knowledge panel for a fresh run; without a new knowledge event
    // (option not used) the panel stays hidden.
    setKnowledgeItems([]);
    setKnowledgeEmpty(false);

    try {
      const res = await authedFetch(`${API_BASE}/api/wizard/architect-design`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          requirement_id: id,
          read_knowledge: useKnowledge,
          // Per-request model override — empty means the role's configured model.
          ...(architectModel ? { model: architectModel } : {}),
        }),
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

  // Design-entry gates first show a "read project knowledge?" checkbox modal
  // (default unchecked) — per 修改意见 "做成可选项，默认不勾选". needsTransition
  // covers the draft/analyzing entries that must move status to designing
  // before launching the job; status==='designing' entries run directly.
  const requestDesignKnowledge = (needsTransition: boolean) => {
    designNeedsTransitionRef.current = needsTransition;
    setReadKnowledgeDesign(false); // reset to default (unchecked) each time
    setShowDesignKnowledgeModal(true);
  };

  const confirmDesignKnowledge = async () => {
    setShowDesignKnowledgeModal(false);
    const useKnowledge = readKnowledgeDesign;
    if (designNeedsTransitionRef.current) {
      await transition('designing', '生成技术方案');
      await runArchitectDesign(useKnowledge);
    } else {
      await runArchitectDesign(useKnowledge);
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
      transition('designing', '生成技术方案').then(() => runArchitectDesign(false));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [req]);

  // Auto-open the branch selection modal when the user just created a
  // requirement with skip_design (navigated here with the autoStartCoding
  // intent flag). We deliberately stop at the branch modal — the 30-minute
  // coding job needs the user to confirm the dev branch first, matching the
  // manual "开始开发" interaction. One-shot guarded like autoStartDesign.
  useEffect(() => {
    if (!req || autoStartRef.current) return;
    const auto = (location.state as { autoStartCoding?: boolean } | null)?.autoStartCoding;
    if (!auto) return;
    if (req.skip_design && req.status === 'draft' && !req.coding_session_id) {
      autoStartRef.current = true;
      openBranchModal();
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
        const { status, exit_code, log } = json.data as { status: string; exit_code: number; log: LogLine[] };
        if (log && log.length > 0) {
          const kb = extractKnowledge(log);
          if (kb.items.length > 0 || kb.empty) { setKnowledgeItems(kb.items); setKnowledgeEmpty(kb.empty); }
          if (kb.lines.length > 0) setDesignLines(coalesceLogLines(kb.lines));
        }
        if (status === 'running') streamDesignJob(jobId);
        else { setDesigning(false); setDesignError(status === 'error' || exit_code !== 0); refresh(); }
      })
      .catch(() => setDesigning(false));
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id, req?.design_job_id]);

  useEffect(() => {
    if (designRef.current) designRef.current.scrollTop = designRef.current.scrollHeight;
  }, [designLines]);

  // ── Edit requirement (title/description/priority) ─────────────────────────
  const openEdit = () => {
    if (!req) return;
    setEditTitle(req.title);
    setEditDesc(req.description);
    setEditPriority(req.priority);
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
  // writing the localStorage "done" marker like the first coding pass. When
  // keepDone is set, persistDone additionally writes the "done" marker so a
  // refresh reloads THIS job's durable log — used by 继续开发, whose job log
  // becomes the new authoritative record replacing the one lost to a restart.
  // skipFirst is used on reconnect (mount-time restore): the caller has just
  // hydrated codingLines from the job snapshot, so we drop the first N SSE
  // events the backend replays (they are already in codingLines) and only
  // append new lines from there on. Without this, reconnecting to a running
  // job doubles every historical line — once from the snapshot, once from
  // the replay.
  const streamJob = useCallback((jobId: string, opts?: { keepDone?: boolean; persistDone?: boolean; skipFirst?: number }) => {
    if (esRef.current) esRef.current.close();
    setCoding(true);
    // Fresh coding stream → drop the prior usage snapshot so the bar doesn't
    // briefly show a stale percentage from a previous coding/adjust round.
    setCodingUsage(undefined);

    // Skip counter is captured per-stream — each new createEventStream call
    // resets it, so multiple reconnects within the same component lifetime
    // work correctly.
    let seenCount = 0;
    const skip = opts?.skipFirst ?? 0;

    esRef.current = createEventStream(
      `/api/wizard/jobs/${jobId}/stream`,
      (evt) => {
        // job_done is terminal — never skip it, even if it lands within the
        // replay window (it carries status/exit_code we need to act on).
        if (evt.type !== 'job_done') {
          // knowledge / message / tool_call / phase / error / done are all
          // subject to the replay-skip: they correspond 1:1 to backend LogLines
          // counted in `skip`.
          if (seenCount < skip) {
            seenCount++;
            return;
          }
          seenCount++;
        }
        if (evt.type === 'knowledge') {
          // Optional knowledge pre-read: surface the read titles instead of
          // appending the raw line to the coding panel.
          try {
            const kb = JSON.parse(evt.content ?? '{}') as { count?: number; items?: { title: string }[] };
            setKnowledgeItems(kb.items ?? []);
            setKnowledgeEmpty((kb.count ?? 0) === 0);
          } catch { /* malformed frame — ignore */ }
          return;
        }
        // Live context-usage snapshot (end of each claude turn). Parse into
        // UsageInfo and feed the coding-panel's ContextUsageBar; compute
        // used/pct client-side so the bar fills before the backend stamps
        // them. NOT appended to codingLines — otherwise the raw JSON shows
        // up as a garbage "coding-line-usage" row. (Subject to the replay-
        // skip above, so reconnect doesn't re-stamp a stale snapshot.)
        if (evt.type === 'usage') {
          try {
            const parsed = JSON.parse(evt.content ?? '{}') as UsageInfo;
            const used = parsed.input_tokens + parsed.cache_creation_tokens + parsed.cache_read_tokens;
            const cw = parsed.context_window || 200000;
            const pct = cw > 0 ? (used / cw) * 100 : 0;
            setCodingUsage({ ...parsed, used, pct });
          } catch { /* malformed payload — ignore */ }
          return;
        }
        if (evt.type === 'job_done') {
          esRef.current?.close();
          esRef.current = null;
          setCoding(false);
          const ok = evt.status === 'done' || evt.exit_code === 0;
          if (opts?.keepDone) {
            // 追加调整 / 继续开发: preserve current status, just refresh on success.
            if (ok) {
              if (opts.persistDone) {
                localStorage.setItem(`coding_job_${id}`, `done:${jobId}`);
              }
              refresh();
            }
          } else if (id && ok) {
            requirementsApi.updateStatus(id, 'developing').then(() => refresh());
            localStorage.setItem(`coding_job_${id}`, `done:${jobId}`);
          } else {
            localStorage.removeItem(`coding_job_${id}`);
          }
          return;
        }
        // Coalesce consecutive "模型思考中… (N tokens)" phase lines into one
        // updatable row instead of stacking one per heartbeat. Use the
        // backend-stamped `at` so phase timings stay accurate.
        const at = typeof evt.at === 'number' ? evt.at : Date.now();
        setCodingLines(prev => appendLogLine(prev, { type: evt.type, content: evt.content ?? '', at }));
      },
      () => {
        esRef.current = null;
        setCoding(false);
      },
    );
  }, [id, refresh]);

  const doStartCoding = async (bName: string, bBase: string, useKnowledge: boolean, splitTasks: boolean) => {
    if (!req || !project || !id) return;
    setCoding(true);
    setCodingLines([]);
    // Reset the knowledge panel for a fresh coding run; without a new knowledge
    // event (option not used) the panel stays hidden.
    setKnowledgeItems([]);
    setKnowledgeEmpty(false);

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
          read_knowledge: useKnowledge,
          // Whether to split the requirement into sub-tasks (developer persona
          // decomposition + auto-dispatch). Default false = do not split; the
          // backend runs the developer persona in its direct-implementation
          // branch (mirrors the agent role's behavior). Sent explicitly even
          // when false so the backend never sees a missing field.
          split_tasks: splitTasks,
          // Per-request model override — empty means the role's configured model.
          ...(developerModel ? { model: developerModel } : {}),
          // Remote Agent-server execution. Empty string = local execution (the
          // wizardH.StartCoding default branch handles the legacy path).
          ...(agentServerId ? { agent_server_id: agentServerId } : {}),
          // Remote Agent-server execution: empty string = local (legacy); a
          // server id routes the coding job through SSH instead.
          ...(agentServerId ? { agent_server_id: agentServerId } : {}),
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
    setReadKnowledgeDev(false); // default unchecked each time
    setSplitTasksDev(false); // default unchecked each time — 默认不拆分
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
    doStartCoding(branchName, baseBranch, readKnowledgeDev, splitTasksDev);
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
          // Per-request model override — empty means the role's configured model.
          ...(developerModel ? { model: developerModel } : {}),
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

  // ── 继续开发: recover from a backend restart that cleared the in-memory
  // coding log. Resumes coding_session_id (--resume) so Claude continues the
  // interrupted task and re-reports what was done; the new job's durable log
  // then "fills back" the lost development record. Unlike doAdjustCoding, no
  // user message is needed — the prompt is a system-generated "continue"
  // instruction. Only shown when status=developing and codingLines is empty.
  const doContinueCoding = async () => {
    if (!req || !id) return;
    setCoding(true);
    setCodingLines([]); // fresh continuation fills the panel back
    try {
      const res = await authedFetch(`${API_BASE}/api/wizard/continue-coding`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ requirement_id: id }),
      });
      const json = await res.json();
      const jobId = json.data?.job_id;
      if (!jobId) throw new Error(json.error?.message || '未获取到任务 ID');
      localStorage.setItem(`coding_job_${id}`, jobId);
      streamJob(jobId, { keepDone: true, persistDone: true });
    } catch (err: any) {
      setCodingLines([{ type: 'error', content: '❌ ' + err.message }]);
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
  const applyMergeSignals = useCallback((lines: LogLine[]) => {
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

    let acc: LogLine[] = [];
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
        const line: LogLine = {
          type: evt.type,
          content: evt.content ?? '',
          at: typeof evt.at === 'number' ? evt.at : Date.now(),
        };
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
        const { status, log } = json.data as { status: string; log: LogLine[] };
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
        const { status, log } = json.data as { status: string; log: LogLine[] };
        if (!log || log.length === 0) return;
        // rawCount = backend snapshot's total LogLine count, including
        // knowledge rows that extractKnowledge filters out of codingLines.
        // The SSE replay emits exactly `rawCount` events before the first
        // live one, so we pass this as skipFirst to streamJob — otherwise the
        // replay would re-append every historical line that the snapshot
        // already hydrated, doubling the entire history on the panel.
        const rawCount = log.length;
        const kb = extractKnowledge(log);
        if (kb.items.length > 0 || kb.empty) { setKnowledgeItems(kb.items); setKnowledgeEmpty(kb.empty); }
        if (kb.lines.length > 0) setCodingLines(coalesceLogLines(kb.lines));
        if (status === 'running') streamJob(savedJobId, { skipFirst: rawCount });
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
  // Requirement has at least one sub-task: either seeded by the GET response
  // (req.sub_task_count, present once the requirement has been decomposed)
  // or reported live by the mounted SubTaskPanel (liveSubTaskCount, covers
  // the brief window between "user clicks 创建子任务" and the next refetch).
  // Once true, the requirement-level "追加调整" composer is hidden — all
  // further adjustments must flow through the sub-task composer so the main
  // agent's task breakdown stays the source of truth.
  const hasSubTasks = (req.sub_task_count ?? 0) > 0 || liveSubTaskCount > 0;
  const stage = stageFor(req.status, req.skip_design);
  // Design (architect) stream state. While the job runs the panel stays open;
  // once finished it collapses behind the "思考过程" toggle.
  const designProcessActive = designing || !!req.design_job_id;
  // Keep the panel open while the job runs OR when the last run errored, so
  // the error line stays visible instead of collapsing behind the toggle.
  const designPanelOpen = designProcessActive || designError || showDesignProcess;
  const showDesignToggle = designLines.length > 0 && !designProcessActive && !designError;
  // Claude working status. Analysis signal comes from DeepRefineChat's live
  // onWorkingChange (the persisted analysis_job_id is only refreshed after a
  // turn finishes, so it lags during the turn); design/apply use the persisted
  // active job ids; coding/design add the local streaming states.
  const claudeWorking = coding || designing || analystWorking ||
    !!req.analysis_job_id || !!req.design_job_id || !!req.apply_job_id;
  // Per-stage working flags drive the model-switch disable (task requirement:
  // Claude 工作状态下禁止切换模型).
  const architectWorking = designing || !!req.design_job_id;

  const STEPS = [
    { key: 'analyst', label: '需求分析', icon: '🔍', doneStatus: 'designing', modelKey: 'analyst_model' as const },
    { key: 'architect', label: '方案设计', icon: '📐', doneStatus: 'designed', modelKey: 'architect_model' as const },
    { key: 'developer', label: '开发实现', icon: '🚀', doneStatus: 'done', modelKey: 'developer_model' as const },
  ] as const;
  // Per-kind stepper visibility: an Idea only walks the analyst stage.
  const reqKind: Kind = kindOf(req);
  const visibleStepKeys = STAGE_VISIBILITY[reqKind];
  const visibleSteps = STEPS.filter((s) => (visibleStepKeys as readonly string[]).includes(s.key));
  const stageIndex = stage === 'done' ? visibleSteps.length : visibleSteps.findIndex(s => s.key === stage);
  // 「转为需求」CTA visible for finished Issue / Idea rows.
  const showPromoteCta = (reqKind === 'idea' || reqKind === 'issue') && (req.status === 'done' || req.status === 'archived');
  // 「总结转需求」CTA — only for Idea, available the moment there's something
  // to summarize (chat started, OR status is done/archived). Draft + no chat
  // would summarize from the bare description which is fine too: the user may
  // want a quick jump from "rough idea text" to "structured requirement".
  const canSummarize = reqKind === 'idea' && (
    req.status === 'draft' || req.status === 'analyzing' ||
    req.status === 'done' || req.status === 'archived'
  );

  return (
    <div className="req-detail">
      {/* Optional "read project knowledge" confirm modal for the design stage */}
      {showDesignKnowledgeModal && (
        <div className="modal-overlay" onClick={() => setShowDesignKnowledgeModal(false)}>
          <div className="modal-box" onClick={e => e.stopPropagation()}>
            <h3>📐 生成技术方案</h3>
            <p style={{ fontSize: 13, color: 'var(--color-text-muted)', marginBottom: 8 }}>
              开始方案设计前，可选择先读取项目知识库中与需求相关的知识。
            </p>
            <label className="merge-check" style={{ margin: '8px 0 12px' }}>
              <input type="checkbox" checked={readKnowledgeDesign} onChange={e => setReadKnowledgeDesign(e.target.checked)} />
              📚 开始前先读取项目知识库（默认不勾选）
            </label>
            <div className="modal-actions btn-row-2col">
              <button className="btn btn-primary" onClick={confirmDesignKnowledge}>确认</button>
              <button className="btn" onClick={() => setShowDesignKnowledgeModal(false)}>取消</button>
            </div>
          </div>
        </div>
      )}

      {/* Branch modal — pre-flight checklist */}
      {showBranchModal && (() => {
        // Derived values for the flight-strip status bar. Built once per
        // render so the strip stays consistent with the form state.
        const stripBase = baseBranch || 'main';
        const stripNew = branchName || (req ? `feat/${req.id}` : '');
        const stripEnv = agentServerId
          ? (agentServers.find(s => s.id === agentServerId)?.name || 'remote')
          : 'local';
        const stripModel = developerModel || developerDefaultModel || 'default';
        return (
          <div className="modal-overlay" onClick={() => setShowBranchModal(false)}>
            <div className="modal-box preflight-box" onClick={e => e.stopPropagation()}>
              <div className="preflight-header">
                <div className="preflight-eyebrow">Pre-flight</div>
                <h3 className="preflight-title">启动开发会话</h3>
                <p className="preflight-subtitle">
                  Claude 将按下列配置进入编码执行阶段。
                </p>
              </div>

              {/* Flight strip — the signature element. Compresses the
                  configured mission into one monospace line the user can
                  scan at a glance before launching. */}
              <div className="flight-strip" aria-label="配置摘要">
                <span className="flight-leg">
                  <span className="flight-leg-label">GIT</span>
                  <span className="flight-leg-value" title={stripBase}>{stripBase}</span>
                  <span className="flight-arrow">→</span>
                  <span className="flight-leg-value" title={stripNew}>{stripNew}</span>
                </span>
                <span className="flight-sep">·</span>
                <span className="flight-leg">
                  <span className="flight-leg-label">EXEC</span>
                  <span className="flight-leg-value" title={stripEnv}>{stripEnv}</span>
                  <span className="flight-arrow">·</span>
                  <span className="flight-leg-value" title={stripModel}>{stripModel}</span>
                </span>
                <span className="flight-ready" aria-live="polite">
                  <span className="flight-ready-dot" />
                  READY
                </span>
              </div>

              <div className="preflight-body">
                <div className="preflight-section">
                  <div className="preflight-section-label">Git · 工作分支</div>
                  {/* Branch fields wear the same rail+chip+monospace card as
                      ModelSelect, only tinted for the Git section. Keeps the
                      panel's three editable surfaces speaking one vocabulary. */}
                  <div className="modal-field">
                    <label>基础分支（从哪里签出）</label>
                    <div className="preflight-field-card preflight-field-card--git">
                      <span className="preflight-field-chip" aria-hidden="true">BASE</span>
                      <select
                        className="form-input preflight-field-input"
                        value={baseBranch}
                        onChange={e => setBaseBranch(e.target.value)}
                      >
                        {availableBranches.length === 0 && <option value={baseBranch}>{baseBranch}</option>}
                        {availableBranches.map(b => <option key={b} value={b}>{b}</option>)}
                      </select>
                    </div>
                  </div>
                  <div className="modal-field">
                    <label>新分支名</label>
                    <div className="preflight-field-card preflight-field-card--git">
                      <span className="preflight-field-chip" aria-hidden="true">NEW</span>
                      <input
                        className="form-input preflight-field-input"
                        list="branch-suggestions"
                        value={branchName}
                        onChange={e => setBranchName(e.target.value)}
                        placeholder={`feat/${req.id}`}
                      />
                    </div>
                    <datalist id="branch-suggestions">
                      {availableBranches.map(b => <option key={b} value={b} />)}
                    </datalist>
                  </div>
                </div>

                <div className="preflight-section">
                  <div className="preflight-section-label">Execution · 执行计划</div>
                  {/* Agent-server selector: empty = local execution (legacy default).
                      Only ready servers are listed; the wizard remote branch refuses
                      to start on a non-ready target so this stays consistent with the
                      server-side guard. Violet-tinted field card — distinguishes
                      "how to run" from the Git section's "where to run". */}
                  <div className="modal-field">
                    <label>执行环境</label>
                    <div className="preflight-field-card preflight-field-card--exec">
                      <span className="preflight-field-chip" aria-hidden="true">ENV</span>
                      <select
                        className="form-input preflight-field-input"
                        value={agentServerId}
                        onChange={e => setAgentServerId(e.target.value)}
                        disabled={coding}
                        title={agentServers.length === 0 ? '设置 → Agent 服务器 添加一台并完成环境检查后可用' : ''}
                      >
                        <option value="">本地执行</option>
                        {agentServers.map(s => (
                          <option key={s.id} value={s.id}>{s.name} ({s.host})</option>
                        ))}
                      </select>
                    </div>
                    {agentServers.length === 0 && (
                      <div style={{ fontSize: 12, color: 'var(--color-text-muted)', marginTop: 4 }}>
                        未配置就绪的 Agent 服务器，请在「设置 → Agent 服务器」中添加并检查环境。
                      </div>
                    )}
                  </div>
                  {/* Developer-stage model selection, visible right before launching
                      the coding job. Disabled while a coding job runs (Claude 工作中
                      禁止切换模型). */}
                  <div className="modal-field">
                    <ModelSelect
                      value={developerModel}
                      onChange={setDeveloperModel}
                      disabled={coding}
                      working={coding}
                      stage="developer"
                      label="开发模型"
                      defaultModelName={developerDefaultModel}
                      title={coding ? 'Claude 正在开发中，暂不能切换模型' : '开发实现阶段使用的模型，开始前即可选择'}
                    />
                  </div>
                </div>

                <div className="preflight-section">
                  <div className="preflight-section-label">Strategy · 上下文与策略</div>
                  {/* Optional knowledge pre-read: default unchecked. When checked, the
                      coding job first reads the project knowledge base relevant to the
                      requirement before diving into the code. */}
                  <label className={`preflight-toggle ${readKnowledgeDev ? 'is-checked' : ''}`}>
                    <input type="checkbox" checked={readKnowledgeDev} onChange={e => setReadKnowledgeDev(e.target.checked)} />
                    <div className="preflight-toggle-body">
                      <div className="preflight-toggle-title">📚 读取项目知识库</div>
                      <div className="preflight-toggle-desc">
                        编码前先扫描项目内与本需求相关的知识条目，作为额外上下文注入。耗时约几秒，对复杂需求特别有用。
                      </div>
                    </div>
                  </label>
                  {/* Optional sub-task decomposition switch: default unchecked (= 直接执行，不拆分子任务). When checked, the developer persona emits a subtasks.json + [SUBTASKS_READY] sentinel and the backend auto-dispatches sub-agents. When unchecked, the developer persona runs in direct-implementation mode (no sub-task orchestration). Mirrors readKnowledgeDev's pattern: reset to false in openBranchModal, explicit value (even when false) on the wire. */}
                  <label className={`preflight-toggle ${splitTasksDev ? 'is-checked' : ''}`}>
                    <input type="checkbox" checked={splitTasksDev} onChange={e => setSplitTasksDev(e.target.checked)} />
                    <div className="preflight-toggle-body">
                      <div className="preflight-toggle-title">🧩 拆分任务并自动派发</div>
                      <div className="preflight-toggle-desc">
                        由主 Agent 把需求拆成子任务，再串行调度子 Agent 执行。默认关闭—— developer persona 会直接实现，更快。
                      </div>
                    </div>
                  </label>
                </div>
              </div>

              <div className="preflight-launch">
                <button className="btn-launch" onClick={confirmBranchAndStart}>
                  🚀 启动会话
                </button>
                <button className="btn-cancel" onClick={() => setShowBranchModal(false)}>
                  取消
                </button>
              </div>
            </div>
          </div>
        );
      })()}

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
                <div className="modal-actions btn-row-2col">
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
                <div className="modal-actions btn-row-2col">
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
              <input className="form-input" value={editTitle} onChange={e => setEditTitle(e.target.value)} />
            </div>
            <div className="modal-field">
              <label>描述</label>
              <AtMentionTextarea
                className="form-input form-textarea"
                rows={6}
                value={editDesc}
                onChange={setEditDesc}
                placeholder="输入 @ 可引用 Skill，例如 @frontend"
              />
            </div>
            <div className="modal-field">
              <label>优先级</label>
              <select className="form-input" value={editPriority} onChange={e => setEditPriority(e.target.value)}>
                <option value="low">low</option>
                <option value="medium">medium</option>
                <option value="high">high</option>
                <option value="critical">critical</option>
              </select>
            </div>
            {/* skip_analysis toggle — only meaningful before architect-design runs */}
            {req && (req.status === 'draft' || req.status === 'analyzing') && (
              <div className="modal-field">
                <label className="edit-skip-row">
                  <input type="checkbox" checked={editSkipAnalysis}
                    onChange={e => setEditSkipAnalysis(e.target.checked)} />
                  跳过需求分析，直接进入方案设计
                </label>
                <div className="form-hint">
                  勾选后在详情页主操作变为「生成技术方案」；取消勾选则恢复「开始需求分析」入口。
                </div>
              </div>
            )}
            <div className="modal-actions btn-row-2col">
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
          {canSummarize && (
            <button
              className="btn btn-sm"
              onClick={() => setSummarizeOpen(true)}
              title="将整段讨论总结为新的可开发需求（不会修改原想法）"
            >
              📋 总结转需求
            </button>
          )}
          <button className="btn btn-sm" onClick={openEdit}>✏️ 编辑</button>
          <button className="btn btn-sm btn-danger" onClick={handleDelete}>🗑️ 删除</button>
        </div>
      </div>

      <h1>{req.title}</h1>

      <div className="detail-meta">
        <span className={`kind-badge kind-${reqKind}`} title={reqKind === 'idea' ? '想法 — 仅讨论方案，不进入开发' : reqKind === 'issue' ? '问题 — 排查根因并修复' : '需求 — 标准 3 阶段实现'}>{kindLabels[reqKind]}</span>
        <span className={`status-badge status-${req.status}`}>{statusLabels[req.status] || req.status}</span>
        <span className={`priority-tag ${req.priority}`}>{req.priority.toUpperCase()}</span>
        <span className={`claude-status${claudeWorking ? ' working' : ''}`} title={claudeWorking ? 'Claude 正在执行分析/方案/开发任务' : '当前无 Claude 任务在运行'}>
          {claudeWorking ? '🤖 Claude 工作中' : '😴 Claude 空闲'}
        </span>
        {project && <span className="project-tag">📁 {project.name}</span>}
        {req.source_requirement_id && (
          <Link
            to={`/requirements/${req.source_requirement_id}`}
            className="source-link"
            title="点击查看来源想法的讨论"
          >
            ← 来源想法
          </Link>
        )}
        {/* Per-stage "已压缩" badges. Each one is a passive indicator of
            whether that wizard stage's session has been summarized into the
            {step}_context_summary column — the chat components own the
            compress button + summary modal, this is just a header-level
            reminder so the user knows "压缩上下文" has been used without
            scrolling into the chat panel. Hover for the timestamp. */}
        {req.analyst_compressed_at && (
          <span
            className="compressed-badge"
            title={`需求分析已于 ${req.analyst_compressed_at} 压缩`}
          >
            📦 分析已压缩
          </span>
        )}
        {req.design_compressed_at && (
          <span
            className="compressed-badge"
            title={`方案设计已于 ${req.design_compressed_at} 压缩`}
          >
            📦 设计已压缩
          </span>
        )}
        {req.coding_compressed_at && (
          <span
            className="compressed-badge"
            title={`开发调整已于 ${req.coding_compressed_at} 压缩`}
          >
            📦 开发已压缩
          </span>
        )}
      </div>

      {/* Always-on session-context strip. Sits in the header so it survives
          stage completion / panel collapse / page refresh — the whole point
          of making usage a session-level attribute. Reads the three live
          usage values (seeded from req.usage_snapshots, updated by SSE /
          onUsage) and the compressed_at badges. */}
      <SessionContextStrip
        analyst={analystUsage}
        design={designUsage}
        coding={codingUsage}
        req={req}
      />

      {req.description && (
        <div className="detail-desc spec-card">
          <div className="spec-card-tag" aria-hidden>
            <span className="spec-card-tag-label">BRIEF</span>
            <span className="spec-card-tag-date">{req.created_at?.slice(0, 10) ?? ''}</span>
          </div>
          <div className="spec-card-body">
            <div className="analysis-summary">
              <ReactMarkdown remarkPlugins={[remarkGfm]}>{req.description}</ReactMarkdown>
            </div>
          </div>
        </div>
      )}

      {/* Token usage — per-step breakdown + total for this requirement.
          input = input_tokens + cache_creation + cache_read (billed input). */}
      <div className="detail-section usage-section ledger">
        <div className="section-header ledger-header" style={{ marginBottom: 10 }}>
          <span className="ledger-title">
            <span className="ledger-title-mark" aria-hidden />
            Token 消耗 · 账目
          </span>
          {usageLoading && <span className="ledger-loading">刷新中…</span>}
        </div>
        {usage && usage.by_step.length > 0 ? (
          <>
            {/* Mobile-only receipt layout — replaced by CSS at ≤768px to give
                the section a "bill" feel: hero total + segmented proportion
                bar, then one card per stage with a thin left stripe in the
                stage's accent color. The desktop table below stays untouched
                for ≥769px viewports. */}
            {(() => {
              const primaryCost = (c?: CostItem[]): number => (c && c.length ? c[0].amount : 0);
              const fmtCount = (n: number) => n >= 1000 ? `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}k` : n.toLocaleString();
              const stages = new Map<string, {
                key: string; label: string; icon: string; accent: string;
                cost: number; costs: CostItem[]; count: number;
                input: number; output: number; cacheRead: number; cacheCreate: number;
                models: string[];
              }>();
              for (const s of usage.by_step) {
                const visual = STAGE_VISUALS[s.step] ?? { icon: '⚙️', accent: '#94A3B8' };
                const cur = stages.get(s.step) ?? {
                  key: s.step,
                  label: s.label || stepLabels[s.step] || s.step,
                  icon: visual.icon,
                  accent: visual.accent,
                  cost: 0, costs: [] as CostItem[], count: 0,
                  input: 0, output: 0, cacheRead: 0, cacheCreate: 0,
                  models: [],
                };
                cur.cost += primaryCost(s.costs);
                if (s.costs && s.costs.length) cur.costs = cur.costs.concat(s.costs);
                cur.count += s.count;
                cur.input += s.input_tokens;
                cur.output += s.output_tokens;
                cur.cacheRead += s.cache_read_tokens;
                cur.cacheCreate += s.cache_creation_tokens;
                if (s.model && !cur.models.includes(s.model)) cur.models.push(s.model);
                stages.set(s.step, cur);
              }
              const ordered = Array.from(stages.values()).sort((a, b) => {
                const ai = RECEIPT_STAGE_ORDER.indexOf(a.key);
                const bi = RECEIPT_STAGE_ORDER.indexOf(b.key);
                return (ai === -1 ? 999 : ai) - (bi === -1 ? 999 : bi);
              });
              const totalCost = ordered.reduce((acc, s) => acc + s.cost, 0);
              const totalCount = ordered.reduce((acc, s) => acc + s.count, 0);
              const totalTokens = usage.total.input_tokens + usage.total.output_tokens
                + usage.total.cache_creation_tokens + usage.total.cache_read_tokens;
              const cacheHitRate = (usage.total.input_tokens + usage.total.cache_creation_tokens + usage.total.cache_read_tokens) > 0
                ? usage.total.cache_read_tokens / (usage.total.input_tokens + usage.total.cache_creation_tokens + usage.total.cache_read_tokens)
                : 0;
              return (
                <div className="usage-receipt">
                  <div className="usage-receipt-hero">
                    <div className="usage-receipt-eyebrow">本需求总费用</div>
                    <div className="usage-receipt-total">{fmtCost(usage.total.costs)}</div>
                    <div className="usage-receipt-meta">
                      <span>{totalCount.toLocaleString()} 次调用</span>
                      <span className="usage-receipt-dot">·</span>
                      <span>{fmtCount(totalTokens)} tokens</span>
                      {cacheHitRate > 0 && (
                        <>
                          <span className="usage-receipt-dot">·</span>
                          <span className="usage-receipt-cache">
                            缓存命中 {Math.round(cacheHitRate * 100)}%
                          </span>
                        </>
                      )}
                    </div>
                    {ordered.length > 0 && (
                      <div className="usage-receipt-bar" role="img" aria-label="各阶段费用占比">
                        {ordered.map(s => totalCost > 0 ? (
                          <div
                            key={s.key}
                            className="usage-receipt-bar-seg"
                            style={{
                              width: `${(s.cost / totalCost) * 100}%`,
                              background: s.accent,
                            }}
                            title={`${s.label}：${Math.round((s.cost / totalCost) * 100)}%`}
                          />
                        ) : null)}
                      </div>
                    )}
                    {ordered.length > 0 && (
                      <div className="usage-receipt-legend">
                        {ordered.map(s => (
                          <span key={s.key} className="usage-receipt-legend-item">
                            <span className="usage-receipt-legend-swatch" style={{ background: s.accent }} />
                            <span>{s.icon} {s.label}</span>
                            <span className="usage-receipt-legend-pct">
                              {totalCost > 0 ? Math.round((s.cost / totalCost) * 100) : 0}%
                            </span>
                          </span>
                        ))}
                      </div>
                    )}
                  </div>
                  <div className="usage-receipt-cards">
                    {ordered.map(s => (
                      <div
                        key={s.key}
                        className="usage-receipt-card"
                        style={{ '--accent': s.accent } as CSSProperties}
                      >
                        <div className="usage-receipt-card-head">
                          <div className="usage-receipt-card-title">
                            <span className="usage-receipt-card-icon" aria-hidden>{s.icon}</span>
                            <span className="usage-receipt-card-label">{s.label}</span>
                          </div>
                          <div className="usage-receipt-card-cost">{fmtCost(s.costs)}</div>
                        </div>
                        <div className="usage-receipt-card-sub">
                          {s.models.length > 0 ? (
                            <span className="usage-receipt-card-model" title={s.models.join(', ')}>
                              {s.models.length === 1
                                ? s.models[0]
                                : `${s.models[0]} +${s.models.length - 1}`}
                            </span>
                          ) : (
                            <span className="usage-receipt-card-model">未知模型</span>
                          )}
                          <span className="usage-receipt-card-dot">·</span>
                          <span>{s.count.toLocaleString()} 次调用</span>
                        </div>
                        <div className="usage-receipt-card-bar" aria-hidden>
                          <div
                            className="usage-receipt-card-bar-fill"
                            style={{ width: totalCost > 0 ? `${(s.cost / totalCost) * 100}%` : '0%' }}
                          />
                        </div>
                        <div className="usage-receipt-card-stats">
                          <div className="usage-receipt-card-stat">
                            <span className="usage-receipt-card-stat-label">输入</span>
                            <span className="usage-receipt-card-stat-value">{fmtCount(s.input + s.cacheRead + s.cacheCreate)}</span>
                          </div>
                          <div className="usage-receipt-card-stat">
                            <span className="usage-receipt-card-stat-label">输出</span>
                            <span className="usage-receipt-card-stat-value">{fmtCount(s.output)}</span>
                          </div>
                        </div>
                        {(s.cacheRead > 0 || s.cacheCreate > 0) && (
                          <div className="usage-receipt-card-cache">
                            <span>缓存读 {s.cacheRead.toLocaleString()}</span>
                            {s.cacheCreate > 0 && <span>· 缓存建 {s.cacheCreate.toLocaleString()}</span>}
                          </div>
                        )}
                      </div>
                    ))}
                  </div>
                </div>
              );
            })()}

            <table className="pr-table table-cards" style={{ marginBottom: 8 }}>
              <thead>
                <tr>
                  <th>步骤</th>
                  <th style={{ width: 170 }}>模型</th>
                  <th style={{ width: 110 }}>输入</th>
                  <th style={{ width: 110 }}>输出</th>
                  <th style={{ width: 110 }}>缓存读</th>
                  <th style={{ width: 110 }}>缓存建</th>
                  <th style={{ width: 110 }}>费用</th>
                  <th style={{ width: 60 }}>次数</th>
                </tr>
              </thead>
              <tbody>
                {usage.by_step.map(s => (
                  <Fragment key={`${s.step}:${s.model}`}>
                    <tr>
                      <td data-label="步骤">{s.label || stepLabels[s.step] || s.step}</td>
                      <td data-label="模型"><code className="pr-branch">{s.model || '未知模型'}</code></td>
                      <td data-label="输入" style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{usageTotalInput(s).toLocaleString()}</td>
                      <td data-label="输出" style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{s.output_tokens.toLocaleString()}</td>
                      <td data-label="缓存读" style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{s.cache_read_tokens.toLocaleString()}</td>
                      <td data-label="缓存建" style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{s.cache_creation_tokens.toLocaleString()}</td>
                      <td data-label="费用" style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{fmtCost(s.costs)}</td>
                      <td data-label="次数" style={{ fontSize: 12 }}>{s.count}</td>
                    </tr>
                  </Fragment>
                ))}
                <tr className="table-cards-total" style={{ borderTop: '2px solid var(--color-border)' }}>
                  <td data-label="合计" style={{ fontWeight: 600 }}>合计</td>
                  <td></td>
                  <td data-label="输入" style={{ fontFamily: 'var(--font-mono)', fontSize: 12, fontWeight: 600 }}>{usageTotalInput(usage.total).toLocaleString()}</td>
                  <td data-label="输出" style={{ fontFamily: 'var(--font-mono)', fontSize: 12, fontWeight: 600 }}>{usage.total.output_tokens.toLocaleString()}</td>
                  <td data-label="缓存读" style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{usage.total.cache_read_tokens.toLocaleString()}</td>
                  <td data-label="缓存建" style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{usage.total.cache_creation_tokens.toLocaleString()}</td>
                  <td data-label="费用" style={{ fontFamily: 'var(--font-mono)', fontSize: 12, fontWeight: 600 }}>{fmtCost(usage.total.costs)}</td>
                  <td data-label="次数"></td>
                </tr>
              </tbody>
            </table>
            <small style={{ color: 'var(--color-text-muted)', fontSize: 12 }}>
              输入 = 直接输入 + 缓存读 + 缓存建（均按计费输入计入）。仅统计已完成的完整调用。
            </small>

            {adjustRows && adjustRows.length > 0 && (
              <div className="adjust-history">
                <div className="adjust-history-title">
                  📋 追问 / 调整历史（{adjustRows.length} 次）
                </div>
                <div className="adjust-history-list">
                  {adjustRows.map((r, i) => (
                    <div key={r.id} className="adjust-history-card">
                      <div className="adjust-history-head">
                        <span className="adjust-history-head-left">
                          <span className="adjust-history-index">#{i + 1}</span>
                          <span className="adjust-history-stage">
                            {stepLabels[r.step] || r.step}
                          </span>
                          <code className="pr-branch adjust-history-model">{r.model || '未知模型'}</code>
                        </span>
                        <span className="adjust-history-time">
                          {new Date(r.created_at).toLocaleString()}
                        </span>
                      </div>
                      {r.summary && (
                        <div className="adjust-history-summary">
                          📤 {r.summary}
                        </div>
                      )}
                      <div className="adjust-history-stats">
                        <span>输入 <strong>{usageTotalInput(r).toLocaleString()}</strong></span>
                        <span>输出 <strong>{r.output_tokens.toLocaleString()}</strong></span>
                        <span className="adjust-history-stat-muted">缓存读 {r.cache_read_tokens.toLocaleString()}</span>
                        <span className="adjust-history-stat-muted">缓存建 {r.cache_creation_tokens.toLocaleString()}</span>
                        <span className="adjust-history-stat-cost">费用 {fmtCost(r.costs)}</span>
                      </div>
                    </div>
                  ))}
                </div>
              </div>
            )}
          </>
        ) : (
          <div className="ledger-empty">
            <span className="ledger-empty-icon" aria-hidden>📭</span>
            <span>
              {usageLoading ? (
                <>正在汇总本需求的账目…</>
              ) : (
                <>
                  <strong>账目尚未生成。</strong>
                  完成一次分析 / 方案 / 编码后，每一笔 token 与费用会按阶段写入此台账。
                </>
              )}
            </span>
          </div>
        )}
      </div>

      {/* Stage stepper — only the steps visible to this requirement's kind
          are rendered (an Idea walks the analyst stage only). */}
      <div className="stage-stepper">
        {visibleSteps.map((s, i) => {
          const isDone = stageIndex > i || (stage === 'done');
          const isActive = stageIndex === i;
          const stageModel = req[s.modelKey];
          return (
            <div key={s.key} className={`stage-step${isActive ? ' active' : ''}${isDone ? ' done' : ''}`}>
              <span className="stage-num">{isDone ? '✅' : s.icon}</span>
              <span className="stage-label">{s.label}</span>
              {stageModel && (
                <span className="stage-model-tag" title={`${s.label}使用的执行模型`}>
                  🤖 {stageModel === '默认模型'
                    ? (roleDefaultModels[s.key] ? `默认模型（${roleDefaultModels[s.key]}）` : '默认模型')
                    : stageModel}
                </span>
              )}
              {i < visibleSteps.length - 1 && <span className="stage-sep">→</span>}
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
          kind={reqKind}
          model={analystModel}
          defaultModel={analystDefaultModel}
          onTurnDone={refresh}
          onWorkingChange={setAnalystWorking}
          usage={analystUsage}
          onUsage={setAnalystUsage}
          onGenerateDesign={() => requestDesignKnowledge(true)}
          onReset={() => setReq(prev => prev ? { ...prev, status: 'draft' } : prev)}
        />
      )}

      {req.status === 'draft' && (
        <div className="detail-section analysis-section">
          <div className="section-header"><h3>🔍 需求分析</h3></div>
          <div className="tab-empty">
            {req.skip_analysis ? (
              <>
                <p>
                  {reqKind === 'idea'
                    ? '想法已记录。开始与 AI 讨论可行性。'
                    : reqKind === 'issue'
                      ? '问题已记录。可直接进入方案设计，或先进行根因分析完善信息。'
                      : '需求已创建（已跳过需求分析）。可直接进入方案设计，或先进行需求分析完善需求。'}
                </p>
                <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                  {/* Primary: go straight to architect-design. status stays draft;
                      the backend ArchitectDesign handler tolerates the missing
                      analyst session when skip_analysis is set, and UpdateDesign
                      then moves status to designing. We transition locally first
                      so the UI flips to the architect stage immediately.
                      Hidden for kind=idea: ideas stay in the discussion stage
                      and never reach architecture/code. */}
                  {reqKind !== 'idea' && (
                    <button className="btn btn-primary"
                      onClick={() => requestDesignKnowledge(true)}
                      disabled={!!busy}>
                      {busy === '生成技术方案' ? '⏳ ...' : '📐 生成技术方案'}
                    </button>
                  )}
                  {/* Architect-model selectable BEFORE generating the plan.
                      Irrelevant for kind=idea — the architect stage is hidden. */}
                  {reqKind !== 'idea' && (
                    <ModelSelect
                      value={architectModel}
                      onChange={setArchitectModel}
                      stage="architect"
                      label="方案模型"
                      defaultModelName={architectDefaultModel}
                      title="方案设计阶段使用的模型，生成技术方案前即可选择"
                    />
                  )}
                  {/* For kind=idea this is the only CTA — promote it from a
                      muted "或先进行..." link to a primary button. */}
                  <button
                    className={reqKind === 'idea' ? 'btn btn-primary' : 'btn btn-sm'}
                    onClick={() => transition('analyzing', '开始分析')} disabled={!!busy}
                    title={reqKind === 'idea' ? '与 AI 讨论这个想法的可行性' : '先进行需求分析，完善需求后再生成方案'}
                  >
                    {busy === '开始分析'
                      ? '⏳ ...'
                      : reqKind === 'idea'
                        ? '💬 与 AI 探讨这个想法'
                        : reqKind === 'issue'
                          ? '🔍 先排查根因'
                          : '或先进行需求分析 →'}
                  </button>
                  {/* Analyst-model selectable before opting into the analysis. */}
                  <ModelSelect
                    value={analystModel}
                    onChange={setAnalystModel}
                    stage="analyst"
                    label="分析模型"
                    defaultModelName={analystDefaultModel}
                    title="需求分析阶段使用的模型，开始分析前即可选择"
                  />
                </div>
              </>
            ) : (
              <>
                <p>
                  {reqKind === 'idea'
                    ? '想法已记录。结合项目情况，与 AI 一起探讨可行性。'
                    : reqKind === 'issue'
                      ? '问题已记录。结合项目代码，定位根因并提出修复方案。'
                      : '需求已创建。结合项目情况完善需求。'}
                </p>
                <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                  <button className="btn btn-primary" onClick={() => transition('analyzing', '开始分析')} disabled={!!busy}>
                    {busy === '开始分析'
                      ? '⏳ ...'
                      : reqKind === 'idea'
                        ? '💬 与 AI 探讨这个想法'
                        : reqKind === 'issue'
                          ? '🐞 开始排查问题'
                          : '🤖 开始需求分析'}
                  </button>
                  {/* Analyst-stage model, selectable BEFORE starting the first
                      analysis turn; the in-chat dropdown is otherwise disabled
                      while the auto-started first turn runs. */}
                  <ModelSelect
                    value={analystModel}
                    onChange={setAnalystModel}
                    stage="analyst"
                    label="分析模型"
                    defaultModelName={analystDefaultModel}
                    title="需求分析阶段使用的模型，开始分析前即可选择"
                  />
                </div>
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
          <div className="design-toolbar model-row" style={{ gap: 8 }}>
            {/* Per-stage architect model. Default = 已设置的方案模型; disabled
                while a design job runs (Claude 工作中禁止切换). */}
            <ModelSelect
              value={architectModel}
              onChange={setArchitectModel}
              disabled={architectWorking}
              working={architectWorking}
              stage="architect"
              label="方案模型"
              defaultModelName={architectDefaultModel}
              title={architectWorking ? 'Claude 正在制定技术方案，暂不能切换模型' : '方案设计阶段使用的模型，开始前即可选择'}
            />
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
              <button className="btn btn-sm" onClick={() => requestDesignKnowledge(false)} disabled={designing}>🔄 重新生成</button>
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
            <FullscreenButton isFullscreen={designFs.isFullscreen} onClick={designFs.toggle} />
          </div>

          {/* Optional knowledge pre-read display (renders only when the user
              opted in and the backend emitted a knowledge event). */}
          <KnowledgeReadPanel items={knowledgeItems} empty={knowledgeEmpty} projectId={project?.id} />

          {designPanelOpen && (
            <div
              className={`coding-panel ${designFs.isFullscreen ? 'is-fullscreen' : ''}`}
              ref={designRef}
              style={designFs.isFullscreen ? undefined : { marginBottom: 16 }}
            >
              {designFs.isFullscreen && (
                <FullscreenButton isFullscreen onClick={designFs.exit} variant="floating" />
              )}
              {/* Live context-usage bar for the plan-mode design run. Design is
                  a one-shot plan-mode product (no multi-turn conversation to
                  compress), so compressible=false hides the 压缩按钮 and only
                  the usage readout remains. Mirrors DocRefineChat's design-doc
                  bar; onCompress is a no-op since the button is suppressed. */}
              <ContextUsageBar
                usage={designUsage}
                onCompress={() => {}}
                compressible={false}
                disabled
                stepLabel="方案设计"
              />
              <CodingLines lines={designLines} working={designing} />
              {designProcessActive && <div className="coding-line coding-line-tool_call">⏳ Claude 正在 plan 模式下制定技术方案...</div>}
            </div>
          )}

          {req.status === 'designing' && !hasDesign && !designing && !req.design_job_id && reqKind !== 'idea' && (
            <div className="tab-empty">
              <p>需求分析已完成。方案设计阶段将在 <strong>plan 模式</strong>下探索项目代码，制定具体可执行的技术实现方案（Markdown）。</p>
              <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                <button className="btn btn-primary" onClick={() => requestDesignKnowledge(false)} disabled={!!busy || designing}>
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
              <div className={isLongDesign && !designExpanded ? 'design-content design-content-collapsed' : 'design-content'}>
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
              </div>
              {isLongDesign && (
                <button
                  type="button"
                  className="btn btn-sm design-toggle-btn"
                  onClick={() => setDesignExpanded(v => !v)}
                  aria-expanded={designExpanded}
                >
                  {designExpanded ? '▲ 收起方案' : '▼ 展开全文'}
                </button>
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
              {reqKind !== 'idea' && (
              <DocRefineChat
                reqId={req.id}
                projectPath={project?.local_path || ''}
                docType="design"
                currentDoc={req.design_docs}
                model={architectModel}
                defaultModel={architectDefaultModel}
                applyJobId={req.apply_job_id}
                onTurnDone={refresh}
                usage={designUsage}
                onUsage={setDesignUsage}
              />
              )}
            </>
          )}
        </div>
      )}

      {/* ── Developer stage ── */}
      {(stage === 'developer' || stage === 'done') && (hasDesign || req.skip_design) && (
        <div className="detail-section">
          <div className="section-header"><h3>🚀 开发实现</h3></div>

          {/* Optional knowledge pre-read display (renders only when the user
              opted in and the backend emitted a knowledge event). */}
          <KnowledgeReadPanel items={knowledgeItems} empty={knowledgeEmpty} projectId={project?.id} />

          {(req.status === 'designed' || (req.status === 'draft' && req.skip_design)) && codingLines.length === 0 && !coding && reqKind !== 'idea' && (
            <div className="tab-empty">
              <p>{req.status === 'designed'
                ? '方案已完成。将根据技术方案进行开发实现。'
                : '直接开发模式：已跳过分析与设计，将直接根据需求内容进行开发实现。'}</p>
              <p style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>项目路径：<code>{project?.local_path}</code></p>
              <p style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>
                将在独立 git worktree 中隔离开发（<code>{project?.local_path}.worktrees/{req.id}</code>），多需求并行互不干扰。
              </p>
              <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                <button className="btn btn-primary" onClick={() => openBranchModal()}>🚀 开始开发</button>
                {/* Per-stage developer model. Default = 已设置的开发模型; disabled
                    while a coding job runs (Claude 工作中禁止切换). */}
                <ModelSelect
                  value={developerModel}
                  onChange={setDeveloperModel}
                  disabled={coding}
                  working={coding}
                  stage="developer"
                  label="开发模型"
                  defaultModelName={developerDefaultModel}
                  title={coding ? 'Claude 正在开发中，暂不能切换模型' : '开发实现阶段使用的模型，开始前即可选择'}
                />
                {/* Agent-server selector. Empty = local execution (the default
                    and the only path before this feature); non-empty routes the
                    claude CLI to that remote target. Only `ready` servers are
                    listed — Check must succeed before coding can target them. */}
                <label style={{ fontSize: 12, color: 'var(--color-text-muted)', display: 'inline-flex', alignItems: 'center', gap: 4 }}>
                  执行环境
                  <select
                    className="form-input"
                    style={{ minWidth: 140 }}
                    value={agentServerId}
                    onChange={(e) => setAgentServerId(e.target.value)}
                    disabled={coding}
                    title={agentServerId
                      ? `将在 ${agentServers.find((s) => s.id === agentServerId)?.name ?? ''} 上执行 Claude CLI`
                      : '本地执行（默认）'}
                  >
                    <option value="">本地执行</option>
                    {agentServers.map((s) => (
                      <option key={s.id} value={s.id}>{s.name} ({s.host})</option>
                    ))}
                  </select>
                </label>
                {agentServers.length === 0 && (
                  <Link to="/settings/agent-servers" style={{ fontSize: 12 }}>
                    配置 Agent 服务器 →
                  </Link>
                )}
              </div>
            </div>
          )}

          {(codingLines.length > 0 || coding) && (
            <div className={`coding-panel ${codingFs.isFullscreen ? 'is-fullscreen' : ''}`} ref={codingRef}>
              {codingFs.isFullscreen && (
                <FullscreenButton isFullscreen onClick={codingFs.exit} variant="floating" />
              )}
              {/* Live context-usage bar + 压缩上下文 entry point for the coding
                  stage. Multi-turn (--resume coding_session_id), so compressible
                  is true — the button hands off to wizardApi.compressContext
                  (step:'coding'). Mirrors CodingChat / DeepRefineChat. Disabled
                  while a coding/adjust turn is in flight or a compression runs. */}
              <ContextUsageBar
                usage={codingUsage}
                onCompress={handleCodingCompress}
                compressing={codingCompressing}
                disabled={coding || codingCompressing}
                stepLabel="开发"
                compressedAt={codingCompressedAt}
                onShowSummary={handleShowCodingSummary}
              />
              <CodingLines lines={codingLines} working={coding} />
              {coding && <div className="coding-line coding-line-tool_call">⏳ Claude 正在工作...</div>}
            </div>
          )}

          {/* Compressed-summary preview modal for the coding stage. Same shape
              as the other chat components' modals so the visual treatment is
              consistent wherever a compression is invoked. */}
          {codingSummaryModal !== null && (
            <div
              className="modal-backdrop"
              onClick={() => setCodingSummaryModal(null)}
              role="dialog"
              aria-modal="true"
            >
              <div
                className="modal"
                onClick={e => e.stopPropagation()}
                style={{ maxWidth: 640 }}
              >
                <div className="modal-header">
                  <h3>📦 已压缩上下文摘要</h3>
                  <button className="btn btn-sm" onClick={() => setCodingSummaryModal(null)}>关闭</button>
                </div>
                <div
                  className="modal-body"
                  style={{ whiteSpace: 'pre-wrap', lineHeight: 1.6, maxHeight: '60vh', overflowY: 'auto' }}
                >
                  {codingSummaryModal}
                </div>
              </div>
            </div>
          )}

          {/* ── 追加调整 ── 续接 coding session（--resume），仅携带本指令；
              输出追加到上方 coding-panel，与首轮开发连贯。developing/done 均可。
              当需求已拆分为子任务（hasSubTasks）时隐藏 — 所有调整改走子任务，
              避免子 Agent 的并行上下文被主会话续接覆盖。 */}
          {req.coding_session_id && (req.status === 'developing' || req.status === 'done') && !coding && !hasSubTasks && (
            <div className="adjust-composer">
              <div className="adjust-composer-header">
                <span className="ac-title">🔧 追加调整</span>
                <span className="ac-tag">续接原开发会话 · 仅携带本指令</span>
                <div style={{ marginLeft: 'auto' }}>
                  {/* Switch model for the next adjust round; the dropdown itself
                      is disabled while a coding job runs. */}
                  <ModelSelect
                    value={developerModel}
                    onChange={setDeveloperModel}
                    disabled={coding}
                    working={coding}
                    stage="developer"
                    defaultModelName={developerDefaultModel}
                  />
                </div>
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
              <div className="adjust-composer-footer stack-mobile">
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
                {req.coding_session_id && codingLines.length === 0 && (
                  <button
                    className="btn btn-primary"
                    title="续接原开发会话（--resume），继续完成任务并补齐丢失的开发记录"
                    onClick={doContinueCoding}
                    disabled={!!busy}
                  >
                    ▶️ 继续开发
                  </button>
                )}
                <button className="btn btn-primary" onClick={() => transition('done', '开发完成')} disabled={!!busy}>
                  {busy === '开发完成' ? '⏳ ...' : '✅ 开发完成'}
                </button>
                {reqKind !== 'idea' && (
                  <button className="btn" title="从技术方案重新 fork 新会话开始开发，不携带上次开发历史" onClick={() => openBranchModal()}>🔄 重新开发</button>
                )}
              </div>

              {/* ── 子Agent 协作 ── 由用户手动触发的子任务，共享主Agent上下文。
                  在 developing/done 阶段都可用（开发期间创建子任务分工；完成后
                  也可继续触发小修改子任务）。位置紧跟在「开发完成 / 重新开发」
                  之后、Merge/PR 步骤之前，符合从上到下的 stage 流程。
                  onSubTasksChange 把当前子任务数回写到本页的 liveSubTaskCount，
                  让 hasSubTasks 在子任务刚创建时就生效、立刻隐藏需求级
                  「追加调整」入口。setter 引用稳定，不会导致面板重渲染循环。 */}
              {(req.status === 'developing' || req.status === 'done') && reqKind !== 'idea' && (
                <SubTaskPanel
                  requirementId={req.id}
                  codingSessionId={req.coding_session_id}
                  requirement={req}
                  onSubTasksChange={setLiveSubTaskCount}
                />
              )}

              {/* ── Merge / PR step ── */}
              <div className="merge-section">
                <div className="merge-actions stack-mobile">
                  <button className="btn" onClick={() => openMergeModal('local')} disabled={merging}>🔀 本地合入</button>
                  <button className="btn" onClick={() => openMergeModal('push')} disabled={merging}>🌐 推送并发起 PR</button>
                </div>

                {mergeState?.worktree_path && (
                  <div className="merge-hint merge-hint--worktree">
                    <span>隔离开发目录</span>
                    <code>{mergeState.worktree_path}</code>
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
                  <div className={`coding-panel merge-panel ${mergeFs.isFullscreen ? 'is-fullscreen' : ''}`}>
                    {mergeFs.isFullscreen && (
                      <FullscreenButton isFullscreen onClick={mergeFs.exit} variant="floating" />
                    )}
                    <CodingLines lines={mergeLines} working={merging} />
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
              <div className="merge-actions stack-mobile">
                <button className="btn" onClick={() => openMergeModal('local')} disabled={merging}>🔀 本地合入</button>
                <button className="btn" onClick={() => openMergeModal('push')} disabled={merging}>🌐 推送并发起 PR</button>
              </div>
              {mergeState?.worktree_path && (
                <div className="merge-hint merge-hint--worktree">
                  <span>隔离开发目录</span>
                  <code>{mergeState.worktree_path}</code>
                  <button className="btn btn-sm" onClick={cleanWorktree} disabled={merging || !!busy}>🧹 清理开发环境</button>
                </div>
              )}
              {/* ── 子Agent 协作（done 阶段也开放）── 隐藏「追加调整」后，
                  done 状态下唯一可用的调整入口就是子任务。位置与
                  developing 分支一致：merge/PR 操作区之后、「📦 归档到知识库」
                  按钮之前。Idea 不展示（避免对探索性想法暴露开发工具）。 */}
              {reqKind !== 'idea' && (
                <SubTaskPanel
                  requirementId={req.id}
                  codingSessionId={req.coding_session_id}
                  requirement={req}
                  onSubTasksChange={setLiveSubTaskCount}
                />
              )}
              <div className="merge-actions stack-mobile" style={{ marginTop: 8 }}>
                <button className="btn btn-primary" onClick={handleArchive} disabled={!!busy}>
                  {busy === '归档' ? '⏳ ...' : '📦 归档到知识库'}
                </button>
                {showPromoteCta && (
                  <button className="btn" onClick={handlePromoteToRequirement} disabled={!!busy}>
                    {busy === '转为需求' ? '⏳ ...' : '📋 转为需求'}
                  </button>
                )}
              </div>
            </div>
          )}

          {req.status === 'archived' && (
            <div className="merge-section">
              <div className="tab-empty">
                <p>📦 已归档至项目知识库（最终需求 + 技术方案）。</p>
              </div>
              <div className="merge-actions stack-mobile" style={{ marginTop: 8 }}>
                <button className="btn" onClick={handleUnarchive} disabled={!!busy}>
                  {busy === '取消归档' ? '⏳ ...' : '↩ 取消归档'}
                </button>
                {showPromoteCta && (
                  <button className="btn" onClick={handlePromoteToRequirement} disabled={!!busy}>
                    {busy === '转为需求' ? '⏳ ...' : '📋 转为需求'}
                  </button>
                )}
              </div>
            </div>
          )}
        </div>
      )}

      {summarizeOpen && (
        <SummarizeToRequirementModal
          sourceId={req.id}
          sourceTitle={req.title}
          onClose={() => setSummarizeOpen(false)}
          onCreated={newId => {
            setSummarizeOpen(false);
            navigate(`/requirements/${newId}`);
          }}
        />
      )}
    </div>
  );
}
