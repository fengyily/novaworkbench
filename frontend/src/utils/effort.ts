// The reasoning-effort (claude CLI --effort) vocabulary.
//
// Split out of EffortBar.tsx so the component file exports only a
// component: the tier constants and the normalisation helpers are pure
// data, and the `react(only-export-components)` lint rule (Fast Refresh
// only works when a file exports components alone) would otherwise fire on
// a file that mixes the two.
//
// The tier NAMES are the CLI's own literals — "low" / "medium" / "high" /
// "xhigh" / "max" — and are deliberately NOT translated: they are the
// values sent to `--effort`, and a translated label would hide what the
// backend actually receives.

/** The CLI's accepted --effort tiers, in ascending order of depth. */
export const EFFORT_TIERS = ['low', 'medium', 'high', 'xhigh', 'max'] as const;

export type EffortTier = (typeof EFFORT_TIERS)[number];

/** The pipeline stages a tier can be the default for. */
export type EffortStage = 'design' | 'coding' | 'commit' | 'other';

/**
 * The tier each stage falls back to when the user hasn't picked one.
 * Mirrors the backend's llm.DefaultEffortForStage — the two must agree,
 * because this value is what the UI shows before the settings page has
 * been read.
 */
export const EFFORT_STAGE_DEFAULTS: Record<EffortStage, EffortTier> = {
  design: 'high',
  coding: 'medium',
  commit: 'low',
  other: 'high',
};

/**
 * Narrows an arbitrary string to a known tier, or returns "" when it isn't
 * one.
 *
 * The whitelist is CASE-SENSITIVE, matching the backend's
 * llm.NormalizeEffort exactly: the CLI's value domain is all-lowercase
 * literals, so "HIGH" is an unknown tier rather than something to quietly
 * rewrite into "high". Two implementations of the same whitelist that
 * disagree would be a silent cross-boundary divergence.
 */
export function normalizeEffortTier(value: string | undefined | null): EffortTier | '' {
  const v = (value ?? '').trim();
  return (EFFORT_TIERS as readonly string[]).includes(v) ? (v as EffortTier) : '';
}

/**
 * Renders a tier string for display: the canonical tier when it is one we
 * know, otherwise the raw string (so an unexpected value from the backend
 * is visible rather than silently swallowed).
 */
export function effortTierLabel(tier: string): string {
  return tier || '';
}
