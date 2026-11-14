// Sticky toolbar rendered as the FIRST child of an SSE stream panel
// (.coding-panel). It hosts two controls:
//
//   1. the "↓ N new messages" jump button (only rendered while there are
//      missed lines), and
//   2. the explicit auto-scroll toggle.
//
// The bar lives INSIDE the panel rather than in the page header on purpose:
// in fullscreen mode the panel becomes a fixed overlay and the page header
// is no longer visible, so controls placed outside the panel would be
// unreachable exactly when the user wants them.

import { useTranslation } from 'react-i18next';
import { IconChevronDown } from './icons';
import { ScrollToBottomButton } from './ScrollToBottomButton';
import './StreamPanelBar.css';

export interface StreamPanelBarProps {
  autoScroll: boolean;
  onToggleAutoScroll: () => void;
  pendingCount: number;
  onJumpToBottom: () => void;
}

export function StreamPanelBar({
  autoScroll,
  onToggleAutoScroll,
  pendingCount,
  onJumpToBottom,
}: StreamPanelBarProps) {
  const { t } = useTranslation();

  return (
    <div className="stream-panel-bar">
      <ScrollToBottomButton count={pendingCount} onClick={onJumpToBottom} />
      <button
        type="button"
        className={`stream-autoscroll-toggle ${autoScroll ? 'is-on' : ''}`}
        onClick={onToggleAutoScroll}
        aria-pressed={autoScroll}
        title={autoScroll ? t('components.autoScroll.disable') : t('components.autoScroll.enable')}
      >
        <IconChevronDown size={12} className="icon-mr" />
        <span className="stream-autoscroll-label">{t('components.autoScroll.label')}</span>
        <span className="stream-autoscroll-state">
          {autoScroll ? t('components.autoScroll.on') : t('components.autoScroll.off')}
        </span>
      </button>
    </div>
  );
}

export default StreamPanelBar;
