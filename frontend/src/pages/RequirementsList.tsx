// 需求列表（跨项目）
//
// 之前 /requirements 路由指向 PlaceholderPages 占位页，无法创建需求。
// 这里替换为真实列表：
//   1. 顶部有「项目 / 类型 / 状态」三个下拉 + 搜索框
//   2. 表格列：类型标签 / 标题 / 状态 / 项目 / 优先级 / 更新时间
//   3. 右上角放「新建需求」按钮，弹出跨项目版的 CreateRequirementForm
// 类型筛选多选（issue / requirement / idea 三个 toggle）—— 后端 List 支持
// 用逗号分隔的 kind 列表，所以 toggle 集合直接拼成 CSV。

import { useEffect, useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import {
  requirementsApi, projectsApi,
  statusLabels, priorityLabels, kindLabels, kindShortLabels, kindOf,
  type Kind, type Project, type Requirement,
} from '../api/client';
import { CreateRequirementForm } from '../components/CreateRequirementForm/CreateRequirementForm';
import './RequirementsList.css';

const KIND_FILTERS: { value: Kind; label: string; emoji: string }[] = [
  { value: 'issue', label: '问题', emoji: '🐛' },
  { value: 'requirement', label: '需求', emoji: '📋' },
  { value: 'idea', label: '想法', emoji: '💡' },
];

export default function RequirementsList() {
  const navigate = useNavigate();
  const [projects, setProjects] = useState<Project[]>([]);
  const [requirements, setRequirements] = useState<Requirement[]>([]);
  const [loading, setLoading] = useState(false);

  // Filters
  const [projectFilter, setProjectFilter] = useState('');
  const [activeKinds, setActiveKinds] = useState<Set<Kind>>(
    new Set<Kind>(['issue', 'requirement', 'idea']),
  );
  const [statusFilter, setStatusFilter] = useState('');
  const [search, setSearch] = useState('');

  const [showCreate, setShowCreate] = useState(false);

  useEffect(() => {
    projectsApi.list().then(setProjects).catch(() => {});
  }, []);

  // Re-fetch when filters change. kind filter is comma-separated per backend
  // contract (splitKinds() in the Go service); toggling a chip rewrites it.
  useEffect(() => {
    loadRequirements();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectFilter, statusFilter, search]);

  const kindFilterCsv = useMemo(() => {
    if (activeKinds.size === 0 || activeKinds.size === 3) return '';
    return Array.from(activeKinds).join(',');
  }, [activeKinds]);

  useEffect(() => {
    loadRequirements();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [kindFilterCsv]);

  const loadRequirements = async () => {
    setLoading(true);
    try {
      const list = await requirementsApi.list({
        project_id: projectFilter || undefined,
        status: statusFilter || undefined,
        kind: kindFilterCsv || undefined,
      });
      const term = search.trim().toLowerCase();
      setRequirements(
        term
          ? list.filter(r =>
              (r.title || '').toLowerCase().includes(term) ||
              (r.description || '').toLowerCase().includes(term),
            )
          : list,
      );
    } catch (err: any) {
      console.error('load requirements failed', err);
      setRequirements([]);
    } finally {
      setLoading(false);
    }
  };

  const toggleKind = (k: Kind) => {
    setActiveKinds(prev => {
      const next = new Set(prev);
      if (next.has(k)) next.delete(k);
      else next.add(k);
      return next;
    });
  };

  const projectNameOf = (pid: string) =>
    projects.find(p => p.id === pid)?.name || pid;

  return (
    <div className="requirements-list-page">
      <div className="page-header">
        <h2>📋 需求列表</h2>
        <button className="btn btn-primary" onClick={() => setShowCreate(s => !s)}>
          {showCreate ? '收起' : '➕ 新建需求'}
        </button>
      </div>

      {showCreate && (
        <CreateRequirementForm
          projectOptions={projects.map(p => ({ id: p.id, name: p.name }))}
          onClose={() => setShowCreate(false)}
          onCreated={req => {
            setShowCreate(false);
            navigate(`/requirements/${req.id}`);
          }}
        />
      )}

      {/* Filters bar */}
      <div className="req-filter-bar">
        <select
          className="form-input req-filter-select"
          value={projectFilter}
          onChange={e => setProjectFilter(e.target.value)}
        >
          <option value="">全部项目</option>
          {projects.map(p => (
            <option key={p.id} value={p.id}>{p.name}</option>
          ))}
        </select>

        <select
          className="form-input req-filter-select"
          value={statusFilter}
          onChange={e => setStatusFilter(e.target.value)}
        >
          <option value="">全部状态</option>
          {Object.entries(statusLabels).map(([k, v]) => (
            <option key={k} value={k}>{v}</option>
          ))}
        </select>

        <input
          className="form-input req-filter-search"
          placeholder="搜索标题或描述..."
          value={search}
          onChange={e => setSearch(e.target.value)}
        />
      </div>

      {/* Kind filter chips (multi-select) */}
      <div className="req-kind-chips">
        {KIND_FILTERS.map(k => (
          <button
            key={k.value}
            type="button"
            className={`req-kind-chip kind-${k.value}${activeKinds.has(k.value) ? ' active' : ''}`}
            onClick={() => toggleKind(k.value)}
            title={k.label}
          >
            {k.emoji} {k.label}
          </button>
        ))}
        <span className="req-kind-chip-hint">
          {activeKinds.size === 0
            ? '（未选，显示全部）'
            : activeKinds.size === 3
              ? '（显示全部）'
              : `已选 ${activeKinds.size} 类`}
        </span>
      </div>

      {/* Table */}
      <div className="req-table-wrap">
        {loading ? (
          <div className="tab-empty"><p>⏳ 加载中...</p></div>
        ) : requirements.length === 0 ? (
          <div className="tab-empty">
            <p>暂无需求。点击右上角「➕ 新建需求」开始，或切换筛选条件。</p>
          </div>
        ) : (
          <table className="req-table">
            <thead>
              <tr>
                <th>类型</th>
                <th>标题</th>
                <th>状态</th>
                <th>项目</th>
                <th>优先级</th>
                <th>更新时间</th>
              </tr>
            </thead>
            <tbody>
              {requirements.map(r => {
                const k: Kind = kindOf(r);
                return (
                  <tr key={r.id} onClick={() => navigate(`/requirements/${r.id}`)} className="req-row">
                    <td>
                      <span
                        className={`kind-badge kind-${k}`}
                        title={kindLabels[k]}
                      >
                        {kindShortLabels[k]}
                      </span>
                    </td>
                    <td className="req-row-title">
                      {r.title || <em style={{ color: '#94A3B8' }}>（无标题）</em>}
                    </td>
                    <td>{statusLabels[r.status] || r.status}</td>
                    <td>{projectNameOf(r.project_id)}</td>
                    <td>{priorityLabels[r.priority || ''] || r.priority || '-'}</td>
                    <td>{new Date(r.updated_at).toLocaleString()}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}