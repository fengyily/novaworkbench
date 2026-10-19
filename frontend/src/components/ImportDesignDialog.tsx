// ImportDesignDialog — paste or upload an externally-generated Markdown
// plan (ChatGPT / DeepSeek / Gemini) as the requirement's design_docs.
//
// Bypasses Claude / JobStore / SSE entirely. The user has already trusted
// the plan outside; this dialog just hands it to the backend and flips
// status → "designed" so they can immediately move into coding.
//
// Backed by two endpoints:
//   POST /api/wizard/import-design-file — multipart, returns {content: "..."}
//   PUT  /api/wizard/import-design      — JSON, persists the content
//
// Two-step upload (file → preview → confirm) keeps the validation pipeline
// (wiki reject / busy job / 500 KB cap) running once at the persist
// boundary instead of twice. The user gets to see the loaded text in the
// preview before confirming, mirroring the review pipeline's "show, then
// commit" shape.
//
// i18n: requirements.detail2.importDesign* (12 keys added alongside this
// component in the same PR). No new top-level namespace.
import { useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { wizardApi, type Requirement } from '../api/client';
import { errorMessage } from '../utils/errMsg';
import './ImportDesignDialog.css';

const MAX_BYTES = 500_000; // 500 KB — mirrors backend importDesignMaxBytes

interface Props {
  requirement: Requirement;
  onClose: () => void;
  onSuccess: (req: Requirement) => void;
}

type TabKey = 'paste' | 'file';

// hasExistingDesign mirrors the design_doc non-empty check on the backend:
// the schema default is '[]' (legacy JSON placeholder) and the import
// path treats both the JSON placeholder and the empty string as "nothing
// to overwrite". A real plan will be non-empty Markdown.
function hasExistingDesign(req: Requirement): boolean {
  if (!req.design_docs) return false;
  if (req.design_docs === '[]') return false;
  return req.design_docs.trim().length > 0;
}

export default function ImportDesignDialog({ requirement, onClose, onSuccess }: Props) {
  const { t } = useTranslation();
  const fileInputRef = useRef<HTMLInputElement | null>(null);

  const [tab, setTab] = useState<TabKey>('paste');
  const [pasteContent, setPasteContent] = useState('');
  const [fileName, setFileName] = useState<string | null>(null);
  const [fileSize, setFileSize] = useState<number>(0);
  const [loadedFromFile, setLoadedFromFile] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [errorText, setErrorText] = useState('');

  function fmtSize(bytes: number): string {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  }

  // Switch tab. The two tabs serve different mental models (paste vs
  // upload), and the user is meant to commit to one before the final
  // submit. Clearing the other tab's state avoids the "I pasted text and
  // accidentally submitted the cached file content" foot-gun.
  function switchTab(next: TabKey) {
    if (next === tab) return;
    setTab(next);
    setErrorText('');
    if (next === 'paste') {
      setFileName(null);
      setFileSize(0);
      setLoadedFromFile('');
      if (fileInputRef.current) fileInputRef.current.value = '';
    } else {
      setPasteContent('');
    }
  }

  function handleFileChange(e: React.ChangeEvent<HTMLInputElement>) {
    setErrorText('');
    const f = e.target.files?.[0] || null;
    if (!f) {
      setFileName(null);
      setFileSize(0);
      setLoadedFromFile('');
      return;
    }
    // Frontend double-check: the backend rejects everything but .md /
    // .markdown / .txt, but failing here gives a clearer error and lets
    // the user retry without an HTTP roundtrip.
    const lower = f.name.toLowerCase();
    if (!lower.endsWith('.md') && !lower.endsWith('.markdown') && !lower.endsWith('.txt')) {
      setErrorText(t('requirements.detail2.importDesignFailFormat'));
      if (fileInputRef.current) fileInputRef.current.value = '';
      return;
    }
    if (f.size > 2 * 1024 * 1024) {
      setErrorText(t('requirements.detail2.importDesignFailFileTooLarge'));
      if (fileInputRef.current) fileInputRef.current.value = '';
      return;
    }
    setFileName(f.name);
    setFileSize(f.size);
    void loadFile(f);
  }

  // Pull the bytes through the upload endpoint, then stash the returned
  // content string for the final import. We do NOT pre-validate length
  // here — the backend's 500 KB cap is on the JSON content body, and the
  // file-size check above only ensures the multipart upload fits.
  async function loadFile(f: File) {
    setSubmitting(true);
    setErrorText('');
    try {
      const res = await wizardApi.importDesignFile(f);
      if (res.content.length > MAX_BYTES) {
        setErrorText(t('requirements.detail2.importDesignFailContentTooLarge'));
        setLoadedFromFile('');
        return;
      }
      setLoadedFromFile(res.content);
    } catch (err) {
      setErrorText(errorMessage(err));
      setLoadedFromFile('');
    } finally {
      setSubmitting(false);
    }
  }

  function handleSelectClick() {
    fileInputRef.current?.click();
  }

  // Build the (content, source) tuple the final PUT will hand to the
  // backend. Tab-specific because the user might forget which tab they
  // are on; an empty active-tab content gets blocked by the disabled
  // button, so we never need to fall back to the other tab.
  function activeContent(): { content: string; source: 'paste' | 'file' } | null {
    if (tab === 'paste') {
      if (pasteContent.trim().length === 0) return null;
      return { content: pasteContent, source: 'paste' };
    }
    if (loadedFromFile.length === 0) return null;
    return { content: loadedFromFile, source: 'file' };
  }

  async function handleSubmit() {
    const payload = activeContent();
    if (!payload) {
      setErrorText(t('requirements.detail2.importDesignFailEmpty'));
      return;
    }
    if (hasExistingDesign(requirement)) {
      // Second-line guard: the dialog already shows the ⚠ hint and the
      // button label, but window.confirm is the only synchronous check
      // that survives a user who just clicks through. Cancel = no-op.
      const ok = window.confirm(t('requirements.detail2.importDesignConfirmOverwrite'));
      if (!ok) return;
    }
    setSubmitting(true);
    setErrorText('');
    try {
      const updated = await wizardApi.importDesign(
        requirement.id,
        payload.content,
        payload.source,
      );
      onSuccess(updated);
      onClose();
    } catch (err: unknown) {
      // Backend returns structured error.code values: KIND_NOT_SUPPORTED
      // / DESIGN_BUSY get the i18n'd key; everything else falls back to
      // the raw error message (which the API already includes the
      // English backend code prefix, e.g. "Content exceeds 500000 byte
      // limit").
      const code = (err as { code?: string } | null)?.code;
      if (code === 'KIND_NOT_SUPPORTED') {
        setErrorText(t('requirements.detail2.importDesignFailKind'));
      } else if (code === 'DESIGN_BUSY') {
        setErrorText(t('requirements.detail2.importDesignFailBusy'));
      } else {
        setErrorText(errorMessage(err));
      }
    } finally {
      setSubmitting(false);
    }
  }

  const overCap = pasteContent.length > MAX_BYTES;
  const canSubmit = !submitting && activeContent() !== null;
  const showOverwriteWarn = hasExistingDesign(requirement);

  return (
    <div className="import-design-overlay modal-overlay" onClick={onClose}>
      <div
        className="import-design-box modal-box"
        onClick={(e) => e.stopPropagation()}
        role="dialog"
        aria-label={t('requirements.detail2.importDesignDialogTitle')}
      >
        <div className="import-design-header">
          <h3>📥 {t('requirements.detail2.importDesignDialogTitle')}</h3>
          <button
            className="import-design-close"
            onClick={onClose}
            disabled={submitting}
            aria-label="close"
          >
            ×
          </button>
        </div>

        <div className="import-design-tabs" role="tablist">
          <button
            type="button"
            className={`import-design-tab ${tab === 'paste' ? 'is-active' : ''}`}
            onClick={() => switchTab('paste')}
            disabled={submitting}
            role="tab"
            aria-selected={tab === 'paste'}
          >
            {t('requirements.detail2.importDesignTabPaste')}
          </button>
          <button
            type="button"
            className={`import-design-tab ${tab === 'file' ? 'is-active' : ''}`}
            onClick={() => switchTab('file')}
            disabled={submitting}
            role="tab"
            aria-selected={tab === 'file'}
          >
            {t('requirements.detail2.importDesignTabFile')}
          </button>
        </div>

        {tab === 'paste' ? (
          <div className="form-group">
            <textarea
              className="import-design-textarea"
              placeholder={t('requirements.detail2.importDesignPlaceholder')}
              value={pasteContent}
              onChange={(e) => setPasteContent(e.target.value)}
              disabled={submitting}
              rows={14}
            />
            <div
              className={`import-design-char-count ${overCap ? 'is-over' : ''}`}
              aria-live="polite"
            >
              {pasteContent.length} / {MAX_BYTES.toLocaleString()}
            </div>
          </div>
        ) : (
          <div className="form-group">
            <input
              ref={fileInputRef}
              type="file"
              accept=".md,.markdown,.txt"
              onChange={handleFileChange}
              className="import-design-file-input"
              disabled={submitting}
            />
            <button
              type="button"
              className="btn"
              onClick={handleSelectClick}
              disabled={submitting}
            >
              {t('requirements.detail2.importDesignTabFile')}
            </button>
            {fileName && (
              <div className="import-design-selected">
                {fileName} ({fmtSize(fileSize)})
                {loadedFromFile && (
                  <span className="import-design-loaded-hint">
                    {' '}· {loadedFromFile.length.toLocaleString()} chars
                  </span>
                )}
              </div>
            )}
            {loadedFromFile && (
              <details className="import-design-preview">
                <summary>{t('requirements.detail2.importDesignPreviewLabel')}</summary>
                <pre className="import-design-preview-body">{loadedFromFile}</pre>
              </details>
            )}
          </div>
        )}

        {showOverwriteWarn && (
          <div className="import-design-warn">
            ⚠ {t('requirements.detail2.importDesignConfirmOverwrite')}
          </div>
        )}

        {errorText && <div className="import-design-error">{errorText}</div>}

        <div className="form-actions btn-row-2col">
          <button className="btn" onClick={onClose} disabled={submitting}>
            {t('requirements.detail2.btnCancel')}
          </button>
          <button
            className="btn btn-primary"
            onClick={handleSubmit}
            disabled={!canSubmit}
          >
            {submitting ? (
              <>
                <span className="import-design-spinner" aria-hidden="true" />
                {t('requirements.detail2.importDesignSubmitting')}
              </>
            ) : (
              t('requirements.detail2.importDesignSubmit')
            )}
          </button>
        </div>
      </div>
    </div>
  );
}
