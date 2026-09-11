// i18n entry point. Imported for its side effects from main.tsx BEFORE App so
// every component (and every non-React helper that calls i18next.t()) sees an
// initialized instance.
//
// Resources are bundled synchronously (no backend round-trip) so the first
// paint is already in the right language — there is no suspense fallback and
// no flash of the wrong locale.
import i18next, { type i18n as I18n } from 'i18next';
import { initReactI18next } from 'react-i18next';
import { DEFAULT_LANG, normalizeLang } from './constants';
import { readStoredLang, watchExternalLangChanges } from '../utils/lang';
import zhCN from './locales/zh-CN';
import enUS from './locales/en-US';

// initI18n is idempotent: React 19 StrictMode double-invokes effects in dev,
// and HMR can re-run this module. Calling init twice logs a warning and
// re-applies resources, which is harmless, but we'd rather not.
let ready = false;

export function initI18n(): I18n {
  if (ready) return i18next;
  ready = true;

  void i18next.use(initReactI18next).init({
    resources: {
      'zh-CN': { translation: zhCN },
      'en-US': { translation: enUS },
    },
    lng: readStoredLang(),
    fallbackLng: DEFAULT_LANG,
    // The tree only interpolates from our own code, never from user input,
    // and escaping would double-escape Markdown rendered through
    // react-markdown.
    interpolation: { escapeValue: false },
    react: {
      // Suspense would blank the whole tree on the first render because the
      // resources resolve synchronously anyway; the useTranslation hook then
      // always has translations available.
      useSuspense: false,
    },
    returnNull: false,
  });

  // Keep <html lang> in sync so fonts, screen readers and the browser's
  // own translation prompts follow the UI language.
  const syncLangAttr = (lng: string) => {
    document.documentElement.lang = lng;
  };
  syncLangAttr(i18next.language || DEFAULT_LANG);
  i18next.on('languageChanged', syncLangAttr);

  // Other tabs: localStorage writes from a *different* tab fire `storage`.
  // Writing the same value from this tab never fires it here, so there is no
  // echo loop.
  watchExternalLangChanges((raw) => {
    const lng = normalizeLang(raw);
    if (lng && i18next.language !== lng) void i18next.changeLanguage(lng);
  });

  if (import.meta.env.DEV) assertSameKeys(zhCN, enUS, '');

  return i18next;
}

// assertSameKeys walks two resource trees and warns when their key sets
// diverge. zh-CN is canonical: an en-US-only key is dead weight, a missing
// en-US key renders the Chinese fallback (better than nothing, but a bug).
function assertSameKeys(a: unknown, b: unknown, path: string): void {
  if (typeof a !== 'object' || a === null || typeof b !== 'object' || b === null) {
    if (typeof a !== typeof b) {
      // eslint-disable-next-line no-console
      console.warn(`[i18n] type mismatch at "${path}"`);
    }
    return;
  }
  const ka = new Set(Object.keys(a as Record<string, unknown>));
  const kb = new Set(Object.keys(b as Record<string, unknown>));
  for (const k of ka) {
    if (!kb.has(k)) {
      // eslint-disable-next-line no-console
      console.warn(`[i18n] key missing in en-US: "${path}${k}"`);
      continue;
    }
    assertSameKeys(
      (a as Record<string, unknown>)[k],
      (b as Record<string, unknown>)[k],
      `${path}${k}.`,
    );
  }
  for (const k of kb) {
    if (!ka.has(k)) {
      // eslint-disable-next-line no-console
      console.warn(`[i18n] key missing in zh-CN: "${path}${k}"`);
    }
  }
}

// applyUserLocale applies the signed-in user's server-side preference.
// Called by utils/auth.tsx after login()/me(). An empty/unknown locale means
// "no explicit preference" and leaves the current (browser-level) language
// untouched.
export function applyUserLocale(locale: string | null | undefined): void {
  const lng = normalizeLang(locale);
  if (!lng) return;
  if (i18next.language === lng) return;
  void i18next.changeLanguage(lng);
}

export default i18next;

// Run the initializer as a module-level side effect so main.tsx's
// `import './i18n'` actually wires up i18next before any component renders.
// (Without this top-level call, the function below would only fire when
// something explicitly invoked initI18n() — which nothing does — and every
// t() call would fall back to returning its raw key string.) The `ready`
// guard above makes a duplicate invocation harmless.
initI18n();
