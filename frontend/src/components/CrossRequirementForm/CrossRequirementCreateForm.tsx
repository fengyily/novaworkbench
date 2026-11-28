// CrossRequirementCreateForm — inline create panel for a cross-project
// requirement (replaces the old .xreq-modal on the list page).
//
// The one non-obvious thing here is the **draft row**: the panel pre-creates
// a placeholder cross requirement the moment it opens, exactly like
// CreateRequirementForm does for ordinary requirements. Inline images and
// file attachments need a row that already exists to hang off — the upload
// endpoint is keyed by owner id, and there is no owner id until something
// exists.
//
// Consequences of the draft row:
//
//   - The panel's submit does NOT create anything: it completes the draft
//     (title / description / priority) and then writes the participating
//     projects. The draft id stays the same, so the attachments uploaded
//     while drafting stay attached to the row the user ends up on.
//   - Abandoning the panel deletes the draft. A draft the user never
//     touched leaves nothing behind.
//
// 删除是**两步**的（先移进回收站，再彻底删除），但草稿行不该留在回收站里 ——
// 用户从没提交过它，让它占着回收站的一行是纯粹的噪音。所以草稿清理两步连着
// 走（见 discardDraft）。
//
// The cleanup effect is deliberately mount-only and reads its state from refs:
// a dependency-driven cleanup would re-run on every keystroke (React runs the
// previous render's cleanup before re-running a changed effect) and delete
// the draft the user is typing into. Same trap CreateRequirementForm
// documents at length.

import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import RichDescriptionEditor from '../RichDescriptionEditor/RichDescriptionEditor';
import AttachmentPanel from '../AttachmentPanel';
import { crossRequirementsApi, type Project } from '../../api/client';
import { errorMessage } from '../../utils/errMsg';
import './CrossRequirementForm.css';

const PRIORITIES = ['low', 'medium', 'high', 'urgent'] as const;

/**
 * discardDraft removes a never-submitted draft row for good.
 *
 * 删除是两步的（软删 → 彻底删除），但草稿不该在回收站里占一行 —— 用户从没
 * 提交过它，让它躺在回收站里等一次「清空回收站」纯属噪音。软删只是为了满足
 * 后端「只有回收站里的行才能 purge」这道闸门，两步连着走，用户看不到中间态。
 */
function discardDraft(id: string) {
  crossRequirementsApi.trash(id)
    .then(() => crossRequirementsApi.purge(id))
    .catch(() => {});
}

export interface CrossRequirementCreateFormProps {
  /** Every project in the workspace — the panel owns the picker. */
  projects: Project[];
  /** Called with the finished cross requirement's id (the draft's id). */
  onCreated: (id: string) => void | Promise<void>;
  onCancel: () => void;
}

