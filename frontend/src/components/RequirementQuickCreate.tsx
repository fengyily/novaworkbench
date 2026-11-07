// RequirementQuickCreate — the "add a sub-requirement" affordance on a
// requirement row.
//
// Why a popover of its own instead of the full CreateRequirementForm: the
// user asked for a FAST path. The full form brings a type picker, a priority
// picker, a rich-text body, a LaunchSpec switch and a reference selector —
// and, worse, it runs an LLM title distillation, which is a network round
// trip the "quick" promise cannot afford. This component sends an explicit
// title (the first line of what the user typed) plus `skip_organize`, so the
// backend never calls the LLM for this path.
//
// The child row and its parent link land in ONE request: the backend writes
// `parent_id` in the same INSERT as the requirement. The old shape — create,
// then PUT /parent — could leave an orphan top-level row behind whenever the
// second call failed, which reads to the user as "a requirement appeared out
// of nowhere".
//
// The popover is rendered through a portal with fixed positioning: the
// requirements table lives inside `.pr-list { overflow-x: auto }`, so an
// absolutely-positioned panel inside a cell would be clipped by that scroll
// container whenever it opened near the bottom of the page.
//
// `open` is CONTROLLED by the page, which keeps exactly one popover open
// across the whole table — two open panels would each steal the other's
// clicks and the user could not tell which master they were typing for.

import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { useTranslation } from 'react-i18next';
import { ApiError, requirementsApi, type Kind, type Requirement } from '../api/client';
import { IconClose } from './icons';
import './RequirementQuickCreate.css';

interface Props {
  /** Project the new requirement belongs to. */
  projectId: string;
  /** The master requirement the new row is nested under. */
  parentId: string;
  /** Master's title — shown in the popover heading. */
  parentTitle: string;
  /** Master's kind. The child inherits it, except `wiki`: a knowledge-base
   *  entry is a document, not a piece of work, so it never gets children. */
  parentKind?: Kind;
  /** Whether this row's popover is the open one. */
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: (req: Requirement) => void;
}

/**
 * Maps the backend's hierarchy error codes onto a translation key. The codes
 * are stable (handler/requirement.go writes them on the 400), so the UI can
 * say WHICH rule was broken instead of echoing a raw server string.
 */
function parentErrorKey(err: unknown): string | null {
  if (!(err instanceof ApiError)) return null;
  switch (err.code) {
    case 'PARENT_NOT_FOUND': return 'requirements.quickCreate.errParentNotFound';
    case 'PARENT_CROSS_PROJECT': return 'requirements.tree.errCrossProject';
    case 'PARENT_IS_CHILD': return 'requirements.tree.errTargetIsChild';
    case 'PARENT_SELF': return 'requirements.quickCreate.errParentSelf';
    case 'PARENT_ARCHIVED': return 'requirements.quickCreate.errParentArchived';
    default: return null;
  }
}

