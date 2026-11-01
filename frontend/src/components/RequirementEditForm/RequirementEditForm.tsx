// RequirementEditForm — inline edit panel for an existing requirement.
//
// Replaces the narrow modal on the detail page. It reuses the building blocks
// of CreateRequirementForm (RichDescriptionEditor, `create-req-*` styles,
// file import) but deliberately is NOT CreateRequirementForm: that form
// pre-creates/deletes a draft row, overwrites the title from the description
// and rewrites kind/skip_design — all wrong for a row that already exists.
// Only title / description / priority / skip_analysis are sent.
//
// Form state is seeded from a mount-time snapshot so background refreshes of
// the requirement never reset what the user is typing.

import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import RichDescriptionEditor from '../RichDescriptionEditor/RichDescriptionEditor';
import RequirementReferencePicker from '../RequirementReferencePicker/RequirementReferencePicker';
import { requirementsApi, type Requirement, type RequirementReference } from '../../api/client';
import { errorMessage } from '../../utils/errMsg';
import '../CreateRequirementForm/CreateRequirementForm.css';
import './RequirementEditForm.css';

const ACCEPTED_IMPORT_EXTS = ['.md', '.markdown', '.pdf', '.docx', '.txt'];
const PRIORITIES = ['low', 'medium', 'high', 'critical'] as const;

// referenceKey renders the direct reference set as a comparable string for
// the dirty check. Only direct entries count — inherited ones come from the
// master requirement and are not writable from here.
function referenceKey(refs: RequirementReference[]): string {
  return refs
    .filter(r => r.origin === 'direct')
    .map(r => `${r.requirement_id}:${r.note}`)
    .join('|');
}

export interface RequirementEditFormProps {
  requirement: Requirement;
  onSaved: (next: Requirement) => void | Promise<void>;
  onCancel: () => void;
}

