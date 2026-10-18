// CreateRequirementForm — the standalone "new requirement" panel.
//
// Layout is three steps: kind → description → secondary settings.
//   1. A one-row segmented kind switcher (issue / requirement / idea) with a
//      per-kind hint about what to write underneath.
//   2. The description textarea is the body: autofocused, char-counted,
//      ⌘/Ctrl+Enter submits. The "skip AI organize" toggle sits under it.
//   3. Project / priority / workflow are secondary and live below; the
//      workflow picker reuses the segmented style and defaults to "code now"
//      so the common small-change path stays zero-clicks.
//
// kind=idea hides priority + workflow (an idea is not yet committed to being
// built — it always lands in the analyst discussion). The skip-organize
// toggle applies to every kind: it only controls whether the description
// goes through the LLM-organized Markdown pass.
//
// props:
//   - projectId / projectOptions: exactly one is required. projectId means
//     "create inside a known project"; projectOptions renders a project
//     picker for cross-project creation.
//   - onCreated / onClose: success and close callbacks.

import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import RichDescriptionEditor from '../RichDescriptionEditor/RichDescriptionEditor';
import LaunchPlanSection from './LaunchPlanSection';
import { ApiError, kindLabelKeys, kindHintKeys, kindPlaceholderKeys, kindCreateLabelKeys, type Kind, type AgentServer, type LaunchSpec, type KnowledgeItem, type Project, agentServersApi, projectsApi, requirementsApi, type Requirement } from '../../api/client';
import { tLabel } from '../../i18n/label';
import { errorMessage } from '../../utils/errMsg';
import PdfUploadDialog from '../PdfUploadDialog';
import './CreateRequirementForm.css';

const ACCEPTED_IMPORT_EXTS = ['.md', '.markdown', '.pdf', '.docx', '.txt'];
const ACCEPTED_IMPORT_ATTR = ACCEPTED_IMPORT_EXTS.join(',');

type Flow = 'skip-analysis' | 'direct';

// Workflow options hold translation KEYS, resolved during render — a
// literal label here would freeze the language at import time.
const FLOW_OPTIONS: { value: Flow; labelKey: string }[] = [
  { value: 'direct', labelKey: 'components.createRequirement.flowDirect' },
  { value: 'skip-analysis', labelKey: 'components.createRequirement.flowSkipAnalysis' },
];

const FLOW_NOTE_KEYS: Record<Flow, string> = {
  'skip-analysis': 'components.createRequirement.flowNoteSkipAnalysis',
  direct: 'components.createRequirement.flowNoteDirect',
};

const KIND_SUBMIT_HINT_KEYS: Record<Kind, string> = {
  issue: 'components.createRequirement.submitHintIssue',
  idea: 'components.createRequirement.submitHintIdea',
  requirement: 'components.createRequirement.submitHintRequirement',
  wiki: 'components.createRequirement.submitHintWiki',
};

// Placeholder description for the pre-create draft row. The server's Create
// handler refuses empty descriptions, so we seed a single-character string
// that the user never sees (it's only stored in the DB until the final
// update overwrites it). Kept deliberately short and self-explaining so a
// stray listing render can't be mistaken for real content.
const DRAFT_PLACEHOLDER_DESCRIPTION = '（草稿）';

// Mirror of backend fallbackTitle (service/requirement.go:fallbackTitle).
// Used to seed the title on the final update, since the existing Update
// SQL sets title=? (no COALESCE) and we don't want to lose the auto-derived
// first-line title.
// isRequirementNotFound — true when the server reports that the draft row
// this form was updating no longer exists. The pre-created draft (see the
// mount effect below) can vanish between mount and submit: it may be deleted
// from another tab, or the page may have been left open across a database
// switch / backend restart. The raw INTERNAL form is accepted too because
// older backends returned the same condition that way.
function isRequirementNotFound(err: unknown): boolean {
  if (!(err instanceof ApiError)) return false;
  return (
    err.code === 'NOT_FOUND' ||
    (err.code === 'INTERNAL' && /requirement not found/i.test(err.detail))
  );
}

function fallbackTitle(content: string): string {
  const trimmed = content.trim();
  if (!trimmed) return '新需求';
  const newlineIdx = trimmed.indexOf('\n');
  const first = (newlineIdx >= 0 ? trimmed.slice(0, newlineIdx) : trimmed).trim();
  const chars = Array.from(first);
  if (chars.length > 60) return chars.slice(0, 60).join('') + '...';
  return first;
}

