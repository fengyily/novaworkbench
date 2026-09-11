import { useTranslation } from 'react-i18next';

// Toolbar button that toggles an SSE output panel between normal and
// fullscreen mode. The actual "fullscreen" visual is a CSS-only transform
// (see .is-fullscreen in RequirementDetail.css), so this button is purely
// a state toggle + label flip.

export interface FullscreenButtonProps {
  isFullscreen: boolean;
  onClick: () => void;
  // When true, render a compact borderless style suitable for floating
  // overlays; otherwise match the existing .btn .btn-sm toolbar buttons.
  variant?: 'toolbar' | 'floating';
}

export function FullscreenButton({ isFullscreen, onClick, variant = 'toolbar' }: FullscreenButtonProps) {
  const { t } = useTranslation();
  const cls = variant === 'floating' ? 'fullscreen-close-btn' : 'btn btn-sm fullscreen-toggle-btn';
  const label = isFullscreen ? t('components.fullscreen.exit') : t('components.fullscreen.enter');
  const title = isFullscreen ? t('components.fullscreen.exitTitle') : t('components.fullscreen.enterTitle');
  return (
    <button
      type="button"
      className={cls}
      onClick={onClick}
      title={title}
      aria-label={title}
    >
      {label}
    </button>
  );
}