// RequirementTreeToggle — the shared [+] / [−] expander for master
// requirements. Real <button> so it is keyboard-operable; the click never
// bubbles so it doesn't trigger the surrounding row/card navigation.

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
   *
   * Both render the same `+` because they answer the same user question
   * ("what about sub-requirements here?") — but they are genuinely
   * different actions, hence the mode. Before this split, `+` appeared
   * ONLY on rows that already had children, which made the whole
   * hierarchy undiscoverable: the slot was a blank spacer on every other
   * row, and drag-and-drop was the only way to ever create a first child.
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
      {/* Chevron glyph: ▸ collapsed, ▾ expanded. Same glyph for `add`
          mode — both answer "what about sub-requirements here?". The
          visual delta between add/expand only shows on row hover (the
          add variant picks up a softer color), so we don't need a
          different character. */}
      <span aria-hidden className="req-tree-toggle-glyph">{expanded ? '▾' : '▸'}</span>
    </button>
  );
}

/** Same-width spacer for rows without children so title edges align. */
export function RequirementTreeSpacer() {
  return <span className="req-tree-spacer" aria-hidden />;
}
