// Lightweight fullscreen-state hook for SSE output panels. The "fullscreen"
// mode is CSS-only (toggle a class on the panel), so React state, refs, and
// the running EventStream are not disrupted — important because the SSE pipe
// drives continuous UI updates and must not be torn down while a job is live.
//
// Pressing Esc exits fullscreen, matching the platform convention.

import { useCallback, useEffect, useState } from 'react';

export interface FullscreenController {
  isFullscreen: boolean;
  enter: () => void;
  exit: () => void;
  toggle: () => void;
}

export function useFullscreen(): FullscreenController {
  const [isFullscreen, setIsFullscreen] = useState(false);

  const enter = useCallback(() => setIsFullscreen(true), []);
  const exit = useCallback(() => setIsFullscreen(false), []);
  const toggle = useCallback(() => setIsFullscreen(v => !v), []);

  useEffect(() => {
    if (!isFullscreen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setIsFullscreen(false);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [isFullscreen]);

  return { isFullscreen, enter, exit, toggle };
}