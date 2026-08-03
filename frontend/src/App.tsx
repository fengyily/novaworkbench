import { BrowserRouter, Routes, Route } from 'react-router-dom';
import Layout from './components/Layout';
import Dashboard from './pages/Dashboard';
import AddProject from './pages/AddProject';
import {
  ProjectsList, Chat, Reports,
} from './pages/PlaceholderPages';
import Settings from './pages/Settings';
import SettingsTokens from './pages/SettingsTokens';
import SettingsRoles from './pages/SettingsRoles';
import SettingsClaude from './pages/SettingsClaude';
import KnowledgePage from './pages/KnowledgePage';
import RequirementDetail from './pages/RequirementDetail';
import WizardPage from './pages/WizardPage';
import ProjectDetail from './pages/ProjectDetail';

export default function App() {
  return (
    <BrowserRouter>
      <Routes>
        <Route element={<Layout />}>
          <Route index element={<Dashboard />} />
          <Route path="wizard" element={<WizardPage />} />
          <Route path="projects/add" element={<AddProject />} />
          <Route path="projects" element={<ProjectsList />} />
          <Route path="projects/:id" element={<ProjectDetail />} />
          <Route path="requirements/:id" element={<RequirementDetail />} />
          <Route path="knowledge" element={<KnowledgePage />} />
          <Route path="chat" element={<Chat />} />
          <Route path="reports" element={<Reports />} />
          <Route path="settings" element={<Settings />}>
            <Route index element={<SettingsTokens />} />
            <Route path="roles" element={<SettingsRoles />} />
            <Route path="claude" element={<SettingsClaude />} />
          </Route>
        </Route>
      </Routes>
    </BrowserRouter>
  );
}
