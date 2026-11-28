// CrossRequirementEditForm — inline edit panel for an existing cross-project
// requirement.
//
// Mirrors RequirementEditForm (the ordinary requirement's inline edit panel)
// but is NOT it: that form rewrites kind / skip_design / the reference set,
// all of which belong to `requirements` and have no meaning here. Only the
// fields this entity actually owns are sent — title / description / priority.
//
// The description goes through RichDescriptionEditor, so it takes the same
// Markdown toolbar and the same inline-image paste as the create-requirement
// form. The attachments live in the cross-requirement's own collection
// (resource='cross-requirements'), which is a different table from the
// ordinary requirement's — so the marker the editor writes has to be told
// where to look the bytes up.
//
// Form state is seeded from a mount-time snapshot so a background refresh of
// the detail payload never resets what the user is typing.

import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import RichDescriptionEditor from '../RichDescriptionEditor/RichDescriptionEditor';
import AttachmentPanel from '../AttachmentPanel';
import { crossRequirementsApi, type CrossRequirement } from '../../api/client';
import { errorMessage } from '../../utils/errMsg';
import './CrossRequirementForm.css';

const PRIORITIES = ['low', 'medium', 'high', 'urgent'] as const;

export interface CrossRequirementEditFormProps {
  requirement: CrossRequirement;
  onSaved: (next: CrossRequirement) => void | Promise<void>;
  onCancel: () => void;
}

export default function CrossRequirementEditForm({
  requirement,
  onSaved,
  onCancel,
}: CrossRequirementEditFormProps) {
  const { t } = useTranslation();
  // Mount-time snapshot — see header comment.
  const initial = useRef({
    title: requirement.title,
    description: requirement.description,
    priority: requirement.priority,
  }).current;
  const [title, setTitle] = useState(initial.title);
  const [description, setDescription] = useState(initial.description);
  const [priority, setPriority] = useState(initial.priority);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);

  const dirty =
    title !== initial.title ||
    description !== initial.description ||
    priority !== initial.priority;

  useEffect(() => {
    rootRef.current?.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
    rootRef.current?.querySelector('textarea')?.focus();
  }, []);

  // Warn before the tab goes away with unsaved edits.
  useEffect(() => {
    if (!dirty) return;
    const handler = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      e.returnValue = '';
    };
    window.addEventListener('beforeunload', handler);
    return () => window.removeEventListener('beforeunload', handler);
  }, [dirty]);

  const handleCancel = () => {
    if (saving) return;
    if (dirty && !window.confirm(t('crossRequirements.form.discardConfirm'))) return;
    onCancel();
  };

  const handleSave = async () => {
    if (saving) return;
    if (!title.trim()) {
      setError(t('crossRequirements.form.errNeedTitle'));
      return;
    }
    setSaving(true);
    setError(null);
    try {
      // description is sent verbatim (including an empty string) — the
      // server distinguishes "field absent" from "field present but empty",
      // so an emptied description is actually cleared instead of kept.
      const next = await crossRequirementsApi.update(requirement.id, {
        title: title.trim(),
        description,
        priority,
      });
      await onSaved(next);
    } catch (err) {
      setError(t('crossRequirements.form.saveFailed') + errorMessage(err));
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

  const canSave = !saving && !!title.trim() && dirty;

  return (
    <div
      className="xreq-form"
      data-testid="cross-requirement-edit-form"
      ref={rootRef}
      onKeyDown={handleKeyDown}
    >
      <div className="xreq-form-header">
        <h3>{t('crossRequirements.form.editTitle')}</h3>
        <span className="xreq-form-id">{requirement.id}</span>
      </div>

      <div className="form-group">
        <label htmlFor="xreq-edit-title">{t('crossRequirements.form.labelTitle')}</label>
        <input
          id="xreq-edit-title"
          className="form-input"
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          placeholder={t('crossRequirements.form.titlePlaceholder')}
          disabled={saving}
        />
      </div>

      <div className="form-group">
        <label>{t('crossRequirements.form.labelDescription')}</label>
        <RichDescriptionEditor
          value={description}
          onChange={setDescription}
          ownerId={requirement.id}
          resource="cross-requirements"
          placeholder={t('crossRequirements.form.descriptionPlaceholder')}
          minRows={10}
          disabled={saving}
        />
        <div className="xreq-form-desc-meta">
          <small className="form-hint">{t('crossRequirements.form.descriptionHint')}</small>
          <span className="xreq-form-count">
            {t('components.createRequirement.charCount', { n: description.trim().length })}
          </span>
        </div>
      </div>

      <div className="form-group">
        <label htmlFor="xreq-edit-priority">{t('crossRequirements.form.labelPriority')}</label>
        <select
          id="xreq-edit-priority"
          className="form-input"
          value={priority}
          onChange={(e) => setPriority(e.target.value)}
          disabled={saving}
        >
          {PRIORITIES.map((p) => (
            <option key={p} value={p}>
              {t(`crossRequirements.priority.${p}` as never)}
            </option>
          ))}
        </select>
      </div>

      <div className="form-group xreq-form-attachments">
        <div className="xreq-form-attachments-head">
          <span>{t('crossRequirements.form.attachments')}</span>
          <span className="xreq-form-attachments-hint">
            {t('crossRequirements.form.attachmentsHint')}
          </span>
        </div>
        <AttachmentPanel ownerId={requirement.id} resource="cross-requirements" compact />
      </div>

      {error && (
        <div className="xreq-form-error" role="alert">
          {error}
        </div>
      )}

      <div className="xreq-form-actions">
        <span className="xreq-form-shortcut" aria-hidden>
          {t('crossRequirements.form.shortcut')}
        </span>
        <button type="button" className="btn" onClick={handleCancel} disabled={saving}>
          {t('crossRequirements.form.cancel')}
        </button>
        <button type="button" className="btn btn-primary" onClick={handleSave} disabled={!canSave}>
          {saving ? t('crossRequirements.form.saving') : t('crossRequirements.form.save')}
        </button>
      </div>
    </div>
  );
}
