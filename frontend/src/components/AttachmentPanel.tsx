// AttachmentPanel — file attachments for one owner. Used on the requirement
// detail page (owner = a requirement) and in the branch-Review composer
// (owner = a code_reviews session); the `resource` prop picks which backend
// collection the same five calls hit. The backend keeps a row per
// (owner, sha256); the UI is a thin client that calls attachmentsApi.
//
// Three concerns this component owns:
//   1. list+refresh — re-fetch after every mutation so a re-upload reuses
//      the existing row (sha256 dedup) without a stale list
//   2. upload UX — multi-file picker + drag-drop overlay; descriptions
//      travel in a per-file textbox next to the picker
//   3. inline description edit — click the description row, blur to save
//
// Empty state: a "+ 上传附件" button plus a dashed drop-zone. The dropzone
// is the same zone as the picker trigger — clicking opens the file dialog,
// dragging highlights the zone. Cap (currently 2 GB) surfaced inline so users
// know — wizard pipelines regularly attach multi-hundred-MB log dumps.

import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { attachmentsApi, type Attachment, type AttachmentResource } from '../api/client';
import { errorMessage } from '../utils/errMsg';
import './AttachmentPanel.css';

interface Props {
  /** Owner id — a requirement id (req_*) or a review session id (crv_*). */
  ownerId: string;
  /** Which backend collection owns the files. Defaults to requirements. */
  resource?: AttachmentResource;
}

interface PendingUpload {
  /** The File the user actually selected. */
  file: File;
  /** Description the user typed next to this file (may be empty). */
  description: string;
}

