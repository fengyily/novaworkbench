import { useState, useEffect, useRef, useCallback, Fragment, type ReactNode, type CSSProperties } from 'react';
import { useParams, useNavigate, useLocation, Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { requirementsApi, projectsApi, API_BASE, authedFetch, statusLabelKeys, mergeApi, usageApi, usageTotalInput, fmtCost, stepLabelKeys, rolesApi, claudeApi, claudeSettingsPrefix, wizardApi, agentServersApi, subTasksApi, DefaultModelLabel, type AgentServer, type Requirement, type Project, type MergeState, type RequirementUsage, type UsageRow, kindLabelKeys, kindOf, STAGE_VISIBILITY, type Kind, type CostItem, type OrchestrationBatch } from '../api/client';
import { tLabel } from '../i18n/label';
import { createEventStream, type EventStream } from '../api/stream';
import DeepRefineChat from '../components/DeepRefineChat';
import DocRefineChat from '../components/DocRefineChat';
import ModelSelect from '../components/ModelSelect';
import AtMentionTextarea from '../components/AtMentionTextarea';
import SubTaskPanel from '../components/SubTaskPanel';
import { DevSourceBadge } from '../components/DevSourceBadge';
import { ExecEnvBadge } from '../components/ExecEnvBadge';
import { ExecEnvSelect } from '../components/ExecEnvSelect';
import { SummarizeToRequirementModal } from '../components/SummarizeToRequirementModal';
import { ScheduleModal } from '../components/ScheduleModal';
import { schedulesApi, type ScheduledTask } from '../api/client';
import {
  StageIcon,
  IconCheck,
  IconClock,
  IconMailbox,
  IconRobot,
  IconCopy,
  IconFolderOpen,
  IconBroom,
  IconAlert,
  IconSave,
  IconBook,
  IconRocket,
  IconFileText,
  IconHourglass,
  IconMagnifier,
  IconBug,
  IconRefresh,
  IconGlobe,
  IconArchive,
  IconWrench,
  IconHand,
  IconArrowBack,
  IconListOrdered,
  IconPin,
  IconPlay,
  IconTriangle,
  IconDatabase,
  IconSendOut,
  IconSleep,
  IconBotBadge,
  IconFolder,
  IconRefine,
  IconMerge,
  IconChat,
  IconTrash,
  IconSparkles,
} from '../components/icons';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { exportDesignPdf } from '../utils/exportDesignPdf';
import { fmtDateTime, fmtNumber } from '../utils/intl';
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

// Per-step accent color for the mobile token receipt. Color is the
// `--accent` custom property the receipt card uses for its left stripe and
// proportion-bar segment, so the visual identity of each stage is consistent
// across the hero bar and the individual cards. The icon for each step is
// resolved through `STAGE_ICONS` (see components/icons) so the entire
// requirement detail page shares one outline-icon family.
const STAGE_ACCENTS: Record<string, string> = {
  requirement_create: '#94A3B8',
  analyst_chat:       '#4F46E5',
  architect_design:   '#7C3AED',
  refine_doc:         '#7C3AED',
  apply_doc:          '#7C3AED',
  coding:             '#0E7490',
  developer_chat:     '#0E7490',
  adjust_coding:      '#0E7490',
  continue_coding:    '#0E7490',
  merge:              '#059669',
  review:             '#D97706',
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
      // "Skip design" (skip_design) drafts land directly in the developer
      // stage — analyst/architect sections are not rendered and the detail
      // page enters through the "Start development" CTA.
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
      // LogLine (e.g. "All done. Here's the implementation summary." +
      // Markdown). Render it through ReactMarkdown too — otherwise the
      // summary's headings/code blocks/lists show as a garbled wall of
      // plain text.
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

// The wizard's optional "Read project knowledge base" step display: rendered
// at the top of the architect / developer cards when a knowledge event arrived
// (i.e. the user opted in and the backend read the project knowledge base
// before the stage).
// Shows the titles read, a per-entry usage verdict (after the run emits
// "knowledge_result": {used:true} = referenced, {used:false} = not-directly referenced — a cheap signal, not
// an exact measurement), and a link to the full knowledge page. Hidden entirely
// when the option was not used (no knowledge event).
function KnowledgeReadPanel({ items, empty, projectId }: { items: KnowledgeEntry[]; empty: boolean; projectId?: string }) {
  const { t } = useTranslation();
  if (items.length === 0 && !empty) return null;
  const usedCount = items.filter(k => k.used === true).length;
  const assessed = items.length > 0 && items.some(k => k.used !== undefined);
  return (
    <div className="knowledge-read-panel">
      <div className="knowledge-read-header">
        <span><IconBook size={14} className="icon-mr" />{t('requirements.detail2.knowledgeReadTitle')}</span>
        {projectId && (
          <Link className="btn btn-sm knowledge-read-link" to={`/knowledge?project_id=${projectId}`}>
            {t('requirements.detail2.knowledgeReadLink')}
          </Link>
        )}
      </div>
      {empty ? (
        <p className="knowledge-read-empty">{t('requirements.detail2.knowledgeReadEmpty')}</p>
      ) : (
        <>
          <p className="knowledge-read-count">{t('requirements.detail2.knowledgeReadCount', { n: items.length })}</p>
          <div className="knowledge-read-tags">
            {items.map((k, i) => (
              <span
                key={i}
                className={`knowledge-read-tag${k.used === true ? ' tag-used' : k.used === false ? ' tag-unused' : ''}`}
                title={k.used === true ? t('requirements.detail2.knowledgeReadUsedTrue') : k.used === false ? t('requirements.detail2.knowledgeReadUsedFalse') : undefined}
              >
                {k.title}
              </span>
            ))}
          </div>
          {assessed && (
            <p className="knowledge-read-usage">
              {t('requirements.detail2.knowledgeReadAssessment', { used: usedCount, total: items.length })}
            </p>
          )}
        </>
      )}
    </div>
  );
}

// ── WorktreePathHint ─────────────────────────────────────────────────
// Rendered once per requirement detail page (developing / done stages) when
// the requirement has a live git worktree directory. Three affordances:
//
//   1. <code>{path}</code> wraps long POSIX or Windows paths via word-break
//      so they don't push the page width past the viewport on phones.
//   2. [Copy] path: drops the bare absolute path on the clipboard.
//   3. [Open in file manager]: detects the user's OS via
//      navigator.userAgentData.platform (with a navigator.platform fallback
//      for older browsers) and copies the platform-appropriate shell command
//      — `open` on macOS, `explorer` on Windows, `xdg-open` on Linux — onto
//      the clipboard. A small inline toast confirms the copy.
//
// We deliberately do NOT shell out from the frontend (would require a new
// backend endpoint + audit, and we're not in scope for this UI pass).
// Copying the command lets the user paste it into their terminal in one
// keystroke, which is the same muscle-memory flow VS Code's "Reveal in
// Finder → copy path" gesture uses.
function WorktreePathHint({ path, onClean, cleaning, disabled }: {
  path: string;
  onClean?: () => void;
  cleaning?: boolean;
  disabled?: boolean;
}) {
  const { t } = useTranslation();
  const [toast, setToast] = useState<string | null>(null);

  // Auto-dismiss the toast after 1.8s so the hint row returns to its rest
  // state without manual interaction.
  useEffect(() => {
    if (!toast) return;
    const t = setTimeout(() => setToast(null), 1800);
    return () => clearTimeout(t);
  }, [toast]);

  // `userAgentData.platform` is the modern (Chrome/Edge) API and returns
  // one of "macOS" / "Windows" / "Linux" / "Android" / "Chrome OS" / etc.
  // `navigator.platform` is the legacy fallback and returns things like
  // "MacIntel" / "Win32" / "Linux x86_64". Combine the two so older
  // browsers still pick the right command.
  const detectOS = (): 'mac' | 'windows' | 'linux' | 'unknown' => {
    if (typeof navigator === 'undefined') return 'unknown';
    const uaData = (navigator as Navigator & { userAgentData?: { platform?: string } }).userAgentData;
    const plat = (uaData?.platform || navigator.platform || '').toLowerCase();
    if (plat.includes('mac')) return 'mac';
    if (plat.includes('win')) return 'windows';
    if (plat.includes('linux') || plat.includes('ubuntu') || plat.includes('debian')) return 'linux';
    return 'unknown';
  };

  // Build the platform-appropriate shell command. Windows paths need their
  // backslashes intact (we don't quote with `"` because explorer accepts
  // bare paths with spaces up to Windows 10; for Windows 11 / PowerShell
  // users we add double quotes around the path). POSIX paths are always
  // wrapped in double quotes so a path containing spaces survives shell
  // parsing.
  const buildOpenCommand = (os: ReturnType<typeof detectOS>, target: string): string => {
    if (os === 'mac') return `open "${target}"`;
    if (os === 'windows') return `explorer "${target}"`;
    if (os === 'linux') return `xdg-open "${target}"`;
    return target;
  };

  const copyToClipboard = async (text: string): Promise<boolean> => {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // Clipboard API unavailable (insecure context, old browser). Fall
      // back to a transient textarea + legacy execCommand.
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.left = '-9999px';
      document.body.appendChild(ta);
      ta.select();
      let ok = false;
      try { ok = document.execCommand('copy'); } catch { /* noop */ }
      ta.remove();
      return ok;
    }
  };

  const handleCopyPath = async () => {
    const ok = await copyToClipboard(path);
    setToast(ok ? t('requirements.detail2.worktreeCopiedOk') : t('requirements.detail2.worktreeCopyFail'));
  };

  const handleOpenInFileManager = async () => {
    const os = detectOS();
    if (os === 'unknown') {
      setToast(t('requirements.detail2.worktreeUnknownOs'));
      await copyToClipboard(path);
      return;
    }
    const cmd = buildOpenCommand(os, path);
    const ok = await copyToClipboard(cmd);
    const osLabel = os === 'mac' ? 'macOS' : os === 'windows' ? 'Windows' : 'Linux';
    const verb = os === 'mac' ? 'open' : os === 'windows' ? 'explorer' : 'xdg-open';
    setToast(ok ? t('requirements.detail2.worktreeOpenCmdCopied', { os: osLabel, verb }) : t('requirements.detail2.worktreeCopyFail'));
  };

  const os = typeof navigator !== 'undefined' ? detectOS() : 'unknown';
  const osLabel = os === 'mac' ? 'macOS'
    : os === 'windows' ? 'Windows'
    : os === 'linux' ? 'Linux'
    : t('requirements.detail2.worktreeOpenHintFallback');

  return (
    <div className="merge-hint merge-hint--worktree">
      <span className="merge-hint-label">{t('requirements.detail2.worktreeLabel')}</span>
      <code className="merge-hint-path" title={path}>{path}</code>
      <div className="merge-hint-actions">
        <button
          type="button"
          className="btn btn-sm merge-hint-btn"
          onClick={handleCopyPath}
          disabled={disabled}
          title={t('requirements.detail2.worktreeCopyPathTitle')}
        >
          <IconCopy size={14} className="btn-icon" /> {t('requirements.detail2.worktreeCopyPathBtn')}
        </button>
        <button
          type="button"
          className="btn btn-sm merge-hint-btn"
          onClick={handleOpenInFileManager}
          disabled={disabled}
          title={t('requirements.detail2.worktreeOpenHint', {
            os: osLabel,
            verb: os === 'mac' ? 'open'
              : os === 'windows' ? 'explorer'
              : os === 'linux' ? 'xdg-open'
              : t('requirements.detail2.worktreeOpenHintFallback'),
          })}
        >
          <IconFolderOpen size={14} className="btn-icon" /> {t('requirements.detail2.worktreeOpenBtn')}
        </button>
        {onClean && (
          <button
            className="btn btn-sm merge-hint-btn merge-hint-btn--danger"
            onClick={onClean}
            disabled={disabled || cleaning}
            title={t('requirements.detail2.worktreeCleanTitle')}
          >
            <IconBroom size={14} className="btn-icon" /> {t('requirements.detail2.worktreeCleanBtn')}
          </button>
        )}
        {toast && <span className="merge-hint-toast" role="status">{toast}</span>}
      </div>
    </div>
  );
}

export default function RequirementDetail() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const location = useLocation();
  const { t } = useTranslation();
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
  // DocRefineChat (refine-doc / apply-doc) reports its in-flight turns
  // through onWorkingChange; refine-doc streams straight to the response
  // without entering JobStore, so this callback is the only signal we
  // get while a refine turn runs. apply-doc does enter JobStore and is
  // also picked up by activeReqIds (below), so this state is mostly a
  // fallback that keeps the badge in sync within the same page.
  const [refineWorking, setRefineWorking] = useState(false);
  // Global view of all wizard jobs currently running in this backend
  // process, projected to just the requirement ids. Populated by polling
  // GET /api/wizard/active-jobs every 5s. Combined with the local in-page
  // signals (coding/designing/analystWorking/refineWorking and the
  // persisted *_job_id columns) to drive the global claudeWorking flag
  // and the amber pulse animation on the status badge.
  const [activeReqIds, setActiveReqIds] = useState<Set<string>>(new Set());

  // Per-stage model selection (analyst / architect / developer). Seeded once
  // from the server-persisted stage model so each dropdown defaults to the
  // previously-set model; a user switch is sent with the next stage POST.
  // These are selectable
  // BEFORE the stage starts (draft / designing-empty / designed-empty gates).
  const [analystModel, setAnalystModel] = useState('');
  const [architectModel, setArchitectModel] = useState('');
  const [developerModel, setDeveloperModel] = useState('');
  // The user-picked claude_configs row id from the developer-stage
  // ModelSelect. Forwarded to /api/wizard/start-coding as `claude_config_id`
  // so the backend resolves gateway auth + base URL from the SAME row the
  // model came from (fixes the "BASE URL doesn't match selected model" bug).
  const [developerConfigId, setDeveloperConfigId] = useState('');
  // Agent-server selector for the developer stage. Empty string = local
  // execution (the historical default); non-empty = run claude on the chosen
  // remote target. Only `ready` servers are listed — the wizard refuses to
  // start coding on a target whose dependencies haven't been verified.
  //
  // agentServerId is seeded from the persisted requirements.agent_server_id so
  // a page refresh / re-entry preselects the server the requirement last ran
  // on (and so adjust-coding / continue-coding re-send the same target without
  // the user re-picking it). useState's initial value only applies on first
  // render — by then the requirement row may not have loaded yet, so we sync
  // it in an effect below once req.agent_server_id arrives.
  const [agentServerId, setAgentServerId] = useState('');
  const [agentServers, setAgentServers] = useState<AgentServer[]>([]);
  useEffect(() => {
    agentServersApi.list()
      .then((rows) => setAgentServers((rows ?? []).filter((s) => s.status === 'ready')))
      .catch(() => {/* settings tab is the source of truth — silently ignore */});
  }, []);
  // Preselect the dropdown from the persisted binding once the requirement
  // loads. Only fills the dropdown when the user hasn't already picked
  // something locally this session (agentServerId === ''), so switching
  // selections mid-session is never clobbered by a re-fetch.
  useEffect(() => {
    if (req?.agent_server_id) {
      setAgentServerId((cur) => (cur === '' ? req.agent_server_id! : cur));
    }
  }, [req?.agent_server_id]);
  // Design-stage binding: the architect stage writes its own column
  // (requirements.design_agent_server_id), independent of the dev-stage
  // binding above. If the two bindings differ, prefer the design binding
  // whenever the user hasn't already picked something this session — that
  // way an "I've designed on A but haven't coded yet" requirement lands in
  // the design section's selector pre-pointed at A without losing the dev
  // binding when the developer stage later runs (the dev modal already
  // reseeds itself from req.agent_server_id). The `cur === ''` guard
  // preserves any in-session user choice, matching the dev-stage policy.
  useEffect(() => {
    const designID = req?.design_agent_server_id;
    if (!designID) return;
    if (designID === req?.agent_server_id) return; // dev seed already covers it
    setAgentServerId((cur) => (cur === '' ? designID : cur));
  }, [req?.design_agent_server_id, req?.agent_server_id]);

  // Poll /api/wizard/active-jobs every 5s so the status badge + claude-status
  // row can pulse while a coding/design/apply job is running on this
  // requirement — even if the persisted *_job_id column hasn't refreshed
  // yet (it lags until the goroutine finishes). The endpoint walks the
  // in-memory JobStore ring buffer (cap 50) so it's cheap enough to poll
  // here plus on the list page simultaneously. The `cancelled` flag prevents
  // a late tick from stomping on the cleanup after the requirement id
  // changes (we don't want a stale set lingering into the next requirement).
  useEffect(() => {
    if (!req?.id) return;
    let cancelled = false;
    const tick = async () => {
      try {
        const { jobs } = await wizardApi.listActiveJobs();
        if (cancelled) return;
        setActiveReqIds(new Set(jobs.map((j) => j.requirement_id).filter(Boolean)));
      } catch {
        /* transient network blip — keep the previous set until the next tick */
      }
    };
    tick();
    const id = setInterval(tick, 5000);
    return () => { cancelled = true; clearInterval(id); };
  }, [req?.id]);

  // Development-mode selector for the coding stage. '' = the UI hasn't
  // picked yet (the StartCoding request omits dev_mode and the backend
  // falls back to the persisted value, or 'session' on rows that predate
  // the column); 'session' = session-based dev (fork the design session,
  // legacy default); 'design' = design-based dev (fresh session, hand the
  // stored design doc to the agent via the -p prompt). Persisted in
  // requirements.dev_mode and seeded from the row so a re-run preserves
  // the previous choice by default.
  const [devMode, setDevMode] = useState<'' | 'session' | 'design'>('');
  useEffect(() => {
    if (req?.dev_mode) {
      setDevMode((cur) => (cur === '' ? req.dev_mode! : cur));
    }
  }, [req?.dev_mode]);

  // Agent-server code-transport (sync) mode selector, only shown when an Agent
  // server is picked. 'remote' = 远程 Git 仓库同步 (origin clone/push); 'local'
  // = 本地仓库同步 (git-bundle over SFTP for a self-hosted repo with no
  // reachable remote). Default: the requirement's persisted sync_mode, else
  // inferred from the project (a repo with no remote_url → 'local'). Persisted
  // in requirements.sync_mode; adjust/continue reuse the stored value.
  const [syncMode, setSyncMode] = useState<'local' | 'remote'>('remote');
  useEffect(() => {
    if (req?.sync_mode === 'local') {
      setSyncMode('local');
    } else if (req?.sync_mode === '' && project) {
      // No persisted choice yet — infer from the project's remote config.
      setSyncMode(project.remote_url ? 'remote' : 'local');
    }
  }, [req?.sync_mode, project?.remote_url]);

  // ── Scheduled-task state ──
  // pendingByType[taskType] holds the pending row (if any) so the detail
  // page can render "Scheduled HH:MM ... [Cancel]" hints and disable the
  // ScheduleModal once there's a pending row (the backend enforces 409
  // anyway, but the UI being upfront avoids the round-trip). Modal is
  // controlled by modalState — null when closed; otherwise carries the
  // taskType we want to create.
  const [pendingByType, setPendingByType] = useState<Record<'design' | 'coding', ScheduledTask | null>>({
    design: null,
    coding: null,
  });
  const [scheduleModal, setScheduleModal] = useState<{ taskType: 'design' | 'coding' } | null>(null);
  const loadPendingSchedules = useCallback(async () => {
    if (!req) return;
    try {
      const rows = await schedulesApi.list({ requirement_id: req.id, status: 'pending' });
      const map: Record<'design' | 'coding', ScheduledTask | null> = { design: null, coding: null };
      for (const r of rows ?? []) {
        if (r.task_type === 'design' || r.task_type === 'coding') {
          map[r.task_type] = r;
        }
      }
      setPendingByType(map);
    } catch {
      // Silent — the SchedulesPage is the source of truth; this is a hint only.
    }
  }, [req]);
  useEffect(() => {
    loadPendingSchedules();
  }, [loadPendingSchedules]);

  // Seed the selector from the requirement's persisted development source so
  // a re-run (re-develop / start-development after a restart) defaults to the
  // SAME Agent server the code already lives on, instead of silently
  // dropping back to local execution. Runs once, and only when that server
  // is still in the ready list (a deleted / unhealthy server falls back to
  // local rather than failing).
  const agentSeedRef = useRef(false);
  useEffect(() => {
    if (!req || agentSeedRef.current || agentServers.length === 0) return;
    agentSeedRef.current = true;
    if (req.dev_source === 'agent' && req.agent_server_id &&
        agentServers.some((s) => s.id === req.agent_server_id)) {
      setAgentServerId(req.agent_server_id);
    }
  }, [req, agentServers]);

  const modelSeedRef = useRef(false);
  useEffect(() => {
    if (!req || modelSeedRef.current) return;
    modelSeedRef.current = true;
    // i18n: protocol literal — '默认模型' sentinel mirrored from backend handler.DefaultModelLabel.
    const norm = (m?: string) => (!m || m === '默认模型' ? '' : m); // i18n: protocol literal
    setAnalystModel(norm(req.analyst_model ?? ''));
    setArchitectModel(norm(req.architect_model ?? ''));
    setDeveloperModel(norm(req.developer_model ?? ''));
  }, [req]);

  // Effective default model per role (role-config model > active Claude
  // config default model).
  // Used so ModelSelect's "默认模型" option shows the actual model name that // i18n: protocol literal — DefaultModelLabel sentinel
  // will run for each stage before the stage starts. activeBaseURL feeds the
  // copy-paste launch command's --settings prefix (same gateway Nova uses).
  const [roleDefaultModels, setRoleDefaultModels] = useState<Record<string, string>>({});
  const [activeBaseURL, setActiveBaseURL] = useState('');
  useEffect(() => {
    let cancelled = false;
    Promise.all([rolesApi.list(), claudeApi.active()])
      .then(([roles, active]) => {
        if (cancelled) return;
        const configDefault = active?.default_model || '';
        setActiveBaseURL(active?.base_url || '');
        const map: Record<string, string> = {};
        for (const r of roles ?? []) {
          // The role's model field may be empty (no override) or the literal
          // "默认模型" sentinel — both fall back to the config default. // i18n: protocol literal
          const rm = r.model && r.model !== '默认模型' ? r.model : ''; // i18n: protocol literal
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
  // Live context-usage snapshot for the coding job (start-coding / adjust /
  // continue), pushed by the backend's `usage` SSE event. Rendered via
  // ContextUsageBar at the top of the coding-panel. The coding stage is
  // multi-turn (--resume coding_session_id), so compressible=true — the
  // compress button hands off to wizardApi.compressContext(step:'coding')
  // which summarizes + clears the session, mirroring CodingChat /
  // DeepRefineChat.
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
  // level "follow-up adjustment" composer. The state lives here (not just on req) so a
  // newly-created child agent immediately hides the composer without waiting
  // for the next refetch.
  const [liveSubTaskCount, setLiveSubTaskCount] = useState(0);

  // Orchestration batch snapshot for this requirement (the new
  // restartable-orchestration flow). null when the backend has no batch
  // row yet — before StartCoding, or for a requirement that never went
  // through the auto-orchestrator. Drives the summary CTAs in
  // SubTaskPanel (the summary CTA cluster: early / manual / progress).
  // The polling interval shortens while children are alive (3s) so the
  // banner transitions surface within one frame of the queue tick.
  const [orchBatch, setOrchBatch] = useState<OrchestrationBatch | null>(null);
  const fetchOrchBatch = useCallback(async () => {
    if (!id) return;
    try {
      const data = await subTasksApi.getOrchestrationBatch(id);
      // subTasksApi.getOrchestrationBatch wraps a 404-or-not-implemented
      // backend as `null` (see client.ts catch fallback) so callers don't
      // need try/catch — a missing batch is a normal terminal state.
      setOrchBatch(data ?? null);
    } catch {
      setOrchBatch(null);
    }
  }, [id]);
  useEffect(() => {
    fetchOrchBatch();
  }, [fetchOrchBatch]);
  useEffect(() => {
    const intervalMs = liveSubTaskCount > 0 ? 3000 : 5000;
    const t = setInterval(fetchOrchBatch, intervalMs);
    return () => clearInterval(t);
  }, [fetchOrchBatch, liveSubTaskCount]);

  // Page-level summary-done toast. The server-pushed job_done frame may carry
  // `batch_id` + `summary_status === 'done'` whenever the OrchestrationQueue
  // finishes a summary round (auto path OR manual flow triggered from
  // SubTaskPanel). The user is on this page; the brief inline hint confirms
  // the round landed without forcing them to inspect `orchBatch`. Mirrors the
  // WorktreePathHint toast: local state, 1.8s auto-dismiss, reuses the global
  // `.merge-hint-toast` styling so no new CSS is needed.
  const [summaryDoneToast, setSummaryDoneToast] = useState<string | null>(null);
  const summaryDoneToastTimerRef = useRef<number | null>(null);
  const showSummaryDoneToast = useCallback((text: string) => {
    setSummaryDoneToast(text);
    if (summaryDoneToastTimerRef.current !== null) {
      window.clearTimeout(summaryDoneToastTimerRef.current);
    }
    summaryDoneToastTimerRef.current = window.setTimeout(() => {
      setSummaryDoneToast(null);
      summaryDoneToastTimerRef.current = null;
    }, 1800);
  }, []);
  useEffect(() => () => {
    if (summaryDoneToastTimerRef.current !== null) {
      window.clearTimeout(summaryDoneToastTimerRef.current);
    }
  }, []);

  // Branch modal state
  const [showBranchModal, setShowBranchModal] = useState(false);
  const [branchName, setBranchName] = useState('');
  const [baseBranch, setBaseBranch] = useState('');
  const [availableBranches, setAvailableBranches] = useState<string[]>([]);

  // Optional "Read project knowledge base" step (default off). The dev
  // checkbox lives in the branch modal; the design one has its own confirm
  // modal so the user can opt in right before generating the technical plan.
  const [readKnowledgeDev, setReadKnowledgeDev] = useState(false);
  // Optional "Split tasks" switch (default off — i.e. no subtask split).
  // When checked, the backend runs the developer persona's task-decomposition
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

  // Merge / PR step state (post-coding merge-in).
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
  // hides behind the "Thinking process" toggle and the user has no idea the
  // run failed.
  const [designError, setDesignError] = useState(false);
  const designRef = useRef<HTMLDivElement>(null);
  const designEsRef = useRef<EventStream | null>(null);

  // Collapsible "Thinking process" toggle for the architect design stream.
  // While the design job is actively running the panel stays open; once it
  // finishes the panel collapses and a toggle lets the user re-expand it.
  const [showDesignProcess, setShowDesignProcess] = useState(false);

  // Collapsible design-doc state. Long design documents default to collapsed
  // (truncated with a fade-mask + "Expand full text" button); short ones
  // render in full as before. Re-evaluated whenever the requirement or its
  // stored design_docs change, and any switch collapses the view back to its
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

  // Follow-up adjustment input. Resume the prior coding session
  // (--resume coding_session_id) with ONLY the user's follow-up as -p — the
  // resumed conversation already carries the requirement/design/persona, so
  // no system prompt or project context is re-injected. Output reuses
  // codingLines/coding-panel for continuity.
  const [adjustInput, setAdjustInput] = useState('');

  // PDF export state for the technical design doc.
  const [exporting, setExporting] = useState(false);

  // Token usage for this requirement (per-step + total). Refreshed on mount
  // and after each stage completes (refresh()), so the breakdown reflects the
  // latest claude turns.
  const [usage, setUsage] = useState<RequirementUsage | null>(null);
  const [usageLoading, setUsageLoading] = useState(false);
  // Per-invocation rows for the "follow-up adjustment" steps (adjust_coding +
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
  // sees the summary as prepended context instead of full history. The
  // bar's compress button is only meaningful for the multi-turn coding
  // stage (not the one-shot plan-mode design stage).
  const handleCodingCompress = useCallback(async () => {
    if (!id || codingCompressing) return;
    if (!confirm(t('requirements.detail2.codingCompressConfirm'))) return;
    setCodingCompressing(true);
    try {
      const data = await wizardApi.compressContext(id, 'coding');
      setCodingCompressedAt(data.compressed_at ?? null);
      // Reset usage so the bar doesn't keep reporting the soon-cleared
      // session's token counts; the next turn pushes a fresh snapshot.
      setCodingUsage(undefined);
    } catch (err: any) {
      alert(t('requirements.detail2.codingCompressFailPrefix') + (err?.message || String(err)));
    } finally {
      setCodingCompressing(false);
    }
  }, [id, codingCompressing, t]);

  // Lazy fetch of the persisted summary text for the modal preview.
  const handleShowCodingSummary = useCallback(async () => {
    if (!id) return;
    try {
      const data = await wizardApi.getContextSummary(id, 'coding');
      setCodingSummaryModal(data.summary || t('requirements.detail2.codingCompressEmpty'));
    } catch {
      setCodingSummaryModal(t('requirements.detail2.codingCompressLoadFail'));
    }
  }, [id, t]);

  // Copy a ready-to-paste resume command to the clipboard. Instead of just the
  // bare session id, we compose `cd "<project_path>" && claude --settings '...'
  // --resume "<sid>"` so the user can paste it straight into a shell, land in
  // the right CWD, AND hit the same model + base URL Nova itself launches with
  // (the --settings env block mirrors the backend gateway's settingsArg; the
  // auth token is intentionally absent — the user's own claude auth applies).
  // Falls back to copying the sid alone when no project path is known.
  const copySessionId = async (sid: string): Promise<void> => {
    const path = project?.local_path;
    const settings = claudeSettingsPrefix(activeBaseURL, roleDefaultModels['developer'] || '');
    const cmd = (path
      ? `cd "${path}" && claude ${settings} --resume "${sid}"`
      : `claude ${settings} --resume "${sid}"`).replace(/ {2,}/g, ' ').trim();
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
    if (d.overview) parts.push(`## ${t('requirements.detail2.designMdOverview')}\n${d.overview}`);
    if (d.steps?.length) parts.push(`## ${t('requirements.detail2.designMdSteps')}\n${d.steps.map((s, i) => `${i + 1}. ${s}`).join('\n')}`);
    if (d.files?.length) parts.push(`## ${t('requirements.detail2.designMdFiles')}\n${d.files.map(f => `- ${f}`).join('\n')}`);
    if (d.model_changes && d.model_changes !== t('requirements.detail2.designModelChangesNone')) parts.push(`## ${t('requirements.detail2.designModelChangesHeader')}\n${d.model_changes}`);
    if (d.risks?.length) parts.push(`## ${t('requirements.detail2.designMdRisks')}\n${d.risks.map(r => `- ${r}`).join('\n')}`);
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
      alert(t('requirements.detail2.pdfExportFailPrefix') + err.message);
    } finally {
      setExporting(false);
    }
  };

  const handleDelete = async () => {
    if (!req) return;
    if (!window.confirm(t('requirements.detail2.deleteWithName', { title: req.title }))) return;
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
    // Boot-fetch the coding stage's compression record so the bar's
    // "compressed" badge is correct after a page refresh, before the user
    // clicks anything.
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

  // 设计文档存在多种存储形态:
  //   - 计划模式 (新): 原始 Markdown 字符串
  //   - legacy JSON 对象: {overview, files, steps, model_changes, risks}
  //   - legacy JSON 数组: ["# 方案..."]
  //   - 边缘情形 1: finalResult fallback 可能产出以 { 开头的非设计 JSON
  //   - 边缘情形 2: Claude 在 apply-doc 输出被 ```markdown ... ``` 围栏包裹,
  //     ReactMarkdown 会把整段当代码块渲染,需要 strip 外层围栏
  //
  // 仅当 JSON 解析结果显式携带 plan_markdown 或任意 legacy 字段,
  // 才视为 DesignData;否则一律把原始字符串当 Markdown 渲染;
  // 当作 Markdown 之前先剥掉外层代码围栏。
  const parseDesign = (raw: string): DesignData => {
    if (!raw) return {};
    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch {
      return { plan_markdown: stripOuterFence(raw) };
    }
    // legacy JSON 数组形态:["# 方案\n## 详情", ...]
    if (Array.isArray(parsed)) {
      const first = parsed.find((v) => typeof v === 'string' && v.trim() !== '');
      return { plan_markdown: typeof first === 'string' ? stripOuterFence(first) : raw };
    }
    if (parsed && typeof parsed === 'object') {
      const obj = parsed as Partial<DesignData>;
      const objHasShape = typeof obj.plan_markdown === 'string'
          || obj.overview !== undefined
          || obj.files !== undefined
          || obj.steps !== undefined
          || obj.model_changes !== undefined
          || obj.risks !== undefined;
      if (objHasShape) {
        if (typeof obj.plan_markdown === 'string') {
          obj.plan_markdown = stripOuterFence(obj.plan_markdown);
        }
        return obj;
      }
    }
    // JSON 解析成功但不符合 DesignData 形态 → 原始内容视为 Markdown
    return { plan_markdown: stripOuterFence(raw) };
  };

  // 剥离外层 ```lang ... ``` 围栏(以及无 lang 标签的 ``` ... ``` 形式)。
  // 仅当整段内容首尾恰好是一对完整代码围栏时才剥离,避免误删正文中
  // 偶然配对的三个反引号对(例如示例代码块)。
  const FENCE_RE = /^```[^\n]*\n([\s\S]*?)\n```\s*$/;
  const stripOuterFence = (s: string): string => {
    if (!s) return s;
    const m = FENCE_RE.exec(s.trim());
    return m ? m[1] : s;
  };

  // ── Status gate transitions ────────────────────────────────────────────────
  const transition = async (newStatus: string, label: string) => {
    if (!id) return;
    setBusy(label);
    try {
      await requirementsApi.updateStatus(id, newStatus);
      await refresh();
    } catch (err: any) {
      alert(t('requirements.detail2.statusTransitionFailPrefix') + err.message);
    } finally {
      setBusy('');
    }
  };

  // Archive a finished requirement into the project knowledge base (final
  // requirement + design docs become reusable AI context). Re-archiving the
  // same requirement overwrites the previous knowledge entry.
  const handleArchive = async () => {
    if (!id) return;
    setBusy(t('requirements.detail2.actionBusyArchive')); // i18n: protocol literal — state sentinel compared at render
    try {
      await requirementsApi.archive(id);
      await refresh();
    } catch (err: any) {
      alert(t('requirements.detail2.archiveFailPrefix') + err.message);
    } finally {
      setBusy('');
    }
  };

  // Reverse archive: status returns to "done" and the knowledge entry produced
  // by archiving is removed from the project knowledge base.
  const handleUnarchive = async () => {
    if (!id) return;
    if (!confirm(t('requirements.detail2.archiveConfirmUndo'))) return;
    setBusy(t('requirements.detail2.actionBusyUnarchive')); // i18n: protocol literal — state sentinel compared at render
    try {
      await requirementsApi.unarchive(id);
      await refresh();
    } catch (err: any) {
      alert(t('requirements.detail2.unarchiveFailPrefix') + err.message);
    } finally {
      setBusy('');
    }
  };

  // Promote an issue/idea (only allowed when status is done/archived) into a
  // full "requirement" so the developer stage becomes reachable. One-way gate:
  // the backend's UpdateKind rejects demotions back to issue/idea.
  const handlePromoteToRequirement = async () => {
    if (!id) return;
    if (!confirm(t('requirements.detail2.promoteConfirm'))) return;
    setBusy(t('requirements.detail2.actionBusyPromote')); // i18n: protocol literal — state sentinel compared at render
    try {
      await requirementsApi.updateKind(id, 'requirement');
      await refresh();
    } catch (err: any) {
      alert(t('requirements.detail2.promoteFailPrefix') + err.message);
    } finally {
      setBusy('');
    }
  };

  // ── Summarize-to-requirement modal (kind=idea) ────────────────────────────
  // The modal lives at the page level so any CTA that triggers the
  // "summarize to requirement" flow (in the draft section, the chat header,
  // or the done/archived footer) can open the same component. onCreated
  // navigates to the brand-new requirement — the user lands on its
  // freshly-minted detail page.
  const [summarizeOpen, setSummarizeOpen] = useState(false);

  // ── Architect phase: async design generation via JobStore ─────────────────
  // The architect-design endpoint creates a background job and returns its id
  // immediately (same pattern as start-coding). We stream the job's log lines
  // over SSE; on job_done we refresh so design_docs (now persisted server-side)
  // renders. The active job id is persisted on the requirement as design_job_id,
  // so a page refresh reconnects to the running job instead of re-launching it
  // or re-showing the "Start design" button.
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
        // Coalesce consecutive "Claude thinking… (N tokens)" phase lines into one
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
          // Route the architect stage to a remote Agent server. Empty =
          // local execution (legacy default). The wizard remote branch
          // refuses servers that aren't in `ready` status, so an empty /
          // stale value here silently degrades to local without erroring.
          ...(agentServerId ? { agent_server_id: agentServerId } : {}),
        }),
      });
      const json = await res.json();
      const jobId = json.data?.job_id;
      if (!jobId) throw new Error(json.error?.message || t('requirements.detail2.jobIdMissing'));
      streamDesignJob(jobId);
    } catch (err: any) {
      setDesignLines([{ type: 'error', content: err.message }]);
      setDesigning(false);
    }
  };

  // Design-entry gates first show a "read project knowledge?" checkbox modal
  // (default unchecked) — per feedback "make it optional, default unchecked". needsTransition
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
      await transition('designing', t('requirements.detail2.intentGenerateDesign'));
      await runArchitectDesign(useKnowledge);
    } else {
      await runArchitectDesign(useKnowledge);
    }
  };

  // Auto-start the architect-design flow when the user just created a
  // requirement with skip_analysis (navigated here with the autoStartDesign
  // intent flag). This replaces the manual "Generate design" click for the
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
      transition('designing', t('requirements.detail2.intentGenerateDesign')).then(() => runArchitectDesign(false));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [req]);

  // Auto-open the branch selection modal when the user just created a
  // requirement with skip_design (navigated here with the autoStartCoding
  // intent flag). We deliberately stop at the branch modal — the 30-minute
  // coding job needs the user to confirm the dev branch first, matching the
  // manual "Start development" interaction. One-shot guarded like autoStartDesign.
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
    setBusy('保存'); // i18n: protocol literal — state sentinel compared at render
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
      alert(t('requirements.detail2.saveFailPrefix') + err.message);
    } finally {
      setBusy('');
    }
  };

  // ── Developer phase: job streaming ────────────────────────────────────────
  // streamJob subscribes to a wizard job's SSE stream and appends events to
  // codingLines (the shared coding-panel). keepDone=true is used by the
  // "Further adjust" rounds: on job_done it leaves the requirement
  // status untouched (stays
  // developing/done) and only refreshes, instead of flipping to developing and
  // writing the localStorage "done" marker like the first coding pass. When
  // keepDone is set, persistDone additionally writes the "done" marker so a
  // refresh reloads THIS job's durable log — used by "Continue coding", whose job log
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
            // Further adjust / continue coding: preserve current status, just refresh on success.
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
          // Server-pushed orchestration summary completion. The
          // OrchestrationQueue marks the batch via runOrchestratorSummary
          // (success → MarkSummary('done') + MarkCompleted); when the summary
          // round's JobStore job_done frame lands here we surface a brief
          // inline hint and refresh the page-level batch snapshot so the
          // SubTaskPanel banner transitions out of the progress state. Guarded so
          // unrelated job_done frames (a single sub-task finish, a coding
          // round) don't trigger the toast.
          if (ok && evt.batch_id && evt.summary_status === 'done') {
            showSummaryDoneToast(t('requirements.detail2.summaryDone'));
            fetchOrchBatch();
            refresh();
          }
          return;
        }
        // Coalesce consecutive "Claude thinking… (N tokens)" phase lines into one
        // updatable row instead of stacking one per heartbeat. Use the
        // backend-stamped `at` so phase timings stay accurate.
        const at = typeof evt.at === 'number' ? evt.at : Date.now();
        setCodingLines(prev => appendLogLine(prev, { type: evt.type, content: evt.content ?? '', at }));
      },
      () => {
        esRef.current = null;
        setCoding(false);
        // SSE died (dropped link, backend hang, or a stream that ended without
        // the job_done frame) — the local `coding` flag alone is not enough:
        // the requirement row may still read `developing` in the DB, leaving
        // the UI out of sync with the real state (this was half of the
        // "stuck forever, refresh does not help" symptom). Reconcile by
        // re-reading the requirement from the server. Deliberately NOT
        // writing a status here and NOT touching the `coding_job_<id>`
        // localStorage marker — this is the error path, so we let the DB
        // state stand. Fire-and-forget (never await: this callback must stay
        // synchronous) with a no-op catch so a still-broken network does not
        // surface an unhandled rejection.
        refresh().catch(() => {});
      },
    );
  }, [id, refresh, fetchOrchBatch, showSummaryDoneToast, t]);

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
      ? `## 技术方案\n${design.plan_markdown}` // i18n: protocol literal — sent to LLM
      : [
          design.overview ? `## 技术方案\n${design.overview}` : '', // i18n: protocol literal
          design.steps?.length ? `## 实现步骤\n${design.steps.map((s, i) => `${i + 1}. ${s}`).join('\n')}` : '', // i18n: protocol literal
          design.files?.length ? `## 涉及文件\n${design.files.map(f => `- ${f}`).join('\n')}` : '', // i18n: protocol literal
          design.model_changes && design.model_changes !== '无' ? `## 数据模型变更\n${design.model_changes}` : '', // i18n: protocol literal
        ].filter(Boolean).join('\n\n')) || req.description;

    let desc = baseDesc;
    if (extraDescRef.current) {
      desc = baseDesc + `\n\n## 追加调整\n${extraDescRef.current}`; // i18n: protocol literal
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
          // Per-request claude_config id (the user-picked "config" from
          // ModelSelect). When empty the backend resolves gateway via
          // resolveConfigIDForRun (model owner > role binding > global active).
          // Sending this explicitly fixes the "BASE URL doesn't match selected
          // model" bug — without it the gateway routes the picked model to
          // the active config's ANTHROPIC_BASE_URL.
          ...(developerConfigId ? { claude_config_id: developerConfigId } : {}),
          // Remote Agent-server execution. Empty string = local execution (the
          // wizardH.StartCoding default branch handles the legacy path).
          ...(agentServerId ? { agent_server_id: agentServerId } : {}),
          // Agent-server code-transport mode. Only meaningful (and only sent)
          // when an Agent server is picked: 'remote' = origin clone/push,
          // 'local' = git-bundle over SFTP for a self-hosted repo with no
          // reachable remote. The backend persists it and every follow-up
          // action (adjust / continue / sub-task / merge / cleanup) reuses the
          // stored value, so adjust/continue below deliberately omit it.
          ...(agentServerId ? { sync_mode: syncMode } : {}),
          // Development-mode: 'session' (default when not set — fork the
          // design session) or 'design' (fresh session, hand the stored
          // design doc to the agent via the -p prompt). Sent only when the
          // user explicitly picked one; otherwise the backend falls back to
          // the persisted requirements.dev_mode (or 'session' on legacy
          // rows), which keeps "Rebuild" consistent with the previous run.
          ...(devMode ? { dev_mode: devMode } : {}),
        }),
      });
      const json = await res.json();
      const jobId = json.data?.job_id;
      if (!jobId) throw new Error(t('requirements.detail2.jobIdMissing'));
      localStorage.setItem(`coding_job_${id}`, jobId);
      streamJob(jobId);
    } catch (err: any) {
      setCodingLines([{ type: 'error', content: err.message }]);
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
    setSplitTasksDev(false); // default unchecked each time
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

  // ── Further adjust: resume the prior coding session, output appends to codingLines
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
          // Per-request claude_config id — see doStartCoding for the rationale.
          ...(developerConfigId ? { claude_config_id: developerConfigId } : {}),
          // agent_server_id is intentionally NOT sent here: adjust-coding
          // resumes the same coding session on the same worktree, so the
          // dev_source / agent_server_id stamped by the original StartCoding
          // prologue stays authoritative. To switch servers the user has to
          // re-run start-coding from scratch.
        }),
      });
      const json = await res.json();
      const jobId = json.data?.job_id;
      if (!jobId) throw new Error(json.error?.message || t('requirements.detail2.jobIdMissing'));
      setAdjustInput('');
      streamJob(jobId, { keepDone: true });
    } catch (err: any) {
      setCodingLines(prev => [...prev, { type: 'error', content: err.message }]);
      setCoding(false);
    }
  };

  // ── Continue coding: recover from a backend restart that cleared the in-memory
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
        // agent_server_id intentionally NOT sent: continue-coding resumes the same
        // coding session / worktree as the original StartCoding, so the
        // existing dev_source / agent_server_id binding stays authoritative.
        body: JSON.stringify({ requirement_id: id }),
      });
      const json = await res.json();
      const jobId = json.data?.job_id;
      if (!jobId) throw new Error(json.error?.message || t('requirements.detail2.jobIdMissing'));
      localStorage.setItem(`coding_job_${id}`, jobId);
      streamJob(jobId, { keepDone: true, persistDone: true });
    } catch (err: any) {
      setCodingLines([{ type: 'error', content: err.message }]);
      setCoding(false);
    }
  };

  // ── Merge / PR step (post-coding merge) ──────────────────────────────────────
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
        // Coalesce consecutive "Claude thinking… (N tokens)" phase lines into one
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
      setMergeLines([{ type: 'error', content: err.message }]);
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
      setMergeLines([{ type: 'error', content: err.message }]);
      setMerging(false);
    }
  };

  // cleanWorktree drops the requirement's isolated dev worktree + branch so
  // finished/abandoned parallel dev dirs don't accumulate on disk. A dirty
  // worktree is refused unless the user confirms a force cleanup.
  const cleanWorktree = async () => {
    if (!id || !mergeState?.worktree_path) return;
    if (!window.confirm(t('requirements.detail2.worktreeConfirm'))) return;
    const run = async (force: boolean) => {
      setBusy('清理'); // i18n: protocol literal — state sentinel compared at render
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
        if (window.confirm(t('requirements.detail2.worktreeConfirmForce'))) {
          try {
            await run(true);
          } catch (e: any) {
            setMergeLines([{ type: 'error', content: e.message }]);
          }
        }
      } else {
        setMergeLines([{ type: 'error', content: msg }]);
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

  if (loading) return <div className="detail-loading"><IconHourglass size={16} className="icon-mr" />{t('requirements.detail2.logLoading')}</div>;
  if (!req) return <div className="detail-error"><IconAlert size={16} className="icon-mr" />{t('requirements.detail2.reqNotFound')}</div>;

  const design = parseDesign(req.design_docs);
  const hasDesign = !!(design.overview || (design.steps && design.steps.length > 0) || design.plan_markdown);
  // Requirement has at least one sub-task: either seeded by the GET response
  // (req.sub_task_count, present once the requirement has been decomposed)
  // or reported live by the mounted SubTaskPanel (liveSubTaskCount, covers
  // the brief window between "user clicks 'Create sub-task'" and the next refetch).
  // Once true, the requirement-level "Further adjust" composer is hidden — all
  // further adjustments must flow through the sub-task composer so the main
  // agent's task breakdown stays the source of truth.
  const hasSubTasks = (req.sub_task_count ?? 0) > 0 || liveSubTaskCount > 0;
  const stage = stageFor(req.status, req.skip_design);
  // Design (architect) stream state. While the job runs the panel stays open;
  // once finished it collapses behind the "Thinking process" toggle.
  const designProcessActive = designing || !!req.design_job_id;
  // Keep the panel open while the job runs OR when the last run errored, so
  // the error line stays visible instead of collapsing behind the toggle.
  const designPanelOpen = designProcessActive || designError || showDesignProcess;
  const showDesignToggle = designLines.length > 0 && !designProcessActive && !designError;
  // Claude working status. Analysis signal comes from DeepRefineChat's live
  // onWorkingChange (the persisted analysis_job_id is only refreshed after a
  // turn finishes, so it lags during the turn); design/apply use the persisted
  // active job ids; coding/design add the local streaming states. The global
  // `activeReqIds` set (populated by the 5s /api/wizard/active-jobs poll)
// catches jobs that have no per-requirement *_job_id column at all —
// currently that means start-coding / adjust-coding / continue-coding, but
// the aggregation also double-covers analyst/design/apply so the pulse stays
// on even when this page hasn't loaded the latest persisted pointer yet.
  const claudeWorking = coding || designing || analystWorking || refineWorking ||
    !!req.analysis_job_id || !!req.design_job_id || !!req.apply_job_id ||
    activeReqIds.has(req.id);
  // Per-stage working flags drive the model-switch disable (task requirement:
  // Claude working — model switch disabled).
  const architectWorking = designing || !!req.design_job_id;

  const STEPS = [
    { key: 'analyst', label: t('requirements.detail2.stageAnalyst'), stage: 'analyst_chat', doneStatus: 'designing', modelKey: 'analyst_model' as const },
    { key: 'architect', label: t('requirements.detail2.stageArchitect'), stage: 'architect_design', doneStatus: 'designed', modelKey: 'architect_model' as const },
    { key: 'developer', label: t('requirements.detail2.stageDeveloper'), stage: 'coding', doneStatus: 'done', modelKey: 'developer_model' as const },
  ] as const;
  // Per-kind stepper visibility: an Idea only walks the analyst stage.
  const reqKind: Kind = kindOf(req);
  const visibleStepKeys = STAGE_VISIBILITY[reqKind];
  const visibleSteps = STEPS.filter((s) => (visibleStepKeys as readonly string[]).includes(s.key));
  const stageIndex = stage === 'done' ? visibleSteps.length : visibleSteps.findIndex(s => s.key === stage);
  // "Convert to requirement" CTA visible for finished Issue / Idea rows.
  const showPromoteCta = (reqKind === 'idea' || reqKind === 'issue') && (req.status === 'done' || req.status === 'archived');
  // "Summarize to requirement" CTA — only for Idea, available the moment there's something
  // to summarize (chat started, OR status is done/archived). Draft + no chat
  // would summarize from the bare description which is fine too: the user may
  // want a quick jump from "rough idea text" to "structured requirement".
  const canSummarize = reqKind === 'idea' && (
    req.status === 'draft' || req.status === 'analyzing' ||
    req.status === 'done' || req.status === 'archived'
  );

  return (
    <div className="req-detail">
      {/* Page-level summary-done toast — mirrors WorktreePathHint's toast
          pattern (`.merge-hint-toast` style, 1.8s auto-dismiss). Fired when
          a server-pushed job_done frame carries batch_id + summary_status
          'done', confirming an OrchestrationQueue summary round landed. */}
      {summaryDoneToast && (
        <span className="merge-hint-toast" role="status">{summaryDoneToast}</span>
      )}
      {/* Optional "read project knowledge" confirm modal for the design stage */}
      {showDesignKnowledgeModal && (
        <div className="modal-overlay" onClick={() => setShowDesignKnowledgeModal(false)}>
          <div className="modal-box" onClick={e => e.stopPropagation()}>
            <h3><IconTriangle size={16} className="icon-mr" />{t('requirements.detail2.intentGenerateDesign')}</h3>
            <p style={{ fontSize: 13, color: 'var(--color-text-muted)', marginBottom: 8 }}>
              {t('requirements.detail2.designKnowledgeHint')}
            </p>
            <label className="merge-check" style={{ margin: '8px 0 12px' }}>
              <input type="checkbox" checked={readKnowledgeDesign} onChange={e => setReadKnowledgeDesign(e.target.checked)} />
              <IconBook size={13} className="icon-mr" />{t('requirements.detail2.preflightReadKnowledgeDefault')}
            </label>
            <div className="modal-actions btn-row-2col">
              <button className="btn btn-primary" onClick={confirmDesignKnowledge}>{t('requirements.detail2.btnConfirm')}</button>
              <button className="btn" onClick={() => setShowDesignKnowledgeModal(false)}>{t('requirements.detail2.btnCancel')}</button>
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
                <div className="preflight-eyebrow">{t('requirements.detail2.preflightEyebrow')}</div>
                <h3 className="preflight-title">{t('requirements.detail2.preflightTitle')}</h3>
                <p className="preflight-subtitle">
                  {t('requirements.detail2.preflightSubtitle')}
                </p>
              </div>

              {/* Flight strip — the signature element. Compresses the
                  configured mission into one monospace line the user can
                  scan at a glance before launching. */}
              <div className="flight-strip" aria-label={t('requirements.detail2.preflightAria')}>
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
                  <div className="preflight-section-label">{t('requirements.detail2.preflightGitSection')}</div>
                  {/* Branch fields wear the same rail+chip+monospace card as
                      ModelSelect, only tinted for the Git section. Keeps the
                      panel's three editable surfaces speaking one vocabulary. */}
                  <div className="modal-field">
                    <label>{t('requirements.detail2.preflightBaseBranchLabel')}</label>
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
                    <label>{t('requirements.detail2.preflightNewBranchLabel')}</label>
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
                  <div className="preflight-section-label">{t('requirements.detail2.preflightExecSection')}</div>
                  {/* Agent-server selector: empty = local execution (legacy default).
                      Only ready servers are listed; the wizard remote branch refuses
                      to start on a non-ready target so this stays consistent with the
                      server-side guard. Violet-tinted field card — distinguishes
                      "how to run" from the Git section's "where to run". */}
                  <div className="modal-field">
                    <label>{t('requirements.detail2.preflightExecEnvLabel')}</label>
                    <div className="preflight-field-card preflight-field-card--exec">
                      <span className="preflight-field-chip" aria-hidden="true">ENV</span>
                      <ExecEnvSelect
                        className="form-input preflight-field-input"
                        servers={agentServers}
                        value={agentServerId}
                        onChange={setAgentServerId}
                        disabled={coding}
                        title={agentServers.length === 0 ? t('requirements.detail2.preflightAgentEmptyTitle') : ''}
                        localOptionLabel={t('requirements.detail2.preflightLocalExec')}
                      />
                    </div>
                    {agentServers.length === 0 && (
                      <div style={{ fontSize: 12, color: 'var(--color-text-muted)', marginTop: 4 }}>
                        {t('requirements.detail2.preflightNoAgentHint')}
                      </div>
                    )}
                  </div>
                  {/* Sync-mode selector: only meaningful when running on an Agent
                      server. 'remote' ships code via origin clone/push (needs a
                      reachable remote); 'local' ships it as a git bundle over
                      SFTP and syncs back to the local worktree (self-hosted repo
                      with no reachable remote). Defaults to the project's remote
                      config; the user can override before launch. */}
                  {agentServerId && (
                    <div className="modal-field">
                      <label>{t('requirements.detail2.syncModeLabel')}</label>
                      <select
                        className="form-input"
                        value={syncMode}
                        onChange={e => setSyncMode(e.target.value as 'local' | 'remote')}
                        disabled={coding}
                      >
                        <option value="remote">{t('requirements.detail2.syncModeRemote')}</option>
                        <option value="local">{t('requirements.detail2.syncModeLocal')}</option>
                      </select>
                      <div style={{ fontSize: 12, color: 'var(--color-text-muted)', marginTop: 4 }}>
                        {syncMode === 'local'
                          ? t('requirements.detail2.syncModeLocalHint')
                          : t('requirements.detail2.syncModeRemoteHint')}
                      </div>
                    </div>
                  )}
                  {/* Developer-stage model selection, visible right before launching
                      the coding job. Disabled while a coding job runs (Claude working —
                      model switch disabled). */}
                  <div className="modal-field">
                    <ModelSelect
                      value={developerModel}
                      onChange={setDeveloperModel}
                      disabled={coding}
                      working={coding}
                      stage="developer"
                      label={t('requirements.detail2.devModelLabel')}
                      defaultModelName={developerDefaultModel}
                      title={coding ? t('requirements.detail2.devModelBusyTitle') : t('requirements.detail2.devModelTitle')}
                      configId={developerConfigId}
                      onConfigChange={setDeveloperConfigId}
                    />
                  </div>
                </div>

                <div className="preflight-section">
                  <div className="preflight-section-label">{t('requirements.detail2.preflightStrategySection')}</div>
                  {/* Optional knowledge pre-read: default unchecked. When checked, the
                      coding job first reads the project knowledge base relevant to the
                      requirement before diving into the code. */}
                  <label className={`preflight-toggle ${readKnowledgeDev ? 'is-checked' : ''}`}>
                    <input type="checkbox" checked={readKnowledgeDev} onChange={e => setReadKnowledgeDev(e.target.checked)} />
                    <div className="preflight-toggle-body">
                      <div className="preflight-toggle-title"><IconBook size={14} className="icon-mr" />{t('requirements.detail2.preflightReadKnowledge')}</div>
                      <div className="preflight-toggle-desc">
                        {t('requirements.detail2.preflightReadKnowledgeHint')}
                      </div>
                    </div>
                  </label>
                  {/* Optional sub-task decomposition switch: default unchecked (= direct execution, no sub-task split). When checked, the developer persona emits a subtasks.json + [SUBTASKS_READY] sentinel and the backend auto-dispatches sub-agents. When unchecked, the developer persona runs in direct-implementation mode (no sub-task orchestration). Mirrors readKnowledgeDev's pattern: reset to false in openBranchModal, explicit value (even when false) on the wire. */}
                  <label className={`preflight-toggle ${splitTasksDev ? 'is-checked' : ''}`}>
                    <input type="checkbox" checked={splitTasksDev} onChange={e => setSplitTasksDev(e.target.checked)} />
                    <div className="preflight-toggle-body">
                      <div className="preflight-toggle-title"><IconPin size={14} className="icon-mr" />{t('requirements.detail2.preflightSplitTasks')}</div>
                      <div className="preflight-toggle-desc">
                        {t('requirements.detail2.preflightSplitTasksHint')}
                      </div>
                    </div>
                  </label>
                  {/* Development-mode radio: "Session-based" (default) = fork the design
                      session; "Design-based" = create a new session with
                      the design as the only input. Seed stays consistent with
                      dev_source/dev_mode; the local choice can be changed before
                      confirming the launch. */}
                  <div className="preflight-toggle" style={{ display: 'block' }}>
                    <div className="preflight-toggle-body">
                      <div className="preflight-toggle-title">{t('requirements.detail2.preflightDevModeTitle')}</div>
                      <div className="preflight-toggle-desc" style={{ marginBottom: 8 }}>
                        {t('requirements.detail2.preflightDevModeHint')}
                      </div>
                      <div style={{ display: 'flex', gap: 16, flexWrap: 'wrap' }}>
                        <label style={{ display: 'inline-flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}>
                          <input
                            type="radio"
                            name="devModeModal"
                            value="session"
                            checked={devMode === 'session'}
                            onChange={() => setDevMode('session')}
                            disabled={coding}
                          />
                          {t('requirements.detail2.preflightDevModeSession')}
                        </label>
                        <label style={{ display: 'inline-flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}>
                          <input
                            type="radio"
                            name="devModeModal"
                            value="design"
                            checked={devMode === 'design'}
                            onChange={() => setDevMode('design')}
                            disabled={coding}
                          />
                          {t('requirements.detail2.preflightDevModeDesign')}
                        </label>
                        <label style={{ display: 'inline-flex', alignItems: 'center', gap: 6, cursor: 'pointer', color: 'var(--color-text-muted)' }}>
                          <input
                            type="radio"
                            name="devModeModal"
                            value=""
                            checked={devMode === ''}
                            onChange={() => setDevMode('')}
                            disabled={coding}
                          />
                          {t('requirements.detail2.preflightDevModeKeep')}
                        </label>
                      </div>
                    </div>
                  </div>
                </div>
              </div>

              <div className="preflight-launch">
                <button className="btn-launch" onClick={confirmBranchAndStart}>
                  <IconRocket size={14} className="btn-icon" />{t('requirements.detail2.preflightLaunchBtn')}
                </button>
                <button className="btn-cancel" onClick={() => setShowBranchModal(false)}>
                  {t('requirements.detail2.btnCancel')}
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
            <h3>{mergeMode === 'local' ? <><IconMerge size={16} className="icon-mr" />{t('requirements.detail2.mergeLocalTitle')}</> : <><IconGlobe size={16} className="icon-mr" />{t('requirements.detail2.mergePrTitle')}</>}</h3>
            {mergeMode === 'local' ? (
              <>
                <div className="modal-field">
                  <label>{t('requirements.detail2.mergeTargetLabel')}</label>
                  <select className="input" value={mergeTarget} onChange={e => setMergeTarget(e.target.value)}>
                    {availableBranches.length === 0 && <option value={mergeTarget}>{mergeTarget}</option>}
                    {availableBranches.map(b => <option key={b} value={b}>{b}</option>)}
                  </select>
                </div>
                <div className="modal-field">
                  <label>{t('requirements.detail2.mergeDevBranchLabel')}</label>
                  <input className="input" value={mergeState.dev_branch} disabled />
                </div>
                <div className="merge-hint">
                  <span>{t('requirements.detail2.mergeAheadBehind', { ahead: mergeState.ahead, behind: mergeState.behind })}</span>
                  <span>{t('requirements.detail2.mergeUncommitted', { n: mergeState.uncommitted_count })}</span>
                </div>
                {mergeState.uncommitted_count > 0 && (
                  <details className="merge-files">
                    <summary>{t('requirements.detail2.mergeViewUncommitted')}</summary>
                    <ul>{mergeState.uncommitted_files.map((f, i) => <li key={i}><code>{f}</code></li>)}</ul>
                  </details>
                )}
                <div className="modal-field">
                  <label>{t('requirements.detail2.mergeCommitMsgLabel')}</label>
                  <input className="input" value={mergeCommitMsg} onChange={e => setMergeCommitMsg(e.target.value)} />
                </div>
                <label className="merge-check">
                  <input type="checkbox" checked={mergeDeleteBranch} onChange={e => setMergeDeleteBranch(e.target.checked)} />
                  {t('requirements.detail2.mergeDeleteAfter')}
                </label>
                <div className="modal-actions btn-row-2col">
                  <button className="btn btn-primary" onClick={confirmMerge} disabled={!!busy}><IconMerge size={14} className="btn-icon" />{t('requirements.detail2.mergeConfirmBtn')}</button>
                  <button className="btn" onClick={() => setShowMergeModal(false)} disabled={!!busy}>{t('requirements.detail2.btnCancel')}</button>
                </div>
              </>
            ) : (
              <>
                <div className="modal-field">
                  <label>{t('requirements.detail2.mergePushBranchLabel')}</label>
                  <input className="input" value={mergeState.dev_branch} disabled />
                </div>
                <div className="merge-hint">
                  <span>{t('requirements.detail2.mergeRemoteRepo')}</span>
                  <code>{mergeState.remote_url || t('requirements.detail2.mergeRemoteEmpty')}</code>
                </div>
                {mergeState.behind > 0 && (
                  <p className="merge-warn"><IconAlert size={14} className="icon-mr" />{t('requirements.detail2.mergeBehindWarn', { n: mergeState.behind })}</p>
                )}
                <div className="merge-hint">
                  <span>{t('requirements.detail2.mergeFlowLabel')}</span>
                  <span>{t('requirements.detail2.mergeFlowSteps')}</span>
                </div>
                {mergeState.mid_merge && (
                  <p className="merge-warn"><IconAlert size={14} className="icon-mr" />{t('requirements.detail2.mergeInProgressWarn')}</p>
                )}
                <div className="modal-field">
                  <label>{t('requirements.detail2.mergeCommitMsgLabel')}</label>
                  <input className="input" value={mergeCommitMsg} onChange={e => setMergeCommitMsg(e.target.value)} />
                </div>
                {!mergeState.has_remote && (
                  <p className="merge-warn">{t('requirements.detail2.mergeNoRemoteWarn')}</p>
                )}
                <div className="modal-actions btn-row-2col">
                  <button className="btn btn-primary" onClick={confirmMerge} disabled={!!busy || !mergeState.has_remote}><IconGlobe size={14} className="btn-icon" />{t('requirements.detail2.mergePushBtn')}</button>
                  <button className="btn" onClick={() => setShowMergeModal(false)} disabled={!!busy}>{t('requirements.detail2.btnCancel')}</button>
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
            <h3><IconRefine size={16} className="icon-mr" />{t('requirements.detail2.editTitle')}</h3>
            <div className="modal-field">
              <label>{t('requirements.detail2.editLabelTitle')}</label>
              <input className="form-input" value={editTitle} onChange={e => setEditTitle(e.target.value)} />
            </div>
            <div className="modal-field">
              <label>{t('requirements.detail2.editLabelDesc')}</label>
              <AtMentionTextarea
                className="form-input form-textarea"
                rows={6}
                value={editDesc}
                onChange={setEditDesc}
                placeholder={t('requirements.detail2.editMentionPlaceholder')}
              />
            </div>
            <div className="modal-field">
              <label>{t('requirements.detail2.editLabelPriority')}</label>
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
                  {t('requirements.detail2.editSkipAnalysis')}
                </label>
                <div className="form-hint">
                  {t('requirements.detail2.editSkipAnalysisHint')}
                </div>
              </div>
            )}
            <div className="modal-actions btn-row-2col">
              <button className="btn btn-primary" onClick={saveEdit} disabled={!!busy}>
                {busy === '保存' /* i18n: protocol literal — state sentinel compared at render */
                  ? <><IconHourglass size={13} className="btn-icon" />{t('requirements.detail2.saveBusy')}</>
                  : <><IconSave size={13} className="btn-icon" />{t('requirements.detail2.saveBtn')}</>}
              </button>
              <button className="btn" onClick={() => setShowEditModal(false)}>{t('requirements.detail2.editCancel')}</button>
            </div>
          </div>
        </div>
      )}

      {/* Header */}
      <div className="detail-header">
        <button className="btn" onClick={() => navigate(`/projects/${req.project_id}`)}>{t('requirements.detail2.headerProjectLink')}</button>
        <div className="detail-id">{req.id}</div>
        <div style={{ marginLeft: 'auto', display: 'flex', gap: 8 }}>
          {canSummarize && (
            <button
              className="btn btn-sm"
              onClick={() => setSummarizeOpen(true)}
              title={t('requirements.detail2.summarizeTitle')}
            >
              <IconSparkles size={13} className="btn-icon" /> {t('requirements.detail2.summaryTitle')}
            </button>
          )}
          <button className="btn btn-sm" onClick={openEdit}><IconRefine size={13} className="btn-icon" />{t('requirements.detail2.headerEditBtn')}</button>
          <button className="btn btn-sm btn-danger" onClick={handleDelete}><IconTrash size={13} className="btn-icon" />{t('requirements.detail2.headerDeleteBtn')}</button>
        </div>
      </div>

      <h1>{req.title}</h1>

      <div className="detail-meta">
        <span className={`kind-badge kind-${reqKind}`} title={reqKind === 'idea' ? t('requirements.detail2.headerBadgeIdea') : reqKind === 'issue' ? t('requirements.detail2.headerBadgeIssue') : t('requirements.detail2.headerBadgeRequirement')}>{tLabel(t, kindLabelKeys as Record<string,string>, reqKind)}</span>
        {/* claude-pulse overlay sits on top of status-badge + claude-status:
            amber ripple + micro-scale + brightness bump, 1.6s breathing cycle.
            Lets the detail-page head show at a glance whether a wizard job
            is currently running. Auto-paused under prefers-reduced-motion. */}
        <span className={`status-badge status-${req.status}${claudeWorking ? ' claude-pulse' : ''}`}>{tLabel(t, statusLabelKeys as Record<string,string>, req.status)}</span>
        <span className={`priority-tag ${req.priority}`}>{req.priority.toUpperCase()}</span>
        <span className={`claude-status${claudeWorking ? ' working claude-pulse' : ''}`} title={claudeWorking ? t('requirements.detail2.claudeBusyTitle') : t('requirements.detail2.claudeIdleTitle')}>
          {claudeWorking ? <><IconBotBadge size={12} className="icon-mr" />{t('requirements.detail2.claudeBusy')}</> : <><IconSleep size={12} className="icon-mr" />{t('requirements.detail2.claudeIdle')}</>}
        </span>
        {project && <span className="project-tag"><IconFolder size={12} className="icon-mr" />{project.name}</span>}
        {/* Dev source: Agent Server (with server name + model) or local dev.
            Written when the coding stage starts; not rendered for
            requirements that have not entered coding. */}
        <DevSourceBadge req={req} />
        {/* Development mode: session-based (continue inside the original
            design session) vs design-based (create a new session and hand
            the design as the only input to the Agent). Only rendered once
            the coding stage has started — the badge distinguishes the two
            modes so the user can see at a glance which one was last used. */}
        {req.dev_mode === 'session' && (
          <span className="dev-mode-badge dev-mode-session" title={t('requirements.detail2.devModeSessionTitle')}>{t('requirements.detail2.devModeSession')}</span>
        )}
        {req.dev_mode === 'design' && (
          <span className="dev-mode-badge dev-mode-design" title={t('requirements.detail2.devModeDesignTitle')}>{t('requirements.detail2.devModeDesign')}</span>
        )}
        {/* Local-sync provenance: shown for Agent-server requirements whose
            code is transported via git bundle (no git remote). Reuses the
            dev-mode badge styling. */}
        {req.sync_mode === 'local' && req.agent_server_id && (
          <span className="dev-mode-badge dev-mode-session" title={t('requirements.detail2.syncModeLocalBadgeTitle')}>{t('requirements.detail2.syncModeLocalBadge')}</span>
        )}
        {req.source_requirement_id && (
          <Link
            to={`/requirements/${req.source_requirement_id}`}
            className="source-link"
            title={t('requirements.detail2.sourceIdeaTitle')}
          >
            {t('requirements.detail2.sourceIdeaBackLink')}
          </Link>
        )}
        {/* Per-stage "compressed" badges. Each one is a passive indicator of
            whether that wizard stage's session has been summarized into the
            {step}_context_summary column — the chat components own the
            compress button + summary modal, this is just a header-level
            reminder so the user knows context has been compressed without
            scrolling into the chat panel. Hover for the timestamp. */}
        {req.analyst_compressed_at && (
          <span
            className="compressed-badge"
            title={t('requirements.detail2.compressedBadgeAnalystTitle', { at: req.analyst_compressed_at })}
          >
            <IconArchive size={12} className="icon-mr" />{t('requirements.detail2.compressedBadgeAnalyst')}
          </span>
        )}
        {req.design_compressed_at && (
          <span
            className="compressed-badge"
            title={t('requirements.detail2.compressedBadgeDesignTitle', { at: req.design_compressed_at })}
          >
            <IconArchive size={12} className="icon-mr" />{t('requirements.detail2.compressedBadgeDesign')}
          </span>
        )}
        {req.coding_compressed_at && (
          <span
            className="compressed-badge"
            title={t('requirements.detail2.compressedBadgeCodingTitle', { at: req.coding_compressed_at })}
          >
            <IconArchive size={12} className="icon-mr" />{t('requirements.detail2.compressedBadgeCoding')}
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
            {t('requirements.detail2.usageTitle')}
          </span>
          {usageLoading && <span className="ledger-loading">{t('requirements.detail2.usageRefreshing')}</span>}
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
              const fmtCount = (n: number) => n >= 1000 ? `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}k` : fmtNumber(n);
              const stages = new Map<string, {
                key: string; label: string; stage: string; accent: string;
                cost: number; costs: CostItem[]; count: number;
                input: number; output: number; cacheRead: number; cacheCreate: number;
                models: string[];
              }>();
              for (const s of usage.by_step) {
                const accent = STAGE_ACCENTS[s.step] ?? '#94A3B8';
                const cur = stages.get(s.step) ?? {
                  key: s.step,
                  label: s.label || tLabel(t, stepLabelKeys as Record<string,string>, s.step),
                  stage: s.step,
                  accent,
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
                    <div className="usage-receipt-eyebrow">{t('requirements.detail2.usageTotalCost')}</div>
                    <div className="usage-receipt-total">{fmtCost(usage.total.costs)}</div>
                    <div className="usage-receipt-meta">
                      <span>{t('requirements.detail2.usageCallCount', { n: fmtNumber(totalCount) })}</span>
                      <span className="usage-receipt-dot">·</span>
                      <span>{fmtCount(totalTokens)} {t('requirements.detail2.usageTokensUnit')}</span>
                      {cacheHitRate > 0 && (
                        <>
                          <span className="usage-receipt-dot">·</span>
                          <span className="usage-receipt-cache">
                            {t('requirements.detail2.usageCacheHitRate', { pct: Math.round(cacheHitRate * 100) })}
                          </span>
                        </>
                      )}
                    </div>
                    {ordered.length > 0 && (
                      <div className="usage-receipt-bar" role="img" aria-label={t('requirements.detail2.usageCostBreakdownAria')}>
                        {ordered.map(s => totalCost > 0 ? (
                          <div
                            key={s.key}
                            className="usage-receipt-bar-seg"
                            style={{
                              width: `${(s.cost / totalCost) * 100}%`,
                              background: s.accent,
                            }}
                            title={t('requirements.detail2.usageStageCostTitle', { label: s.label, pct: Math.round((s.cost / totalCost) * 100) })}
                          />
                        ) : null)}
                      </div>
                    )}
                    {ordered.length > 0 && (
                      <div className="usage-receipt-legend">
                        {ordered.map(s => (
                          <span key={s.key} className="usage-receipt-legend-item">
                            <span className="usage-receipt-legend-swatch" style={{ background: s.accent }} />
                            <StageIcon stage={s.stage} size={14} className="usage-receipt-legend-icon" style={{ color: s.accent }} />
                            <span>{s.label}</span>
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
                            <span className="usage-receipt-card-icon" aria-hidden>
                              <StageIcon stage={s.stage} size={16} style={{ color: s.accent }} />
                            </span>
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
                            <span className="usage-receipt-card-model">{t('requirements.detail2.usageUnknownModel')}</span>
                          )}
                          <span className="usage-receipt-card-dot">·</span>
                          <span>{t('requirements.detail2.usageCallCount', { n: fmtNumber(s.count) })}</span>
                        </div>
                        <div className="usage-receipt-card-bar" aria-hidden>
                          <div
                            className="usage-receipt-card-bar-fill"
                            style={{ width: totalCost > 0 ? `${(s.cost / totalCost) * 100}%` : '0%' }}
                          />
                        </div>
                        <div className="usage-receipt-card-stats">
                          <div className="usage-receipt-card-stat">
                            <span className="usage-receipt-card-stat-label">{t('requirements.detail2.usageStatInput')}</span>
                            <span className="usage-receipt-card-stat-value">{fmtCount(s.input + s.cacheRead + s.cacheCreate)}</span>
                          </div>
                          <div className="usage-receipt-card-stat">
                            <span className="usage-receipt-card-stat-label">{t('requirements.detail2.usageStatOutput')}</span>
                            <span className="usage-receipt-card-stat-value">{fmtCount(s.output)}</span>
                          </div>
                        </div>
                        {(s.cacheRead > 0 || s.cacheCreate > 0) && (
                          <div className="usage-receipt-card-cache">
                            <span>{t('requirements.detail2.usageCacheRead', { n: fmtNumber(s.cacheRead) })}</span>
                            {s.cacheCreate > 0 && <span>{t('requirements.detail2.usageCacheCreate', { n: fmtNumber(s.cacheCreate) })}</span>}
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
                  <th>{t('requirements.detail2.usageColStep')}</th>
                  <th style={{ width: 170 }}>{t('requirements.detail2.usageColModel')}</th>
                  <th style={{ width: 110 }}>{t('requirements.detail2.usageColInput')}</th>
                  <th style={{ width: 110 }}>{t('requirements.detail2.usageColOutput')}</th>
                  <th style={{ width: 110 }}>{t('requirements.detail2.usageColCacheRead')}</th>
                  <th style={{ width: 110 }}>{t('requirements.detail2.usageColCacheCreation')}</th>
                  <th style={{ width: 110 }}>{t('requirements.detail2.usageColCost')}</th>
                  <th style={{ width: 60 }}>{t('requirements.detail2.usageColCount')}</th>
                </tr>
              </thead>
              <tbody>
                {usage.by_step.map(s => (
                  <Fragment key={`${s.step}:${s.model}`}>
                    <tr>
                      <td data-label={t('requirements.detail2.usageColStep')}>{s.label || tLabel(t, stepLabelKeys as Record<string,string>, s.step)}</td>
                      <td data-label={t('requirements.detail2.usageColModel')}><code className="pr-branch">{s.model || t('requirements.detail2.usageUnknownModel')}</code></td>
                      <td data-label={t('requirements.detail2.usageColInput')} style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{fmtNumber(usageTotalInput(s))}</td>
                      <td data-label={t('requirements.detail2.usageColOutput')} style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{fmtNumber(s.output_tokens)}</td>
                      <td data-label={t('requirements.detail2.usageColCacheRead')} style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{fmtNumber(s.cache_read_tokens)}</td>
                      <td data-label={t('requirements.detail2.usageColCacheCreation')} style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{fmtNumber(s.cache_creation_tokens)}</td>
                      <td data-label={t('requirements.detail2.usageColCost')} style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{fmtCost(s.costs)}</td>
                      <td data-label={t('requirements.detail2.usageColCount')} style={{ fontSize: 12 }}>{s.count}</td>
                    </tr>
                  </Fragment>
                ))}
                <tr className="table-cards-total" style={{ borderTop: '2px solid var(--color-border)' }}>
                  <td data-label={t('requirements.detail2.usageColTotal')} style={{ fontWeight: 600 }}>{t('requirements.detail2.usageColTotal')}</td>
                  <td></td>
                  <td data-label={t('requirements.detail2.usageColInput')} style={{ fontFamily: 'var(--font-mono)', fontSize: 12, fontWeight: 600 }}>{fmtNumber(usageTotalInput(usage.total))}</td>
                  <td data-label={t('requirements.detail2.usageColOutput')} style={{ fontFamily: 'var(--font-mono)', fontSize: 12, fontWeight: 600 }}>{fmtNumber(usage.total.output_tokens)}</td>
                  <td data-label={t('requirements.detail2.usageColCacheRead')} style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{fmtNumber(usage.total.cache_read_tokens)}</td>
                  <td data-label={t('requirements.detail2.usageColCacheCreation')} style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--color-text-muted)' }}>{fmtNumber(usage.total.cache_creation_tokens)}</td>
                  <td data-label={t('requirements.detail2.usageColCost')} style={{ fontFamily: 'var(--font-mono)', fontSize: 12, fontWeight: 600 }}>{fmtCost(usage.total.costs)}</td>
                  <td data-label={t('requirements.detail2.usageColCount')}></td>
                </tr>
              </tbody>
            </table>
            <small style={{ color: 'var(--color-text-muted)', fontSize: 12 }}>
              {t('requirements.detail2.usageBillingNote')}
            </small>

            {adjustRows && adjustRows.length > 0 && (
              <div className="adjust-history">
                <div className="adjust-history-title">
                  <IconListOrdered size={14} className="icon-mr" />{t('requirements.detail2.adjustHistoryTitle', { n: adjustRows.length })}
                </div>
                <div className="adjust-history-list">
                  {adjustRows.map((r, i) => (
                    <div key={r.id} className="adjust-history-card">
                      <div className="adjust-history-head">
                        <span className="adjust-history-head-left">
                          <span className="adjust-history-index">#{i + 1}</span>
                          <span className="adjust-history-stage">
                            {tLabel(t, stepLabelKeys as Record<string,string>, r.step)}
                          </span>
                          <code className="pr-branch adjust-history-model">{r.model || t('requirements.detail2.adjustHistoryModel')}</code>
                        </span>
                        <span className="adjust-history-time">
                          {fmtDateTime(r.created_at)}
                        </span>
                      </div>
                      {r.summary && (
                        <div className="adjust-history-summary">
                          <IconSendOut size={13} className="icon-mr" />{r.summary}
                        </div>
                      )}
                      <div className="adjust-history-stats">
                        <span>{t('requirements.detail2.usageStatInput')} <strong>{fmtNumber(usageTotalInput(r))}</strong></span>
                        <span>{t('requirements.detail2.usageStatOutput')} <strong>{fmtNumber(r.output_tokens)}</strong></span>
                        <span className="adjust-history-stat-muted">{t('requirements.detail2.usageCacheRead', { n: fmtNumber(r.cache_read_tokens) })}</span>
                        <span className="adjust-history-stat-muted">{t('requirements.detail2.usageCacheCreate', { n: fmtNumber(r.cache_creation_tokens) })}</span>
                        <span className="adjust-history-stat-cost">{t('requirements.detail2.adjustHistoryCost', { cost: fmtCost(r.costs) })}</span>
                      </div>
                    </div>
                  ))}
                </div>
              </div>
            )}
          </>
        ) : (
          <div className="ledger-empty">
            <span className="ledger-empty-icon" aria-hidden><IconMailbox size={28} /></span>
            <span>
              {usageLoading ? (
                <>{t('requirements.detail2.usageEmptyLoading')}</>
              ) : (
                <>
                  <strong>{t('requirements.detail2.usageEmptyTitle')}</strong>
                  {t('requirements.detail2.usageEmptyDesc')}
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
              <span className="stage-num">
                {isDone ? <IconCheck size={16} /> : <StageIcon stage={s.stage} size={16} />}
              </span>
              <span className="stage-label">{s.label}</span>
              {stageModel && (
                <span className="stage-model-tag" title={t('requirements.detail2.stageModelTagTitle', { label: s.label })}>
                  <IconRobot size={13} /> {stageModel === DefaultModelLabel
                    ? (roleDefaultModels[s.key] ? `${DefaultModelLabel}（${roleDefaultModels[s.key]}）` : DefaultModelLabel)
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
          { stage: t('requirements.detail2.logSessionStageAnalyst'), sid: req.analysis_session_id },
          { stage: t('requirements.detail2.logSessionStageArchitect'), sid: req.design_session_id },
          { stage: t('requirements.detail2.logSessionStageDeveloper'), sid: req.coding_session_id },
        ].filter(r => r.sid);
        if (rows.length === 0) return null;
        return (
          <details className="session-panel">
            <summary>
              <span className="session-caret"><IconPlay size={11} /></span>
              <IconWrench size={13} className="icon-mr" />{t('requirements.detail2.claudeSessionTitle', { n: rows.length })}
            </summary>
            <div className="session-body">
              <p className="session-hint" dangerouslySetInnerHTML={{ __html: t('requirements.detail2.claudeSessionHint') }} />
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
                    <IconCopy size={12} className="btn-icon" />{t('requirements.detail2.copyBtn')}
                  </button>
                </div>
              ))}
            </div>
          </details>
        );
      })()}

      {/* ── Analyst stage ── */}
      {/* While analyzing, DeepRefineChat is itself the section (own card + header),
          so we render it standalone — no outer "Analysis" card around it, which
          would otherwise create a card-in-card with two overlapping magnifier headers. */}
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
          <div className="section-header"><h3><IconMagnifier size={16} className="icon-mr" />{t('requirements.detail2.logSessionStageAnalyst')}</h3></div>
          <div className="tab-empty">
            {req.skip_analysis ? (
              <>
                <p>
                  {reqKind === 'idea'
                    ? t('requirements.detail2.draftIdeaSkipHint')
                    : reqKind === 'issue'
                      ? t('requirements.detail2.draftIssueSkipHint')
                      : t('requirements.detail2.draftReqSkipHint')}
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
                      {busy === '生成技术方案' /* i18n: protocol literal — state sentinel compared at render */
                        ? <><IconHourglass size={13} className="btn-icon" />...</>
                        : <><IconTriangle size={13} className="btn-icon" />{t('requirements.detail2.intentGenerateDesign')}</>}
                    </button>
                  )}
                  {/* Scheduled design generation: opens ScheduleModal pre-loaded
                      with the current architect model. Hidden for kind=idea
                      (same gating as the primary button above). Disabled
                      when a pending design row already exists (backend 409s
                      in that case; the UI just gets there first). */}
                  {reqKind !== 'idea' && !pendingByType.design && (
                    <button
                      className="btn btn-sm"
                      onClick={() => setScheduleModal({ taskType: 'design' })}
                      disabled={!!busy}
                      title={t('requirements.detail2.scheduleDesignTitle')}
                    >
                      <IconClock size={13} className="btn-icon" />{t('requirements.detail2.scheduleDesignBtn')}
                    </button>
                  )}
                  {/* Pending schedule hint — surfaces the planned time and
                      offers an inline cancel link so the user doesn't have
                      to navigate to the SchedulesPage. */}
                  {pendingByType.design && (
                    <PendingScheduleHint
                      task={pendingByType.design}
                      onCancel={async () => {
                        await schedulesApi.cancel(pendingByType.design!.id);
                        loadPendingSchedules();
                      }}
                    />
                  )}
                  {/* Architect-model selectable BEFORE generating the plan.
                      Irrelevant for kind=idea — the architect stage is hidden. */}
                  {reqKind !== 'idea' && (
                    <ModelSelect
                      value={architectModel}
                      onChange={setArchitectModel}
                      stage="architect"
                      label={t('requirements.detail2.architectModelLabel')}
                      defaultModelName={architectDefaultModel}
                      title={t('requirements.detail2.architectModelTitle')}
                    />
                  )}
                  {/* For kind=idea this is the only CTA — promote it from a
                      muted "run analysis first..." link to a primary button. */}
                  {/* i18n: protocol literal — '开始分析' is the busy-state sentinel */}
                  <button
                    className={reqKind === 'idea' ? 'btn btn-primary' : 'btn btn-sm'}
                    onClick={() => transition('analyzing', '开始分析' /* i18n: protocol literal */)} disabled={!!busy}
                    title={reqKind === 'idea' ? t('requirements.detail2.startAnalysisIdeaTitle') : t('requirements.detail2.startAnalysisIssueTitle')}
                  >
                    {busy === '开始分析' /* i18n: protocol literal — state sentinel */
                      ? <><IconHourglass size={13} className="btn-icon" />...</>
                      : reqKind === 'idea'
                        ? <><IconChat size={13} className="btn-icon" />{t('requirements.detail2.startAnalysisIdeaBtn')}</>
                        : reqKind === 'issue'
                          ? <><IconMagnifier size={13} className="btn-icon" />{t('requirements.detail2.startAnalysisIssueBtn')}</>
                          : t('requirements.detail2.startAnalysisReqBtn')}
                  </button>
                  {/* Analyst-model selectable before opting into the analysis. */}
                  <ModelSelect
                    value={analystModel}
                    onChange={setAnalystModel}
                    stage="analyst"
                    label={t('requirements.detail2.analystModelLabel')}
                    defaultModelName={analystDefaultModel}
                    title={t('requirements.detail2.analystModelTitle')}
                  />
                </div>
              </>
            ) : (
              <>
                <p>
                  {reqKind === 'idea'
                    ? t('requirements.detail2.draftIdeaNormalHint')
                    : reqKind === 'issue'
                      ? t('requirements.detail2.draftIssueNormalHint')
                      : t('requirements.detail2.draftReqNormalHint')}
                </p>
                <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                  {/* i18n: protocol literal — '开始分析' is the busy-state sentinel */}
                  <button className="btn btn-primary" onClick={() => transition('analyzing', '开始分析' /* i18n: protocol literal */)} disabled={!!busy}>
                    {busy === '开始分析' /* i18n: protocol literal — state sentinel */
                      ? <><IconHourglass size={13} className="btn-icon" />...</>
                      : reqKind === 'idea'
                        ? <><IconChat size={13} className="btn-icon" />{t('requirements.detail2.startAnalysisIdeaBtn')}</>
                        : reqKind === 'issue'
                          ? <><IconBug size={13} className="btn-icon" />{t('requirements.detail2.startIssueBugBtn')}</>
                          : <><IconRobot size={13} className="btn-icon" />{t('requirements.detail2.startAnalysisBtn')}</>}
                  </button>
                  {/* Analyst-stage model, selectable BEFORE starting the first
                      analysis turn; the in-chat dropdown is otherwise disabled
                      while the auto-started first turn runs. */}
                  <ModelSelect
                    value={analystModel}
                    onChange={setAnalystModel}
                    stage="analyst"
                    label={t('requirements.detail2.analystModelLabel')}
                    defaultModelName={analystDefaultModel}
                    title={t('requirements.detail2.analystModelTitle')}
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
            {/* Per-stage architect model. Default = currently configured design
                model; disabled while a design job runs (Claude is working —
                model switch is locked). */}
            <ModelSelect
              value={architectModel}
              onChange={setArchitectModel}
              disabled={architectWorking}
              working={architectWorking}
              stage="architect"
              label={t('requirements.detail2.architectModelLabel')}
              defaultModelName={architectDefaultModel}
              title={architectWorking ? t('requirements.detail2.architectModelBusyTitle') : t('requirements.detail2.architectModelTitle')}
            />
            {/* Design-stage Agent server selector. Mirrors the dev-stage
                picker in the coding modal: empty = local execution (default),
                non-empty = route the architect run through that Agent server
                (plan mode on the remote worker, see
                backend/internal/handler/wizard_architect.go execArchitectDesign).
                The same shared `agentServerId` state is reused — it covers
                design + dev within a single requirement so a page refresh
                doesn't drop the user's choice between stages. The seed
                effect above prefers req.design_agent_server_id when it
                differs from req.agent_server_id. Only `ready` servers are
                populated (server-side guard mirrors this in the handler). */}
            <label className="design-agent-server" style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
              <span style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>
                {t('requirements.detail2.designAgentServerLabel')}
              </span>
              <ExecEnvSelect
                servers={agentServers}
                value={agentServerId}
                onChange={setAgentServerId}
                disabled={architectWorking}
                title={architectWorking
                  ? t('requirements.detail2.designAgentServerBusyTitle')
                  : (agentServers.length === 0 ? t('requirements.detail2.designAgentServerEmptyTitle') : '')}
                localOptionLabel={t('requirements.detail2.preflightLocalExec')}
                style={{ minWidth: 160 }}
              />
            </label>
            {/* Persisted design environment: shows which Agent Server (or 本地)
                the architect stage last ran on, read back from
                requirements.design_agent_server_id. Only rendered once a design
                has actually run on a remote server so the toolbar stays clean
                for local-only requirements. */}
            {req.design_agent_server_id && (
              <ExecEnvBadge
                serverId={req.design_agent_server_id}
                serverName={req.design_agent_server_name}
                compact
              />
            )}
            {agentServers.length === 0 && (
              <div style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>
                {t('requirements.detail2.designAgentServerNoHint')}
              </div>
            )}
            {showDesignToggle && (
              <button
                className="btn btn-sm process-toggle"
                onClick={() => setShowDesignProcess(v => !v)}
                aria-expanded={designPanelOpen}
              >
                {designPanelOpen ? t('requirements.detail2.processToggleHide') : t('requirements.detail2.processToggleShow')}
              </button>
            )}
            {req.status === 'designing' && hasDesign && (
              <button className="btn btn-sm" onClick={() => requestDesignKnowledge(false)} disabled={designing}><IconRefresh size={13} className="btn-icon" />{t('requirements.detail2.regenerateBtn')}</button>
            )}
            {hasDesign && (
              <button
                className="btn btn-sm"
                onClick={handleExportPdf}
                disabled={exporting}
                style={{ marginLeft: 'auto' }}
                title={t('requirements.detail2.exportPdfTitle')}
              >
                {exporting ? <><IconHourglass size={13} className="btn-icon" />{t('requirements.detail2.exportingPdf')}</> : <><IconFileText size={13} className="btn-icon" />{t('requirements.detail2.exportPdfBtn')}</>}
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
                  compress), so compressible=false hides the compress button and only
                  the usage readout remains. Mirrors DocRefineChat's design-doc
                  bar; onCompress is a no-op since the button is suppressed. */}
              <ContextUsageBar
                usage={designUsage}
                onCompress={() => {}}
                compressible={false}
                disabled
                stepLabel={t('requirements.detail2.logSessionStageArchitect')}
              />
              <CodingLines lines={designLines} working={designing} />
              {designProcessActive && <div className="coding-line coding-line-tool_call"><IconHourglass size={12} className="icon-mr" />{t('requirements.detail2.architectPlanModeHint')}</div>}
            </div>
          )}

          {req.status === 'designing' && !hasDesign && !designing && !req.design_job_id && reqKind !== 'idea' && (
            <div className="tab-empty">
              <p dangerouslySetInnerHTML={{ __html: t('requirements.detail2.architectIdleHint') }} />
              <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                <button className="btn btn-primary" onClick={() => requestDesignKnowledge(false)} disabled={!!busy || designing}>
                  {busy === '生成技术方案' /* i18n: protocol literal — state sentinel */
                    ? <><IconHourglass size={13} className="btn-icon" />...</>
                    : <><IconTriangle size={13} className="btn-icon" />{t('requirements.detail2.startArchitectBtn')}</>}
                </button>
                {/* Roll back to the analyst stage. The backend allows
                    designing → analyzing; this is the recovery path when the
                    user advanced with a failed/incomplete analysis.
                    Hidden for skip-analysis requirements since there is no
                    prior analysis session to return to. */}
                {!req.skip_analysis && (
                  <button
                    className="btn btn-sm"
                    onClick={() => transition('analyzing', '返回重新分析' /* i18n: protocol literal — state sentinel */
                    )}
                    disabled={!!busy}
                    title={t('requirements.detail2.backToAnalysisTitle')}
                  >
                    <IconArrowBack size={12} className="btn-icon" />{t('requirements.detail2.backToAnalysisBtn')}
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
                        <h4><IconFileText size={13} className="icon-mr" />{t('requirements.detail2.designSectionFiles')}</h4>
                        <ul>{design.files.map((f, i) => <li key={i}><code>{f}</code></li>)}</ul>
                      </div>
                    )}
                    {design.steps && design.steps.length > 0 && (
                      <div className="analysis-block">
                        <h4><IconListOrdered size={13} className="icon-mr" />{t('requirements.detail2.designSectionSteps')}</h4>
                        <ol>{design.steps.map((s, i) => <li key={i}>{s}</li>)}</ol>
                      </div>
                    )}
                    {design.model_changes && design.model_changes !== t('requirements.detail2.designModelChangesNone') && (
                      <div className="analysis-block"><h4><IconDatabase size={13} className="icon-mr" />{t('requirements.detail2.designSectionModelChanges')}</h4><p>{design.model_changes}</p></div>
                    )}
                    {design.risks && design.risks.length > 0 && (
                      <div className="analysis-block">
                        <h4><IconAlert size={13} className="icon-mr" />{t('requirements.detail2.designSectionRisks')}</h4>
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
                  {designExpanded ? t('requirements.detail2.designExpandCollapse') : t('requirements.detail2.designExpandShow')}
                </button>
              )}
            </>
          )}

          {req.status === 'designing' && hasDesign && (
            <>
              <div style={{ display: 'flex', gap: 8, marginTop: 12, flexWrap: 'wrap' }}>
                <button className="btn btn-primary" onClick={() => transition('designed', '方案完成' /* i18n: protocol literal — state sentinel */
                )} disabled={!!busy}>
                  {busy === '方案完成' /* i18n: protocol literal — state sentinel */
                    ? <><IconHourglass size={13} className="btn-icon" />...</>
                    : <><IconTriangle size={13} className="btn-icon" />{t('requirements.detail2.designCompleteBtn')}</>}
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
                onWorkingChange={setRefineWorking}
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
          <div className="section-header"><h3><IconRocket size={16} className="icon-mr" />{t('requirements.detail2.logSessionStageDeveloper')}</h3></div>

          {/* Optional knowledge pre-read display (renders only when the user
              opted in and the backend emitted a knowledge event). */}
          <KnowledgeReadPanel items={knowledgeItems} empty={knowledgeEmpty} projectId={project?.id} />

          {(req.status === 'designed' || (req.status === 'draft' && req.skip_design)) && codingLines.length === 0 && !coding && reqKind !== 'idea' && (
            <div className="tab-empty">
              <p>{req.status === 'designed'
                ? t('requirements.detail2.devReadyDesignedHint')
                : t('requirements.detail2.devReadySkipHint')}</p>
              <p style={{ fontSize: 12, color: 'var(--color-text-muted)' }}>{t('requirements.detail2.devReadyPathLabel')}<code>{project?.local_path}</code></p>
              <p style={{ fontSize: 12, color: 'var(--color-text-muted)' }}
                dangerouslySetInnerHTML={{ __html: t('requirements.detail2.devReadyWorktreeHint', { path: project?.local_path || '', id: req.id }) }}
              />
              <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                <button className="btn btn-primary" onClick={() => openBranchModal()}><IconRocket size={13} className="btn-icon" />{t('requirements.detail2.ctaStartCoding')}</button>
                {/* Scheduled coding: opens ScheduleModal pre-loaded with the
                    current developer model + branch defaults. Same gating
                    as the primary button (kind=idea hidden; status in
                    {designed, draft+skip_design} only). Disabled when a
                    pending coding row already exists. */}
                {!pendingByType.coding && (
                  <button
                    className="btn btn-sm"
                    onClick={() => setScheduleModal({ taskType: 'coding' })}
                    title={t('requirements.detail2.scheduleCodingTitle')}
                  >
                    <IconClock size={13} className="btn-icon" />{t('requirements.detail2.scheduleCodingBtn')}
                  </button>
                )}
                {pendingByType.coding && (
                  <PendingScheduleHint
                    task={pendingByType.coding}
                    onCancel={async () => {
                      await schedulesApi.cancel(pendingByType.coding!.id);
                      loadPendingSchedules();
                    }}
                  />
                )}
                {/* Per-stage developer model. Default = currently configured dev
                    model; disabled while a coding job runs (Claude is working —
                    model switch is locked). */}
                <ModelSelect
                  value={developerModel}
                  onChange={setDeveloperModel}
                  disabled={coding}
                  working={coding}
                  stage="developer"
                  label={t('requirements.detail2.devModelLabel')}
                  defaultModelName={developerDefaultModel}
                  title={coding ? t('requirements.detail2.devModelBusyTitle') : t('requirements.detail2.devModelIdleTitle')}
                  configId={developerConfigId}
                  onConfigChange={setDeveloperConfigId}
                />
                {/* Agent-server selector. Empty = local execution (the default
                    and the only path before this feature); non-empty routes the
                    claude CLI to that remote target. Only `ready` servers are
                    listed — Check must succeed before coding can target them. */}
                <label style={{ fontSize: 12, color: 'var(--color-text-muted)', display: 'inline-flex', alignItems: 'center', gap: 4 }}>
                  {t('requirements.detail2.devEnvLabel')}
                  <ExecEnvSelect
                    servers={agentServers}
                    value={agentServerId}
                    onChange={setAgentServerId}
                    disabled={coding}
                    title={agentServerId
                      ? t('requirements.detail2.devEnvOnTitle', { name: agentServers.find((s) => s.id === agentServerId)?.name ?? '' })
                      : t('requirements.detail2.devEnvLocalTitle')}
                    localOptionLabel={t('requirements.detail2.devEnvLocalOption')}
                    style={{ minWidth: 140 }}
                  />
                </label>
                {/* Development-mode selector. Empty = reuse previous setting
                    (first-run default = session); 'session' = session-based
                    dev (continue in the original design session, legacy
                    behavior); 'design' = design-based dev (create a new
                    session and hand the design as the only input to the
                    Agent). Seed matches dev_source: first entry reads
                    req.dev_mode. */}
                <label style={{ fontSize: 12, color: 'var(--color-text-muted)', display: 'inline-flex', alignItems: 'center', gap: 4 }}>
                  {t('requirements.detail2.devModeLabel')}
                  <select
                    className="form-input"
                    style={{ minWidth: 150 }}
                    value={devMode}
                    onChange={(e) => setDevMode(e.target.value as '' | 'session' | 'design')}
                    disabled={coding}
                    title={devMode === 'design'
                      ? t('requirements.detail2.devModeNewSessionTitle')
                      : devMode === 'session'
                      ? t('requirements.detail2.devModeResumeSessionTitle')
                      : t('requirements.detail2.devModeEmptyTitle')}
                  >
                    <option value="">{t('requirements.detail2.devModeKeepOption')}</option>
                    <option value="session">{t('requirements.detail2.devModeSessionOption')}</option>
                    <option value="design">{t('requirements.detail2.devModeDesignOption')}</option>
                  </select>
                </label>
                {agentServers.length === 0 && (
                  <Link to="/settings/agent-servers" style={{ fontSize: 12 }}>
                    {t('requirements.detail2.devModeAgentServerLink')}
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
              {/* Live context-usage bar + compress-context entry point for the
                  coding stage. Multi-turn (--resume coding_session_id), so
                  compressible is true — the button hands off to
                  wizardApi.compressContext (step:'coding'). Mirrors
                  CodingChat / DeepRefineChat. Disabled while a coding /
                  adjust turn is in flight or a compression runs. */}
              <ContextUsageBar
                usage={codingUsage}
                onCompress={handleCodingCompress}
                compressing={codingCompressing}
                disabled={coding || codingCompressing}
                stepLabel={t('requirements.detail2.logSessionStageDeveloper')}
                compressedAt={codingCompressedAt}
                onShowSummary={handleShowCodingSummary}
              />
              <CodingLines lines={codingLines} working={coding} />
              {coding && <div className="coding-line coding-line-tool_call"><IconHourglass size={12} className="icon-mr" />{t('requirements.detail2.codingWorkingHint')}</div>}
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
                  <h3><IconArchive size={16} className="icon-mr" />{t('requirements.detail2.codingCompressModalTitle')}</h3>
                  <button className="btn btn-sm" onClick={() => setCodingSummaryModal(null)}>{t('requirements.detail2.btnClose')}</button>
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

          {/* Follow-up adjustment: resumes the coding session (--resume),
              carrying only this prompt; output is appended to the coding
              panel above, in continuity with the first coding round. Shown
              in both developing and done states. Hidden when the requirement
              has been decomposed into sub-tasks (hasSubTasks) — all further
              adjustments must flow through the sub-task composer so the
              sub-agents' parallel contexts are not overwritten by the main
              session resume. */}
          {req.coding_session_id && (req.status === 'developing' || req.status === 'done') && !coding && !hasSubTasks && (
            <div className="adjust-composer">
              <div className="adjust-composer-header">
                <span className="ac-title"><IconWrench size={13} className="icon-mr" />{t('requirements.detail2.adjustSectionTitle')}</span>
                <span className="ac-tag">{t('requirements.detail2.adjustSessionTag')}</span>
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
                    configId={developerConfigId}
                    onConfigChange={setDeveloperConfigId}
                  />
                </div>
              </div>
              <textarea
                className="ac-textarea"
                rows={3}
                value={adjustInput}
                onChange={e => setAdjustInput(e.target.value)}
                placeholder={t('requirements.detail2.adjustComposerPlaceholder')}
                onKeyDown={e => {
                  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); doAdjustCoding(); }
                }}
              />
              <div className="adjust-composer-footer stack-mobile">
                <span className="ac-hint">{t('requirements.detail2.adjustSendHint')}</span>
                <button className="btn btn-primary" onClick={doAdjustCoding} disabled={!adjustInput.trim()}>
                  <IconRocket size={13} className="btn-icon" />{t('requirements.detail2.adjustSectionTitle')}
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
                  {t('requirements.detail2.codingResumeLostHint')}
                </p>
              )}
              <div style={{ display: 'flex', gap: 8, marginTop: 12, flexWrap: 'wrap' }}>
                {req.coding_session_id && codingLines.length === 0 && (
                  <button
                    className="btn btn-primary"
                    title={t('requirements.detail2.codingContinueTitle')}
                    onClick={doContinueCoding}
                    disabled={!!busy}
                  >
                    {t('requirements.detail2.codingContinueBtn')}
                  </button>
                )}
                <button className="btn btn-primary" onClick={() => transition('done', '开发完成' /* i18n: protocol literal — state sentinel */
                )} disabled={!!busy}>
                  {busy === '开发完成' /* i18n: protocol literal — state sentinel */
                    ? <><IconHourglass size={13} className="btn-icon" />...</>
                    : <><IconCheck size={13} className="btn-icon" />{t('requirements.detail2.codingCompleteBtn')}</>}
                </button>
                {reqKind !== 'idea' && (
                  <button className="btn" title={t('requirements.detail2.codingRedoTitle')} onClick={() => openBranchModal()}><IconRefresh size={13} className="btn-icon" />{t('requirements.detail2.codingRedoBtn')}</button>
                )}
              </div>

              {/* Sub-agent collaboration: user-triggered sub-tasks that share
                  the main agent's context. Available in both developing and
                  done states (during dev to divide work; after completion
                  for small follow-up tweaks). Positioned right after
                  "Mark complete / Re-develop" and before Merge / PR, matching
                  the top-to-bottom stage flow. onSubTasksChange writes the
                  current sub-task count back into liveSubTaskCount so
                  hasSubTasks takes effect the moment a sub-task is created
                  and hides the requirement-level "Follow-up" entry. Setter
                  ref is stable so it does not loop the panel render. */}
              {(req.status === 'developing' || req.status === 'done') && reqKind !== 'idea' && (
                <SubTaskPanel
                  requirementId={req.id}
                  codingSessionId={req.coding_session_id}
                  requirement={req}
                  onSubTasksChange={setLiveSubTaskCount}
                  developerDefaultModel={developerDefaultModel}
                  batch={orchBatch}
                  onBatchChange={fetchOrchBatch}
                  agentServers={agentServers}
                />
              )}

              {/* ── Merge / PR step ── */}
              <div className="merge-section">
                <div className="merge-actions stack-mobile">
                  <button className="btn" onClick={() => openMergeModal('local')} disabled={merging}><IconMerge size={13} className="btn-icon" />{t('requirements.detail2.mergeLocalTitle')}</button>
                  <button className="btn" onClick={() => openMergeModal('push')} disabled={merging}><IconGlobe size={13} className="btn-icon" />{t('requirements.detail2.mergePrTitle')}</button>
                </div>

                {mergeState?.worktree_path && (
                  <WorktreePathHint
                    path={mergeState.worktree_path}
                    onClean={cleanWorktree}
                    cleaning={!!busy}
                    disabled={merging}
                  />
                )}

                {mergeState?.mid_merge && (
                  <div className="conflict-panel">
                    <p className="conflict-title"><IconAlert size={14} className="icon-mr" />{t('requirements.detail2.mergeConflictTitle')}</p>
                    {conflictFiles && conflictFiles.length > 0 && (
                      <ul className="conflict-file-list">
                        {conflictFiles.map((f, i) => <li key={i} className="conflict-file"><code>{f}</code></li>)}
                      </ul>
                    )}
                    <div className="conflict-actions">
                      <button className="btn btn-primary" onClick={() => doMergeAction('resolve')} disabled={merging}><IconRobot size={13} className="btn-icon" />{t('requirements.detail2.mergeConflictResolveAiBtn')}</button>
                      <button className="btn" onClick={() => doMergeAction('continue')} disabled={merging}><IconHand size={13} className="btn-icon" />{t('requirements.detail2.mergeConflictResolveManualBtn')}</button>
                      <button className="btn btn-danger" onClick={() => doMergeAction('abort')} disabled={merging}><IconArrowBack size={13} className="btn-icon" />{t('requirements.detail2.mergeConflictAbortBtn')}</button>
                    </div>
                  </div>
                )}

                {mergeLines.length > 0 && (
                  <div className={`coding-panel merge-panel ${mergeFs.isFullscreen ? 'is-fullscreen' : ''}`}>
                    {mergeFs.isFullscreen && (
                      <FullscreenButton isFullscreen onClick={mergeFs.exit} variant="floating" />
                    )}
                    <CodingLines lines={mergeLines} working={merging} />
                    {merging && <div className="coding-line coding-line-tool_call"><IconHourglass size={12} className="icon-mr" />{t('requirements.detail2.mergeRunningHint')}</div>}
                  </div>
                )}

                {prLink && !merging && (
                  <a className="btn btn-primary pr-link-btn" href={prLink} target="_blank" rel="noreferrer">
                    <IconGlobe size={13} className="btn-icon" />{t('requirements.detail2.mergeCreatePrBtn')}
                  </a>
                )}
              </div>
            </>
          )}

          {req.status === 'done' && (
            <div className="merge-section">
              {prLink ? (
                <a className="btn btn-primary pr-link-btn" href={prLink} target="_blank" rel="noreferrer"><IconGlobe size={13} className="btn-icon" />{t('requirements.detail2.mergeViewOrCreatePrBtn')}</a>
              ) : (
                <div className="tab-empty"><p><IconCheck size={14} className="icon-mr" />{t('requirements.detail2.devCompleteHint')}</p></div>
              )}
              <div className="merge-actions stack-mobile">
                <button className="btn" onClick={() => openMergeModal('local')} disabled={merging}><IconMerge size={13} className="btn-icon" />{t('requirements.detail2.mergeLocalTitle')}</button>
                <button className="btn" onClick={() => openMergeModal('push')} disabled={merging}><IconGlobe size={13} className="btn-icon" />{t('requirements.detail2.mergePrTitle')}</button>
              </div>
              {mergeState?.worktree_path && (
                <WorktreePathHint
                  path={mergeState.worktree_path}
                  onClean={cleanWorktree}
                  cleaning={!!busy}
                  disabled={merging}
                />
              )}
              {/* Sub-agent collaboration (also open in the done stage): once
                  "Follow-up" is hidden, sub-tasks are the only adjustment
                  entry point in the done state. Position matches the
                  developing branch: after the merge / PR area, before the
                  "Archive to knowledge base" button. Hidden for Idea to
                  avoid surfacing dev tools for exploratory ideas. */}
              {reqKind !== 'idea' && (
                <SubTaskPanel
                  requirementId={req.id}
                  codingSessionId={req.coding_session_id}
                  requirement={req}
                  onSubTasksChange={setLiveSubTaskCount}
                  developerDefaultModel={developerDefaultModel}
                  batch={orchBatch}
                  onBatchChange={fetchOrchBatch}
                  agentServers={agentServers}
                />
              )}
              <div className="merge-actions stack-mobile" style={{ marginTop: 8 }}>
                <button className="btn btn-primary" onClick={handleArchive} disabled={!!busy}>
                  {busy === '归档' /* i18n: protocol literal — state sentinel */
                    ? <><IconHourglass size={13} className="btn-icon" />...</>
                    : <><IconArchive size={13} className="btn-icon" />{t('requirements.detail2.archiveToKbBtn')}</>}
                </button>
                {showPromoteCta && (
                  <button className="btn" onClick={handlePromoteToRequirement} disabled={!!busy}>
                    {busy === '转为需求' /* i18n: protocol literal — state sentinel */
                      ? <><IconHourglass size={13} className="btn-icon" />...</>
                      : <><IconCopy size={13} className="btn-icon" />{t('requirements.detail2.ctaPromote')}</>}
                  </button>
                )}
              </div>
            </div>
          )}

          {req.status === 'archived' && (
            <div className="merge-section">
              <div className="tab-empty">
                <p><IconArchive size={14} className="icon-mr" />{t('requirements.detail2.archivedHint')}</p>
              </div>
              <div className="merge-actions stack-mobile" style={{ marginTop: 8 }}>
                <button className="btn" onClick={handleUnarchive} disabled={!!busy}>
                  {busy === '取消归档' /* i18n: protocol literal — state sentinel */
                    ? <><IconHourglass size={13} className="btn-icon" />...</>
                    : <><IconArrowBack size={13} className="btn-icon" />{t('requirements.detail2.ctaUnarchive')}</>}
                </button>
                {showPromoteCta && (
                  <button className="btn" onClick={handlePromoteToRequirement} disabled={!!busy}>
                    {busy === '转为需求' /* i18n: protocol literal — state sentinel */
                      ? <><IconHourglass size={13} className="btn-icon" />...</>
                      : <><IconCopy size={13} className="btn-icon" />{t('requirements.detail2.ctaPromote')}</>}
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

      {scheduleModal && (
        <ScheduleModal
          open
          taskType={scheduleModal.taskType}
          requirementId={req.id}
          requirementTitle={req.title}
          initialModel={scheduleModal.taskType === 'design' ? architectModel : developerModel}
          defaultBranchName={
            scheduleModal.taskType === 'coding'
              ? `feat/${req.id.replace(/^req_/, '')}`
              : undefined
          }
          defaultBaseBranch={
            scheduleModal.taskType === 'coding' ? (project?.default_branch ?? 'main') : undefined
          }
          agentServers={agentServers.map(s => ({ id: s.id, name: s.name, host: s.host }))}
          onClose={() => setScheduleModal(null)}
          onScheduled={() => {
            setScheduleModal(null);
            loadPendingSchedules();
          }}
        />
      )}
    </div>
  );
}

// PendingScheduleHint is the inline "Scheduled HH:MM ... [Cancel]" strip
// rendered at the top of the design / developer sections when a pending
// row exists for the current requirement. Kept tiny so it never displaces
// the real wizard controls.
function PendingScheduleHint({
  task,
  onCancel,
}: {
  task: ScheduledTask;
  onCancel: () => void;
}) {
  const { t } = useTranslation();
  const when = new Date(task.run_at);
  const formatted = fmtDateTime(when.toISOString());
  return (
    <span
      style={{
        fontSize: 12,
        color: '#0e7490',
        background: '#cffafe',
        borderRadius: 4,
        padding: '4px 8px',
        display: 'inline-flex',
        alignItems: 'center',
        gap: 6,
      }}
    >
      <IconClock size={12} />{t('requirements.detail2.pendingScheduleLabel', { when: formatted })}
      <button
        className="btn btn-sm"
        onClick={onCancel}
        style={{ padding: '0 6px', fontSize: 11 }}
        title={t('requirements.detail2.pendingScheduleCancelTitle')}
      >
        {t('requirements.detail2.pendingScheduleCancelBtn')}
      </button>
    </span>
  );
}
