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
// Exports a callback ref + isAtBottom + pendingCount + scrollToBottom; the
// consumer spreads the ref onto its scroll container. We deliberately use a
// callback ref instead of a RefObject because several panels mount their
// scroll container conditionally (e.g. only when SSE lines exist), and a
// RefObject-based effect would leave the scroll listener unbound — the panel
// would silently degrade to unconditional scroll-to-bottom.

import {
  useCallback,
  useEffect,
  useReducer,
  useRef,
  useState,
} from 'react';

export interface UseStickToBottomOptions {
  /** Distance from bottom (px) considered "still at bottom". Default 32. */
  threshold?: number;
  /** Deps that should trigger an auto-stick attempt. Default []. */
  deps?: ReadonlyArray<unknown>;
  /** Disable auto-follow entirely (e.g. global user toggle). */
  disabled?: boolean;
  /** When provided, pendingCount is derived from the growth of this counter
   *  instead of counting dep ticks. Accurate for log arrays whose tail line is
   *  replaced in place by a thinking heartbeat (which would otherwise inflate
   *  the counter). */
  itemCount?: number;
}

export interface StickToBottomHandle<T extends HTMLElement> {
  /** Callback ref — spread onto the scroll container. */
  ref: (el: T | null) => void;
  isAtBottom: boolean;
  pendingCount: number;
  scrollToBottom: (behavior?: 'auto' | 'smooth') => void;
}

export function useStickToBottom<T extends HTMLElement = HTMLDivElement>(
  opts: UseStickToBottomOptions = {},
): StickToBottomHandle<T> {
  const [node, setNode] = useState<T | null>(null);
  const [isAtBottom, setIsAtBottom] = useState(true);
  const pendingCountRef = useRef(0);
  const [, force] = useReducer((x: number) => x + 1, 0);
  const lastItemCountRef = useRef<number | undefined>(opts.itemCount);
  const wasDisabledRef = useRef<boolean>(!!opts.disabled);

  const threshold = opts.threshold ?? 32;

  const compute = useCallback((): boolean => {
    const el = node;
    if (!el) return true;
    return el.scrollTop + el.clientHeight >= el.scrollHeight - threshold;
  }, [node, threshold]);

  // 1) scroll listener: sync `isAtBottom`, clear pendingCount when at bottom.
  useEffect(() => {
    if (opts.disabled) return;
    const el = node;
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
  }, [node, compute, opts.disabled]);

  // 2) deps change: conditionally auto-scroll.
  useEffect(() => {
    if (opts.disabled) return;
    const el = node;
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
    // The hook's other callbacks (compute) are stable per node + threshold.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, opts.deps ?? []);

  // 3) itemCount tick: alternative counter source for log panels whose tail
  //    row is replaced in place by a thinking heartbeat (deps don't change,
  //    but the array length grows).
  useEffect(() => {
    if (opts.disabled) return;
    if (opts.itemCount === undefined) return;
    const prev = lastItemCountRef.current;
    lastItemCountRef.current = opts.itemCount;
    if (prev === undefined) return;
    if (opts.itemCount <= prev) return;
    const el = node;
    if (!el) return;
    if (compute()) {
      el.scrollTop = el.scrollHeight;
    } else {
      pendingCountRef.current += 1;
      force();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [opts.itemCount]);

  // 4) disabled false → true transition: nothing to do (consumer controls).
  //    disabled true → false transition: snap back to bottom if we're still
  //    considered at-bottom. The compute() check guards against the rare case
  //    where the user scrolled away while disabled — we don't want to yank
  //    them back when they re-enable auto-follow.
  useEffect(() => {
    if (opts.disabled) {
      wasDisabledRef.current = true;
      return;
    }
    if (!wasDisabledRef.current) return;
    wasDisabledRef.current = false;
    const el = node;
    if (!el) return;
    if (compute()) {
      el.scrollTop = el.scrollHeight;
    }
  }, [opts.disabled, node, compute]);

  const scrollToBottom = useCallback((behavior: 'auto' | 'smooth' = 'smooth') => {
    const el = node;
    if (!el) return;
    el.scrollTo({ top: el.scrollHeight, behavior });
    // The `scroll` listener will set isAtBottom=true and clear pendingCount.
  }, [node]);

  // The callback ref must be stable so consumers can pass it inline. setNode
  // is already referentially stable across renders.
  const ref = useCallback((el: T | null) => {
    setNode(el);
  }, []);

  return { ref, isAtBottom, pendingCount: pendingCountRef.current, scrollToBottom };
}
