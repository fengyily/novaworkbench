// Browser-level language preference storage (localStorage `nova_lang`).
//
// Kept separate from i18n/constants.ts so the constants module stays free of
// window access and can be imported from tests/tooling. The server-side
// per-user preference (users.locale via PUT /api/auth/locale) takes priority
// once signed in; this storage is the pre-login / anonymous-device layer.
import { DEFAULT_LANG, LANG_STORAGE_KEY, normalizeLang, type Lang } from '../i18n/constants';

// readStoredLang resolves the initial language synchronously: explicit user
// choice → browser language → default. Called during i18n init before the
// first render, so it must never throw (localStorage can be unavailable in
// private-mode edge cases).
export function readStoredLang(): Lang {
  try {
    const fromStorage = normalizeLang(localStorage.getItem(LANG_STORAGE_KEY));
    if (fromStorage) return fromStorage;
  } catch {
    // ignore — fall through to navigator detection
  }
  const navLangs =
    typeof navigator !== 'undefined' && navigator.languages && navigator.languages.length > 0
      ? navigator.languages
      : typeof navigator !== 'undefined'
        ? [navigator.language]
        : [];
  for (const candidate of navLangs) {
    const lng = normalizeLang(candidate);
    if (lng) return lng;
  }
  return DEFAULT_LANG;
}

export function writeStoredLang(lang: Lang): void {
  try {
    localStorage.setItem(LANG_STORAGE_KEY, lang);
  } catch {
    // ignore — persistence is best-effort
  }
}

export function clearStoredLang(): void {
  try {
    localStorage.removeItem(LANG_STORAGE_KEY);
  } catch {
    // ignore
  }
}

// watchExternalLangChanges subscribes to `storage` events so other tabs that
// switch the language are followed immediately. The event only fires for
// writes from a different tab, so a tab never reacts to its own change.
export function watchExternalLangChanges(onChange: (raw: string | null) => void): () => void {
  if (typeof window === 'undefined') return () => {};
  const handler = (e: StorageEvent) => {
    if (e.key !== LANG_STORAGE_KEY) return;
    onChange(e.newValue);
  };
  window.addEventListener('storage', handler);
  return () => window.removeEventListener('storage', handler);
}
