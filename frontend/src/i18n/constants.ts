// i18n language constants — the single source of truth for which UI
// languages the app ships translations for, and how the browser-level
// preference is stored.
//
// Language data flow (see i18n/index.ts + utils/lang.ts):
//   1. First paint reads localStorage `nova_lang` (validated against
//      SUPPORTED_LANGS), falling back to navigator.language. Initialization
//      is synchronous so there is never a flash of the wrong language.
//   2. Once the session is restored, the server-side `users.locale` wins
//      (per-user preference) — utils/auth.tsx applies it via applyUserLocale.
//   3. Switching writes localStorage + (when logged in) PUT /api/auth/locale.
//   4. Other tabs follow through the `storage` event.

export const DEFAULT_LANG = 'zh-CN' as const;

export const LANG_STORAGE_KEY = 'nova_lang';

export interface LangOption {
  code: string;
  /** Native label shown in the switcher (never translated). */
  nativeLabel: string;
  /** English label, for switchers rendered in the other language. */
  enLabel: string;
}

export const SUPPORTED_LANGS: readonly LangOption[] = [
  { code: 'zh-CN', nativeLabel: '中文', enLabel: 'Chinese' },
  { code: 'en-US', nativeLabel: 'English', enLabel: 'English' },
] as const;

// Lang is a plain string (not a union) so persisted/unknown values can flow
// through normalizeLang without casts; callers only ever receive whitelist
// members back from it.
export type Lang = string;

// normalizeLang maps an arbitrary stored/browser value onto the whitelist.
// Accepts exact codes ("zh-CN"), case-insensitive matches and bare prefixes
// ("zh", "en"). Returns null for anything unsupported so the caller can fall
// back to DEFAULT_LANG.
export function normalizeLang(value: string | null | undefined): Lang | null {
  if (!value) return null;
  const v = value.trim().toLowerCase();
  if (!v) return null;
  const exact = SUPPORTED_LANGS.find((l) => l.code.toLowerCase() === v);
  if (exact) return exact.code;
  // "zh-TW" / "zh-Hans" → zh-CN, "en-GB" → en-US. We only ship one variant
  // per language family; an unsupported regional variant must never leave
  // the app untranslated.
  const byPrefix = SUPPORTED_LANGS.find((l) => v.startsWith(l.code.split('-')[0]));
  return byPrefix ? byPrefix.code : null;
}

// isSupportedLang narrows a raw string for consumers that want a boolean check
// (e.g. the language switcher marking the active option).
export function isSupportedLang(value: string | null | undefined): boolean {
  return normalizeLang(value) !== null;
}
