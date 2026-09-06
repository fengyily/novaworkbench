// 需求列表（跨项目）
//
// 之前 /requirements 路由指向 PlaceholderPages 占位页，无法创建需求。
// 这里替换为真实列表：
//   1. 顶部有「项目 / 类型 / 状态」三个下拉 + 搜索框
//   2. 桌面端用表格展示（类型标签 / 标题 / 状态 / 项目 / 优先级 / 更新时间）
//   3. 移动端用卡片列表展示（解决表格在窄屏被挤成乱码的问题）
//   4. 右上角放「新建需求」按钮，弹出跨项目版的 CreateRequirementForm
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

// 表格 / 卡片通用的「相对时间」格式化。卡片在移动端用这个更易读，桌面
// 端表格保留 toLocaleString() 的完整时间戳。
function relativeTime(s: string): string {
  const t = new Date(s).getTime();
  if (Number.isNaN(t)) return s;
  const diff = Math.max(0, Date.now() - t);
  const min = Math.floor(diff / 60_000);
  if (min < 1) return '刚刚';
  if (min < 60) return `${min} 分钟前`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr} 小时前`;
  const day = Math.floor(hr / 24);
  if (day < 30) return `${day} 天前`;
  const mo = Math.floor(day / 30);
  if (mo < 12) return `${mo} 个月前`;
  const yr = Math.floor(mo / 12);
  return `${yr} 年前`;
}

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

  // The desktop "新建需求" button and the mobile FAB trigger the same
  // handler — one lives in the page header (desktop), one floats above
  // the tab bar (mobile). The desktop-only / fab rules in the CSS swap
  // their visibility at the 768px breakpoint so each surface is only
  // shown in its lane.
  const openCreate = () => setShowCreate(s => !s);

  return (
    <div className="requirements-list-page">
      <div className="page-header">
        <h2>📋 需求列表</h2>
        <button
          className="btn btn-primary desktop-only"
          onClick={openCreate}
        >
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

      {/* Table (desktop view). `table-cards` is a no-op class on desktop
          (the rules only kick in below 768px); on mobile the table is
          hidden via .req-table { display: none } and the dedicated
          card list below takes over. data-label on each <td> is the
          fallback for environments that fall back to the table-cards
          utility (e.g. width between 720-768px). */}
      <div className="req-table-wrap desktop-only">
        {loading ? (
          <div className="tab-empty"><p>⏳ 加载中...</p></div>
        ) : requirements.length === 0 ? (
          <div className="tab-empty">
            <p>暂无需求。点击右上角「➕ 新建需求」开始，或切换筛选条件。</p>
          </div>
        ) : (
          <table className="req-table table-cards">
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
                    <td data-label="类型">
                      <span
                        className={`kind-badge kind-${k}`}
                        title={kindLabels[k]}
                      >
                        {kindShortLabels[k]}
                      </span>
                    </td>
                    <td className="req-row-title" data-label="标题">
                      {r.title || <em style={{ color: '#94A3B8' }}>（无标题）</em>}
                    </td>
                    <td data-label="状态">
                      <span className={`status-badge status-${r.status}`}>
                        {statusLabels[r.status] || r.status}
                      </span>
                    </td>
                    <td data-label="项目">{projectNameOf(r.project_id)}</td>
                    <td data-label="优先级">
                      {r.priority ? (
                        <span className={`priority-tag priority-${r.priority}`}>
                          {priorityLabels[r.priority] || r.priority}
                        </span>
                      ) : (
                        <span className="req-row-dim">-</span>
                      )}
                    </td>
                    <td data-label="更新时间">{new Date(r.updated_at).toLocaleString()}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>

      {/* Mobile-only card list. Each card surfaces the kind + status as
          prominent badges, the title as the headline, then a compact
          footer with the project, priority and relative time. The whole
          card is a tappable row that navigates to the detail page. */}
      <div className="req-cards-mobile mobile-only">
        {loading ? (
          <div className="mobile-empty">
            <span className="mobile-empty-mark">⏳</span>
            <div className="mobile-empty-title">加载中...</div>
          </div>
        ) : requirements.length === 0 ? (
          <div className="mobile-empty">
            <span className="mobile-empty-mark">📋</span>
            <div className="mobile-empty-title">还没有需求</div>
            <p className="mobile-empty-desc">
              点击右下角「新建需求」开始，或切换筛选条件查看其它项目。
            </p>
          </div>
        ) : (
          requirements.map(r => {
            const k: Kind = kindOf(r);
            const projectName = projectNameOf(r.project_id);
            return (
              <button
                key={r.id}
                type="button"
                className="req-card-mobile"
                onClick={() => navigate(`/requirements/${r.id}`)}
              >
                <div className="req-card-mobile-head">
                  <span className={`kind-badge kind-${k}`} title={kindLabels[k]}>
                    {kindShortLabels[k]}
                  </span>
                  <span className={`status-badge status-${r.status}`}>
                    {statusLabels[r.status] || r.status}
                  </span>
                </div>
                <div className="req-card-mobile-title">
                  {r.title || <em style={{ color: '#94A3B8' }}>（无标题）</em>}
                </div>
                <div className="req-card-mobile-foot">
                  <span className="req-card-mobile-project" title={projectName}>
                    📁 {projectName}
                  </span>
                  <span className="req-card-mobile-spacer" />
                  {r.priority && (
                    <span className={`priority-dot priority-${r.priority}`} title={`优先级: ${priorityLabels[r.priority] || r.priority}`} />
                  )}
                  <span className="req-card-mobile-time">{relativeTime(r.updated_at)}</span>
                </div>
              </button>
            );
          })
        )}
      </div>

      {/* Mobile FAB — mirrors the desktop "+ 新建需求" button. The label
          tells users at a glance what the floating button does. On
          desktop it's hidden by .fab's display:none rule. */}
      <button
        type="button"
        className="fab fab-extended"
        aria-label="新建需求"
        onClick={openCreate}
      >
        <span>＋</span>
        <span>新建需求</span>
      </button>
    </div>
  );
}
