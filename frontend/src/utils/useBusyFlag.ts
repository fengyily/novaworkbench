// useBusyFlag — synchronous guard against re-entrant async handlers.
//
// Why a useRef instead of only useState:
//   React 18+ batches setState and defers commits. Between
//   `setLoading(true)` and the next paint, additional clicks can still
//   invoke the handler with `loading === false` in the closure. A
//   useRef flips synchronously, so the second invocation sees the
//   in-flight flag immediately.
//
// Usage (one-line):
//   const { busy, withBusy } = useBusyFlag();
//   const handleSubmit = () => withBusy(async () => {
//     if (!remoteUrl) return;
//     await api.submit(...);
//   });
//   <button disabled={busy || !remoteUrl}>...</button>
//
// Usage (manual control for state-machine components):
//   const { busy, guard, release } = useBusyFlag();
//   const handleX = async () => {
//     if (!guard()) return;
//     setPhase('running');
//     try { ... } finally { release(); }
//   };

import { useCallback, useRef, useState } from 'react';

export interface BusyFlag {
  busy: boolean;
  withBusy: <T>(fn: () => Promise<T>) => Promise<T | undefined>;
  guard: () => boolean;
  release: () => void;
}

export function useBusyFlag(): BusyFlag {
  const ref = useRef(false);
  const [busy, setBusy] = useState(false);

  const guard = useCallback(() => {
    if (ref.current) return false;
    ref.current = true;
    setBusy(true);
    return true;
  }, []);

  const release = useCallback(() => {
    ref.current = false;
    setBusy(false);
  }, []);

  const withBusy = useCallback(
    async <T>(fn: () => Promise<T>): Promise<T | undefined> => {
      if (!guard()) return undefined;
      try {
        return await fn();
      } finally {
        release();
      }
    },
    [guard, release],
  );

  return { busy, withBusy, guard, release };
}