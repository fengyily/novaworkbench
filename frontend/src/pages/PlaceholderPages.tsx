import { useState, useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import { projectsApi, type Project } from '../api/client';

interface PlaceholderPageProps {
  title: string;
  emoji: string;
  description?: string;
}

function PlaceholderPage({ title, emoji, description }: PlaceholderPageProps) {
  return (
    <div style={{ width: '100%' }}>
      <h1 className="page-title">{emoji} {title}</h1>
      <div style={{
        background: 'var(--color-surface)',
        border: '1px solid var(--color-border)',
        borderRadius: 8,
        padding: 40,
        textAlign: 'center',
        color: 'var(--color-text-secondary)',
      }}>
        <p style={{ fontSize: 16, marginBottom: 8 }}>{description || '功能开发中...'}</p>
        <p style={{ fontSize: 13, color: 'var(--color-text-muted)' }}>Phase 2+ 实现</p>
      </div>
    </div>
  );
}

export function ProjectsList() {
  const navigate = useNavigate();
  const [projects, setProjects] = useState<Project[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    projectsApi.list()
      .then(data => setProjects(Array.isArray(data) ? data : []))
      .catch(err => setError(err.message))
      .finally(() => setLoading(false));
  }, []);

  const statusBadge = (status: string) => {
    const map: Record<string, string> = { active: '🟢 active', archived: '📦 archived', missing: '⚠️ missing' };
    return map[status] || status;
  };

  if (loading) return <div className="loading">⏳ 加载中...</div>;
  if (error) return <div className="error-toast">❌ {error}</div>;

  return (
    <div style={{ width: '100%' }}>
      <div className="section-header" style={{ marginBottom: 20 }}>
        <h1 className="page-title" style={{ marginBottom: 0 }}>📁 项目</h1>
        <button className="btn btn-primary" onClick={() => navigate('/projects/add')}>
          + 添加
        </button>
      </div>

      {projects.length === 0 ? (
        <div className="empty-state">
          <p>还没有添加项目</p>
          <button className="btn btn-primary" onClick={() => navigate('/projects/add')}>
            添加你的第一个项目
          </button>
        </div>
      ) : (
        <table className="project-table" style={{ width: '100%' }}>
          <thead>
            <tr>
              <th>名称</th>
              <th>类型</th>
              <th>路径</th>
              <th>状态</th>
              <th>更新时间</th>
            </tr>
          </thead>
          <tbody>
            {projects.map(p => (
              <tr key={p.id} onClick={() => navigate(`/projects/${p.id}`)} className="clickable-row">
                <td className="project-name">{p.name}</td>
                <td><span className="type-tag">{p.project_type || 'Unknown'}</span></td>
                <td className="path-cell">{p.local_path}</td>
                <td><span className={`status-badge status-${p.status}`}>{statusBadge(p.status)}</span></td>
                <td>{new Date(p.updated_at).toLocaleDateString()}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

export function Requirements() {
  return <PlaceholderPage title="需求看板" emoji="📋" description="Kanban 需求管理和 AI 拆解" />;
}

export function Knowledge() {
  return <PlaceholderPage title="知识库" emoji="🧠" description="项目知识管理、语义搜索、AI Review" />;
}

export function Chat() {
  return <PlaceholderPage title="AI 对话" emoji="💬" description="基于项目上下文的智能对话面板" />;
}

export function Reports() {
  return <PlaceholderPage title="周报" emoji="📝" description="自动生成开发周报" />;
}

export function Settings() {
  return <PlaceholderPage title="设置" emoji="⚙️" description="LLM 配置、代码风格、使用习惯" />;
}
