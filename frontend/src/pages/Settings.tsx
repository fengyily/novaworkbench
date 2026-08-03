import { NavLink, Outlet } from 'react-router-dom';
import './Settings.css';

export default function Settings() {
  return (
    <div className="settings-page">
      <h2 className="page-title">⚙️ 设置</h2>

      <nav className="settings-tabs">
        <NavLink to="/settings" end className="settings-tab">平台 Token</NavLink>
        <NavLink to="/settings/roles" className="settings-tab">角色管理</NavLink>
        <NavLink to="/settings/claude" className="settings-tab">Claude 配置</NavLink>
        <NavLink to="/settings/llm" className="settings-tab">直连 LLM</NavLink>
      </nav>

      <div className="settings-content">
        <Outlet />
      </div>
    </div>
  );
}
