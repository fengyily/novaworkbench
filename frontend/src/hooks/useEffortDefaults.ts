import { useEffect, useState } from 'react';
import { settingsApi, type EffortConfig } from '../api/client';
import {
  EFFORT_STAGE_DEFAULTS,
  normalizeEffortTier,
  type EffortStage,
  type EffortTier,
} from '../utils/effort';

// useEffortDefaults — the per-stage reasoning-effort defaults, fetched once
// per page load and shared by every picker on the screen.
//
// WHY A MODULE-LEVEL CACHE: the defaults are needed by a dozen surfaces
// (design toolbar, coding modal, sub-task composer, review composer, split
// modal, schedule modal, issue monitor...). Each of them wants the initial
// value of its own tier picker, and each of them renders on a page that may
// mount several of them at once. Without a cache, that is a dozen parallel
// GET /api/settings/effort calls for one immutable-ish answer.
//
// The cache is NOT refreshed on a timer, but it IS refreshed when the
// settings page saves: the save is broadcast to the other open tabs so
// their pickers stop advertising a tier that is no longer the configured
// one. Without that, a tab opened before the change would keep showing
// the OLD tier while the backend — the authority — ran the NEW one, and
// the UI would be describing a run that is not the one about to happen.
//
// FAILURE IS SILENT AND HARMLESS: if the request fails (backend older than
// this frontend, network blip, auth expiry), we fall back to the shipped
// defaults. The picker then shows a tier that is very likely correct, and
// the backend — which is the authority anyway — resolves the real tier.
//
// The returned `enabled` flag mirrors the backend's master switch. When it
// is false the backend sends NO --effort at all, so the pickers are
// disabled — showing an active-looking control whose value has no effect
// would be a lie.

/** The shipped defaults, used before (and instead of) a successful fetch. */
const FALLBACK: EffortConfig = {
  enabled: true,
  design: EFFORT_STAGE_DEFAULTS.design,
  coding: EFFORT_STAGE_DEFAULTS.coding,
  commit: EFFORT_STAGE_DEFAULTS.commit,
  other: EFFORT_STAGE_DEFAULTS.other,
};

let cached: EffortConfig | null = null;
let inflight: Promise<EffortConfig> | null = null;

// ── Cross-tab freshness ─────────────────────────────────────────────────
//
// A save on the settings page has to reach the other open tabs, otherwise
// they keep showing the previous tier. A BroadcastChannel does it without
// a server round-trip and without persistence.
//
// The channel is created LAZILY: a module-scope `new BroadcastChannel()`
// would be constructed in every environment that imports this file, and in
// Node it would hold the event loop open. Browsers without the API (or a
// non-browser runtime) simply keep the stale display until a reload —
// which is the pre-existing behaviour, so the fallback is safe.
const CHANNEL_NAME = 'nova-effort-defaults';
let channel: BroadcastChannel | null = null;
let channelChecked = false;

function effortChannel(): BroadcastChannel | null {
  if (channelChecked) return channel;
  channelChecked = true;
  if (typeof BroadcastChannel === 'undefined') return null;
  try {
    channel = new BroadcastChannel(CHANNEL_NAME);
    channel.addEventListener('message', (ev) => {
      const cfg = ev.data as EffortConfig | undefined;
      if (!cfg) return;
      // setEffortDefaults updates the cache AND notifies the mounted
      // pickers, so one call covers both halves.
      setEffortDefaults(cfg);
    });
  } catch {
    channel = null;
  }
  return channel;
}

// The tabs currently showing a picker. Each one re-renders when the
// config changes (a save in another tab, or a save in this one).
const listeners = new Set<(cfg: EffortConfig) => void>();

/**
 * Announce a newly-saved config to the other open tabs, so their pickers
 * stop advertising the previous tier.
 */
export function broadcastEffortDefaults(cfg: EffortConfig): void {
  effortChannel()?.postMessage(cfg);
}

/**
 * Fetch the effort defaults, reusing the module-level cache. Concurrent
 * callers share one request.
 */
export function fetchEffortDefaults(): Promise<EffortConfig> {
  if (cached) return Promise.resolve(cached);
  if (inflight) return inflight;
  inflight = settingsApi
    .getEffort()
    .then((cfg) => {
      cached = {
        enabled: cfg?.enabled ?? FALLBACK.enabled,
        design: normalizeEffortTier(cfg?.design) || FALLBACK.design,
        coding: normalizeEffortTier(cfg?.coding) || FALLBACK.coding,
        commit: normalizeEffortTier(cfg?.commit) || FALLBACK.commit,
        other: normalizeEffortTier(cfg?.other) || FALLBACK.other,
      };
      return cached;
    })
    .catch(() => FALLBACK)
    .finally(() => {
      inflight = null;
    });
  return inflight;
}

/** Drop the cached defaults — called after the settings page saves. */
export function invalidateEffortDefaults(): void {
  cached = null;
}

/**
 * Replaces the cached defaults with a freshly-saved config.
 *
 * The mounted pickers are notified, not just the cache: a picker that is
 * already on screen would otherwise keep rendering the previous tier
 * while the backend — the authority — runs the new one.
 */
export function setEffortDefaults(cfg: EffortConfig): void {
  cached = {
    enabled: cfg.enabled,
    design: normalizeEffortTier(cfg.design) || FALLBACK.design,
    coding: normalizeEffortTier(cfg.coding) || FALLBACK.coding,
    commit: normalizeEffortTier(cfg.commit) || FALLBACK.commit,
    other: normalizeEffortTier(cfg.other) || FALLBACK.other,
  };
  for (const listener of listeners) listener(cached);
}

export interface EffortDefaults {
  /** The backend's master switch. False = no run sends --effort. */
  enabled: boolean;
  design: EffortTier;
  coding: EffortTier;
  commit: EffortTier;
  other: EffortTier;
  /** The default tier for a given pipeline stage. */
  forStage: (stage: EffortStage) => EffortTier;
}

/**
 * useEffortDefaults returns the configured per-stage tiers.
 *
 * The first render already carries the shipped defaults, so a picker never
 * flashes empty or renders an uncontrolled radio group; the fetched values
 * replace them once they arrive.
 */
export function useEffortDefaults(): EffortDefaults {
  const [cfg, setCfg] = useState<EffortConfig>(cached ?? FALLBACK);

  useEffect(() => {
    let cancelled = false;
    void fetchEffortDefaults().then((next) => {
      if (!cancelled) setCfg(next);
    });
    // Re-render when another tab saves a new config — otherwise this
    // picker would keep advertising the previous tier while the backend
    // (the authority) runs the new one.
    const listener = (next: EffortConfig) => {
      if (!cancelled) setCfg(next);
    };
    listeners.add(listener);
    effortChannel();
    return () => {
      cancelled = true;
      listeners.delete(listener);
    };
  }, []);

  const design = (normalizeEffortTier(cfg.design) || FALLBACK.design) as EffortTier;
  const coding = (normalizeEffortTier(cfg.coding) || FALLBACK.coding) as EffortTier;
  const commit = (normalizeEffortTier(cfg.commit) || FALLBACK.commit) as EffortTier;
  const other = (normalizeEffortTier(cfg.other) || FALLBACK.other) as EffortTier;

  return {
    enabled: cfg.enabled,
    design,
    coding,
    commit,
    other,
    forStage: (stage: EffortStage): EffortTier => {
      switch (stage) {
        case 'design':
          return design;
        case 'coding':
          return coding;
        case 'commit':
          return commit;
        default:
          return other;
      }
    },
  };
}
