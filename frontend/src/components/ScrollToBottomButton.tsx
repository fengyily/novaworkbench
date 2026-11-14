// Inline "↓ N new messages" pill shown inside an SSE stream panel when the
// user has scrolled up. Reads pendingCount from useStickToBottom and
// triggers the panel's scrollToBottom on click.
//
// Positioning: rendered inside the sticky panel bar (.stream-panel-bar) at
// the top of the scroll container, so it stays visible while the log scrolls
// and remains reachable in fullscreen mode.

import { useTranslation } from 'react-i18next';
import './ScrollToBottomButton.css';

interface ScrollToBottomButtonProps {
  count: number;
  onClick: () => void;
}

export function ScrollToBottomButton({ count, onClick }: ScrollToBottomButtonProps) {
  const { t } = useTranslation();
  if (count <= 0) return null;

  return (
    <button
      type="button"
      className="scroll-to-bottom-button"
      onClick={onClick}
      aria-label={t('components.scrollToBottom.ariaLabel')}
      title={t('components.scrollToBottom.button')}
    >
      <span className="stb-icon" aria-hidden="true">
        ↓
      </span>
      <span className="stb-label">
        {t('components.scrollToBottom.newMessages', { count })}
      </span>
    </button>
  );
}

export default ScrollToBottomButton;