export default function RequirementEditForm({ requirement, onSaved, onCancel }: RequirementEditFormProps) {
  const { t } = useTranslation();
  // Mount-time snapshot — see header comment.
  const initial = useRef({
    title: requirement.title,
    description: requirement.description,
    priority: requirement.priority,
    skipAnalysis: requirement.skip_analysis,
  }).current;
  const [title, setTitle] = useState(initial.title);
  const [description, setDescription] = useState(initial.description);
  const [priority, setPriority] = useState(initial.priority);
  const [skipAnalysis, setSkipAnalysis] = useState(initial.skipAnalysis);
  const [saving, setSaving] = useState(false);
  const [importing, setImporting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  // Referenced requirements — loaded on mount (the panel is only mounted when
  // the user clicks 编辑, so this is a one-shot fetch, not a poll).
  //
  // Inherited entries (origin === 'parent') come from the master requirement
  // and are shown read-only; only the direct ones are written back.
  const [refs, setRefs] = useState<RequirementReference[]>([]);
  const [refsLoaded, setRefsLoaded] = useState(false);
  // The baseline the dirty check compares against. Captured when the load
  // resolves — capturing it on the first render would capture the empty
  // pre-load state and make the form look dirty the moment it opens.
  const initialRefKeyRef = useRef<string>('');
  useEffect(() => {
    let alive = true;
    initialRefKeyRef.current = '';
    setRefsLoaded(false);
    requirementsApi.getReferences(requirement.id)
      .then(rows => {
        if (!alive) return;
        const list = rows || [];
        setRefs(list);
        initialRefKeyRef.current = referenceKey(list);
      })
      .catch(() => { if (alive) setRefs([]); })
      .finally(() => { if (alive) setRefsLoaded(true); });
    return () => { alive = false; };
  }, [requirement.id]);

  // The direct set is what we write back, so the dirty check compares only
  // those — an inherited entry the user can't touch must not make the form
  // look permanently dirty.
  const directRefKey = referenceKey(refs);

  const showSkipAnalysis = requirement.status === 'draft' || requirement.status === 'analyzing';
  const dirty =
    title !== initial.title ||
    description !== initial.description ||
    priority !== initial.priority ||
    (showSkipAnalysis && skipAnalysis !== initial.skipAnalysis) ||
    (refsLoaded && directRefKey !== initialRefKeyRef.current);

  useEffect(() => {
    rootRef.current?.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
    rootRef.current?.querySelector('textarea')?.focus();
  }, []);

  useEffect(() => {
    if (!dirty) return;
    const handler = (e: BeforeUnloadEvent) => { e.preventDefault(); e.returnValue = ''; };
    window.addEventListener('beforeunload', handler);
    return () => window.removeEventListener('beforeunload', handler);
  }, [dirty]);

  const handleCancel = () => {
    if (saving) return;
    if (dirty && !window.confirm(t('requirements.edit.discardConfirm'))) return;
    onCancel();
  };

  const handleImportFile = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (fileInputRef.current) fileInputRef.current.value = '';
    if (!file) return;
    const ext = file.name.slice(file.name.lastIndexOf('.')).toLowerCase();
    if (!ACCEPTED_IMPORT_EXTS.includes(ext)) {
      setError(t('components.createRequirement.importFileUnsupported'));
      return;
    }
    if (description.trim() && !window.confirm(t('components.createRequirement.importFileConfirm'))) return;
    setImporting(true);
    setError(null);
    try {
      const { content } = await requirementsApi.parseFile(file);
      setDescription(content);
    } catch (err) {
      setError(t('components.createRequirement.importFileError', { error: errorMessage(err) }));
    } finally {
      setImporting(false);
    }
  };

  const handleSave = async () => {
    if (saving || importing) return;
    if (!title.trim()) {
      setError(t('requirements.edit.errNeedTitle'));
      return;
    }
    if (!description.trim()) {
      setError(t('components.createRequirement.errNeedDesc'));
      return;
    }
    setSaving(true);
    setError(null);
    try {
      const next = await requirementsApi.update(requirement.id, {
        title: title.trim(),
        description,
        priority,
        skip_analysis: showSkipAnalysis ? skipAnalysis : requirement.skip_analysis,
      });
      // The reference set is written separately (its own endpoint). Only the
      // direct entries go back — writing an inherited entry would freeze it
      // as a direct reference and break the inheritance.
      //
      // A failure here is NOT swallowed: onSaved closes the panel, so calling
      // it would hide the error and lose the user's chance to retry. The
      // requirement's own fields are already saved (that write succeeded), so
      // the panel just stays open with the error and the Save button stays
      // enabled — the refs are still "dirty" because their baseline was never
      // advanced.
      try {
        const saved = await requirementsApi.setReferences(
          requirement.id,
          refs
            .filter(r => r.origin === 'direct')
            .map(r => ({ requirement_id: r.requirement_id, note: r.note })),
        );
        setRefs(saved || []);
        initialRefKeyRef.current = referenceKey(saved || []);
      } catch (err) {
        setError(t('requirements.references.saveFailed') + errorMessage(err));
        setSaving(false);
        return;
      }
      await onSaved(next);
    } catch (err) {
      setError(t('requirements.detail2.saveFailPrefix') + errorMessage(err));
      setSaving(false);
    }
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      handleSave();
    } else if (e.key === 'Escape') {
      e.stopPropagation();
      handleCancel();
    }
  };

  const canSave = !saving && !importing && !!title.trim() && !!description.trim() && dirty;

  return (
    <div
      className="create-req-form req-edit-form"
      data-testid="requirement-edit-form"
      ref={rootRef}
      onKeyDown={handleKeyDown}
    >
      <div className="create-req-form-header">
        <h3>{t('requirements.detail2.editTitle')}</h3>
        <span className="req-edit-id">{requirement.id}</span>
      </div>

      <div className="form-group">
        <label htmlFor="req-edit-title">{t('requirements.detail2.editLabelTitle')}</label>
        <input
          id="req-edit-title"
          className="form-input req-edit-title-input"
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          placeholder={t('requirements.detail.editTitlePlaceholder')}
          disabled={saving}
        />
      </div>

      <div className="form-group create-req-desc">
        <div className="req-edit-desc-head">
          <label>{t('requirements.detail2.editLabelDesc')}</label>
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
            accept={ACCEPTED_IMPORT_EXTS.join(',')}
            style={{ display: 'none' }}
            onChange={handleImportFile}
          />
        </div>
        <RichDescriptionEditor
          value={description}
          onChange={setDescription}
          requirementId={requirement.id}
          placeholder={t('requirements.detail2.editMentionPlaceholder')}
          minRows={12}
          disabled={saving || importing}
        />
        <div className="create-req-desc-meta">
          <small className="form-hint">{t('requirements.edit.uploadKept')}</small>
          <span className="create-req-count">
            {t('components.createRequirement.charCount', { n: description.trim().length })}
          </span>
        </div>
      </div>

      <div className="form-group">
        <label>{t('requirements.detail2.editLabelPriority')}</label>
        <div className="create-req-kind req-edit-priority" role="radiogroup" aria-label={t('requirements.detail2.editLabelPriority')}>
          {PRIORITIES.map((p) => (
            <button
              key={p}
              type="button"
              role="radio"
              aria-checked={priority === p}
              className={`create-req-kind-tab${priority === p ? ' selected' : ''}`}
              onClick={() => setPriority(p)}
              disabled={saving}
              data-priority={p}
            >
              {p}
            </button>
          ))}
        </div>
      </div>

      <div className="form-group">
        <label>{t('requirements.references.label')}</label>
        <small className="form-hint">{t('requirements.references.hint')}</small>
        <RequirementReferencePicker
          value={refs}
          onChange={setRefs}
          projectId={requirement.project_id}
          excludeIds={[requirement.id]}
          disabled={saving}
        />
      </div>

      {showSkipAnalysis && (
        <label className={`create-req-toggle${skipAnalysis ? ' on' : ''}`}>
          <input
            type="checkbox"
            checked={skipAnalysis}
            onChange={(e) => setSkipAnalysis(e.target.checked)}
            disabled={saving}
          />
          <span className="create-req-toggle-track" aria-hidden>
            <span className="create-req-toggle-thumb" />
          </span>
          <span className="create-req-toggle-text">
            <span className="create-req-toggle-label">{t('requirements.detail2.editSkipAnalysis')}</span>
            <span className="create-req-toggle-note">{t('requirements.detail2.editSkipAnalysisHint')}</span>
          </span>
        </label>
      )}

      {error && <div className="create-req-error" role="alert">{error}</div>}

      <div className="form-actions req-edit-actions">
        <span className="create-req-shortcut" aria-hidden>{t('requirements.edit.shortcut')}</span>
        <button className="btn" onClick={handleCancel} disabled={saving}>{t('requirements.detail2.editCancel')}</button>
        <button className="btn btn-primary" onClick={handleSave} disabled={!canSave}>
          {saving ? t('requirements.detail2.saveBusy') : t('requirements.detail2.saveBtn')}
        </button>
      </div>
    </div>
  );
}