export interface CreateRequirementFormProps {
  projectId?: string;
  projectOptions?: { id: string; name: string }[];
  defaultKind?: Kind;
  onClose: () => void;
  onCreated: (req: Requirement) => void;
  /** Pre-existing requirement id; if provided the form skips the draft
   *  pre-create and submits via PUT /api/requirements/{id}. Reserved for
   *  the upcoming edit-existing flow; this PR leaves it as an optional
   *  escape hatch. */
  existingRequirementId?: string;
  /** Fired when the pre-create draft row lands, so the parent can show
   *  "草稿已自动保存" copy or pre-emptively link to the new id. */
  onDraftCreated?: (reqId: string) => void;
}

export function CreateRequirementForm({
  projectId: fixedProjectId,
  projectOptions,
  defaultKind = 'requirement',
  onClose,
  onCreated,
  existingRequirementId,
  onDraftCreated,
}: CreateRequirementFormProps) {
  const { t } = useTranslation();
  const [kind, setKind] = useState<Kind>(defaultKind);
  const [description, setDescription] = useState('');
  const [priority, setPriority] = useState('medium');
  // Default to "code now" so the common small-change path is zero-clicks.
  const [flow, setFlow] = useState<Flow>('direct');
  // Skip the LLM-organized description pass by default. Most creators already
  // write their own structured prose; the LLM round-trip is mostly cost with
  // marginal gain. Users who want the structured output opt back in.
  const [skipOrganize, setSkipOrganize] = useState(true);
  const [projectId, setProjectId] = useState(fixedProjectId || '');
  const [saving, setSaving] = useState(false);
  const [importing, setImporting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // id of the draft requirement pre-created on mount so image pastes have a
  // valid owner. Set to existingRequirementId when the form is opened in
  // edit mode (reserved for a future flow; today both this state and the
  // prop are read by the same RichDescriptionEditor instance).
  const [pendingReqId, setPendingReqId] = useState<string | null>(existingRequirementId || null);
  // Tracks whether the user has actually edited the description so the
  // unmount-cleanup effect knows whether to delete the empty draft.
  const [draftTouched, setDraftTouched] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  // PDF 上传(仅 wiki kind 可见) — 上传成功后:
  //   1) 后端已经在 knowledge 表写入条目;
  //   2) 把解析出的 Markdown 预填到 description,让用户基于已解析内容
  //      进一步编辑或直接提交需求(同时关联到项目知识库)。
  const [showPdfUpload, setShowPdfUpload] = useState(false);

  // Launch-plan state. The section is collapsed by default — opening it is
  // an explicit "I'm opting into a long-running task" gesture, matching the
  // existing fast-notepad ergonomics of this panel.
  const [launchOpen, setLaunchOpen] = useState(false);
  const [launchSpec, setLaunchSpec] = useState<LaunchSpec | null>(null);
  // Filter to ready servers here so LaunchPlanSection receives a stable
  // contract; mirrors RequirementDetail.tsx:674-677 exactly.
  const [agentServers, setAgentServers] = useState<AgentServer[]>([]);
  // Active project's metadata — used to seed the launch plan's BASE
  // input (default_branch) + branch-list auto-complete (local_path).
  // Fetched on projectId change; project-picker switches trigger a
  // re-fetch so the BASE default always reflects the user's current
  // selection.
  const [activeProject, setActiveProject] = useState<Project | null>(null);

  // Cross-project mode: pick the first project as a sensible default so the
  // submit button isn't disabled on first paint.
  useEffect(() => {
    if (!fixedProjectId && projectOptions && projectOptions.length > 0 && !projectId) {
      setProjectId(projectOptions[0].id);
    }
  }, [fixedProjectId, projectOptions, projectId]);

  // The description is the one thing every user has to fill in — put the
  // caret there on mount so opening the panel is one click, not two.
  useEffect(() => {
    rootRef.current?.querySelector('textarea')?.focus();
  }, []);

  // Load ready agent servers once on mount. Even when the launch-plan
  // section stays collapsed we still pay this cost — the data is tiny and
  // toggling open later must never trigger a second spinner.
  useEffect(() => {
    let alive = true;
    agentServersApi.list()
      .then((rows) => {
        if (!alive) return;
        setAgentServers(rows.filter((s) => s.status === 'ready'));
      })
      .catch(() => {
        // Non-fatal: an empty list means ExecEnvSelect will show only the
        // local option, which is still a valid choice.
      });
    return () => { alive = false; };
  }, []);

  // Refetch project metadata whenever the user switches projects in the
  // picker (or on first paint). The launch plan's BASE input + branch
  // list both depend on this so an outdated project never leaks a stale
  // default_branch into a new requirement. 404 (rare: project removed
  // mid-session) is treated as "no defaults" — the input falls back to
  // "main" inside LaunchPlanSection.
  useEffect(() => {
    if (!projectId) {
      setActiveProject(null);
      return;
    }
    let alive = true;
    projectsApi.get(projectId)
      .then(p => { if (alive) setActiveProject(p); })
      .catch(() => { if (alive) setActiveProject(null); });
    return () => { alive = false; };
  }, [projectId]);

  // Pre-create a draft requirement row so the image-paste path has a valid
  // owner to attach to. The server's Create handler rejects empty
  // descriptions, so we seed it with a tiny placeholder; the user never
  // sees that string in the textarea (it lives only in the DB row until the
  // final update lands).
  //
  // Skipped when the form is opened in edit mode (existingRequirementId is
  // set) or when there's no project yet. Errors are surfaced to the form's
  // error banner — without a draft, image pastes are disabled but text
  // editing still works, so the form is never blocked.
  useEffect(() => {
    if (existingRequirementId || !projectId || pendingReqId) return;
    let alive = true;
    requirementsApi.create({
      project_id: projectId,
      description: DRAFT_PLACEHOLDER_DESCRIPTION,
      priority: 'medium',
      kind,
      skip_analysis: kind !== 'idea',
      skip_design: kind === 'idea' ? false : kind === 'wiki' ? true : flow === 'direct',
      skip_organize: true,
    })
      .then((req) => {
        if (!alive) return;
        setPendingReqId(req.id);
        onDraftCreated?.(req.id);
      })
      .catch((err) => {
        if (alive) setError(errorMessage(err));
      });
    return () => { alive = false; };
    // kind/flow are intentionally out of the dep list — we only want to
    // mint the draft once per project, then patch it on submit.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [existingRequirementId, projectId]);

  // Cleanup: if the user closed the form without typing anything, drop the
  // pre-created draft row so requirements.list doesn't fill with empty
  // stubs. Best-effort (fire-and-forget); we don't block unmount.
  //
  // The verdict is recomputed on every render into a ref; only a MOUNT-ONLY
  // effect reads it. The obvious version of this — listing description /
  // pendingReqId / draftTouched in the dep array — is wrong in a way that
  // breaks creation outright: React runs the *previous* render's cleanup
  // before re-running an effect whose deps changed, so the cleanup triggered
  // by the very first keystroke still closed over description="" and deleted
  // the draft the user was typing into. Submit then PUT
  // /api/requirements/<id> against a row that no longer existed, and the
  // server answered "requirement not found" (Update → Get → sql.ErrNoRows).
  //
  // draftTouched is set only on a successful submit, so it can't carry the
  // "has the user typed?" signal on its own — description.trim() is the real
  // guard, and reading it at unmount time is what makes it correct.
  const dropDraftIdRef = useRef<string | null>(null);
  dropDraftIdRef.current =
    !existingRequirementId && pendingReqId && !draftTouched && !description.trim()
      ? pendingReqId
      : null;
  useEffect(() => {
    return () => {
      const id = dropDraftIdRef.current;
      if (id) requirementsApi.remove(id).catch(() => {});
    };
    // Mount-only on purpose — a dep array here re-runs the cleanup mid-life
    // (every keystroke) instead of only at unmount. See the comment above.
  }, []);

  const handleImportFile = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    // Reset input so selecting the same file again triggers onChange.
    if (fileInputRef.current) fileInputRef.current.value = '';
    if (!file) return;

    const ext = file.name.slice(file.name.lastIndexOf('.')).toLowerCase();
    if (!ACCEPTED_IMPORT_EXTS.includes(ext)) {
      setError(t('components.createRequirement.importFileUnsupported'));
      return;
    }
    if (description.trim() && !window.confirm(t('components.createRequirement.importFileConfirm'))) {
      return;
    }

    setImporting(true);
    setError(null);
    try {
      const { content } = await requirementsApi.parseFile(file);
      setDescription(content);
      setDraftTouched(true);
    } catch (err) {
      setError(t('components.createRequirement.importFileError', { error: errorMessage(err) }));
    } finally {
      setImporting(false);
    }
  };

  const handleSubmit = async () => {
    if (saving) return;
    if (!description.trim()) {
      setError(t('components.createRequirement.errNeedDesc'));
      return;
    }
    if (!projectId) {
      setError(t('components.createRequirement.errNeedProject'));
      return;
    }
    setSaving(true);
    setError(null);
    try {
      // Idea kind is discussion-first by design: drop the user straight into
      // the analyst stage so they can talk to Claude about feasibility. The
      // architect/dev stages remain hidden in the UI, so the `flow` controls
      // (and any skip flags) only apply to issue/requirement.
      // `full` removed — both remaining flows skip analysis, so the flag is
      // now derived from `kind` alone (idea never skips).
      // Wiki kind is single-stage by design — no analyst, no architect.
      // Both skip flags force true so a freshly-created wiki row lands on
      // status='draft' with no analyst session / design session attached,
      // and the detail page's 知识库文档 tab is the only next-step UI.
      const skipAnalysis = kind !== 'idea';
      const skipDesign = kind === 'idea' ? false : kind === 'wiki' ? true : flow === 'direct';

      // Update path: the pre-create effect minted a draft row on mount so
      // the image-paste pipeline had a valid owner. We just overwrite its
      // description / title / priority now. The launch spec is also
      // applied here so a user who typed, pasted images, then opted into a
      // launch plan still gets the dispatch fired against the existing row.
      //
      // Create path: legacy flow for forms that don't have a draft (e.g.
      // an edit-mode form that pre-loaded an existingRequirementId but is
      // somehow missing the live state — defensive fallback).
      const targetId = existingRequirementId || pendingReqId;
      const resolvedPriority = kind === 'idea' ? 'medium' : priority;
      let created: Requirement;
      if (targetId) {
        try {
          created = await requirementsApi.update(targetId, {
            title: fallbackTitle(description),
            description,
            priority: resolvedPriority,
            skip_analysis: skipAnalysis,
            kind,
            skip_design: skipDesign,
            project_id: projectId || undefined,
            launch: launchSpec ?? undefined,
          });
        } catch (err) {
          // The draft row disappeared between mount and submit (deleted in
          // another tab, or this page outlived a database switch/restart).
          // The user's text only exists in this form at this point, so
          // recreate the row rather than failing the submit. skip_organize
          // mirrors the draft path — Update never ran the organizer, so the
          // fallback must not silently rewrite what the user typed either.
          if (!isRequirementNotFound(err)) throw err;
          created = await requirementsApi.create({
            project_id: projectId,
            description,
            priority: resolvedPriority,
            kind,
            skip_analysis: skipAnalysis,
            skip_design: skipDesign,
            skip_organize: true,
            launch: launchSpec ?? undefined,
          });
        }
      } else {
        created = await requirementsApi.create({
          project_id: projectId,
          description,
          priority: resolvedPriority,
          kind,
          skip_analysis: skipAnalysis,
          skip_design: skipDesign,
          skip_organize: skipOrganize,
          launch: launchSpec ?? undefined,
        });
      }
      // Once we've committed the row, the unmount cleanup is a no-op.
      setDraftTouched(true);
      onCreated(created);
    } catch (err: any) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  const handleTextareaKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      handleSubmit();
    }
  };

  const showOptions = kind !== 'idea';
  // The 开发流程 (direct / skip-analysis) switch is hidden for wiki as well as
  // idea. Wiki is a single-stage kind: handleSubmit forces skip_design=true
  // for it, so `flow` is never read — the control would toggle a value
  // nothing consumes, and its note ("小改动，创建后立即进入开发") describes
  // stages a wiki row never enters. LaunchPlanSection agrees: its
  // resolveTaskType short-circuits on kind === 'wiki' before consulting
  // `flow`, so nothing downstream is affected by hiding this.
  // Priority stays visible — it is persisted and rendered for every kind.
  const showFlow = kind !== 'idea' && kind !== 'wiki';
  const showProjectPicker = !fixedProjectId && !!projectOptions && projectOptions.length > 0;
  const canSubmit = !saving && !importing && !!description.trim() && !!projectId;
  const charCount = description.trim().length;

  return (
    <div className="create-req-form" data-testid="create-requirement-form" ref={rootRef}>
      <div className="create-req-form-header">
        <h3>{t('components.createRequirement.title')}</h3>
        <button className="btn btn-secondary btn-sm" onClick={onClose} disabled={saving}>{t('components.createRequirement.collapse')}</button>
      </div>

      {/* Step 1 — Kind switcher. One row, so the panel opens on the writing
          area rather than on a wall of category cards. */}
      <div className="create-req-kind" role="radiogroup" aria-label={t('components.createRequirement.kindAria')}>
        {(Object.keys(kindLabelKeys) as Kind[]).map((k) => (
          <button
            key={k}
            type="button"
            role="radio"
            aria-checked={kind === k}
            className={`create-req-kind-tab${kind === k ? ' selected' : ''}`}
            onClick={() => setKind(k)}
            disabled={saving}
            data-kind={k}
          >
            {tLabel(t, kindLabelKeys as Record<string, string>, k)}
          </button>
        ))}
      </div>
      <p className="create-req-kind-hint">{tLabel(t, kindHintKeys as Record<string, string>, kind)}</p>

      {/* Step 2 — Description (the panel's main job) */}
      <div className="form-group create-req-desc">
        {/* Import-from-file button: available for all kinds. Sits above the
            textarea so it's discoverable without scrolling. */}
        <div className="create-req-import-row">
          <button
            type="button"
            className="btn btn-secondary btn-sm"
            onClick={() => fileInputRef.current?.click()}
            disabled={saving || importing}
            title={t('components.createRequirement.importFileTitle')}
          >
            {importing ? t('components.createRequirement.importing') : t('components.createRequirement.importFile')}
          </button>
          <input
            ref={fileInputRef}
            type="file"
            accept={ACCEPTED_IMPORT_ATTR}
            style={{ display: 'none' }}
            onChange={handleImportFile}
          />
        </div>
        {/* wiki kind 提供 PDF 上传入口:上传后把 Markdown 回填到 description,
            同时后端已经在 knowledge 表留下条目,无需再走 LLM organize。 */}
        {kind === 'wiki' && (
          <div className="create-req-pdf-row">
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => setShowPdfUpload(true)}
              disabled={saving || !projectId}
              title={t('knowledge.uploadPdfTitle')}
            >
              📄 {t('knowledge.uploadPdf')}
            </button>
            <small className="form-hint">{t('components.createRequirement.uploadPdfHint')}</small>
          </div>
        )}
        <RichDescriptionEditor
          value={description}
          onChange={(v) => {
            setDescription(v);
            setDraftTouched(true);
          }}
          requirementId={pendingReqId}
          placeholder={tLabel(t, kindPlaceholderKeys as Record<string, string>, kind)}
          minRows={7}
          disabled={saving || importing}
          onKeyDown={handleTextareaKeyDown}
        />
        <div className="create-req-desc-meta">
          <small className="form-hint">{t(KIND_SUBMIT_HINT_KEYS[kind])}</small>
          <span className="create-req-count">{t('components.createRequirement.charCount', { n: charCount })}</span>
        </div>
        {/* Skip-LLM-organize toggle. Lives directly under the description
            because the toggle's only effect is on how that text gets stored.
            Default ON (skip): the panel is a fast notepad, not an editor that
            runs an LLM round-trip on every submit. */}
        <label className={`create-req-toggle${skipOrganize ? ' on' : ''}`}>
          <input
            type="checkbox"
            checked={skipOrganize}
            onChange={(e) => setSkipOrganize(e.target.checked)}
            disabled={saving}
          />
          <span className="create-req-toggle-track" aria-hidden>
            <span className="create-req-toggle-thumb" />
          </span>
          <span className="create-req-toggle-text">
            <span className="create-req-toggle-label">{t('components.createRequirement.skipOrganize')}</span>
            <span className="create-req-toggle-note">
              {skipOrganize
                ? t('components.createRequirement.skipOrganizeOn')
                : t('components.createRequirement.skipOrganizeOff')}
            </span>
          </span>
        </label>
      </div>

      {/* Step 3 — Secondary settings */}
      {(showProjectPicker || showOptions) && (
        <div className="create-req-meta-row">
          {showProjectPicker && (
            <div className="form-group">
              <label htmlFor="create-req-project">{t('components.createRequirement.project')}</label>
              <select
                id="create-req-project"
                className="form-input"
                value={projectId}
                onChange={(e) => setProjectId(e.target.value)}
                disabled={saving}
              >
                <option value="" disabled>{t('components.createRequirement.projectPlaceholder')}</option>
                {projectOptions!.map((p) => (
                  <option key={p.id} value={p.id}>{p.name}</option>
                ))}
              </select>
            </div>
          )}

          {showOptions && (
            <div className="form-group">
              <label htmlFor="create-req-priority">{t('components.createRequirement.priority')}</label>
              <select
                id="create-req-priority"
                value={priority}
                onChange={(e) => setPriority(e.target.value)}
                className="form-input"
                disabled={saving}
              >
                <option value="high">🔴 High</option>
                <option value="medium">🟡 Medium</option>
                <option value="low">🟢 Low</option>
              </select>
            </div>
          )}
        </div>
      )}

      {showFlow && (
        <div className="form-group">
          <label>{t('components.createRequirement.flow')}</label>
          {/* Shared segmented-control style — same look as the kind switcher
              so the panel reads as one visual family rather than two
              competing selection patterns. The note below explains what the
              current selection actually does (the segmented control only
              shows the labels). */}
          <div className="create-req-flow" role="radiogroup" aria-label={t('components.createRequirement.flow')}>
            {FLOW_OPTIONS.map((opt) => (
              <button
                key={opt.value}
                type="button"
                role="radio"
                aria-checked={flow === opt.value}
                className={`create-req-kind-tab${flow === opt.value ? ' selected' : ''}`}
                onClick={() => setFlow(opt.value)}
                disabled={saving}
                data-flow={opt.value}
              >
                {t(opt.labelKey)}
              </button>
            ))}
          </div>
          <p className="create-req-flow-note">{t(FLOW_NOTE_KEYS[flow])}</p>
        </div>
      )}

      {/* Step 4 — Optional launch plan. Collapsed by default to preserve the
          "fast notepad" ergonomics; opening it is an explicit opt-in to a
          long-running task. kind=idea hides the entire block (matches the
          existing idea-only hide rule above for flow/priority). The
          mounted state is owned here, LaunchPlanSection owns only its
          per-section sub-state via onChange. */}
      {kind !== 'idea' && (
        <details
          className="launch-section"
          open={launchOpen}
          onToggle={(e) => setLaunchOpen((e.target as HTMLDetailsElement).open)}
        >
          <summary>
            <span>{t('components.createRequirement.launchPlan.title')}</span>
            <span className="launch-section__hint">{t('components.createRequirement.launchPlan.hint')}</span>
          </summary>
          <LaunchPlanSection
            flow={flow}
            kind={kind}
            agentServers={agentServers}
            defaultBaseBranch={activeProject?.default_branch}
            projectLocalPath={activeProject?.local_path}
            disabled={saving}
            onChange={setLaunchSpec}
          />
        </details>
      )}

      {error && (
        <div className="create-req-error" role="alert">{error}</div>
      )}

      <div className="form-actions">
        <span className="create-req-shortcut" aria-hidden>{t('components.createRequirement.shortcut')}</span>
        <button className="btn" onClick={onClose} disabled={saving}>{t('common.actions.cancel')}</button>
        <button className="btn btn-primary" onClick={handleSubmit} disabled={!canSubmit}>
          {(() => {
            if (saving) {
              return skipOrganize
                ? t('components.createRequirement.saving')
                : t('components.createRequirement.savingOrganize');
            }
            // When a launch plan is set, the button copy advertises the
            // actual action (start vs schedule) so the user gets one last
            // visual confirmation before kicking off a long-running job.
            if (launchSpec?.mode === 'scheduled') {
              return t('components.createRequirement.launchPlan.submitScheduled');
            }
            if (launchSpec?.mode === 'immediate') {
              return t('components.createRequirement.launchPlan.submitImmediate');
            }
            return tLabel(t, kindCreateLabelKeys as Record<string, string>, kind);
          })()}
        </button>
      </div>

      {/* PDF 上传弹窗 — 仅 wiki kind 触发;上传后:
          - 立即写入 knowledge 表(后端副作用)
          - 用 KnowledgeItem.content 作为 description 预填,用户可继续编辑
          - skipOrganize 强制开启,避免对已结构化的 Markdown 再跑一次 LLM。 */}
      {showPdfUpload && projectId && (
        <PdfUploadDialog
          projectId={projectId}
          onClose={() => setShowPdfUpload(false)}
          onSuccess={(item: KnowledgeItem) => {
            setShowPdfUpload(false);
            if (item.content) {
              setDescription(item.content);
            }
            setSkipOrganize(true);
            setError(null);
          }}
        />
      )}
    </div>
  );
}
