import { NavLink, Outlet } from 'react-router-dom';
import './Layout.css';

const navItems = [
  { to: '/', label: '📊 仪表盘', end: true },
  { to: '/projects', label: '📁 项目', end: false },
  { to: '/knowledge', label: '🧠 知识库', end: false },
  { to: '/chat', label: '💬 AI对话', end: false },
  { to: '/reports', label: '📝 周报', end: false },
  { to: '/settings', label: '⚙️ 设置', end: false },
];

export default function Layout() {
  return (
    <div className="app-layout">
      <header className="app-header">
        <span className="app-logo">🔷 NovaWorkbench</span>
        <a href="/settings" className="app-header-settings">⚙️ 设置</a>
      </header>
      <div className="app-body">
        <nav className="app-sidebar">
          {navItems.map(item => (
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
