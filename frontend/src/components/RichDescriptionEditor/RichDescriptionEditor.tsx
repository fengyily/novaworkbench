// RichDescriptionEditor — Markdown editor for the create-requirement form.
//
// Adds three things the plain AtMentionTextarea doesn't have:
//   1. A toolbar (B / I / code / link / list / code block / image button)
//      that wraps the current selection with the matching Markdown syntax.
//   2. Image-paste / drop / file-picker ingestion: any image/* blob goes
//      through useAttachmentUpload, which compresses (compressImage.ts),
//      uploads to the existing requirement_attachments table, and inserts
//      a `![image](nova://attachment/<att_id>)` marker at the cursor.
//   3. An edit / preview tab toggle so the writer can sanity-check the
//      rendered output without leaving the field.
//
// The `nova://` markers are rewritten to real URLs by rewriteNovaLinks
// (used here for the preview tab and by RequirementDetail for display).
// The textarea's source content is never rewritten — the user always sees
// what will be persisted.
//
// requirementId gates every image-touching interaction. When it's null the
// editor is still usable as a plain Markdown textarea (the @mention popover
// keeps working), but the toolbar's image button and the paste/drop hooks
// are inert.

import { forwardRef, useCallback, useEffect, useImperativeHandle, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import AtMentionTextarea from '../AtMentionTextarea';
import MarkdownRender from '../MarkdownRender';
import { useAttachmentUpload, type UploadTask } from './useAttachmentUpload';
import { rewriteNovaLinks } from './rewriteNovaLinks';
import { extractImageFiles, extractImageFilesFromList, spliceAt } from './imageIngest';
import './RichDescriptionEditor.css';

export interface RichDescriptionEditorHandle {
  insertAtCursor: (text: string) => void;
}

export interface RichDescriptionEditorProps {
  value: string;
  onChange: (value: string) => void;
  /** Owner of any uploaded images. Set to null while the parent is still
   *  pre-creating the draft row; in that state image entry points are
   *  disabled (the toolbar shows a hint instead). */
  requirementId: string | null;
  placeholder?: string;
  minRows?: number;
  disabled?: boolean;
  onUploadsChange?: (tasks: UploadTask[]) => void;
  /** Forwarded to the underlying textarea — CreateRequirementForm wires
   *  Cmd/Ctrl+Enter to submit. */
  onKeyDown?: (e: React.KeyboardEvent<HTMLTextAreaElement>) => void;
}

type Mode = 'edit' | 'preview';

// extractImageFiles / extractImageFilesFromList / spliceAt live in
// ./imageIngest — the sub-task composer pastes through the same helpers.
const RichDescriptionEditor = forwardRef<RichDescriptionEditorHandle, RichDescriptionEditorProps>(function RichDescriptionEditor({
  value,
  onChange,
  requirementId,
  placeholder,
  minRows = 7,
  disabled,
  onUploadsChange,
  onKeyDown,
}: RichDescriptionEditorProps, ref) {
  const { t } = useTranslation();
  const [mode, setMode] = useState<Mode>('edit');
  const [isDragOver, setIsDragOver] = useState(false);
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  // Last known cursor position. Toolbar buttons and the upload-onInsert
  // callback both use this, so we have to track it ourselves — the
  // forwardRef'd textarea is owned by AtMentionTextarea and the editor
  // doesn't get a chance to read selectionStart between renders.
  const cursorRef = useRef<{ start: number; end: number }>({ start: 0, end: 0 });

  const ready = !!requirementId;
  const { uploads, upload } = useAttachmentUpload(requirementId);

  // Bubble upload state out so the parent can show a badge / disable submit.
  // useEffect avoids the "setState during render" foot-gun from prop wiring.
  useEffectWithCallback(uploads, onUploadsChange);

  const updateCursorFromTextarea = useCallback(() => {
    const ta = textareaRef.current;
    if (!ta) return;
    cursorRef.current = { start: ta.selectionStart, end: ta.selectionEnd };
  }, []);

  const insertAtCursor = useCallback(
    (text: string) => {
      const { start, end } = cursorRef.current;
      const { next, cursor } = spliceAt(value, start, end, text);
      onChange(next);
      requestAnimationFrame(() => {
        const ta = textareaRef.current;
        if (ta) {
          ta.focus();
          ta.setSelectionRange(cursor, cursor);
        }
        cursorRef.current = { start: cursor, end: cursor };
      });
    },
    [value, onChange],
  );

  useImperativeHandle(ref, () => ({ insertAtCursor }), [insertAtCursor]);

  /** Run `file` through the upload pipeline. The onInsert callback fires
   *  inside requestAnimationFrame (in the hook) — by that point the editor
   *  has the latest cursor via cursorRef. */
  const ingest = useCallback(
    (file: File) => {
      upload(file, {
        description: t('components.richEditor.uploadPasted'),
        onInsert: (attId) => {
          insertAtCursor(`![image](nova://attachment/${attId})\n`);
        },
      });
    },
    [upload, insertAtCursor, t],
  );

  const handlePaste = useCallback(
    (e: React.ClipboardEvent<HTMLTextAreaElement>) => {
      updateCursorFromTextarea();
      if (!ready) return;
      const files = extractImageFiles(e.clipboardData?.items ?? null);
      if (files.length === 0) return;
      e.preventDefault();
      files.forEach(ingest);
    },
    [ready, ingest, updateCursorFromTextarea],
  );

  const handleDrop = useCallback(
    (e: React.DragEvent<HTMLElement>) => {
      updateCursorFromTextarea();
      setIsDragOver(false);
      if (!ready) return;
      const files = extractImageFilesFromList(e.dataTransfer?.files ?? null);
      if (files.length === 0) return;
      e.preventDefault();
      files.forEach(ingest);
    },
    [ready, ingest, updateCursorFromTextarea],
  );

  const handleDragOver = useCallback(
    (e: React.DragEvent<HTMLElement>) => {
      if (!ready) return;
      if (e.dataTransfer?.types?.includes('Files')) {
        e.preventDefault();
        setIsDragOver(true);
      }
    },
    [ready],
  );

  const handleDragLeave = useCallback(() => {
    setIsDragOver(false);
  }, []);

  const triggerFileInput = useCallback(() => {
    if (!ready) return;
    fileInputRef.current?.click();
  }, [ready]);

  const handleFilesPicked = useCallback(
    (e: React.ChangeEvent<HTMLInputElement>) => {
      const files = extractImageFilesFromList(e.target.files);
      files.forEach(ingest);
      // Reset so picking the same file twice still fires onChange.
      e.target.value = '';
    },
    [ingest],
  );

  // --- Toolbar handlers ---------------------------------------------------

  const wrap = useCallback(
    (before: string, after: string, placeholderText = '') => {
      const { start, end } = cursorRef.current;
      const selected = value.slice(start, end) || placeholderText;
      const inserted = before + selected + after;
      const { next, cursor } = spliceAt(value, start, end, inserted);
      onChange(next);
      // Place cursor between the inserted markers so the user can keep
      // typing without selecting again.
      const inner = cursor - after.length;
      requestAnimationFrame(() => {
        const ta = textareaRef.current;
        if (ta) {
          ta.focus();
          ta.setSelectionRange(inner, inner);
        }
        cursorRef.current = { start: inner, end: inner };
      });
    },
    [value, onChange],
  );

  const prefixLines = useCallback(
    (prefix: string) => {
      const { start, end } = cursorRef.current;
      // Expand to line boundaries so we don't leave half-prefixed lines.
      const lineStart = value.lastIndexOf('\n', start - 1) + 1;
      const slice = value.slice(lineStart, end === start ? value.length : end);
      const replaced = slice
        .split('\n')
        .map(line => (line.startsWith(prefix) ? line : prefix + line))
        .join('\n');
      const { next, cursor } = spliceAt(value, lineStart, lineStart + slice.length, replaced);
      onChange(next);
      requestAnimationFrame(() => {
        const ta = textareaRef.current;
        const newCursor = cursor;
        if (ta) {
          ta.focus();
          ta.setSelectionRange(newCursor, newCursor);
        }
        cursorRef.current = { start: newCursor, end: newCursor };
      });
    },
    [value, onChange],
  );

  const insertLink = useCallback(() => {
    const { start, end } = cursorRef.current;
    const selected = value.slice(start, end) || 'text';
    const url = window.prompt(t('components.richEditor.linkPrompt'), 'https://');
    if (!url) return;
    const inserted = `[${selected}](${url})`;
    const { next, cursor } = spliceAt(value, start, end, inserted);
    onChange(next);
    requestAnimationFrame(() => {
      const ta = textareaRef.current;
      if (ta) {
        ta.focus();
        ta.setSelectionRange(cursor, cursor);
      }
      cursorRef.current = { start: cursor, end: cursor };
    });
  }, [value, onChange, t]);

  // --- Render -------------------------------------------------------------

  return (
    <div
      className={`rich-editor${isDragOver ? ' is-dragover' : ''}${ready ? '' : ' is-disabled'}`}
      onDragOver={handleDragOver}
      onDragLeave={handleDragLeave}
      onDrop={handleDrop}
    >
      <div className="rich-editor__toolbar">
        <button type="button" title={t('components.richEditor.toolbarBold')} onClick={() => wrap('**', '**', 'bold')} disabled={disabled}>B</button>
        <button type="button" title={t('components.richEditor.toolbarItalic')} onClick={() => wrap('*', '*', 'italic')} disabled={disabled}><i>I</i></button>
        <button type="button" title={t('components.richEditor.toolbarInlineCode')} onClick={() => wrap('`', '`', 'code')} disabled={disabled}>{'</>'}</button>
        <button type="button" title={t('components.richEditor.toolbarLink')} onClick={insertLink} disabled={disabled}>🔗</button>
        <button type="button" title={t('components.richEditor.toolbarList')} onClick={() => prefixLines('- ')} disabled={disabled}>•</button>
        <button type="button" title={t('components.richEditor.toolbarCodeBlock')} onClick={() => wrap('\n```\n', '\n```\n', 'code')} disabled={disabled}>{'{ }'}</button>
        <button type="button" title={t('components.richEditor.toolbarImage')} onClick={triggerFileInput} disabled={disabled || !ready}>📷</button>
        <input
          ref={fileInputRef}
          type="file"
          accept="image/*"
          multiple
          hidden
          onChange={handleFilesPicked}
        />
        <div className="rich-editor__tabs">
          <button
            type="button"
            className={mode === 'edit' ? 'active' : ''}
            onClick={() => setMode('edit')}
          >
            {t('components.richEditor.tabEdit')}
          </button>
          <button
            type="button"
            className={mode === 'preview' ? 'active' : ''}
            onClick={() => setMode('preview')}
          >
            {t('components.richEditor.tabPreview')}
          </button>
        </div>
      </div>

      {mode === 'edit' ? (
        <AtMentionTextarea
          ref={textareaRef}
          value={value}
          onChange={(v) => {
            onChange(v);
            updateCursorFromTextarea();
          }}
          onPaste={handlePaste}
          onDrop={handleDrop}
          onKeyDown={onKeyDown}
          placeholder={
            !ready
              ? t('components.richEditor.preparing')
              : placeholder
          }
          rows={minRows}
          disabled={disabled}
        />
      ) : (
        <div className="rich-editor__preview md-prose">
          {value.trim() ? (
            <MarkdownRender content={rewriteNovaLinks(value, requirementId || '')} />
          ) : (
            <span className="rich-editor__preview-empty">
              {t('components.richEditor.previewEmpty')}
            </span>
          )}
        </div>
      )}

      {!ready && (
        <div className="rich-editor__hint">
          {t('components.richEditor.draftPreparing')}
        </div>
      )}

      {uploads.length > 0 && (
        <div className="rich-editor__uploads">
          {uploads.map((u) => (
            <span key={u.id} className={`upload-pill upload-${u.status}`}>
              {u.filename} ·{' '}
              {u.status === 'error'
                ? `✗ ${u.error || t('components.richEditor.upload_error')}`
                : t(`components.richEditor.upload_${u.status}`, u.status)}
            </span>
          ))}
        </div>
      )}
    </div>
  );
});

export default RichDescriptionEditor;

// Tiny helper: a useEffect that fires the callback whenever `value` changes.
// Lives in this file to keep RichDescriptionEditor self-contained; lifted
// out only if a second caller shows up.
function useEffectWithCallback<T>(value: T, cb?: (v: T) => void) {
  const ref = useRef(cb);
  ref.current = cb;
  useEffect(() => {
    ref.current?.(value);
  }, [value]);
}
