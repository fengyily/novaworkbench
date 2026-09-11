import { useState } from 'react';
import { useNavigate, useLocation, Navigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useAuth } from '../utils/auth';
import { errorMessage } from '../utils/errMsg';
import LanguageSwitcher from '../i18n/LanguageSwitcher';
import './Login.css';

export default function Login() {
  const { t } = useTranslation();
  const { login, user } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [submitting, setSubmitting] = useState(false);

  // If a session is restored (user navigates to /login while already authed),
  // bounce to the page the auth guard originally intercepted.
  const from = (location.state as { from?: { pathname?: string } } | null)?.from?.pathname || '/';
  if (user) {
    return <Navigate to={from} replace />;
  }

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');
    setSubmitting(true);
    try {
      await login(username.trim(), password);
      // login() updates the auth state; navigate to the intercepted page
      // (defaults to the dashboard).
      navigate(from, { replace: true });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="login-page">
      <form className="login-card" onSubmit={onSubmit}>
        <div className="login-logo">🔷 NovaWorkbench</div>
        <h1 className="login-title">{t('login.title')}</h1>
        <p className="login-subtitle">{t('login.subtitle')}</p>

        <label className="login-field">
          <span>{t('login.username')}</span>
          <input
            type="text"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoFocus
            autoComplete="username"
            placeholder="admin"
          />
        </label>

        <label className="login-field">
          <span>{t('login.password')}</span>
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
          />
        </label>

        {error && <div className="login-error">{error}</div>}

        <button type="submit" className="login-submit" disabled={submitting || !username || !password}>
          {submitting ? t('login.submitting') : t('login.submit')}
        </button>

        <p className="login-hint">
          {t('login.hintPrefix')}<code>[acl] default admin account</code>{t('login.hintSuffix')}
        </p>

        {/* Anonymous surface: with no session the browser-level choice is the
            only preference there is, so the switcher here must NOT try to
            persist to /api/auth/locale (it won't — no user in context). */}
        <LanguageSwitcher variant="block" />
      </form>
    </div>
  );
}
