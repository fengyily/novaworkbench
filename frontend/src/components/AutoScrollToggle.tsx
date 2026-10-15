// Toolbar toggle button for the global "follow the stream" preference.
// Mirrors the FullscreenButton writing style so the two buttons read as a
// matched pair in the panel header (same .btn .btn-sm shell, same title +
// aria-label pattern). The actual state lives in useAutoScrollPref so every
// panel stays in sync.

import { useTranslation } from 'react-i18next';
import './AutoScrollToggle.css';

export interface AutoScrollToggleProps {
  enabled: boolean;
  onToggle: () => void;
}

export function AutoScrollToggle({ enabled, onToggle }: AutoScrollToggleProps) {
  const { t } = useTranslation();
  return (
    <button
      type="button"
      className={'btn btn-sm auto-scroll-toggle' + (enabled ? ' is-on' : '')}
      onClick={onToggle}
      aria-pressed={enabled}
      title={enabled ? t('components.autoScroll.onTitle') : t('components.autoScroll.offTitle')}
    >
      {enabled ? t('components.autoScroll.on') : t('components.autoScroll.off')}
    </button>
  );
}

export default AutoScrollToggle;
