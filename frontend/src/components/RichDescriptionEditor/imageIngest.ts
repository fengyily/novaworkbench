// imageIngest — the clipboard / drag-drop plumbing shared by every surface
// that accepts pasted screenshots.
//
// These three helpers used to live inside RichDescriptionEditor.tsx as
// module-private functions. The sub-task composer (SubTaskPanel) needs the
// exact same "filter image/* out of a DataTransfer, splice a marker back at
// the caret" logic, so they were lifted here rather than copied — one copy,
// same folder as the other shared image pieces (compressImage.ts,
// rewriteNovaLinks.ts, useAttachmentUpload.ts).
//
// Nothing here talks to the network or touches the DOM: callers own the
// upload (useAttachmentUpload) and the cursor bookkeeping (their own ref).

const IMAGE_MIME_PREFIX = 'image/';

/** Pull every `image/*` File out of a clipboard DataTransferItemList.
 *  Clipboard images arrive as `kind: 'file'` items with a type; everything
 *  else (text/html, plain text, etc.) is filtered out so a normal text paste
 *  still reaches the textarea untouched. */
export function extractImageFiles(items: DataTransferItemList | null): File[] {
  if (!items) return [];
  const out: File[] = [];
  for (let i = 0; i < items.length; i += 1) {
    const it = items[i];
    if (it.kind !== 'file') continue;
    if (!it.type.startsWith(IMAGE_MIME_PREFIX)) continue;
    const f = it.getAsFile();
    if (f) out.push(f);
  }
  return out;
}

/** Same filter for the two entry points that hand us a FileList already —
 *  the HTML5 drop and the hidden file-picker <input>. */
export function extractImageFilesFromList(files: FileList | null): File[] {
  if (!files) return [];
  const out: File[] = [];
  for (let i = 0; i < files.length; i += 1) {
    const f = files[i];
    if (f.type.startsWith(IMAGE_MIME_PREFIX)) out.push(f);
  }
  return out;
}

/** Splice `text` into `value` over the [start, end) range, returning the new
 *  value plus the post-insert caret position (immediately after the inserted
 *  text). Pure string math — the caller decides whether to push the caret
 *  back into the DOM. */
export function spliceAt(
  value: string,
  start: number,
  end: number,
  text: string,
): { next: string; cursor: number } {
  return {
    next: value.slice(0, start) + text + value.slice(end),
    cursor: start + text.length,
  };
}