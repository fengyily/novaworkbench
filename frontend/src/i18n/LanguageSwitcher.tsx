// LanguageSwitcher — the UI language picker. Two placements:
//   1. the app header (Layout.tsx), compact pill form;
//   2. the login page, where no header exists yet.
//
// Switching is optimistic: the language changes instantly (i18next +
// localStorage) and the per-user preference is persisted in the background
// when a session exists. If the PUT fails the UI keeps the chosen language —
// a broken network must not break the switch — and the next successful
// change overwrites the stored preference.
import { useCallback, useId } from 'react';
import { useTranslation } from 'react-i18next';
import { DEFAULT_LANG, SUPPORTED_LANGS, type Lang } from './constants';
import { writeStoredLang } from '../utils/lang';
import { useAuth } from '../utils/auth';
import './LanguageSwitcher.css';

export default function LanguageSwitcher({ variant = 'header' }: { variant?: 'header' | 'block' }) {
  const { t, i18n } = useTranslation();
  const { user, updateUserLocale } = useAuth();
  const selectId = useId();
  const current = i18n.language || DEFAULT_LANG;

  const onChange = useCallback(
    (next: Lang) => {
      if (!next || next === i18n.language) return;
      void i18n.changeLanguage(next);
      writeStoredLang(next);
      if (user) {
        void updateUserLocale(next).catch(() => {
          // Best-effort persistence — see the component docstring.
        });
      }
    },
    [i18n, user, updateUserLocale],
  );

  return (
    <span className={`lang-switcher lang-switcher--${variant}`}>
      <label className="lang-switcher-label" htmlFor={selectId}>
        {variant === 'block' && <span className="lang-switcher-text">{t('common.language')}</span>}
        <select
          id={selectId}
          className="lang-switcher-select"
          value={current}
          onChange={(e) => onChange(e.target.value)}
          aria-label={t('common.language')}
        >
          {SUPPORTED_LANGS.map((l) => (
            <option key={l.code} value={l.code}>
              {l.nativeLabel}
            </option>
          ))}
        </select>
      </label>
    </span>
  );
}
