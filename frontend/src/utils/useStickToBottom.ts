// Conditional "stick to bottom" behavior for SSE stream panels.
//
// Problem: naive `el.scrollTop = el.scrollHeight` on every dep change forces
// the viewport to bottom even when the user has scrolled up to read history,
// interrupting their reading.
//
// Solution: three-state model.
//
//   Following      — the viewport is at the bottom; new content scrolls down.
//   PausedByUser   — the user scrolled up; we stop following and count the
//                    missed lines so the panel can render a "↓ N new
//                    messages" jump button.
//   Frozen         — the user turned auto-scroll off explicitly; nothing
//                    moves, nothing is counted, no hint is shown.
//
// Implementation notes (the two easy-to-get-wrong bits):
//
// 1) "Am I at the bottom?" is answered by a SNAPSHOT written by the scroll
//    listener (`atBottomRef`), never by re-measuring `scrollHeight` inside
//    the deps effect. By the time the deps effect runs, the new content is
//    already in the DOM, so a fresh measurement would compare a stale
//    `scrollTop` against a grown `scrollHeight` and wrongly conclude the
//    user had scrolled away — the panel would stop following forever as
//    soon as it filled one screen.
//
// 2) The scroll listener is attached through a CALLBACK ref. The panels are
//    conditionally rendered (`codingLines.length > 0 || coding || ...`), so
//    on the first render `ref.current` is null and a plain `useEffect` +
//    `ref.current` would never attach the listener (its deps would not
//    change either). A callback ref attaches the listener the moment the
//    element actually mounts.
//
// Exports a callback ref + isAtBottom + pendingCount + autoScroll toggle +
// scrollToBottom; the consumer spreads the ref onto its scroll container and
// renders the returned state in its panel toolbar.

import {
  useCallback,
  useEffect,
  useRef,
  useState,
} from 'react';

export interface UseStickToBottomOptions {
  /** Distance from bottom (px) considered "still at bottom". Default 32. */
  threshold?: number;
  /** Deps that should trigger an auto-stick attempt. Default []. */
  deps?: ReadonlyArray<unknown>;
  /** Whether auto-follow starts enabled. Default true. */
  defaultAutoScroll?: boolean;
}

export interface StickToBottomHandle<T extends HTMLElement> {
  /** Callback ref — spread onto the scroll container. */
  ref: (node: T | null) => void;
  isAtBottom: boolean;
  pendingCount: number;
  autoScroll: boolean;
  setAutoScroll: (v: boolean) => void;
  toggleAutoScroll: () => void;
  scrollToBottom: (behavior?: ScrollBehavior) => void;
}

export function useStickToBottom<T extends HTMLElement = HTMLDivElement>(
  opts: UseStickToBottomOptions = {},
): StickToBottomHandle<T> {
  const threshold = opts.threshold ?? 32;

  const nodeRef = useRef<T | null>(null);
  const onScrollRef = useRef<(() => void) | null>(null);
  // Snapshot of "the viewport is at the bottom", written ONLY by the scroll
  // listener. See the implementation note above.
  const atBottomRef = useRef(true);
  const pendingRef = useRef(0);
  const autoScrollRef = useRef(opts.defaultAutoScroll ?? true);

  const [isAtBottom, setIsAtBottom] = useState(true);
  const [pendingCount, setPendingCount] = useState(0);
  const [autoScroll, setAutoScrollState] = useState(opts.defaultAutoScroll ?? true);

  const computeAtBottom = useCallback((el: T): boolean => {
    return el.scrollTop + el.clientHeight >= el.scrollHeight - threshold;
  }, [threshold]);

  // Callback ref: attach / detach the scroll listener whenever the element
  // actually mounts or unmounts.
  const ref = useCallback((node: T | null) => {
    const prev = nodeRef.current;
    if (prev && onScrollRef.current) {
      prev.removeEventListener('scroll', onScrollRef.current);
    }
    onScrollRef.current = null;
    nodeRef.current = node;

    if (!node) return;

    const onScroll = () => {
      const at = computeAtBottom(node);
      atBottomRef.current = at;
      setIsAtBottom((prevAt) => (prevAt === at ? prevAt : at));
      if (at && pendingRef.current !== 0) {
        pendingRef.current = 0;
        setPendingCount(0);
      }
    };
    onScrollRef.current = onScroll;
    node.addEventListener('scroll', onScroll, { passive: true });
    // Fresh mount: assume the user is at the bottom (the panel has just
    // opened) so the first content tick scrolls down as before.
    atBottomRef.current = true;
    setIsAtBottom(true);
    // Show the latest output right away — the panel may mount with a
    // restored history that already overflows, and waiting for the next
    // content tick would leave the user staring at the oldest lines.
    if (autoScrollRef.current) {
      node.scrollTop = node.scrollHeight;
    }
  }, [computeAtBottom]);

  // New content arrived. Follow only when auto-scroll is on AND the user
  // has not scrolled away.
  useEffect(() => {
    if (!autoScrollRef.current) return; // Frozen — no follow, no count.
    const el = nodeRef.current;
    if (!el) return;
    if (atBottomRef.current) {
      // Immediate assignment — no smooth scroll on auto-follow; SSE streams
      // tick many times per second and animation would queue up.
      el.scrollTop = el.scrollHeight;
    } else {
      pendingRef.current += 1;
      setPendingCount(pendingRef.current);
    }
    // We intentionally depend on the user-provided `deps` array verbatim.
    // The hook's other callbacks are stable per threshold.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, opts.deps ?? []);

  // Auto-scroll toggled. Turning it off freezes the panel (drop any pending
  // hint); turning it back on resumes following only if the viewport is
  // currently at the bottom.
  useEffect(() => {
    if (!autoScroll) {
      pendingRef.current = 0;
      setPendingCount(0);
      return;
    }
    const el = nodeRef.current;
    if (el && atBottomRef.current) {
      el.scrollTop = el.scrollHeight;
    }
  }, [autoScroll]);

  // The ref is updated synchronously in the setter (not only in the effect
  // above) so the "new content arrived" effect never runs with a stale
  // auto-scroll state when a toggle and a stream tick land in the same tick.
  const setAutoScroll = useCallback((v: boolean) => {
    autoScrollRef.current = v;
    setAutoScrollState(v);
  }, []);

  const toggleAutoScroll = useCallback(() => {
    setAutoScroll(!autoScrollRef.current);
  }, [setAutoScroll]);

  const scrollToBottom = useCallback((behavior: ScrollBehavior = 'auto') => {
    const el = nodeRef.current;
    if (!el) return;
    el.scrollTo({ top: el.scrollHeight, behavior });
    // Update the snapshot eagerly — do not wait for the scroll event to
    // come back around, otherwise a burst of new lines in the same tick
    // would be counted as "missed".
    atBottomRef.current = true;
    pendingRef.current = 0;
    setIsAtBottom(true);
    setPendingCount(0);
  }, []);

  return {
    ref,
    isAtBottom,
    pendingCount,
    autoScroll,
    setAutoScroll,
    toggleAutoScroll,
    scrollToBottom,
  };
}
