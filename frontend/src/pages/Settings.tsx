import { NavLink, Outlet } from 'react-router-dom';
import { useAuth } from '../utils/auth';
import './Settings.css';

// settingsTab = a settings sub-route. .permission is the RBAC key that grants
// visibility; tabs are dropped when the current user lacks the key.
const settingsTabs = [
  { to: '/settings/users', label: '用户管理', permission: 'setting.users' },
  { to: '/settings/acl', label: '角色权限', permission: 'setting.acl' },
  { to: '/settings', label: '平台 Token', permission: 'setting.tokens', end: true },
  { to: '/settings/roles', label: 'AI 角色', permission: 'setting.roles_ai' },
  { to: '/settings/claude', label: 'Claude 配置', permission: 'setting.claude' },
  { to: '/settings/llm', label: '直连 LLM', permission: 'setting.llm' },
  { to: '/settings/database', label: '数据库', permission: 'setting.database' },
];

export default function Settings() {
  const { hasPermission } = useAuth();
  const visibleTabs = settingsTabs.filter((t) => hasPermission(t.permission));

  return (
    <div className="settings-page">
      <h2 className="page-title">⚙️ 设置</h2>

      <nav className="settings-tabs">
        {visibleTabs.map((t) => (
          <NavLink
            key={t.to}
            to={t.to}
            end={t.end}
            className="settings-tab"
          >
            {t.label}
          </NavLink>
        ))}
      </nav>

      <div className="settings-content">
        <Outlet />
      </div>
    </div>
  );
}