export default function CrossRequirementCreateForm({
  projects,
  onCreated,
  onCancel,
}: CrossRequirementCreateFormProps) {
  const { t } = useTranslation();
  const [title, setTitle] = useState('');
  const [description, setDescription] = useState('');
  const [priority, setPriority] = useState('medium');
  const [selected, setSelected] = useState<string[]>([]);
  const [bases, setBases] = useState<Record<string, string>>({});
  const [draftId, setDraftId] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);

  // Refs the mount-only cleanup reads at unmount time. See the header
  // comment: a dependency-driven cleanup would delete the draft on the
  // first keystroke.
  const draftIdRef = useRef<string | null>(null);
  const submittedRef = useRef(false);
  const aliveRef = useRef(true);
  // StrictMode runs mount effects twice in dev (mount → unmount → mount).
  // Without this guard the panel would pre-create TWO draft rows.
  const draftRequestedRef = useRef(false);

  // The "still mounted" flag lives in a ref rather than a closure variable:
  // the StrictMode double-invoke re-runs the effect, and a per-run closure
  // would leave the first run's promise looking dead.
  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  // Pre-create the draft row. Failure is not fatal: the panel still works
  // as a plain Markdown editor, only inline images and attachments are
  // unavailable (RichDescriptionEditor shows its own hint for that).
  useEffect(() => {
    if (draftRequestedRef.current) return;
    draftRequestedRef.current = true;
    crossRequirementsApi
      .create({ draft: true })
      .then(created => {
        if (!aliveRef.current) {
          // The panel was closed before the draft landed — don't leave an
          // orphan row behind.
          discardDraft(created.id);
          return;
        }
        draftIdRef.current = created.id;
        setDraftId(created.id);
      })
      .catch(err => {
        if (aliveRef.current) {
          setError(t('crossRequirements.form.draftFailed') + errorMessage(err));
        }
      });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Mount-only cleanup: drop the draft when the panel is abandoned.
  useEffect(() => {
    return () => {
      const id = draftIdRef.current;
      if (id && !submittedRef.current) {
        discardDraft(id);
      }
    };
  }, []);

  useEffect(() => {
    rootRef.current?.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  }, []);

  const toggleProject = (id: string) => {
    setSelected(prev => (prev.includes(id) ? prev.filter(p => p !== id) : [...prev, id]));
  };

  const handleCancel = () => {
    if (submitting) return;
    onCancel();
  };

  const handleSubmit = async () => {
    if (submitting) return;
    if (!title.trim()) {
      setError(t('crossRequirements.form.errNeedTitle'));
      return;
    }
    if (selected.length === 0) {
      setError(t('crossRequirements.form.errNeedProjects'));
      return;
    }
    if (!draftId) {
      setError(t('crossRequirements.form.draftFailed'));
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      // Complete the draft in place — the id must not change, or the
      // attachments uploaded while drafting would be orphaned.
      await crossRequirementsApi.update(draftId, {
        title: title.trim(),
        description,
        priority,
      });
      await crossRequirementsApi.setProjects(
        draftId,
        selected.map(id => ({
          project_id: id,
          base_branch: (bases[id] || '').trim(),
        })),
      );
      submittedRef.current = true;
      await onCreated(draftId);
    } catch (err) {
      setError(t('crossRequirements.form.saveFailed') + errorMessage(err));
      setSubmitting(false);
    }
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      handleSubmit();
    } else if (e.key === 'Escape') {
      e.stopPropagation();
      handleCancel();
    }
  };

  return (
    <div
      className="xreq-form"
      data-testid="cross-requirement-create-form"
      ref={rootRef}
      onKeyDown={handleKeyDown}
    >
      <div className="xreq-form-header">
        <h3>{t('crossRequirements.form.createTitle')}</h3>
        {draftId && <span className="xreq-form-id">{draftId}</span>}
      </div>

      <div className="form-group">
        <label htmlFor="xreq-create-title">{t('crossRequirements.form.labelTitle')}</label>
        <input
          id="xreq-create-title"
          className="form-input"
          value={title}
          onChange={e => setTitle(e.target.value)}
          placeholder={t('crossRequirements.form.titlePlaceholder')}
          disabled={submitting}
        />
      </div>

      <div className="form-group">
        <label>{t('crossRequirements.form.labelDescription')}</label>
        <RichDescriptionEditor
          value={description}
          onChange={setDescription}
          ownerId={draftId}
          resource="cross-requirements"
          placeholder={t('crossRequirements.form.descriptionPlaceholder')}
          minRows={7}
          disabled={submitting}
        />
        <div className="xreq-form-desc-meta">
          <small className="form-hint">{t('crossRequirements.form.descriptionHint')}</small>
          <span className="xreq-form-count">
            {t('components.createRequirement.charCount', { n: description.trim().length })}
          </span>
        </div>
      </div>

      <div className="form-group">
        <label htmlFor="xreq-create-priority">{t('crossRequirements.form.labelPriority')}</label>
        <select
          id="xreq-create-priority"
          className="form-input"
          value={priority}
          onChange={e => setPriority(e.target.value)}
          disabled={submitting}
        >
          {PRIORITIES.map((p) => (
            <option key={p} value={p}>
              {t(`crossRequirements.priority.${p}` as never)}
            </option>
          ))}
        </select>
      </div>

      <div className="form-group">
        <label>{t('crossRequirements.form.labelProjects')}</label>
        <small className="form-hint">{t('crossRequirements.form.projectsHint')}</small>
        <small className="form-hint">{t('crossRequirements.detail.baseBranchHint')}</small>
        <div className="xreq-form-projects">
          {projects.map(p => {
            const checked = selected.includes(p.id);
            return (
              <div key={p.id} className="xreq-form-project-row">
                <input
                  type="checkbox"
                  id={`xreq-create-project-${p.id}`}
                  checked={checked}
                  onChange={() => toggleProject(p.id)}
                  disabled={submitting}
                />
                <label htmlFor={`xreq-create-project-${p.id}`}>{p.name}</label>
                {checked && (
                  <input
                    className="form-input"
                    placeholder={t('crossRequirements.detail.baseBranchPlaceholder')}
                    value={bases[p.id] || ''}
                    onChange={e => setBases(prev => ({ ...prev, [p.id]: e.target.value }))}
                    disabled={submitting}
                  />
                )}
              </div>
            );
          })}
        </div>
      </div>

      <div className="form-group xreq-form-attachments">
        <div className="xreq-form-attachments-head">
          <span>{t('crossRequirements.form.attachments')}</span>
          <span className="xreq-form-attachments-hint">
            {t('crossRequirements.form.attachmentsHint')}
          </span>
        </div>
        {draftId ? (
          <AttachmentPanel ownerId={draftId} resource="cross-requirements" compact />
        ) : (
          <small className="form-hint">{t('crossRequirements.form.draftFailed')}</small>
        )}
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
        <button type="button" className="btn" onClick={handleCancel} disabled={submitting}>
          {t('crossRequirements.form.cancel')}
        </button>
        <button
          type="button"
          className="btn btn-primary"
          onClick={handleSubmit}
          disabled={submitting || !draftId}
        >
          {submitting ? t('crossRequirements.form.saving') : t('crossRequirements.form.submit')}
        </button>
      </div>
    </div>
  );
}
