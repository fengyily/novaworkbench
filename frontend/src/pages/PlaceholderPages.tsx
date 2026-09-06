import { useState, useEffect, useCallback } from 'react';
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
      .catch(err => setError(err.message))
      .finally(() => setLoading(false));
  }, []);

  const loadTrash = useCallback(() => {
    setLoading(true);
    projectsApi.trash()
      .then(data => setTrashProjects(Array.isArray(data) ? data : []))
      .catch(err => setError(err.message))
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
    const map: Record<string, string> = { active: '🟢 active', archived: '📦 archived', missing: '⚠️ missing' };
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
      setToast(deleteDir ? '已删除项目及目录（可在回收站恢复）' : '已删除项目（目录已保留，可在回收站恢复）');
      setDeleteTarget(null);
      setDeleteDir(false);
      setAcknowledgedRisk(false);
    } catch (e) {
      setError((e as Error).message);
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
      setToast('已恢复项目并重新 clone');
      // Refresh trash and active lists.
      await loadTrash();
      await loadActive();
    } catch (e) {
      setError((e as Error).message);
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
      setToast(`补齐完成：生成 ${res.updated}，跳过 ${res.skipped}，失败 ${res.failed}`);
      await loadActive();
    } catch (e) {
      setError((e as Error).message);
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
      setToast(`已彻底删除「${purgeTarget.name}」`);
      setPurgeTarget(null);
      setPurgeAck(false);
    } catch (e) {
      setError((e as Error).message);
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
          <h3>删除项目</h3>
          <div className="modal-confirm-text">
            确认删除项目 <strong>{target.name}</strong>？
          </div>

          <label className="modal-check-row">
            <input
              type="checkbox"
              checked={deleteDir}
              onChange={e => { setDeleteDir(e.target.checked); setAcknowledgedRisk(false); }}
            />
            <span>
              同时删除项目目录
              <div className="modal-check-path break-all">{target.local_path}</div>
            </span>
          </label>

          {showRiskStep && (
            <div className="modal-risk-panel">
              <div className="modal-risk-title">
                ⚠️ 此操作将永久删除目录及其中所有文件，且不可恢复
              </div>
              <div className="modal-risk-desc">
                删除后可通过回收站重新 git clone 恢复（需要项目已配置远程地址）。
              </div>
              <label className="modal-risk-check">
                <input
                  type="checkbox"
                  checked={acknowledgedRisk}
                  onChange={e => setAcknowledgedRisk(e.target.checked)}
                />
                <span>我已了解风险，确认继续</span>
              </label>
            </div>
          )}

          <div className="modal-actions btn-row-2col">
            <button className="btn" onClick={closeDeleteModal} disabled={deleting}>取消</button>
            <button
              className="btn btn-danger"
              onClick={handleDelete}
              disabled={!canConfirm || deleting}
            >
              {deleting ? '删除中…' : (deleteDir ? '确认永久删除' : '确认删除')}
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
          <h3 className="modal-title-danger">⚠️ 彻底删除项目</h3>
          <div className="modal-confirm-text">
            将永久删除 <strong>{target.name}</strong> 的数据库记录，
            以及其下所有需求、知识库、记忆、运行配置、Token 用量记录。
          </div>
          <div className="modal-risk-panel">
            <div className="modal-risk-title">此操作不可恢复</div>
            {target.deleted_dir === 0 && target.local_path ? (
              <div>
                项目目录 <code className="break-all">{target.local_path}</code> 也将被删除。
              </div>
            ) : (
              <div>项目目录此前已删除，此操作仅清理数据库记录。</div>
            )}
          </div>
          <label className="modal-risk-check">
            <input
              type="checkbox"
              checked={purgeAck}
              onChange={e => setPurgeAck(e.target.checked)}
            />
            <span>我已了解风险，确认继续</span>
          </label>
          <div className="modal-actions btn-row-2col">
            <button className="btn" onClick={closePurgeModal} disabled={purging}>取消</button>
            <button
              className="btn btn-danger"
              onClick={handlePurge}
              disabled={!purgeAck || purging}
            >
              {purging ? '删除中…' : '确认彻底删除'}
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
          {view === 'active' ? '📁 项目' : '🗑 回收站'}
        </h1>
        <div style={{ display: 'flex', gap: 8 }}>
          <button
            className={view === 'trash' ? 'btn' : 'btn btn-primary'}
            onClick={() => setView(view === 'active' ? 'trash' : 'active')}
          >
            {view === 'active' ? '🗑 回收站' : '← 返回项目列表'}
          </button>
          {view === 'active' && (
            <>
              <button
                className="btn"
                onClick={handleBackfill}
                disabled={backfilling}
                title="为缺少简介的项目自动生成 AI 简介"
              >
                {backfilling ? '生成中…' : '🤖 生成缺失简介'}
              </button>
              <button className="btn btn-primary" onClick={() => navigate('/projects/add')}>
                + 添加
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
        <div className="loading">⏳ 加载中...</div>
      ) : list.length === 0 ? (
        view === 'active' ? (
          <div className="mobile-empty">
            <span className="mobile-empty-mark">📁</span>
            <div className="mobile-empty-title">还没有添加项目</div>
            <p className="mobile-empty-desc">
              添加一个本地 git 仓库，或从新建一个项目目录开始。
            </p>
            <button className="btn btn-primary" onClick={() => navigate('/projects/add')}>
              + 添加项目
            </button>
          </div>
        ) : (
          <div className="empty-state">
            <p>回收站为空</p>
          </div>
        )
      ) : (
        <table className="project-table" style={{ width: '100%' }}>
          <thead>
            <tr>
              <th>名称</th>
              <th>简介</th>
              <th>类型</th>
              <th>路径</th>
              <th>状态</th>
              <th>{view === 'active' ? '更新时间' : '删除时间'}</th>
              <th>操作</th>
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
                      {p.description || <span style={{ color: 'var(--color-text-muted)' }}>暂无简介</span>}
                    </div>
                  </td>
                  <td><span className="type-tag">{p.project_type || 'Unknown'}</span></td>
                  <td className="path-cell" style={{ wordBreak: 'break-all' }}>
                    {p.local_path}
                    {view === 'trash' && p.deleted_dir ? (
                      <span style={{ color: 'var(--color-warning)', fontSize: 11, marginLeft: 6 }}>目录已删</span>
                    ) : null}
                  </td>
                  <td><span className={`status-badge status-${p.status}`}>{statusBadge(p.status)}</span></td>
                  <td>{new Date(view === 'active' ? p.updated_at : (p.deleted_at || p.updated_at)).toLocaleString()}</td>
                  <td onClick={e => e.stopPropagation()}>
                    {view === 'active' ? (
                      <button
                        className="btn btn-sm btn-danger"
                        onClick={() => openDeleteModal(p)}
                      >
                        删除
                      </button>
                    ) : (
                      <div style={{ display: 'flex', gap: 6, justifyContent: 'flex-end' }}>
                        <button
                          className="btn btn-sm btn-primary"
                          onClick={() => handleRestore(p)}
                          disabled={!!restoringId || noRemote}
                          title={noRemote ? '无远程地址，无法自动恢复' : undefined}
                        >
                          {restoringId === p.id ? '正在 clone…' : '恢复'}
                        </button>
                        <button
                          className="btn btn-sm btn-danger"
                          onClick={() => openPurgeModal(p)}
                          disabled={!!restoringId}
                        >
                          彻底删除
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

      {/* Mobile FAB: "+ 添加项目". Hidden on the trash view where adding
          projects doesn't make sense. CSS (.fab) hides it on desktop. */}
      {view === 'active' && (
        <button
          className="fab fab-extended"
          aria-label="添加项目"
          onClick={() => navigate('/projects/add')}
        >
          <span>＋</span>
          <span>添加项目</span>
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
