import { useCallback, useEffect, useState } from 'react';
import { NavLink, Outlet } from 'react-router-dom';
import { useAuth } from '../utils/auth';
import './Layout.css';

const navItems = [
  { to: '/', label: '📊 仪表盘', end: true, permission: 'menu.dashboard' },
  { to: '/projects', label: '📁 项目', end: false, permission: 'menu.projects' },
  { to: '/knowledge', label: '🧠 知识库', end: false, permission: 'menu.knowledge' },
  { to: '/chat', label: '💬 AI对话', end: false, permission: 'menu.chat' },
  { to: '/reports', label: '📝 周报', end: false, permission: 'menu.reports' },
  { to: '/settings', label: '⚙️ 设置', end: false, permission: 'menu.settings' },
];

export default function Layout() {
  const { user, hasPermission, logout } = useAuth();
  const visibleItems = navItems.filter((item) => hasPermission(item.permission));
  const [sidebarOpen, setSidebarOpen] = useState(false);

  const closeSidebar = useCallback(() => setSidebarOpen(false), []);

  // ESC closes the mobile drawer (matches users' mental model from native
  // modals and saves them a tap on the small ✕ in the corner). The listener
  // is only attached while the drawer is open so we don't shadow other ESC
  // handlers (e.g. inline editing).
  useEffect(() => {
    if (!sidebarOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') closeSidebar();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [sidebarOpen, closeSidebar]);

  // Lock body scroll while the drawer overlay is up so the page underneath
  // doesn't scroll on touch. The single effect with cleanup handles both
  // unmount and toggle-to-closed — the else branch is redundant.
  useEffect(() => {
    if (!sidebarOpen) return;
    const body = document.body;
    // Only compensate on desktop where the scrollbar is visible.
    const sbw = window.innerWidth - document.documentElement.clientWidth;
    if (sbw > 0) body.style.setProperty('--scrollbar-comp', `${sbw}px`);
    body.classList.add('no-scroll');
    return () => {
      body.classList.remove('no-scroll');
      body.style.removeProperty('--scrollbar-comp');
    };
  }, [sidebarOpen]);

  return (
    <div className="app-layout">
      <header className="app-header">
        <div className="app-header-left">
          <button
            className="app-hamburger"
            aria-label="打开导航"
            onClick={() => setSidebarOpen((o) => !o)}
          >
            <span /><span /><span />
          </button>
          <span className="app-logo">🔷 NovaWorkbench</span>
        </div>
        <div className="app-header-right">
          {user && (
            <span className="app-header-user">
              {user.display_name || user.username}
              {user.is_admin && <span className="app-header-admin">管理员</span>}
            </span>
          )}
          {hasPermission('menu.settings') && (
            <a href="/settings" className="app-header-settings">⚙️ 设置</a>
          )}
          <button className="app-header-logout" onClick={() => logout()}>
            退出
          </button>
        </div>
      </header>
      <div className="app-body">
        {sidebarOpen && (
          <div className="sidebar-overlay" onClick={closeSidebar} aria-hidden="true" />
        )}
        <nav className={`app-sidebar${sidebarOpen ? ' sidebar-open' : ''}`}>
          <div className="sidebar-close-row">
            <span className="app-logo">🔷 NovaWorkbench</span>
            <button className="sidebar-close-btn" aria-label="关闭导航" onClick={closeSidebar}>✕</button>
          </div>
          {visibleItems.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) => `nav-item ${isActive ? 'active' : ''}`}
              onClick={closeSidebar}
            >
              {item.label}
            </NavLink>
          ))}
        </nav>
        <main className="app-main">
          <Outlet />
        </main>
      </div>
      <footer className="app-footer">
        🟢 服务运行中 | localhost:9527 | v0.1.0
      </footer>
    </div>
  );
}
