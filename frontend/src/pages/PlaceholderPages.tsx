import { useState, useEffect, useCallback } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { projectsApi, type Project } from '../api/client';
import { errorMessage } from '../utils/errMsg';
import { fmtDateTime } from '../utils/intl';

interface PlaceholderPageProps {
  title: string;
  emoji: string;
  description?: string;
}

function PlaceholderPage({ title, emoji, description }: PlaceholderPageProps) {
  const { t } = useTranslation();
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
        <p style={{ fontSize: 16, marginBottom: 8 }}>{description || t('projects.placeholders.developing')}</p>
        <p style={{ fontSize: 13, color: 'var(--color-text-muted)' }}>{t('projects.placeholders.phase')}</p>
      </div>
    </div>
  );
}

export function ProjectsList() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [view, setView] = useState<'active' | 'trash'>('active');
  const [projects, setProjects] = useState<Project[]>([]);
  const [trashProjects, setTrashProjects] = useState<Project[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [toast, setToast] = useState<string | null>(null);

  // Delete modal state
  const [deleteTarget, setDeleteTarget] = useState<Project | null>(null);
  const [deleteDir, setDeleteDir] = useState(false);
  const [acknowledgedRisk, setAcknowledgedRisk] = useState(false);
  const [deleting, setDeleting] = useState(false);

  // Restore busy state (per-project id)
  const [restoringId, setRestoringId] = useState<string | null>(null);

  // Purge (hard-delete) modal state — separate from the soft-delete modal
  // so the two destructive flows can never share busy state.
  const [purgeTarget, setPurgeTarget] = useState<Project | null>(null);
  const [purgeAck, setPurgeAck] = useState(false);
  const [purging, setPurging] = useState(false);

  // One-shot backfill of missing AI descriptions
  const [backfilling, setBackfilling] = useState(false);

  const loadActive = useCallback(() => {
    setLoading(true);
    projectsApi.list()
      .then(data => setProjects(Array.isArray(data) ? data : []))
      .catch(err => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, []);

  const loadTrash = useCallback(() => {
    setLoading(true);
    projectsApi.trash()
      .then(data => setTrashProjects(Array.isArray(data) ? data : []))
      .catch(err => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    if (view === 'active') loadActive();
    else loadTrash();
  }, [view, loadActive, loadTrash]);

  // Auto-dismiss toast after 4s
  useEffect(() => {
    if (!toast) return;
    const t = setTimeout(() => setToast(null), 4000);
    return () => clearTimeout(t);
  }, [toast]);

  const statusBadge = (status: string) => {
    const map: Record<string, string> = {
      active: t('projects.status.active'),
      archived: t('projects.status.archived'),
      missing: t('projects.status.missing'),
    };
    return map[status] || status;
  };

  const openDeleteModal = (p: Project) => {
    setDeleteTarget(p);
    setDeleteDir(false);
    setAcknowledgedRisk(false);
  };

  const closeDeleteModal = () => {
    if (deleting) return;
    setDeleteTarget(null);
    setDeleteDir(false);
    setAcknowledgedRisk(false);
  };

  const handleDelete = async () => {
    if (!deleteTarget || deleting) return;
    setDeleting(true);
    setError(null);
    try {
      await projectsApi.remove(deleteTarget.id, { delete_dir: deleteDir });
      setProjects(prev => prev.filter(p => p.id !== deleteTarget.id));
      setToast(t(deleteDir
        ? 'projects.toast.deletedWithDir'
        : 'projects.toast.deleted'));
      setDeleteTarget(null);
      setDeleteDir(false);
      setAcknowledgedRisk(false);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setDeleting(false);
    }
  };

  const handleRestore = async (p: Project) => {
    if (restoringId) return;
    setRestoringId(p.id);
    setError(null);
    try {
      await projectsApi.restore(p.id);
      setToast(t('projects.toast.restored'));
      // Refresh trash and active lists.
      await loadTrash();
      await loadActive();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setRestoringId(null);
    }
  };

  const handleBackfill = async () => {
    if (backfilling) return;
    setBackfilling(true);
    setError(null);
    try {
      const res = await projectsApi.backfillDescriptions();
      setToast(t('projects.toast.backfillDone', {
        updated: res.updated, skipped: res.skipped, failed: res.failed,
      }));
      await loadActive();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBackfilling(false);
    }
  };

  const openPurgeModal = (p: Project) => {
    setPurgeTarget(p);
    setPurgeAck(false);
  };
  const closePurgeModal = () => {
    if (purging) return;
    setPurgeTarget(null);
    setPurgeAck(false);
  };
  const handlePurge = async () => {
    if (!purgeTarget || purging || !purgeAck) return;
    setPurging(true);
    setError(null);
    try {
      await projectsApi.purge(purgeTarget.id);
      setTrashProjects(prev => prev.filter(p => p.id !== purgeTarget.id));
      setToast(t('projects.toast.purged', { name: purgeTarget.name }));
      setPurgeTarget(null);
      setPurgeAck(false);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setPurging(false);
    }
  };

  // ---- Purge Modal ----
//
// Hard-delete confirmation. The risk checkbox must be ticked before the
// confirm button enables — same pattern as the delete-dir checkbox on the
// soft-delete modal, but the messaging is sterner because the row is gone
// for good (no Restore can bring it back).

// ---- Delete Modal ----
  const renderDeleteModal = () => {
    if (!deleteTarget) return null;
    const target = deleteTarget;
    const showRiskStep = deleteDir;
    const canConfirm = !showRiskStep || acknowledgedRisk;

    return (
      <div className="modal-overlay" onClick={closeDeleteModal}>
        <div className="modal-box" onClick={e => e.stopPropagation()}>
          <h3>{t('projects.deleteModal.title')}</h3>
          <div className="modal-confirm-text">
            {t('projects.deleteModal.confirmPrefix')} <strong>{target.name}</strong>{t('projects.deleteModal.confirmSuffix')}
          </div>

          <label className="modal-check-row">
            <input
              type="checkbox"
              checked={deleteDir}
              onChange={e => { setDeleteDir(e.target.checked); setAcknowledgedRisk(false); }}
            />
            <span>
              {t('projects.deleteModal.deleteDir')}
              <div className="modal-check-path break-all">{target.local_path}</div>
            </span>
          </label>

          {showRiskStep && (
            <div className="modal-risk-panel">
              <div className="modal-risk-title">
                {t('projects.deleteModal.riskTitle')}
              </div>
              <div className="modal-risk-desc">
                {t('projects.deleteModal.riskDesc')}
              </div>
              <label className="modal-risk-check">
                <input
                  type="checkbox"
                  checked={acknowledgedRisk}
                  onChange={e => setAcknowledgedRisk(e.target.checked)}
                />
                <span>{t('projects.deleteModal.ack')}</span>
              </label>
            </div>
          )}

          <div className="modal-actions btn-row-2col">
            <button className="btn" onClick={closeDeleteModal} disabled={deleting}>{t('projects.deleteModal.cancel')}</button>
            <button
              className="btn btn-danger"
              onClick={handleDelete}
              disabled={!canConfirm || deleting}
            >
              {deleting
                ? t('projects.deleteModal.deleting')
                : (deleteDir ? t('projects.deleteModal.confirmPurgeDir') : t('projects.deleteModal.confirmDelete'))}
            </button>
          </div>
        </div>
      </div>
    );
  };

  // ---- Purge Modal ----
  //
  // Hard-delete confirmation. The risk checkbox must be ticked before the
  // confirm button enables — same pattern as the delete-dir checkbox on
  // the soft-delete modal, but the messaging is sterner because the row is
  // gone for good (no Restore can bring it back).
  const renderPurgeModal = () => {
    if (!purgeTarget) return null;
    const target = purgeTarget;
    return (
      <div className="modal-overlay" onClick={closePurgeModal}>
        <div className="modal-box" onClick={e => e.stopPropagation()}>
          <h3 className="modal-title-danger">{t('projects.purgeModal.title')}</h3>
          <div className="modal-confirm-text">
            {t('projects.purgeModal.confirmPrefix')} <strong>{target.name}</strong>
            {t('projects.purgeModal.confirmSuffix')}
          </div>
          <div className="modal-risk-panel">
            <div className="modal-risk-title">{t('projects.purgeModal.riskTitle')}</div>
            {target.deleted_dir === 0 && target.local_path ? (
              <div>
                {t('projects.purgeModal.dirWillDeletePrefix')} <code className="break-all">{target.local_path}</code> {t('projects.purgeModal.dirWillDeleteSuffix')}
              </div>
            ) : (
              <div>{t('projects.purgeModal.dirAlreadyDeleted')}</div>
            )}
          </div>
          <label className="modal-risk-check">
            <input
              type="checkbox"
              checked={purgeAck}
              onChange={e => setPurgeAck(e.target.checked)}
            />
            <span>{t('projects.purgeModal.ack')}</span>
          </label>
          <div className="modal-actions btn-row-2col">
            <button className="btn" onClick={closePurgeModal} disabled={purging}>{t('projects.purgeModal.cancel')}</button>
            <button
              className="btn btn-danger"
              onClick={handlePurge}
              disabled={!purgeAck || purging}
            >
              {purging ? t('projects.purgeModal.deleting') : t('projects.purgeModal.confirm')}
            </button>
          </div>
        </div>
      </div>
    );
  };

  // ---- Render ----
  const list = view === 'active' ? projects : trashProjects;

  return (
    <div style={{ width: '100%', position: 'relative' }}>
      <div className="section-header" style={{ marginBottom: 20 }}>
        <h1 className="page-title" style={{ marginBottom: 0 }}>
          {view === 'active' ? t('projects.list.title') : t('projects.list.trashTitle')}
        </h1>
        <div style={{ display: 'flex', gap: 8 }}>
          <button
            className={view === 'trash' ? 'btn' : 'btn btn-primary'}
            onClick={() => setView(view === 'active' ? 'trash' : 'active')}
          >
            {view === 'active' ? t('projects.list.openTrash') : t('projects.list.backToList')}
          </button>
          {view === 'active' && (
            <>
              <button
                className="btn"
                onClick={handleBackfill}
                disabled={backfilling}
                title={t('projects.list.backfillTitle')}
              >
                {backfilling ? t('projects.list.generating') : t('projects.list.backfill')}
              </button>
              <button className="btn btn-primary" onClick={() => navigate('/projects/add')}>
                {t('projects.list.add')}
              </button>
            </>
          )}
        </div>
      </div>

      {error && (
        <div className="error-toast" style={{ marginBottom: 16 }}>❌ {error}</div>
      )}
      {toast && (
        <div style={{
          background: 'var(--color-active)', border: '1px solid var(--color-primary)',
          color: 'var(--color-primary)', padding: '8px 12px', borderRadius: 6, marginBottom: 16, fontSize: 13,
        }}>
          ✅ {toast}
        </div>
      )}

      {loading ? (
        <div className="loading">{t('projects.list.loading')}</div>
      ) : list.length === 0 ? (
        view === 'active' ? (
          <div className="mobile-empty">
            <span className="mobile-empty-mark">📁</span>
            <div className="mobile-empty-title">{t('projects.list.emptyTitle')}</div>
            <p className="mobile-empty-desc">
              {t('projects.list.emptyDesc')}
            </p>
            <button className="btn btn-primary" onClick={() => navigate('/projects/add')}>
              {t('projects.list.addProject')}
            </button>
          </div>
        ) : (
          <div className="empty-state">
            <p>{t('projects.list.trashEmpty')}</p>
          </div>
        )
      ) : (
        <table className="project-table" style={{ width: '100%' }}>
          <thead>
            <tr>
              <th>{t('projects.list.colName')}</th>
              <th>{t('projects.list.colDesc')}</th>
              <th>{t('projects.list.colType')}</th>
              <th>{t('projects.list.colPath')}</th>
              <th>{t('projects.list.colStatus')}</th>
              <th>{view === 'active' ? t('projects.list.colUpdated') : t('projects.list.colDeleted')}</th>
              <th>{t('projects.list.colActions')}</th>
            </tr>
          </thead>
          <tbody>
            {list.map(p => {
              const noRemote = view === 'trash' && !p.remote_url;
              return (
                <tr
                  key={p.id}
                  className={view === 'active' ? 'clickable-row' : undefined}
                  onClick={view === 'active' ? () => navigate(`/projects/${p.id}`) : undefined}
                >
                  <td className="project-name">{p.name}</td>
                  <td className="project-desc-cell">
                    <div className="project-desc-clamp">
                      {p.description || <span style={{ color: 'var(--color-text-muted)' }}>{t('projects.list.noDesc')}</span>}
                    </div>
                  </td>
                  <td><span className="type-tag">{p.project_type || 'Unknown'}</span></td>
                  <td className="path-cell" style={{ wordBreak: 'break-all' }}>
                    {p.local_path}
                    {view === 'trash' && p.deleted_dir ? (
                      <span style={{ color: 'var(--color-warning)', fontSize: 11, marginLeft: 6 }}>{t('projects.list.dirDeleted')}</span>
                    ) : null}
                  </td>
                  <td><span className={`status-badge status-${p.status}`}>{statusBadge(p.status)}</span></td>
                  <td>{fmtDateTime(view === 'active' ? p.updated_at : (p.deleted_at || p.updated_at))}</td>
                  <td onClick={e => e.stopPropagation()}>
                    {view === 'active' ? (
                      <button
                        className="btn btn-sm btn-danger"
                        onClick={() => openDeleteModal(p)}
                      >
                        {t('projects.list.delete')}
                      </button>
                    ) : (
                      <div style={{ display: 'flex', gap: 6, justifyContent: 'flex-end' }}>
                        <button
                          className="btn btn-sm btn-primary"
                          onClick={() => handleRestore(p)}
                          disabled={!!restoringId || noRemote}
                          title={noRemote ? t('projects.list.noRemoteTitle') : undefined}
                        >
                          {restoringId === p.id ? t('projects.list.cloning') : t('projects.list.restore')}
                        </button>
                        <button
                          className="btn btn-sm btn-danger"
                          onClick={() => openPurgeModal(p)}
                          disabled={!!restoringId}
                        >
                          {t('projects.list.purge')}
                        </button>
                      </div>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}

      {renderDeleteModal()}
      {renderPurgeModal()}

      {/* Mobile FAB: add project. Hidden on the trash view where adding
          projects doesn't make sense. CSS (.fab) hides it on desktop. */}
      {view === 'active' && (
        <button
          className="fab fab-extended"
          aria-label={t('projects.list.addProjectShort')}
          onClick={() => navigate('/projects/add')}
        >
          <span>＋</span>
          <span>{t('projects.list.addProjectShort')}</span>
        </button>
      )}
    </div>
  );
}

// ---- Purge Modal ---------------------------------------------------------
//
// Hard-delete confirmation. The risk checkbox must be ticked before the
// confirm button enables — same pattern as the delete-dir checkbox on the
// soft-delete modal, but the messaging is sterner because the row is gone
// for good (no Restore can bring it back).

// Placeholder takes translation KEYS (not strings) so these stub pages stay
// live against a language switch. `tStrict` is keyed against the resource
// tree, so we widen it to accept the dynamic `${base}.title` keys here.
function Placeholder({ title, emoji }: { title: string; emoji: string }) {
  const { t: tStrict } = useTranslation();
  const t = tStrict as unknown as (key: string) => string;
  return (
    <PlaceholderPage
      title={t(`${title}.title`)}
      emoji={emoji}
      description={t(`${title}.desc`)}
    />
  );
}

export function Requirements() {
  return <Placeholder title="projects.placeholders.requirements" emoji="📋" />;
}

export function Knowledge() {
  return <Placeholder title="projects.placeholders.knowledge" emoji="🧠" />;
}

export function Chat() {
  return <Placeholder title="projects.placeholders.chat" emoji="💬" />;
}

export function Reports() {
  return <Placeholder title="projects.placeholders.reports" emoji="📝" />;
}

export function Settings() {
  return <Placeholder title="projects.placeholders.settings" emoji="⚙️" />;
}
