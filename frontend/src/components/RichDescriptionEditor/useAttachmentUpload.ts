// useAttachmentUpload — orchestrates the upload pipeline the RichDescriptionEditor
// drives for every paste / drop / file-picker image. The hook is a small
// stateful wrapper around attachmentsApi.upload so the editor component
// stays focused on layout and cursor math.
//
// Failure surface: upload() never throws. Errors land on the per-task
// `error` field and `status: 'error'`, leaving the UI in charge of how to
// render them. The only exception is `requirementId === null`, which is a
// programmer / state-machine error (the editor shouldn't even be enabled
// in that case) and surfaces as a real rejection so the dev sees it.

import { useCallback, useEffect, useRef, useState } from 'react';
import { attachmentsApi } from '../../api/client';
import { compressImage } from './compressImage';

export type UploadStatus = 'compressing' | 'uploading' | 'done' | 'error';

export interface UploadTask {
  /** Local key for React lists. NOT the server attachment id. */
  id: string;
  /** Original filename (best effort — paste events may not provide one). */
  filename: string;
  status: UploadStatus;
  /** Server attachment id, populated on success. */
  attId?: string;
  /** Error message for the UI; only set when status === 'error'. */
  error?: string;
}

export interface UploadOptions {
  description?: string;
  /** Invoked with the new attachment id once the server confirms. The
   *  editor's caller is expected to splice a `![image](nova://attachment/<id>)`
   *  marker into the textarea at the current cursor. Wrapped in
   *  requestAnimationFrame by the hook so the cursor state has settled. */
  onInsert?: (attId: string) => void;
}

let localTaskCounter = 0;
function nextLocalId(): string {
  localTaskCounter += 1;
  return `t_${Date.now().toString(36)}_${localTaskCounter}`;
}

export function useAttachmentUpload(requirementId: string | null) {
  const [uploads, setUploads] = useState<UploadTask[]>([]);
  const abortRef = useRef<AbortController | null>(null);

  // Abort any in-flight fetch on unmount so the editor can be torn down
  // without leaving the network tab with phantom POSTs.
  useEffect(() => {
    return () => {
      abortRef.current?.abort();
      abortRef.current = null;
    };
  }, []);

  // Reset the controller when the requirement id changes (the editor
  // re-mounts in that case anyway, but defensive).
  useEffect(() => {
    abortRef.current?.abort();
    abortRef.current = new AbortController();
  }, [requirementId]);

  const updateTask = useCallback((id: string, patch: Partial<UploadTask>) => {
    setUploads(prev => prev.map(t => (t.id === id ? { ...t, ...patch } : t)));
  }, []);

  const upload = useCallback(
    async (file: File | Blob, opts: UploadOptions = {}): Promise<string | null> => {
      if (!requirementId) {
        throw new Error('requirementId is required');
      }
      const taskId = nextLocalId();
      const filename = (file as File).name || 'pasted-image';

      setUploads(prev => [
        ...prev,
        { id: taskId, filename, status: 'compressing' },
      ]);

      try {
        const compressed = await compressImage(file);
        if (compressed !== file) {
          // Surface the size delta for the compressNotice UI hint.
          // (No toast here — the editor renders a pill on the task.)
        }
        updateTask(taskId, { status: 'uploading' });

        const att = await attachmentsApi.upload(
          'requirements',
          requirementId,
          compressed instanceof File ? compressed : new File([compressed], filename, { type: compressed.type }),
          opts.description,
        );

        updateTask(taskId, { status: 'done', attId: att.id });
        if (opts.onInsert) {
          // Defer the textarea mutation so the React state machine has
          // finished applying the previous state batch.
          requestAnimationFrame(() => opts.onInsert?.(att.id));
        }
        return att.id;
      } catch (err: unknown) {
        const msg = err instanceof Error ? err.message : 'upload failed';
        updateTask(taskId, { status: 'error', error: msg });
        return null;
      }
    },
    [requirementId, updateTask],
  );

  const remove = useCallback((taskId: string) => {
    setUploads(prev => prev.filter(t => t.id !== taskId));
  }, []);

  const reset = useCallback(() => {
    setUploads([]);
  }, []);

  return { uploads, upload, remove, reset };
}
