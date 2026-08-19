import { createContext, useContext, useEffect, useState, type ReactNode } from 'react';
import { authApi, clearToken, setToken, hasPermission, type User } from '../api/client';

interface AuthState {
  user: User | null;
  permissions: string[];
  loading: boolean; // initial /me restore in flight
}

interface AuthContextValue extends AuthState {
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  hasPermission: (key: string) => boolean;
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

  const value: AuthContextValue = {
    ...state,
    login,
    logout,
    hasPermission: (key: string) => hasPermission(state.permissions, key),
  };

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error('useAuth must be used within AuthProvider');
  return ctx;
}
