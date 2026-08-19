import { useState } from 'react';
import { useNavigate, useLocation, Navigate } from 'react-router-dom';
import { useAuth } from '../utils/auth';
import './Login.css';

export default function Login() {
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
      setError(err instanceof Error ? err.message : '登录失败');
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="login-page">
      <form className="login-card" onSubmit={onSubmit}>
        <div className="login-logo">🔷 NovaWorkbench</div>
        <h1 className="login-title">登录</h1>
        <p className="login-subtitle">用户角色权限体系已启用，请使用账号登录。</p>

        <label className="login-field">
          <span>用户名</span>
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
          <span>密码</span>
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
          />
        </label>

        {error && <div className="login-error">{error}</div>}

        <button type="submit" className="login-submit" disabled={submitting || !username || !password}>
          {submitting ? '登录中…' : '登录'}
        </button>

        <p className="login-hint">
          首次启动时管理员账号由系统自动创建，密码打印在后端启动日志中（<code>[acl] default admin account</code>）。
        </p>
      </form>
    </div>
  );
}
