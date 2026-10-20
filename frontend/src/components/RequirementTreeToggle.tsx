// RequirementTreeToggle — the shared expander / attach button for master
// requirements. Real <button> so it is keyboard-operable; the click never
// bubbles so it doesn't trigger the surrounding row/card navigation.
//
// The two modes answer different user questions and so use different
// glyphs:
//   - 'expand' — this master already has sub-requirements; the button
//                folds them away / unfolds them. Drawn as a chevron that
//                rotates 90° on expansion (CSS does the rotation, JS
//                stays untouched).
//   - 'add'    — it has none yet; the button opens the "attach a
//                sub-requirement" picker. Drawn as a `+` glyph to make
//                it discoverable (an idle chevron at #CBD5E1 read as a
//                rendering artifact before this split).
//
// Child rows render RequirementTreeChildMark in their leading column
// instead of either of the above — purely decorative, since the row's
// tint + rail + indent already carry the hierarchy (the old elbow guide
// line was drawn by the cell's pseudo-elements and has since been
// replaced by the grouped-block styling).

import type { MouseEvent } from 'react';
import { useTranslation } from 'react-i18next';
import './RequirementTreeToggle.css';

interface Props {
  expanded: boolean;
  /** required when mode === 'expand' */
  onToggle?: () => void;
  /** id of the element holding the children, for aria-controls */
  controlsId?: string;
  /**
   * 'expand' — this master already has sub-requirements; the button
   *            folds them away / unfolds them.
   * 'add'    — it has none yet; the button opens the "attach a
   *            sub-requirement" picker instead of toggling.
   */
  mode?: 'expand' | 'add';
  /** required when mode === 'add' */
  onAdd?: () => void;
}

export function RequirementTreeToggle({ expanded, onToggle, controlsId, mode = 'expand', onAdd }: Props) {
  const { t } = useTranslation();
  const isAdd = mode === 'add';
  const label = isAdd
    ? t('requirements.tree.addChild')
    : expanded
      ? t('requirements.tree.collapse')
      : t('requirements.tree.expand');
  const handle = (e: MouseEvent) => {
    e.stopPropagation();
    if (isAdd) onAdd?.();
    else onToggle?.();
  };
  return (
    <button
      type="button"
      className={`req-tree-toggle${isAdd ? ' req-tree-add' : ''}${expanded ? ' expanded' : ''}`}
      aria-expanded={expanded}
      aria-controls={controlsId}
      aria-label={label}
      title={label}
      onClick={handle}
      onKeyDown={e => e.stopPropagation()}
    >
      <span aria-hidden className="req-tree-toggle-glyph">
        {isAdd ? (
          <svg width="12" height="12" viewBox="0 0 12 12" fill="none" focusable="false">
            <path d="M6 2.25v7.5M2.25 6h7.5" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" />
          </svg>
        ) : (
          <svg width="12" height="12" viewBox="0 0 12 12" fill="none" focusable="false">
            <path d="M4.25 2.5 8.5 6l-4.25 3.5" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" />
          </svg>
        )}
      </span>
    </button>
  );
}

/** Same-width spacer for rows without children so title edges align. */
export function RequirementTreeSpacer() {
  return <span className="req-tree-spacer" aria-hidden />;
}

/** Static "I am a child" mark for the leading column of a child row.
    Purely decorative — the hierarchy is already carried by the row's
    tint + rail + indent, so this stays aria-hidden and is never a
    second source of truth. Replaces the elbow guide line that used to
    be drawn here with pseudo-elements. */
export function RequirementTreeChildMark() {
  return (
    <span className="req-tree-child-mark" aria-hidden>
      <svg width="12" height="12" viewBox="0 0 12 12" fill="none" focusable="false">
        <path d="M3.5 10.5V5.5A1.5 1.5 0 0 1 5 4H8.75" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
        <path d="M6.75 1.75 9.25 4l-2.5 2.5" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
    </span>
  );
}
