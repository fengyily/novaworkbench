// RequirementTreeToggle — the shared [+] / [−] expander for master
// requirements. Real <button> so it is keyboard-operable; the click never
// bubbles so it doesn't trigger the surrounding row/card navigation.

import type { MouseEvent } from 'react';
import { useTranslation } from 'react-i18next';
import './RequirementTreeToggle.css';

interface Props {
  expanded: boolean;
  onToggle: () => void;
  /** id of the element holding the children, for aria-controls */
  controlsId?: string;
}

export function RequirementTreeToggle({ expanded, onToggle, controlsId }: Props) {
  const { t } = useTranslation();
  const label = expanded ? t('requirements.tree.collapse') : t('requirements.tree.expand');
  const handle = (e: MouseEvent) => {
    e.stopPropagation();
    onToggle();
  };
  return (
    <button
      type="button"
      className={`req-tree-toggle${expanded ? ' expanded' : ''}`}
      aria-expanded={expanded}
      aria-controls={controlsId}
      aria-label={label}
      title={label}
      onClick={handle}
      onKeyDown={e => e.stopPropagation()}
    >
      <span aria-hidden>{expanded ? '−' : '+'}</span>
    </button>
  );
}

/** Same-width spacer for rows without children so title edges align. */
export function RequirementTreeSpacer() {
  return <span className="req-tree-spacer" aria-hidden />;
}
