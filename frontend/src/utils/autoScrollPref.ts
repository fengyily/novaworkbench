// Global "follow the stream" preference, shared by every SSE panel and
// persisted so it survives reloads. Module-level state + useSyncExternalStore
// keeps all panels in sync without introducing a store library.
//
// Default: true — matches the historical "always follow" behavior so existing
// users aren't surprised. Persistence uses localStorage (the
// per-device/per-screen-size preference), NOT the server-side `settings` KV
// (which is for cross-device business configuration).

import { useSyncExternalStore } from 'react';

const STORAGE_KEY = 'nova_auto_scroll';

// Module-level cache so storage writes don't trigger spurious re-renders in
// the same tab that performed the write (the `storage` event only fires for
// writes from a different tab anyway).
let currentValue: boolean = readInitial();
const listeners = new Set<() => void>();

function emit(): void {
  for (const l of listeners) l();
}

function readInitial(): boolean {
  if (typeof localStorage === 'undefined') return true;
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (raw === null) return true;
    return raw !== '0' && raw !== 'false';
  } catch {
    return true;
  }
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  // Cross-tab sync: when another tab flips the toggle, follow it.
  const onStorage = (e: StorageEvent) => {
    if (e.key !== STORAGE_KEY) return;
    const next = e.newValue;
    const parsed = next === null ? true : next !== '0' && next !== 'false';
    if (parsed !== currentValue) {
      currentValue = parsed;
      emit();
    }
  };
  if (typeof window !== 'undefined') {
    window.addEventListener('storage', onStorage);
    return () => {
      listeners.delete(listener);
      window.removeEventListener('storage', onStorage);
    };
  }
  return () => {
    listeners.delete(listener);
  };
}

function getSnapshot(): boolean {
  return currentValue;
}

function getServerSnapshot(): boolean {
  // SSR safety — render-time defaults match the client initial value.
  return true;
}

export function useAutoScrollPref(): { autoScroll: boolean; toggle: () => void } {
  const autoScroll = useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
  const toggle = () => {
    const next = !currentValue;
    currentValue = next;
    try {
      if (typeof localStorage !== 'undefined') {
        localStorage.setItem(STORAGE_KEY, next ? '1' : '0');
      }
    } catch {
      /* localStorage unavailable — in-memory state still wins for this tab */
    }
    emit();
  };
  return { autoScroll, toggle };
}
