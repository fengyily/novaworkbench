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
// The cache is deliberately NOT refreshed on a timer: the settings page is
// where a change is made, and a page reload after saving picks up the new
// value. A long-lived tab keeps the values it loaded with, which is the
// same contract the other settings-backed pickers (model / claude config)
// have.
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
 * Replaces the cached defaults with a freshly-saved config, so the pickers
 * elsewhere on the page update without a reload.
 */
export function setEffortDefaults(cfg: EffortConfig): void {
  cached = {
    enabled: cfg.enabled,
    design: normalizeEffortTier(cfg.design) || FALLBACK.design,
    coding: normalizeEffortTier(cfg.coding) || FALLBACK.coding,
    commit: normalizeEffortTier(cfg.commit) || FALLBACK.commit,
    other: normalizeEffortTier(cfg.other) || FALLBACK.other,
  };
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
    return () => {
      cancelled = true;
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
