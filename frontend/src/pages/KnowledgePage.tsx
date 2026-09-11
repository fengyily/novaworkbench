import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { memoriesApi, knowledgeApi, scannerApi, type Memory, type KnowledgeItem, type Project } from '../api/client';
import { errorMessage } from '../utils/errMsg';
import { fmtDate } from '../utils/intl';
import { projectsApi } from '../api/client';
import { stripMarkdownPreview } from '../utils/preview';
import './KnowledgePage.css';

type Tab = 'memories' | 'knowledge' | 'review';

export default function KnowledgePage() {
  const { t } = useTranslation();
  const [tab, setTab] = useState<Tab>('memories');
  const [projects, setProjects] = useState<Project[]>([]);
  const [selectedProject, setSelectedProject] = useState('');
  const [search, setSearch] = useState('');
  const [loading, setLoading] = useState(false);

  // Memories
  const [memories, setMemories] = useState<Memory[]>([]);
  const [showMemDialog, setShowMemDialog] = useState(false);
  const [editingMem, setEditingMem] = useState<Memory | null>(null);

  // Knowledge
  const [knowledge, setKnowledge] = useState<KnowledgeItem[]>([]);

  // Review
  const [reviewItems, setReviewItems] = useState<KnowledgeItem[]>([]);
  const [reviewIndex, setReviewIndex] = useState(0);

  useEffect(() => {
    projectsApi.list().then(setProjects).catch(() => {});
  }, []);

  useEffect(() => {
    loadTabData();
  }, [tab, selectedProject, search]);

  const loadTabData = async () => {
    setLoading(true);
    try {
      const pid = selectedProject || undefined;
      if (tab === 'memories') {
        const res = await memoriesApi.list({ project_id: pid, search: search || undefined });
        setMemories(res.items);
      } else if (tab === 'knowledge') {
        const res = await knowledgeApi.list({ project_id: pid, search: search || undefined });
        setKnowledge(res.items);
      } else {
        const items = await knowledgeApi.listForReview(pid);
        setReviewItems(items);
        setReviewIndex(0);
      }
    } catch (e) { /* ignore */ } finally { setLoading(false); }
  };

  const handleScan = async (pid: string) => {
    if (!pid) return;
    try {
      const result = await scannerApi.scan(pid);
      alert(t('knowledge.scanDone', { added: result.knowledge_new, updated: result.knowledge_updated }));
      loadTabData();
    } catch (err: any) {
      alert(t('knowledge.scanFailed', { msg: errorMessage(err) }));
    }
  };

  const handleReview = async (action: string) => {
    const item = reviewItems[reviewIndex];
    if (!item) return;
    try {
      await knowledgeApi.batchReview([item.id], action);
      if (reviewIndex < reviewItems.length - 1) {
        setReviewIndex(i => i + 1);
      }
      loadTabData(); // refresh counts
    } catch (err: any) {
      alert(err.message);
    }
  };

  return (
    <div className="knowledge-page">
      <h1 className="page-title">{t('knowledge.title')}</h1>

      {/* Project filter + actions */}
      <div className="kb-toolbar">
        <select
          value={selectedProject}
          onChange={e => setSelectedProject(e.target.value)}
          className="kb-select"
        >
          <option value="">{t('knowledge.allProjects')}</option>
          {projects.map(p => (
            <option key={p.id} value={p.id}>{p.name}</option>
          ))}
        </select>
        <input
          type="text"
          placeholder={t('knowledge.searchPlaceholder')}
          value={search}
          onChange={e => setSearch(e.target.value)}
          className="kb-search"
        />
        {selectedProject && (
          <button className="btn btn-sm" onClick={() => handleScan(selectedProject)}>
            {t('knowledge.scan')}
          </button>
        )}
      </div>

      {/* Tabs */}
      <div className="kb-tabs">
        <button className={`kb-tab ${tab === 'memories' ? 'active' : ''}`} onClick={() => setTab('memories')}>
          {t('knowledge.tabMemories')}
        </button>
        <button className={`kb-tab ${tab === 'knowledge' ? 'active' : ''}`} onClick={() => setTab('knowledge')}>
          {t('knowledge.tabKnowledge')}
        </button>
        <button className={`kb-tab ${tab === 'review' ? 'active' : ''}`} onClick={() => setTab('review')}>
          {t('knowledge.tabReview')} {reviewItems.length > 0 && <span className="kb-badge">{reviewItems.length}</span>}
        </button>
      </div>

      {loading && <div className="kb-loading">{t('knowledge.loading')}</div>}

      {/* Memories Tab */}
      {!loading && tab === 'memories' && (
        <div className="kb-list">
          <div className="kb-list-header">
            <span>{t('knowledge.memoryCount', { n: memories.length })}</span>
            <button className="btn btn-primary btn-sm" onClick={() => { setEditingMem(null); setShowMemDialog(true); }}>
              {t('knowledge.addNew')}
            </button>
          </div>
          {memories.length === 0 ? (
            <div className="kb-empty">{t('knowledge.memoriesEmpty')}</div>
          ) : (
            memories.map(m => (
              <div key={m.id} className="kb-card">
                <div className="kb-card-header">
                  <span className={`kb-type-badge type-${m.type}`}>{m.type}</span>
                  <span className="kb-card-title">{m.title || m.content.substring(0, 60)}</span>
                </div>
                <div className="kb-card-content">{m.content}</div>
                <div className="kb-card-meta">
                  {m.tags && JSON.parse(m.tags).length > 0 && (
                    <span className="kb-tags">{(JSON.parse(m.tags) as string[]).map((t: string) => (
                      <span key={t} className="kb-tag">{t}</span>
                    ))}</span>
                  )}
                  <span className="kb-date">{fmtDate(m.created_at)}</span>
                  <div className="kb-actions">
                    <button className="btn btn-sm" onClick={() => { setEditingMem(m); setShowMemDialog(true); }}>{t('knowledge.edit')}</button>
                    <button className="btn btn-sm" onClick={async () => { await memoriesApi.delete(m.id); loadTabData(); }}>{t('knowledge.delete')}</button>
                  </div>
                </div>
              </div>
            ))
          )}
        </div>
      )}

      {/* Knowledge Tab */}
      {!loading && tab === 'knowledge' && (
        <div className="kb-list">
          <div className="kb-list-header">
            <span>{t('knowledge.knowledgeCount', { n: knowledge.length })}</span>
          </div>
          {knowledge.length === 0 ? (
            <div className="kb-empty">{t('knowledge.knowledgeEmpty')}</div>
          ) : (
            knowledge.map(k => (
              <div key={k.id} className="kb-card">
                <div className="kb-card-header">
                  <span className={`kb-type-badge cat-${k.category}`}>{k.category || 'general'}</span>
                  {!k.is_reviewed && <span className="kb-unreviewed">{t('knowledge.unreviewed')}</span>}
                  {!k.is_approved && <span className="kb-rejected">{t('knowledge.rejected')}</span>}
                  <span className="kb-card-title">{k.title}</span>
                </div>
                <div className="kb-card-content kb-clamp">{stripMarkdownPreview(k.content)}</div>
                <div className="kb-card-meta">
                  <span className="kb-source">{t('knowledge.sourceLabel', { type: k.source_type })}</span>
                  {k.source_ref && <span className="kb-ref">{k.source_ref}</span>}
                  <span className="kb-date">{fmtDate(k.created_at)}</span>
                  <div className="kb-actions">
                    <button className="btn btn-sm" onClick={async () => { await knowledgeApi.delete(k.id); loadTabData(); }}>{t('knowledge.delete')}</button>
                  </div>
                </div>
              </div>
            ))
          )}
        </div>
      )}

      {/* Review Tab */}
      {!loading && tab === 'review' && reviewItems.length > 0 && (
        <div className="review-panel">
          <div className="review-progress">
            {t('knowledge.reviewProgress', { index: reviewIndex + 1, total: reviewItems.length })}
            <div className="review-progress-bar">
              <div className="review-progress-fill" style={{ width: `${(reviewIndex / Math.max(reviewItems.length - 1, 1)) * 100}%` }} />
            </div>
          </div>
          <div className="review-card">
            <div className="review-card-header">
              <span className={`kb-type-badge cat-${reviewItems[reviewIndex].category}`}>
                {reviewItems[reviewIndex].category || 'general'}
              </span>
              <span className="kb-tag">{reviewItems[reviewIndex].source_type}</span>
              {reviewItems[reviewIndex].source_ref && <span className="kb-tag">{reviewItems[reviewIndex].source_ref}</span>}
            </div>
            <h3>{reviewItems[reviewIndex].title}</h3>
            <div className="review-content">{reviewItems[reviewIndex].content}</div>
          </div>
          <div className="review-actions stack-mobile">
            <button className="btn" onClick={() => handleReview('edit')}>{t('knowledge.reviewEditApprove')}</button>
            <button className="btn btn-primary" onClick={() => handleReview('approve')}>{t('knowledge.reviewApprove')}</button>
            <button className="btn" onClick={() => {
              if (reviewIndex < reviewItems.length - 1) setReviewIndex(i => i + 1);
            }}>{t('knowledge.reviewSkip')}</button>
            <button className="btn" onClick={() => handleReview('reject')} style={{ color: 'var(--color-error)' }}>{t('knowledge.reviewReject')}</button>
          </div>
        </div>
      )}
      {!loading && tab === 'review' && reviewItems.length === 0 && (
        <div className="kb-empty">{t('knowledge.reviewEmpty')}</div>
      )}

      {/* Memory Dialog */}
      {showMemDialog && (
        <MemoryDialog
          projects={projects}
          selectedProject={selectedProject}
          memory={editingMem}
          onClose={() => setShowMemDialog(false)}
          onSaved={() => { setShowMemDialog(false); loadTabData(); }}
        />
      )}
    </div>
  );
}

