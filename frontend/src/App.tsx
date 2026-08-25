import { BrowserRouter, Routes, Route, Navigate, useLocation } from 'react-router-dom';
import { AuthProvider, useAuth } from './utils/auth';
import Layout from './components/Layout';
import Login from './pages/Login';
import Dashboard from './pages/Dashboard';
import AddProject from './pages/AddProject';
import {
  ProjectsList, Chat, Reports,
} from './pages/PlaceholderPages';
import RequirementsList from './pages/RequirementsList';
import Settings from './pages/Settings';
import SettingsTokens from './pages/SettingsTokens';
import SettingsRoles from './pages/SettingsRoles';
import SettingsClaude from './pages/SettingsClaude';
import SettingsLLM from './pages/SettingsLLM';
import SettingsDatabase from './pages/SettingsDatabase';
import SettingsPreflight from './pages/SettingsPreflight';
import SettingsUsers from './pages/SettingsUsers';
import SettingsACLRoles from './pages/SettingsACLRoles';
import SettingsSkills from './pages/SettingsSkills';
import KnowledgePage from './pages/KnowledgePage';
import RequirementDetail from './pages/RequirementDetail';
import WizardPage from './pages/WizardPage';
import ProjectDetail from './pages/ProjectDetail';

// RequireAuth gates the authenticated app: while the session is being
// restored it shows a minimal loader; once restored with no user it bounces
// to /login. /login itself is mounted outside this guard.
function RequireAuth({ children }: { children: React.ReactNode }) {
  const { user, loading } = useAuth();
  const location = useLocation();
  if (loading) {
    return (
      <div style={{ minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center', color: '#64748B' }}>
        加载中…
      </div>
    );
  }
  if (!user) {
    return <Navigate to="/login" replace state={{ from: location }} />;
  }
  return <>{children}</>;
}

export default function App() {
  return (
    <AuthProvider>
      <BrowserRouter>
        <Routes>
          <Route path="/login" element={<Login />} />
          <Route element={<RequireAuth><Layout /></RequireAuth>}>
            <Route index element={<Dashboard />} />
            <Route path="wizard" element={<WizardPage />} />
            <Route path="projects/add" element={<AddProject />} />
            <Route path="projects" element={<ProjectsList />} />
            <Route path="projects/:id" element={<ProjectDetail />} />
            <Route path="requirements" element={<RequirementsList />} />
            <Route path="requirements/:id" element={<RequirementDetail />} />
            <Route path="knowledge" element={<KnowledgePage />} />
            <Route path="chat" element={<Chat />} />
            <Route path="reports" element={<Reports />} />
            <Route path="settings" element={<Settings />}>
              <Route index element={<SettingsTokens />} />
              <Route path="users" element={<SettingsUsers />} />
              <Route path="acl" element={<SettingsACLRoles />} />
              <Route path="roles" element={<SettingsRoles />} />
              <Route path="skills" element={<SettingsSkills />} />
              <Route path="claude" element={<SettingsClaude />} />
              <Route path="llm" element={<SettingsLLM />} />
              <Route path="database" element={<SettingsDatabase />} />
              <Route path="preflight" element={<SettingsPreflight />} />
            </Route>
          </Route>
        </Routes>
      </BrowserRouter>
    </AuthProvider>
  );
}
