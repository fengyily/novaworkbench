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

/**
 * The WIRE form of a reasoning-tier pick.
 *
 * It returns an empty object when the user has NOT picked a tier, so the
 * field is left off the request entirely.
 *
 * WHY THE FIELD IS OMITTED RATHER THAN FILLED WITH THE DISPLAYED DEFAULT:
 * the backend owns the "configured stage default" layer of the resolution
 * chain (llm.Gateway.ResolveEffort: explicit pick > configured stage
 * default > built-in default). Sending the frontend's copy of that default
 * would arrive as an EXPLICIT pick and take priority over the configured
 * value — so the two implementations of the same default would become
 * rival sources of truth.
 *
 * That is not a theoretical concern. The frontend's copy can be degraded
 * (a failed GET /api/settings/effort falls back to the shipped values) or
 * stale (a tab that has not yet received the config change — the
 * cross-tab broadcast does not reach a browser without BroadcastChannel,
 * and the first fetch may not have resolved yet). Either way the backend
 * can read the real setting — it is the authority — and a frontend guess
 * must not outrank it.
 *
 * The DISPLAY value (`pick ?? configuredDefault`) is unaffected: the bar
 * still shows which tier the run will use. Only the wire value changes.
 */
export function effortPick(pick: EffortTier | null | undefined): { effort?: string };
export function effortPick(
  pick: EffortTier | null | undefined,
  key: 'design_effort',
): { design_effort?: string };
export function effortPick(
  pick: EffortTier | null | undefined,
  key: 'coding_effort',
): { coding_effort?: string };
export function effortPick(
  pick: EffortTier | null | undefined,
  key: 'effort' | 'design_effort' | 'coding_effort' = 'effort',
): { effort?: string; design_effort?: string; coding_effort?: string } {
  return pick ? ({ [key]: pick } as { effort?: string; design_effort?: string; coding_effort?: string }) : {};
}
