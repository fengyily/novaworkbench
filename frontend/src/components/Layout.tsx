import { useState } from 'react';
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

  const closeSidebar = () => setSidebarOpen(false);

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
