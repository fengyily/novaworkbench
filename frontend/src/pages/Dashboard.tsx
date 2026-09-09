import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { dashboardApi, type DashboardData } from '../api/client';
import { errorMessage } from '../utils/errMsg';
import { fmtDate } from '../utils/intl';
import './Dashboard.css';

export default function Dashboard() {
  const { t } = useTranslation();
  const [data, setData] = useState<DashboardData | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const navigate = useNavigate();

  useEffect(() => {
    dashboardApi.get()
      .then(setData)
      .catch(err => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, []);

  if (loading) return <div className="loading">{t('dashboard.loading')}</div>;
  if (error) return <div className="error-toast">❌ {error}</div>;

  const statusBadge = (status: string) => {
    const map: Record<string, string> = {
      active: t('dashboard.status.active'),
      archived: t('dashboard.status.archived'),
      missing: t('dashboard.status.missing'),
    };
    return map[status] || status;
  };

  return (
    <div className="dashboard">
      <h1 className="page-title">{t('dashboard.title')}</h1>

      <div className="stats-grid">
        <div className="stat-card">
          <div className="stat-value">{data?.total_projects || 0}</div>
          <div className="stat-label">{t('dashboard.statProjects')}</div>
        </div>
        <div className="stat-card">
          <div className="stat-value">{data?.active_requirements || 0}</div>
          <div className="stat-label">{t('dashboard.statActiveReqs')}</div>
        </div>
        <div className="stat-card">
          <div className="stat-value">{data?.pending_reviews || 0}</div>
          <div className="stat-label">{t('dashboard.statPendingReviews')}</div>
        </div>
        <div className="stat-card">
          <div className="stat-value">{data?.weekly_commits || 0}</div>
          <div className="stat-label">{t('dashboard.statWeeklyCommits')}</div>
        </div>
      </div>

      <div className="projects-section">
        <div className="section-header">
          <h2>{t('dashboard.projectList')}</h2>
          <button className="btn btn-primary desktop-only" onClick={() => navigate('/projects/add')}>
            {t('dashboard.add')}
          </button>
        </div>

        {(!data?.projects || data.projects.length === 0) ? (
          <>
            <div className="empty-state desktop-only">
              <p>{t('dashboard.emptyTitle')}</p>
              <button className="btn btn-primary" onClick={() => navigate('/projects/add')}>
                {t('dashboard.emptyCta')}
              </button>
            </div>
            <div className="mobile-empty">
              <span className="mobile-empty-mark">📁</span>
              <div className="mobile-empty-title">{t('dashboard.emptyTitle')}</div>
              <p className="mobile-empty-desc">
                {t('dashboard.mobileEmptyDesc')}
              </p>
              <button className="btn btn-primary" onClick={() => navigate('/projects/add')}>
                {t('dashboard.addProject')}
              </button>
            </div>
          </>
        ) : (
          <div className="project-table-wrap">
            <table className="project-table table-cards">
              <thead>
                <tr>
                  <th>{t('dashboard.colName')}</th>
                  <th>{t('dashboard.colType')}</th>
                  <th>{t('dashboard.colPath')}</th>
                  <th>{t('dashboard.colStatus')}</th>
                  <th>{t('dashboard.colUpdated')}</th>
                </tr>
              </thead>
              <tbody>
                {data?.projects.map(p => (
                  <tr key={p.id} onClick={() => navigate(`/projects/${p.id}`)} className="clickable-row">
                    <td className="project-name" data-label={t('dashboard.colName')}>{p.name}</td>
                    <td data-label={t('dashboard.colType')}><span className="type-tag">{p.project_type || 'Unknown'}</span></td>
                    <td className="path-cell" data-label={t('dashboard.colPath')}>{p.local_path}</td>
                    <td data-label={t('dashboard.colStatus')}><span className={`status-badge status-${p.status}`}>{statusBadge(p.status)}</span></td>
                    <td data-label={t('dashboard.colUpdatedShort')}>{fmtDate(p.updated_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <div className="quick-actions btn-row-2col">
        <button className="btn btn-primary" onClick={() => navigate('/wizard')}>{t('dashboard.wizard')}</button>
        <button className="btn" onClick={() => navigate('/projects/add')}>{t('dashboard.addProjectShort')}</button>
        <button className="btn" onClick={() => navigate('/requirements')}>{t('dashboard.requirements')}</button>
        <button className="btn" onClick={() => navigate('/chat')}>{t('dashboard.startChat')}</button>
        <button className="btn" onClick={() => navigate('/reports')}>{t('dashboard.weeklyReport')}</button>
        <button className="btn" onClick={() => navigate('/knowledge')}>{t('dashboard.knowledgeReview')}</button>
      </div>

      {/* Mobile FAB: a single primary CTA pinned above the tab bar. On
          desktop this button is hidden by .fab's display:none rule. The
          label is the page's primary action — the new-project wizard for
          dashboard, since wizard is the entry point for new work. */}
      <button
        className="fab fab-extended"
        aria-label={t('dashboard.wizard')}
        onClick={() => navigate('/wizard')}
      >
        <span>＋</span>
        <span>{t('dashboard.newProject')}</span>
      </button>
    </div>
  );
}
