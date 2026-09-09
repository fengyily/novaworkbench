// Label resolution helpers for code→key maps.
//
// Module-level dictionaries (e.g. the status/step/kind maps exported from
// api/client.ts) must NOT hold translated strings — a value computed at
// module top-level would freeze in the language active at import time and
// never update when the user switches. They hold *keys* instead, and this
// helper resolves them during render.
import type { TFunction } from 'i18next';

// LooseT sidesteps i18next's strict key union: the maps hold dynamic strings
// resolved at runtime, which the compiler cannot prove are real keys. Every
// key we look up originates from a map we own, and `defaultValue` guarantees
// a sane fallback if a key ever drifts.
type LooseT = (key: string, opts?: Record<string, unknown>) => string;

// Accepts react-i18next's TFunction as well as the already-loosened `t` some
// call sites hold (dynamic-key heavy components).
export function looseT(t: TFunction | LooseT): LooseT {
  return t as LooseT;
}

// tLabel renders the display label for a code through a code→key map,
// falling back to the raw code when unmapped — the same semantics the old
// inline `statusLabels[req.status] || req.status` lookups had. An empty code
// renders as an empty string so optional fields stay blank instead of
// showing "undefined".
export function tLabel(
  t: TFunction | LooseT,
  map: Record<string, string>,
  code: string | null | undefined,
): string {
  if (code === null || code === undefined || code === '') return '';
  const key = map[code];
  if (!key) return code;
  return looseT(t)(key, { defaultValue: code });
}
