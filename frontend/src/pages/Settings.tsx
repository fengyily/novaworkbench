import { NavLink, Outlet } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useAuth } from '../utils/auth';
import './Settings.css';

// settingsTab = a settings sub-route. .permission is the RBAC key that grants
// visibility; tabs are dropped when the current user lacks the key.
// labelKey resolves at render time so the tab rail follows language switches.
const settingsTabs = [
  { to: '/settings/users', labelKey: 'settings.tabs.users', permission: 'setting.users' },
  { to: '/settings/acl', labelKey: 'settings.tabs.acl', permission: 'setting.acl' },
  { to: '/settings', labelKey: 'settings.tabs.tokens', permission: 'setting.tokens', end: true },
  { to: '/settings/agent-servers', labelKey: 'settings.tabs.agentServers', permission: 'setting.tokens' },
  { to: '/settings/roles', labelKey: 'settings.tabs.rolesAI', permission: 'setting.roles_ai' },
  { to: '/settings/skills', labelKey: 'settings.tabs.skills', permission: 'setting.roles_ai' },
  { to: '/settings/claude', labelKey: 'settings.tabs.claude', permission: 'setting.claude' },
  { to: '/settings/llm', labelKey: 'settings.tabs.llm', permission: 'setting.llm' },
  { to: '/settings/database', labelKey: 'settings.tabs.database', permission: 'setting.database' },
  { to: '/settings/preflight', labelKey: 'settings.tabs.preflight', permission: 'setting.preflight' },
];

export default function Settings() {
  const { t } = useTranslation();
  const { hasPermission } = useAuth();
  const visibleTabs = settingsTabs.filter((tab) => hasPermission(tab.permission));

  return (
    <div className="settings-page">
      <h2 className="page-title">{t('settings.title')}</h2>

      <nav className="settings-tabs">
        {visibleTabs.map((tab) => (
          <NavLink
            key={tab.to}
            to={tab.to}
            end={tab.end}
            className="settings-tab"
          >
            {t(tab.labelKey)}
          </NavLink>
        ))}
      </nav>

      <div className="settings-content">
        <Outlet />
      </div>
    </div>
  );
}
