// Locale-aware date/number formatting, collapsed into one place so switching
// the UI language also switches every rendered timestamp.
//
// Rules of the road:
//  - NEVER call `Date.prototype.toLocale*` / `Number.prototype.toLocaleString`
//    directly from a component — they default to the *browser* locale, not
//    the app language, and would go stale after a switch. Use these helpers.
//  - They are plain functions (no hooks) so they work inside render and
//    inside non-React helpers alike; they read i18next.language at call time.
import i18next from 'i18next';
import { DEFAULT_LANG } from '../i18n/constants';

type DateLike = string | number | Date | null | undefined;

// intlLocale returns the BCP-47 tag Intl should format with — the *app*
// language, falling back to the default when i18next has not initialized
// yet (pure-utility call sites during module evaluation).
export function intlLocale(): string {
  return i18next.language || DEFAULT_LANG;
}

function toDate(v: DateLike): Date | null {
  if (v === null || v === undefined || v === '') return null;
  const d = v instanceof Date ? v : new Date(v);
  return Number.isNaN(d.getTime()) ? null : d;
}

const dateOpts: Intl.DateTimeFormatOptions = { year: 'numeric', month: '2-digit', day: '2-digit' };
const timeOpts: Intl.DateTimeFormatOptions = { hour: '2-digit', minute: '2-digit', hour12: false };

// fmtDate — "2026/09/08" (zh) / "09/08/2026" (en). Invalid/empty input
// renders as-is (or an empty string) so unfinished rows don't show "Invalid
// Date".
export function fmtDate(v: DateLike): string {
  const d = toDate(v);
  if (!d) return v === null || v === undefined || v === '' ? '' : String(v);
  return new Intl.DateTimeFormat(intlLocale(), dateOpts).format(d);
}

// fmtTime — "14:05" (24h in both shipped locales).
export function fmtTime(v: DateLike): string {
  const d = toDate(v);
  if (!d) return v === null || v === undefined || v === '' ? '' : String(v);
  return new Intl.DateTimeFormat(intlLocale(), timeOpts).format(d);
}

// fmtDateTime — "2026/09/08 14:05".
export function fmtDateTime(v: DateLike): string {
  const d = toDate(v);
  if (!d) return v === null || v === undefined || v === '' ? '' : String(v);
  const dtf = new Intl.DateTimeFormat(intlLocale(), { ...dateOpts, ...timeOpts });
  return dtf
    .formatToParts(d)
    .map((p) => p.value)
    .join('')
    .replace(/^,\s*/, '')
    .replace(/\s*,\s*/, ' ');
}

// fmtNumber — thousands separators following the app language.
export function fmtNumber(n: number): string {
  return new Intl.NumberFormat(intlLocale()).format(n);
}

// fmtRelative — "just now / N minutes ago / …". Replaces the old
// Chinese-only utils/time.ts relativeTime (which SchedulesPage and
// RequirementsList shared). Uses i18next keys (time.*) so the phrasing is
// translatable; the number is interpolated by us, not by i18next, because
// this runs outside React.
export function fmtRelative(v: DateLike): string {
  const d = toDate(v);
  if (!d) return v === null || v === undefined || v === '' ? '' : String(v);
  const t = d.getTime();
  const diff = Math.max(0, Date.now() - t);
  const min = Math.floor(diff / 60_000);
  const tt = i18next.t.bind(i18next) as (key: string, opts?: Record<string, unknown>) => string;
  if (min < 1) return tt('time.justNow');
  if (min < 60) return tt('time.minutesAgo', { n: min });
  const hr = Math.floor(min / 60);
  if (hr < 24) return tt('time.hoursAgo', { n: hr });
  const day = Math.floor(hr / 24);
  if (day < 30) return tt('time.daysAgo', { n: day });
  const mo = Math.floor(day / 30);
  if (mo < 12) return tt('time.monthsAgo', { n: mo });
  const yr = Math.floor(mo / 12);
  return tt('time.yearsAgo', { n: yr });
}
