// RequirementMarkChips — the interactive preset-mark strip rendered inline
// with a requirement title.
//
// The requirement lists used to render the marks (重要 / 跟进 / 阻塞 / 风险)
// as READ-ONLY text: removing or adding one meant opening the requirement
// and hunting for the chip selector in the detail header. With a hundred
// requirements in a project tab, that round trip is the dominant cost of
// "mark these five as important".
//
// This component is the single implementation shared by every list surface
// (project detail tab, project overview table, cross-project list, and the
// mobile card), so the surfaces cannot drift apart — the same argument that
// produced RequirementMobileCard.
//
// Two affordances:
//   • clicking an existing chip REMOVES that mark (one click — the high
//     frequency action);
//   • the "＋" button opens a preset menu for adding one. Picking a preset
//     that is already applied removes it, so both entry points share ONE
//     `toggle` and there is a single semantics to reason about.
//
// The backend endpoint PUTs the WHOLE array (replace semantics), so the
// component always computes a complete next array and serializes the
// writes: two fast clicks must not overwrite each other.
//
// Every button stops propagation and is draggable={false}: the requirement
// row is a <tr> that navigates on click and can be dragged to nest, and
// HTML5 drag starts are promoted to the row — stopPropagation alone does not
// stop a chip click from starting a drag.

import { useEffect, useLayoutEffect, useRef, useState, type JSX } from 'react';
import { createPortal } from 'react-dom';
import { useTranslation } from 'react-i18next';
import {
  MARK_PRESETS,
  parseMarks,
  requirementsApi,
  type Requirement,
} from '../../api/client';
import { errorMessage } from '../../utils/errMsg';
import './RequirementMarkChips.css';

export interface RequirementMarkChipsProps {
  /** The requirement whose marks this strip edits. */
  reqId: string;
  /** Raw `marks` column — a JSON array string such as '["important"]'. */
  marks: string | undefined;
  /** Requirement status. 'archived' rows are read-only. */
  status?: string;
  /** Called with the server's authoritative Requirement after a change. */
  onChange?: (updated: Requirement) => void;
  /** Called with a user-facing message when a write fails. */
  onError?: (message: string) => void;
  /** Extra classes for the host to align the strip inside its own row. */
  className?: string;
}

/**
 * Toggle one preset code. Pure so it can be exercised on its own.
 */
export function nextMarks(codes: string[], code: string): string[] {
  return codes.includes(code) ? codes.filter(c => c !== code) : [...codes, code];
}