function formatSize(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

function archiveEntryCount(a: Attachment): number | null {
  if (!a.manifest_text) return null;
  // manifest_text starts with "N B  name\n... ── K entries, M B uncompressed"
  const m = a.manifest_text.match(/── (\d+) entries?, ([\d.]+ [KM]?B)/);
  return m ? parseInt(m[1], 10) : null;
}

function archiveUncompressedSize(a: Attachment): string | null {
  if (!a.manifest_text) return null;
  const m = a.manifest_text.match(/── \d+ entries?, ([\d.]+ [KM]?B)/);
  return m ? m[1] : null;
}

export default function AttachmentPanel({ ownerId, resource = 'requirements' }: Props) {
  const { t } = useTranslation();
  const [items, setItems] = useState<Attachment[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // pending uploads queue — accumulated in the pick row, sent one at a time.
  const [pending, setPending] = useState<PendingUpload[]>([]);
  const [dragging, setDragging] = useState(false);
  const [editingDescId, setEditingDescId] = useState<string | null>(null);
  const [editingDescText, setEditingDescText] = useState('');
  const fileInputRef = useRef<HTMLInputElement>(null);

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const rows = await attachmentsApi.list(resource, ownerId);
      setItems(Array.isArray(rows) ? rows : []);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setLoading(false);
    }
  }, [resource, ownerId]);

  useEffect(() => { void refresh(); }, [refresh]);

  // ---- file pick + drag/drop ----
  const addFiles = (files: FileList | File[]) => {
    const list = Array.from(files);
    if (list.length === 0) return;
    setPending((cur) => [
      ...cur,
      ...list.map((file) => ({ file, description: '' })),
    ]);
  };

  const onPickClick = () => fileInputRef.current?.click();
  const onFileInputChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    if (e.target.files) addFiles(e.target.files);
    // Reset so picking the same file again still triggers onChange.
    e.target.value = '';
  };

  const onDragOver = (e: React.DragEvent) => { e.preventDefault(); setDragging(true); };
  const onDragLeave = (e: React.DragEvent) => {
    e.preventDefault();
    // Only clear when leaving the dropzone entirely (not crossing child rows).
    if (e.currentTarget === e.target) setDragging(false);
  };
  const onDrop = (e: React.DragEvent) => {
    e.preventDefault();
    setDragging(false);
    if (e.dataTransfer?.files) addFiles(e.dataTransfer.files);
  };

  // ---- upload commit ----
  const sendOne = async (p: PendingUpload): Promise<void> => {
    try {
      await attachmentsApi.upload(resource, ownerId, p.file, p.description);
      await refresh();
    } catch (err) {
      setError(t('requirements.attachment.uploadFailed', { msg: errorMessage(err) }));
      throw err;
    }
  };

  const uploadAll = async () => {
    if (pending.length === 0) return;
    const queue = [...pending];
    setPending([]);
    for (const p of queue) {
      try { await sendOne(p); }
      catch { /* already surfaced in setError; keep going */ }
    }
  };

  const cancelPending = (idx: number) => {
    setPending((cur) => cur.filter((_, i) => i !== idx));
  };

  // ---- per-row ops ----
  const onDelete = async (a: Attachment) => {
    if (!confirm(t('requirements.attachment.deleteConfirm', { name: a.filename }))) return;
    try {
      await attachmentsApi.delete(resource, ownerId, a.id);
      setItems((cur) => cur.filter((x) => x.id !== a.id));
    } catch (err) {
      setError(t('requirements.attachment.deleteFailed', { msg: errorMessage(err) }));
    }
  };

  const beginEditDesc = (a: Attachment) => {
    setEditingDescId(a.id);
    setEditingDescText(a.description);
  };

  const saveDesc = async (a: Attachment) => {
    const next = editingDescText.trim();
    setEditingDescId(null);
    if (next === a.description) return;
    try {
      const updated = await attachmentsApi.updateDescription(resource, ownerId, a.id, next);
      setItems((cur) => cur.map((x) => (x.id === a.id ? updated : x)));
    } catch (err) {
      setError(t('requirements.attachment.updateFailed', { msg: errorMessage(err) }));
    }
  };

  const downloadHref = (a: Attachment) =>
    attachmentsApi.downloadUrl(resource, ownerId, a.id);

  return (
    <div className="attachment-panel">
      <div className="attachment-panel__header">
        <div>
          <div className="attachment-panel__title">{t('requirements.attachment.sectionTitle')}</div>
          <div className="attachment-panel__hint">{t('requirements.attachment.sectionHint')}</div>
        </div>
      </div>

      {error && (
        <div className="attachment-panel__error" role="alert" onClick={() => setError(null)}>
          {error}
        </div>
      )}

      {/* picker / dropzone */}
      <div
        className={`attachment-panel__dropzone ${dragging ? 'is-dragging' : ''}`}
        onDragOver={onDragOver}
        onDragLeave={onDragLeave}
        onDrop={onDrop}
        onClick={onPickClick}
      >
        <div className="attachment-panel__dropzone-icon">📎</div>
        <div className="attachment-panel__dropzone-hint">{t('requirements.attachment.dragDropHint')}</div>
        <div className="attachment-panel__dropzone-limit">{t('requirements.attachment.sizeLimit')}</div>
        <input
          ref={fileInputRef}
          type="file"
          multiple
          style={{ display: 'none' }}
          onChange={onFileInputChange}
        />
      </div>

      {/* queue — staged uploads not yet committed */}
      {pending.length > 0 && (
        <div className="attachment-panel__queue">
          {pending.map((p, idx) => (
            <div key={idx} className="attachment-panel__queue-row">
              <div className="attachment-panel__queue-name">
                <span className="attachment-panel__name">{p.file.name}</span>
                <span className="attachment-panel__size">{formatSize(p.file.size)}</span>
              </div>
              <input
                type="text"
                className="attachment-panel__queue-desc"
                value={p.description}
                placeholder={t('requirements.attachment.descriptionPlaceholder') ?? ''}
                onChange={(e) =>
                  setPending((cur) => cur.map((x, i) => (i === idx ? { ...x, description: e.target.value } : x)))
                }
                maxLength={500}
              />
              <button
                type="button"
                className="attachment-panel__queue-remove"
                onClick={(e) => { e.stopPropagation(); cancelPending(idx); }}
                aria-label="remove"
              >
                ×
              </button>
            </div>
          ))}
          <button
            type="button"
            className="attachment-panel__upload-btn"
            onClick={uploadAll}
            disabled={pending.length === 0}
          >
            {t('requirements.attachment.uploadBtn')} ({pending.length})
          </button>
        </div>
      )}

      {/* list */}
      <div className="attachment-panel__list">
        {loading && items.length === 0 ? (
          <div className="attachment-panel__empty">…</div>
        ) : items.length === 0 ? (
          <div className="attachment-panel__empty">{t('requirements.attachment.empty')}</div>
        ) : (
          items.map((a) => {
            const isArchive = a.manifest_text ? a.manifest_text.length > 0 : false;
            const entryCount = archiveEntryCount(a);
            const sizeUncompressed = archiveUncompressedSize(a);
            return (
              <div className="attachment-panel__row" key={a.id}>
                <div className="attachment-panel__row-top">
                  <a
                    className="attachment-panel__filename"
                    href={downloadHref(a)}
                    target="_blank"
                    rel="noreferrer"
                    title={a.filename}
                  >
                    {a.filename}
                    {isArchive && <span className="attachment-panel__archive-badge"> {t('requirements.attachment.archiveBadge')}</span>}
                  </a>
                  <span className="attachment-panel__size">{formatSize(a.size_bytes)}</span>
                  <span className="attachment-panel__row-actions">
                    <button type="button" onClick={() => onDelete(a)} title={t('requirements.attachment.delete') ?? ''}>
                      🗑
                    </button>
                  </span>
                </div>

                {/* description — inline editable */}
                {editingDescId === a.id ? (
                  <div className="attachment-panel__desc-edit">
                    <textarea
                      autoFocus
                      value={editingDescText}
                      onChange={(e) => setEditingDescText(e.target.value)}
                      onBlur={() => void saveDesc(a)}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); void saveDesc(a); }
                        if (e.key === 'Escape') { setEditingDescId(null); }
                      }}
                      placeholder={t('requirements.attachment.descriptionPlaceholder') ?? ''}
                      maxLength={500}
                      rows={2}
                    />
                  </div>
                ) : (
                  <div
                    className="attachment-panel__desc"
                    onClick={() => beginEditDesc(a)}
                    role="button"
                    tabIndex={0}
                    title={t('requirements.attachment.editDescription') ?? ''}
                  >
                    {a.description || (
                      <span className="attachment-panel__desc-empty">+ {t('requirements.attachment.addDescription')?.replace(/^\+\s*/, '')}</span>
                    )}
                  </div>
                )}

                {/* archive manifest preview */}
                {isArchive && (
                  <div className="attachment-panel__manifest">
                    {entryCount !== null && sizeUncompressed && (
                      <div className="attachment-panel__manifest-header">
                        {t('requirements.attachment.archiveEntries', {
                          count: entryCount,
                          size: sizeUncompressed,
                        })}
                      </div>
                    )}
                    <pre className="attachment-panel__manifest-body">{a.manifest_text}</pre>
                  </div>
                )}
              </div>
            );
          })
        )}
      </div>
    </div>
  );
}
