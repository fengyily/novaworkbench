import { createContext, useContext, useEffect, useState, type ReactNode } from 'react';
import { authApi, clearToken, setToken, hasPermission, type User } from '../api/client';
import { applyUserLocale } from '../i18n';
import { writeStoredLang } from './lang';

interface AuthState {
  user: User | null;
  permissions: string[];
  loading: boolean; // initial /me restore in flight
}

interface AuthContextValue extends AuthState {
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  hasPermission: (key: string) => boolean;
  // updateUserLocale persists the signed-in user's UI language on the
  // server (users.locale) and keeps the auth context's user copy in sync.
  // LanguageSwitcher calls it after the optimistic i18next change.
  updateUserLocale: (locale: string) => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | undefined>(undefined);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>({
    user: null,
    permissions: [],
    loading: true,
  });

  // Restore the session on first load if a token is present. A missing/expired
  // token (401) is handled by the request wrapper — it clears the token and
  // redirects to /login, so here we just mark loading done on any outcome.
  useEffect(() => {
    let cancelled = false;
    const token = localStorage.getItem('nova_token');
    if (!token) {
      setState({ user: null, permissions: [], loading: false });
      return;
    }
    authApi
      .me()
      .then((prof) => {
        if (cancelled) return;
        // The server-side preference wins once we know who is signed in —
        // a shared browser must not pin the UI to the last anonymous choice.
        applyUserLocale(prof.user.locale);
        setState({ user: prof.user, permissions: prof.permissions, loading: false });
      })
      .catch(() => {
        if (cancelled) return;
        clearToken();
        setState({ user: null, permissions: [], loading: false });
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const login = async (username: string, password: string) => {
    const prof = await authApi.login(username, password);
    setToken(prof.token);
    // Same precedence rule as the restore path above: the account's stored
    // locale overrides whatever the browser had. Mirrored into localStorage
    // so a future anonymous session on this browser starts in the same
    // language instead of snapping back.
    applyUserLocale(prof.user.locale);
    if (prof.user.locale) writeStoredLang(prof.user.locale);
    setState({ user: prof.user, permissions: prof.permissions, loading: false });
  };

  const logout = async () => {
    try {
      await authApi.logout();
    } catch {
      // ignore — token is cleared locally regardless
    }
    clearToken();
    setState({ user: null, permissions: [], loading: false });
  };

  const updateUserLocale = async (locale: string) => {
    const user = await authApi.setLocale(locale);
    setState((s) => ({ ...s, user }));
  };

  const value: AuthContextValue = {
    ...state,
    login,
    logout,
    hasPermission: (key: string) => hasPermission(state.permissions, key),
    updateUserLocale,
  };

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error('useAuth must be used within AuthProvider');
  return ctx;
}
