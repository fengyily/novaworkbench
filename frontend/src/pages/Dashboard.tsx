import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { dashboardApi, type DashboardData } from '../api/client';
import './Dashboard.css';

export default function Dashboard() {
  const [data, setData] = useState<DashboardData | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const navigate = useNavigate();

  useEffect(() => {
    dashboardApi.get()
      .then(setData)
      .catch(err => setError(err.message))
      .finally(() => setLoading(false));
  }, []);

  if (loading) return <div className="loading">⏳ 加载中...</div>;
  if (error) return <div className="error-toast">❌ {error}</div>;

  const statusBadge = (status: string) => {
    const map: Record<string, string> = {
      active: '🟢 active',
      archived: '📦 archived',
      missing: '⚠️ missing',
    };
    return map[status] || status;
  };

  return (
    <div className="dashboard">
      <h1 className="page-title">📊 仪表盘</h1>

      <div className="stats-grid">
        <div className="stat-card">
          <div className="stat-value">{data?.total_projects || 0}</div>
          <div className="stat-label">项目数</div>
        </div>
        <div className="stat-card">
          <div className="stat-value">{data?.active_requirements || 0}</div>
          <div className="stat-label">活跃需求</div>
        </div>
        <div className="stat-card">
          <div className="stat-value">{data?.pending_reviews || 0}</div>
          <div className="stat-label">待 Review 知识</div>
        </div>
        <div className="stat-card">
          <div className="stat-value">{data?.weekly_commits || 0}</div>
          <div className="stat-label">本周提交</div>
        </div>
      </div>

      <div className="projects-section">
        <div className="section-header">
          <h2>项目列表</h2>
          <button className="btn btn-primary" onClick={() => navigate('/projects/add')}>
            + 添加
          </button>
        </div>

        {(!data?.projects || data.projects.length === 0) ? (
          <div className="empty-state">
            <p>还没有添加项目</p>
            <button className="btn btn-primary" onClick={() => navigate('/projects/add')}>
              添加你的第一个项目
            </button>
          </div>
        ) : (
          <div className="project-table-wrap">
            <table className="project-table">
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
                {data?.projects.map(p => (
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
          </div>
        )}
      </div>

      <div className="quick-actions">
        <button className="btn btn-primary" onClick={() => navigate('/wizard')}>🪄 新建项目向导</button>
        <button className="btn" onClick={() => navigate('/projects/add')}>添加项目</button>
        <button className="btn" onClick={() => navigate('/requirements')}>新建需求</button>
        <button className="btn" onClick={() => navigate('/chat')}>开始对话</button>
        <button className="btn" onClick={() => navigate('/reports')}>生成周报</button>
        <button className="btn" onClick={() => navigate('/knowledge')}>知识审查</button>
      </div>
    </div>
  );
}
