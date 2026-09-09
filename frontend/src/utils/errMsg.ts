// errorMessage — user-facing rendering of a thrown error.
//
// The backend envelope carries a stable uppercase `code`; codes listed in the
// i18n `errors` module are translated, everything else falls back to the raw
// server message (which may be Chinese — accepted for this iteration: a toast
// is a snapshot of the moment it happened and is never re-translated on
// switch).
//
// Plain (non-Api) errors keep their message; the two common browser network
// failures are mapped onto the NETWORK key so a dead backend doesn't show raw
// Chrome/WebKit internals.
import i18next from 'i18next';
import { ApiError } from '../api/client';

// loose mirrors i18n/label.ts: dynamic keys can't be proven against the
// strict key union, and defaultValue keeps a miss non-fatal.
function t(key: string, opts?: Record<string, unknown>): string {
  return (i18next.t as unknown as (k: string, o?: Record<string, unknown>) => string)(key, opts);
}

export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    const translated = t(`errors.${err.code}`, { defaultValue: '' });
    if (translated) return translated;
    return err.detail || err.message;
  }
  if (err instanceof Error) {
    if (/failed to fetch|networkerror|load failed|network request failed/i.test(err.message)) {
      const translated = t('errors.NETWORK', { defaultValue: '' });
      if (translated) return translated;
    }
    return err.message;
  }
  return String(err);
}