export default function RequirementMarkChips({
  reqId,
  marks,
  status,
  onChange,
  onError,
  className,
}: RequirementMarkChipsProps): JSX.Element {
  const { t } = useTranslation();

  // Local (optimistic) copy of the applied codes. `codesRef` mirrors it so
  // the write path always reads the freshest value instead of the value
  // captured by an older closure.
  const [codes, setCodes] = useState<string[]>(() => parseMarks(marks));
  const codesRef = useRef<string[]>(codes);

  // Serialized write chain. The endpoint replaces the whole array, so two
  // concurrent PUTs would overwrite each other; chaining them per component
  // makes the LAST write win — which is the user's last intent.
  const queueRef = useRef<Promise<unknown>>(Promise.resolve());

  // Number of writes that are queued or in flight. Used as a counter, not a
  // boolean: with two queued writes the first one settling must not clear
  // the "busy" flag while the second is still on its way.
  const pendingCountRef = useRef(0);
  const [pending, setPending] = useState(false);

  // Bumped on every local edit. A write may only overwrite the local state
  // when no newer edit happened while it was in flight — otherwise the
  // response of an earlier write would drop the newer intent (the server
  // state it describes is already stale).
  const localVersionRef = useRef(0);

  const [menuOpen, setMenuOpen] = useState(false);
  const [anchorVisible, setAnchorVisible] = useState(false);
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null);
  const btnRef = useRef<HTMLButtonElement | null>(null);
  const menuRef = useRef<HTMLDivElement | null>(null);

  const readOnly = status === 'archived';

  const applyCodes = (next: string[]) => {
    codesRef.current = next;
    setCodes(next);
  };

  // Re-sync from the host when the server value arrives — but never while a
  // write of ours is on its way, or the stale prop would undo the
  // optimistic result the user is looking at.
  useEffect(() => {
    if (pendingCountRef.current > 0) return;
    const parsed = parseMarks(marks);
    const current = codesRef.current;
    if (current.length === parsed.length && current.every((c, i) => c === parsed[i])) return;
    // Inlined rather than going through applyCodes: that helper is
    // re-created on every render, and listing it as a dependency would
    // re-run this effect on every render.
    codesRef.current = parsed;
    setCodes(parsed);
  }, [marks]);

  const toggle = (code: string) => {
    if (readOnly) return;
    // Always derive from the LOCAL latest state, never from the server
    // response — that is what makes rapid clicking add up instead of
    // cancelling itself out.
    const next = nextMarks(codesRef.current, code);
    const version = ++localVersionRef.current;
    applyCodes(next);

    pendingCountRef.current += 1;
    setPending(true);

    queueRef.current = queueRef.current
      .then(() => requirementsApi.updateMarks(reqId, next))
      .then(updated => {
        // The server is the authority on the stored value (it drops unknown
        // codes and caps the count), but only the newest write may apply
        // it — an earlier response describes a state the newer write is
        // about to replace.
        if (version === localVersionRef.current) {
          applyCodes(parseMarks(updated.marks));
        }
        onChange?.(updated);
      })
      .catch(async (err: unknown) => {
        if (version === localVersionRef.current) {
          // Failed write: go back to the server for the truth instead of
          // leaving an optimistic value that never landed.
          try {
            const fresh = await requirementsApi.get(reqId);
            applyCodes(parseMarks(fresh.marks));
            onChange?.(fresh);
          } catch {
            // Even the re-read failed — keep the optimistic value; the
            // host's next natural refresh corrects it.
          }
          onError?.(t('requirements.list.marksFail', { msg: errorMessage(err) }));
        }
      })
      .finally(() => {
        pendingCountRef.current = Math.max(0, pendingCountRef.current - 1);
        if (pendingCountRef.current === 0) setPending(false);
      });
  };

  // Is the "＋" trigger actually on screen? A requirement row is rendered
  // TWICE — once in the desktop table, once in the mobile card — and the
  // hidden one has a zero-size rect. A menu anchored to it would be pinned
  // to the top-left corner of the viewport.
  useLayoutEffect(() => {
    if (!menuOpen) {
      setAnchorVisible(false);
      return;
    }
    const btn = btnRef.current;
    if (!btn) {
      setAnchorVisible(false);
      return;
    }
    const r = btn.getBoundingClientRect();
    setAnchorVisible(r.width > 0 || r.height > 0);
  }, [menuOpen]);

  // Position the menu against the trigger, flipping above it when there is
  // no room below. Measured AFTER the menu's first paint so its real size
  // drives the clamp.
  useLayoutEffect(() => {
    if (!menuOpen || !anchorVisible) return;
    const place = () => {
      const menu = menuRef.current;
      const btn = btnRef.current;
      if (!menu || !btn) return;
      const r = btn.getBoundingClientRect();
      const w = menu.offsetWidth;
      const h = menu.offsetHeight;
      const left = Math.max(8, Math.min(r.left, window.innerWidth - w - 8));
      let top = r.bottom + 6;
      if (top + h > window.innerHeight - 8) {
        top = Math.max(8, r.top - h - 6);
      }
      // Scroll fires on every frame; only re-render when the anchor
      // actually moved, otherwise the list would re-render continuously
      // while the user scrolls with the menu open.
      setPos(prev => (prev && prev.top === top && prev.left === left ? prev : { top, left }));
    };
    place();
    window.addEventListener('scroll', place, true);
    window.addEventListener('resize', place);
    return () => {
      window.removeEventListener('scroll', place, true);
      window.removeEventListener('resize', place);
    };
  }, [menuOpen, anchorVisible]);

  // Click outside / Esc closes the menu. The trigger itself is EXCLUDED: it
  // toggles the menu on click, and treating that click as "outside" would
  // close and immediately reopen it.
  useEffect(() => {
    if (!menuOpen) return;
    const dismiss = () => setMenuOpen(false);
    const onDown = (e: globalThis.MouseEvent) => {
      const target = e.target as HTMLElement | null;
      if (target?.closest('.rmc-menu')) return;
      if (target?.closest('.rmc-add') === btnRef.current) return;
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
  }, [menuOpen]);

  // Presets the server knows about; an unknown code (a preset renamed
  // after this row was written) is skipped rather than rendered as a
  // nameless chip.
  const visibleCodes = codes.filter(code => MARK_PRESETS.some(p => p.code === code));

  return (
    <div className={`rmc-strip${className ? ' ' + className : ''}`}>
      {visibleCodes.map(code => {
        const p = MARK_PRESETS.find(x => x.code === code);
        if (!p) return null;
        const label = t(`requirements.detail2.marksPreset.${p.code}`);
        return (
          <button
            key={code}
            type="button"
            className="rmc-chip"
            draggable={false}
            style={{ color: p.color, background: p.bg, borderColor: p.color }}
            disabled={readOnly}
            title={t('requirements.list.marksRemoveTitle', { code: label })}
            onClick={e => { e.stopPropagation(); toggle(p.code); }}
            onMouseDown={e => e.stopPropagation()}
          >
            <span aria-hidden>{p.icon}</span>
            <span>{label}</span>
          </button>
        );
      })}

      {/* The "＋" trigger is always present — it is the only way to ADD a
          mark to a row that has none. */}
      <button
        ref={btnRef}
        type="button"
        className="rmc-add"
        draggable={false}
        disabled={readOnly}
        title={t('requirements.list.marksAddTitle')}
        aria-label={t('requirements.list.marksAddTitle')}
        aria-haspopup="dialog"
        aria-expanded={menuOpen}
        onClick={e => { e.stopPropagation(); setMenuOpen(v => !v); }}
        onMouseDown={e => e.stopPropagation()}
      >
        ＋
      </button>

      {menuOpen && anchorVisible && createPortal(
        <div
          ref={menuRef}
          className="rmc-menu"
          role="dialog"
          aria-label={t('requirements.list.marksMenuLabel')}
          style={{ top: pos?.top ?? -9999, left: pos?.left ?? -9999 }}
        >
          <div className="rmc-menu-title">{t('requirements.list.marksAdd')}</div>
          {MARK_PRESETS.map(p => {
            const label = t(`requirements.detail2.marksPreset.${p.code}`);
            const active = codes.includes(p.code);
            const style = active
              ? { color: p.color, background: p.bg, borderColor: p.color }
              : { color: p.color };
            return (
              <button
                key={p.code}
                type="button"
                className={`rmc-menu-item${active ? ' is-active' : ''}`}
                draggable={false}
                disabled={pending}
                aria-pressed={active}
                style={style}
                onClick={e => { e.stopPropagation(); toggle(p.code); }}
                onMouseDown={e => e.stopPropagation()}
              >
                <span aria-hidden>{p.icon}</span>
                <span className="rmc-menu-label">{label}</span>
              </button>
            );
          })}
        </div>,
        document.body,
      )}
    </div>
  );
}
