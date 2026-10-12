// Conditional "stick to bottom" behavior for SSE chat panels.
//
// Problem: naive `el.scrollTop = el.scrollHeight` on every dep change forces
// the viewport to bottom even when the user has scrolled up to read history,
// interrupting their reading.
//
// Solution: track `isAtBottom` from the scroll listener (with a small
// `threshold` to absorb sub-pixel rounding). Only auto-scroll when at bottom;
// otherwise increment `pendingCount` so the consumer can render a "↓ N new
// messages" jump button. Returning to bottom clears `pendingCount` and
// resumes auto-follow.
//
// Initial state is `true` so the first dep tick matches previous behavior
// (panel mounts and jumps to latest).
//
// Exports a ref + isAtBottom + pendingCount + scrollToBottom; the consumer
// spreads the ref onto its scroll container.

import {
  useCallback,
  useEffect,
  useReducer,
  useRef,
  useState,
  type RefObject,
} from 'react';

export interface UseStickToBottomOptions {
  /** Distance from bottom (px) considered "still at bottom". Default 32. */
  threshold?: number;
  /** Deps that should trigger an auto-stick attempt. Default []. */
  deps?: ReadonlyArray<unknown>;
  /** Disable auto-follow entirely (e.g. for Storybook / isolated tests). */
  disabled?: boolean;
}

export interface StickToBottomHandle<T extends HTMLElement> {
  // React 19: `useRef<T>(null)` returns RefObject<T | null>, not RefObject<T>.
  // Consumers can still pass it as `ref={...}` on a JSX element (RefObject is
  // a valid member of the `Ref<T>` union). We expose the exact type so the
  // returned handle round-trips without casts.
  ref: RefObject<T | null>;
  isAtBottom: boolean;
  pendingCount: number;
  scrollToBottom: (behavior?: 'auto' | 'smooth') => void;
}

export function useStickToBottom<T extends HTMLElement = HTMLDivElement>(
  opts: UseStickToBottomOptions = {},
): StickToBottomHandle<T> {
  const ref = useRef<T>(null);
  const [isAtBottom, setIsAtBottom] = useState(true);
  const pendingCountRef = useRef(0);
  const [, force] = useReducer((x: number) => x + 1, 0);

  const threshold = opts.threshold ?? 32;

  const compute = useCallback((): boolean => {
    const el = ref.current;
    if (!el) return true;
    return el.scrollTop + el.clientHeight >= el.scrollHeight - threshold;
  }, [threshold]);

  // 1) scroll listener: sync `isAtBottom`, clear pendingCount when at bottom.
  useEffect(() => {
    if (opts.disabled) return;
    const el = ref.current;
    if (!el) return;
    const onScroll = () => {
      const at = compute();
      setIsAtBottom(at);
      if (at && pendingCountRef.current !== 0) {
        pendingCountRef.current = 0;
        force();
      }
    };
    el.addEventListener('scroll', onScroll, { passive: true });
    return () => el.removeEventListener('scroll', onScroll);
  }, [compute, opts.disabled]);

  // 2) deps change: conditionally auto-scroll.
  useEffect(() => {
    if (opts.disabled) return;
    const el = ref.current;
    if (!el) return;
    if (compute()) {
      // Immediate assignment — no smooth scroll on auto-follow; SSE streams
      // tick many times per second and animation would queue up.
      el.scrollTop = el.scrollHeight;
    } else {
      pendingCountRef.current += 1;
      force();
    }
    // We intentionally depend on the user-provided `deps` array verbatim.
    // The hook's other callbacks (compute) are stable per threshold.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, opts.deps ?? []);

  const scrollToBottom = useCallback((behavior: 'auto' | 'smooth' = 'smooth') => {
    const el = ref.current;
    if (!el) return;
    el.scrollTo({ top: el.scrollHeight, behavior });
    // The `scroll` listener will set isAtBottom=true and clear pendingCount.
  }, []);

  return { ref, isAtBottom, pendingCount: pendingCountRef.current, scrollToBottom };
}
