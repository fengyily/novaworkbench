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
import AtMentionTextarea from '../AtMentionTextarea';
import { kindLabelKeys, kindHintKeys, kindPlaceholderKeys, kindCreateLabelKeys, type Kind, requirementsApi, type Requirement } from '../../api/client';
import { tLabel } from '../../i18n/label';
import { errorMessage } from '../../utils/errMsg';
import './CreateRequirementForm.css';

type Flow = 'full' | 'skip-analysis' | 'direct';

// Workflow options hold translation KEYS, resolved during render — a
// literal label here would freeze the language at import time.
const FLOW_OPTIONS: { value: Flow; labelKey: string }[] = [
  { value: 'direct', labelKey: 'components.createRequirement.flowDirect' },
  { value: 'skip-analysis', labelKey: 'components.createRequirement.flowSkipAnalysis' },
  { value: 'full', labelKey: 'components.createRequirement.flowFull' },
];

const FLOW_NOTE_KEYS: Record<Flow, string> = {
  full: 'components.createRequirement.flowNoteFull',
  'skip-analysis': 'components.createRequirement.flowNoteSkipAnalysis',
  direct: 'components.createRequirement.flowNoteDirect',
};

const KIND_SUBMIT_HINT_KEYS: Record<Kind, string> = {
  issue: 'components.createRequirement.submitHintIssue',
  idea: 'components.createRequirement.submitHintIdea',
  requirement: 'components.createRequirement.submitHintRequirement',
};

export interface CreateRequirementFormProps {
  projectId?: string;
  projectOptions?: { id: string; name: string }[];
  defaultKind?: Kind;
  onClose: () => void;
  onCreated: (req: Requirement) => void;
}

export function CreateRequirementForm({
  projectId: fixedProjectId,
  projectOptions,
  defaultKind = 'requirement',
  onClose,
  onCreated,
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
  const [error, setError] = useState<string | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);

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
      const skipAnalysis = kind === 'idea' ? false : flow !== 'full';
      const skipDesign = kind === 'idea' ? false : flow === 'direct';
      const created = await requirementsApi.create({
        project_id: projectId,
        description,
        priority: kind === 'idea' ? 'medium' : priority,
        kind,
        skip_analysis: skipAnalysis,
        skip_design: skipDesign,
        skip_organize: skipOrganize,
      });
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
  const showProjectPicker = !fixedProjectId && !!projectOptions && projectOptions.length > 0;
  const canSubmit = !saving && !!description.trim() && !!projectId;
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
        <AtMentionTextarea
          value={description}
          onChange={setDescription}
          onKeyDown={handleTextareaKeyDown}
          className="form-input"
          rows={7}
          placeholder={tLabel(t, kindPlaceholderKeys as Record<string, string>, kind)}
          disabled={saving}
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

      {showOptions && (
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

      {error && (
        <div className="create-req-error" role="alert">{error}</div>
      )}

      <div className="form-actions">
        <span className="create-req-shortcut" aria-hidden>{t('components.createRequirement.shortcut')}</span>
        <button className="btn" onClick={onClose} disabled={saving}>{t('common.actions.cancel')}</button>
        <button className="btn btn-primary" onClick={handleSubmit} disabled={!canSubmit}>
          {saving
            ? (skipOrganize ? t('components.createRequirement.saving') : t('components.createRequirement.savingOrganize'))
            : tLabel(t, kindCreateLabelKeys as Record<string, string>, kind)}
        </button>
      </div>
    </div>
  );
}