// Memory create/edit dialog
function MemoryDialog({ projects, selectedProject, memory, onClose, onSaved }: {
  projects: Project[];
  selectedProject: string;
  memory: Memory | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const [projectId, setProjectId] = useState(memory?.project_id || selectedProject || '');
  const [type, setType] = useState(memory?.type || 'business_context');
  const [title, setTitle] = useState(memory?.title || '');
  const [content, setContent] = useState(memory?.content || '');
  const [tags, setTags] = useState(memory?.tags ? JSON.parse(memory.tags).join(', ') : '');
  const [saving, setSaving] = useState(false);

  const handleSave = async () => {
    if (!projectId || !content) return;
    setSaving(true);
    try {
      const data = {
        project_id: projectId,
        type,
        title,
        content,
        tags: JSON.stringify(tags.split(',').map((t: string) => t.trim()).filter(Boolean)),
      };
      if (memory) {
        await memoriesApi.update(memory.id, data);
      } else {
        await memoriesApi.create(data);
      }
      onSaved();
    } catch (err: any) {
      alert(err.message);
    } finally { setSaving(false); }
  };

  return (
    <div className="modal-overlay modal-fullscreen-overlay" onClick={onClose}>
      <div className="modal-box modal-fullscreen" onClick={e => e.stopPropagation()} style={{ maxWidth: 520 }}>
        <h3>{memory ? t('knowledge.dialog.editTitle') : t('knowledge.dialog.addTitle')}</h3>
        <div className="form-group">
          <label>{t('knowledge.dialog.project')}</label>
          <select value={projectId} onChange={e => setProjectId(e.target.value)} className="form-input">
            <option value="">{t('knowledge.dialog.projectPlaceholder')}</option>
            {projects.map(p => <option key={p.id} value={p.id}>{p.name}</option>)}
          </select>
        </div>
        <div className="form-group">
          <label>{t('knowledge.dialog.type')}</label>
          <select value={type} onChange={e => setType(e.target.value)} className="form-input">
            <option value="business_context">{t('knowledge.memType.business_context')}</option>
            <option value="technical_debt">{t('knowledge.memType.technical_debt')}</option>
            <option value="design_rationale">{t('knowledge.memType.design_rationale')}</option>
            <option value="code_explanation">{t('knowledge.memType.code_explanation')}</option>
          </select>
        </div>
        <div className="form-group">
          <label>{t('knowledge.dialog.title')}</label>
          <input type="text" value={title} onChange={e => setTitle(e.target.value)} className="form-input" />
        </div>
        <div className="form-group">
          <label>{t('knowledge.dialog.content')}</label>
          <textarea value={content} onChange={e => setContent(e.target.value)} className="form-input" rows={4}
            placeholder={t('knowledge.dialog.contentPlaceholder')} />
        </div>
        <div className="form-group">
          <label>{t('knowledge.dialog.tags')}</label>
          <input type="text" value={tags} onChange={e => setTags(e.target.value)} className="form-input"
            placeholder="redis, config" />
        </div>
        <div className="form-actions btn-row-2col">
          <button className="btn" onClick={onClose}>{t('knowledge.dialog.cancel')}</button>
          <button className="btn btn-primary" onClick={handleSave} disabled={saving || !projectId || !content}>
            {saving ? t('knowledge.dialog.saving') : t('knowledge.dialog.save')}
          </button>
        </div>
      </div>
    </div>
  );
}
