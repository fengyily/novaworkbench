import { useState, useEffect } from 'react';
import { memoriesApi, knowledgeApi, scannerApi, type Memory, type KnowledgeItem, type Project } from '../api/client';
import { projectsApi } from '../api/client';
import { stripMarkdownPreview } from '../utils/preview';
import './KnowledgePage.css';

type Tab = 'memories' | 'knowledge' | 'review';

export default function KnowledgePage() {
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
      alert(`扫描完成: 新增 ${result.knowledge_new} 条, 更新 ${result.knowledge_updated} 条`);
      loadTabData();
    } catch (err: any) {
      alert('扫描失败: ' + err.message);
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
      <h1 className="page-title">🧠 知识库</h1>

      {/* Project filter + actions */}
      <div className="kb-toolbar">
        <select
          value={selectedProject}
          onChange={e => setSelectedProject(e.target.value)}
          className="kb-select"
        >
          <option value="">全部项目</option>
          {projects.map(p => (
            <option key={p.id} value={p.id}>{p.name}</option>
          ))}
        </select>
        <input
          type="text"
          placeholder="搜索..."
          value={search}
          onChange={e => setSearch(e.target.value)}
          className="kb-search"
        />
        {selectedProject && (
          <button className="btn btn-sm" onClick={() => handleScan(selectedProject)}>
            🔄 扫描项目
          </button>
        )}
      </div>

      {/* Tabs */}
      <div className="kb-tabs">
        <button className={`kb-tab ${tab === 'memories' ? 'active' : ''}`} onClick={() => setTab('memories')}>
          记忆
        </button>
        <button className={`kb-tab ${tab === 'knowledge' ? 'active' : ''}`} onClick={() => setTab('knowledge')}>
          知识条目
        </button>
        <button className={`kb-tab ${tab === 'review' ? 'active' : ''}`} onClick={() => setTab('review')}>
          待Review {reviewItems.length > 0 && <span className="kb-badge">{reviewItems.length}</span>}
        </button>
      </div>

      {loading && <div className="kb-loading">⏳ 加载中...</div>}

      {/* Memories Tab */}
      {!loading && tab === 'memories' && (
        <div className="kb-list">
          <div className="kb-list-header">
            <span>{memories.length} 条记忆</span>
            <button className="btn btn-primary btn-sm" onClick={() => { setEditingMem(null); setShowMemDialog(true); }}>
              + 新增
            </button>
          </div>
          {memories.length === 0 ? (
            <div className="kb-empty">暂无记忆，点击「+ 新增」或「🔄 扫描项目」来自动生成</div>
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
                  <span className="kb-date">{new Date(m.created_at).toLocaleDateString()}</span>
                  <div className="kb-actions">
                    <button className="btn btn-sm" onClick={() => { setEditingMem(m); setShowMemDialog(true); }}>编辑</button>
                    <button className="btn btn-sm" onClick={async () => { await memoriesApi.delete(m.id); loadTabData(); }}>删除</button>
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
            <span>{knowledge.length} 条知识</span>
          </div>
          {knowledge.length === 0 ? (
            <div className="kb-empty">暂无知识条目，点击「🔄 扫描项目」来自动索引</div>
          ) : (
            knowledge.map(k => (
              <div key={k.id} className="kb-card">
                <div className="kb-card-header">
                  <span className={`kb-type-badge cat-${k.category}`}>{k.category || 'general'}</span>
                  {!k.is_reviewed && <span className="kb-unreviewed">未Review</span>}
                  {!k.is_approved && <span className="kb-rejected">已驳回</span>}
                  <span className="kb-card-title">{k.title}</span>
                </div>
                <div className="kb-card-content kb-clamp">{stripMarkdownPreview(k.content)}</div>
                <div className="kb-card-meta">
                  <span className="kb-source">来源: {k.source_type}</span>
                  {k.source_ref && <span className="kb-ref">{k.source_ref}</span>}
                  <span className="kb-date">{new Date(k.created_at).toLocaleDateString()}</span>
                  <div className="kb-actions">
                    <button className="btn btn-sm" onClick={async () => { await knowledgeApi.delete(k.id); loadTabData(); }}>删除</button>
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
            第 {reviewIndex + 1} 条 / 共 {reviewItems.length} 条
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
            <button className="btn" onClick={() => handleReview('edit')}>✏️ 编辑后确认</button>
            <button className="btn btn-primary" onClick={() => handleReview('approve')}>✅ 确认</button>
            <button className="btn" onClick={() => {
              if (reviewIndex < reviewItems.length - 1) setReviewIndex(i => i + 1);
            }}>⏭️ 跳过</button>
            <button className="btn" onClick={() => handleReview('reject')} style={{ color: 'var(--color-error)' }}>❌ 拒绝</button>
          </div>
        </div>
      )}
      {!loading && tab === 'review' && reviewItems.length === 0 && (
        <div className="kb-empty">🎉 没有待审核的知识条目</div>
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
        <h3>{memory ? '编辑记忆' : '新增记忆'}</h3>
        <div className="form-group">
          <label>项目</label>
          <select value={projectId} onChange={e => setProjectId(e.target.value)} className="form-input">
            <option value="">选择项目</option>
            {projects.map(p => <option key={p.id} value={p.id}>{p.name}</option>)}
          </select>
        </div>
        <div className="form-group">
          <label>类型</label>
          <select value={type} onChange={e => setType(e.target.value)} className="form-input">
            <option value="business_context">业务背景</option>
            <option value="technical_debt">技术债务</option>
            <option value="design_rationale">设计决策</option>
            <option value="code_explanation">代码说明</option>
          </select>
        </div>
        <div className="form-group">
          <label>标题 (可选)</label>
          <input type="text" value={title} onChange={e => setTitle(e.target.value)} className="form-input" />
        </div>
        <div className="form-group">
          <label>内容</label>
          <textarea value={content} onChange={e => setContent(e.target.value)} className="form-input" rows={4}
            placeholder="例如: Redis 连接池使用 deadpool，最大连接数 20" />
        </div>
        <div className="form-group">
          <label>标签 (逗号分隔)</label>
          <input type="text" value={tags} onChange={e => setTags(e.target.value)} className="form-input"
            placeholder="redis, config" />
        </div>
        <div className="form-actions btn-row-2col">
          <button className="btn" onClick={onClose}>取消</button>
          <button className="btn btn-primary" onClick={handleSave} disabled={saving || !projectId || !content}>
            {saving ? '保存中...' : '保存'}
          </button>
        </div>
      </div>
    </div>
  );
}
