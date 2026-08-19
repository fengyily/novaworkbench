import { NavLink, Outlet } from 'react-router-dom';
import { useAuth } from '../utils/auth';
import './Layout.css';

// navItem.to is the route; .permission is the RBAC permission key that grants
// visibility. Items are dropped when the current user lacks the key (admins
// get "*", so they see everything).
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

  return (
    <div className="app-layout">
      <header className="app-header">
        <span className="app-logo">🔷 NovaWorkbench</span>
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
        <nav className="app-sidebar">
          {visibleItems.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) => `nav-item ${isActive ? 'active' : ''}`}
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