export function RequirementQuickCreate({
  projectId,
  parentId,
  parentTitle,
  parentKind,
  open,
  onOpenChange,
  onCreated,
}: Props) {
  const { t } = useTranslation();
  const [value, setValue] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const btnRef = useRef<HTMLButtonElement>(null);
  const popRef = useRef<HTMLDivElement>(null);
  const textRef = useRef<HTMLTextAreaElement>(null);
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null);

  const close = () => {
    onOpenChange(false);
    setError('');
  };

  // Position the popover against the button, flipping above it when there is
  // no room below. Measured AFTER the panel's first paint so its real
  // width/height drive the clamp — assuming a size would misplace it on
  // narrow viewports, where the panel is capped at 86vw.
  useLayoutEffect(() => {
    if (!open) return;
    const place = () => {
      const panel = popRef.current;
      const btn = btnRef.current;
      if (!panel || !btn) return;
      const r = btn.getBoundingClientRect();
      const w = panel.offsetWidth;
      const h = panel.offsetHeight;
      const left = Math.max(8, Math.min(r.right - w, window.innerWidth - w - 8));
      let top = r.bottom + 6;
      if (top + h > window.innerHeight - 8) {
        top = Math.max(8, r.top - h - 6);
      }
      // Scroll fires on every frame; only re-render when the anchor actually
      // moved, otherwise the list would re-render continuously while the user
      // scrolls with the panel open.
      setPos(prev => (prev && prev.top === top && prev.left === left ? prev : { top, left }));
    };
    place();
    window.addEventListener('scroll', place, true);
    window.addEventListener('resize', place);
    return () => {
      window.removeEventListener('scroll', place, true);
      window.removeEventListener('resize', place);
    };
  }, [open]);

  // Focus the textarea as soon as the panel opens — the whole point of the
  // fast path is that the user can type immediately.
  useEffect(() => {
    if (!open) return;
    textRef.current?.focus();
  }, [open]);

  // Click outside / Esc closes the panel. The toggle button itself is
  // EXCLUDED: it toggles on click, and treating the click as "outside" would
  // close and immediately reopen the panel. Any OTHER row's "+" is outside —
  // and since the page keeps only one popover open, opening that row's
  // panel replaces this one.
  useEffect(() => {
    if (!open) return;
    // `close` is inlined rather than called: it is recreated on every render,
    // so depending on it would re-register the listeners on every keystroke
    // in the textarea. The two lines it does are cheap and stable.
    const dismiss = () => {
      onOpenChange(false);
      setError('');
    };
    const onDown = (e: globalThis.MouseEvent) => {
      const target = e.target as HTMLElement | null;
      if (target?.closest('.req-quick-create')) return;
      if (target?.closest('.pr-title-add') === btnRef.current) return;
      dismiss();
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') dismiss();
    };
    document.addEventListener('mousedown', onDown);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('mousedown', onDown);
      document.removeEventListener('keydown', onKey);
    };
  }, [open, onOpenChange]);

  const submit = async () => {
    const text = value.trim();
    if (!text || busy) return;
    setBusy(true);
    setError('');
    try {
      // Title = first line, capped at 60 runes — the same cap the backend's
      // fallbackTitle uses, so a title produced here and one produced by the
      // server look identical. Truncating by rune (not by code unit) keeps a
      // Chinese character from being cut in half.
      const firstLine = text.split('\n')[0].trim();
      const title = [...firstLine].slice(0, 60).join('');
      const req = await requirementsApi.create({
        project_id: projectId,
        description: text,
        title,
        // Belt and braces: an explicit title already skips the LLM distiller
        // (the backend only distills when the title is empty), and
        // skip_organize keeps the raw body verbatim instead of rewriting it.
        skip_organize: true,
        parent_id: parentId,
        kind: parentKind === 'wiki' ? 'requirement' : parentKind,
      });
      setValue('');
      onCreated(req);
    } catch (err) {
      const key = parentErrorKey(err);
      if (key) {
        setError(t(key));
      } else {
        const raw = err instanceof Error ? err.message : String(err);
        setError(t('requirements.quickCreate.error', { msg: raw }));
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <button
        ref={btnRef}
        type="button"
        className="pr-title-add"
        title={t('requirements.quickCreate.buttonTitle')}
        aria-label={t('requirements.quickCreate.buttonTitle')}
        aria-expanded={open}
        onClick={e => {
          e.stopPropagation();
          setError('');
          onOpenChange(!open);
        }}
        onKeyDown={e => e.stopPropagation()}
      >
        <svg width="12" height="12" viewBox="0 0 12 12" fill="none" focusable="false" aria-hidden>
          <path d="M6 2.25v7.5M2.25 6h7.5" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" />
        </svg>
      </button>
      {open && createPortal(
        <div
          ref={popRef}
          className="req-quick-create"
          role="dialog"
          aria-label={t('requirements.quickCreate.heading', { parent: parentTitle })}
          style={{ top: pos?.top ?? -9999, left: pos?.left ?? -9999 }}
          onClick={e => e.stopPropagation()}
        >
          <div className="req-quick-create-head">
            <span className="req-quick-create-title">
              {t('requirements.quickCreate.heading', { parent: parentTitle })}
            </span>
            <button
              type="button"
              className="req-quick-create-close"
              onClick={close}
              aria-label={t('requirements.quickCreate.cancel')}
              title={t('requirements.quickCreate.cancel')}
            >
              <IconClose size={12} />
            </button>
          </div>
          <textarea
            ref={textRef}
            className="req-quick-create-input"
            value={value}
            rows={3}
            placeholder={t('requirements.quickCreate.placeholder')}
            onChange={e => setValue(e.target.value)}
            onClick={e => e.stopPropagation()}
            onKeyDown={e => {
              e.stopPropagation();
              if (e.key === 'Enter' && !e.shiftKey) {
                e.preventDefault();
                void submit();
              }
            }}
          />
          {error && <div className="req-quick-create-error">{error}</div>}
          <div className="req-quick-create-actions">
            <button type="button" className="btn btn-sm" onClick={close} disabled={busy}>
              {t('requirements.quickCreate.cancel')}
            </button>
            <button
              type="button"
              className="btn btn-primary btn-sm"
              onClick={() => void submit()}
              disabled={busy || !value.trim()}
            >
              {busy ? t('requirements.quickCreate.creating') : t('requirements.quickCreate.submit')}
            </button>
          </div>
        </div>,
        document.body,
      )}
    </>
  );
}

export default RequirementQuickCreate;
