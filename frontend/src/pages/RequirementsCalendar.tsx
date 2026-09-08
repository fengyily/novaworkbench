// 需求日历视图页（/requirements/calendar）。
//
// 工具栏：今天 / ‹ › / 当前月份标题 / 日·月·年 segmented + 返回列表。
// 过滤条：项目下拉 + 类型 chips + 标题搜索（客户端本地过滤，与列表页
// 一致）。视图分发到 MonthView / DayView / YearView。
//
// 拖拽：月视图 = 平移 dayDelta；日视图 = 平移 minutesDelta（snap 15min）。
// 乐观更新：先本地合 planned_* 再 PATCH，失败回滚 + toast。

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import {
  requirementsApi,
  projectsApi,
  type Project,
  type Requirement,
} from '../api/client';
import {
  addDays,
  addMinutes,
  dateKey,
  MONTH_LABELS,
  startOfDay,
} from '../utils/time';
import MonthView from '../components/calendar/MonthView';
import DayView from '../components/calendar/DayView';
import YearView from '../components/calendar/YearView';
import RequirementModal from '../components/calendar/RequirementModal';
import './RequirementsCalendar.css';

type ViewMode = 'day' | 'month' | 'year';
type Kind = Requirement['kind'];

const KIND_FILTERS: { value: Kind; label: string }[] = [
  { value: 'issue', label: '🐛 问题' },
  { value: 'requirement', label: '📋 需求' },
  { value: 'idea', label: '💡 想法' },
];

export default function RequirementsCalendar() {
  const navigate = useNavigate();
  const [view, setView] = useState<ViewMode>('month');
  const [focus, setFocus] = useState(() => startOfDay(new Date()));
  const [projects, setProjects] = useState<Project[]>([]);
  const [events, setEvents] = useState<Requirement[]>([]);
  const [loading, setLoading] = useState(false);
  const [projectFilter, setProjectFilter] = useState('');
  const [activeKinds, setActiveKinds] = useState<Set<Kind>>(
    new Set<Kind>(['issue', 'requirement', 'idea']),
  );
  const [search, setSearch] = useState('');
  const [modalFor, setModalFor] = useState<Requirement | null>(null);
  const toastTimer = useRef<number | null>(null);
  const [toast, setToast] = useState<string>('');

  const showToast = useCallback((msg: string) => {
    setToast(msg);
    if (toastTimer.current) window.clearTimeout(toastTimer.current);
    toastTimer.current = window.setTimeout(() => setToast(''), 2400);
  }, []);

  // 计算窗口范围
  const range = useMemo(() => {
    if (view === 'year') {
      const y = focus.getFullYear();
      return {
        from: `${y}-01-01`,
        to: `${y + 1}-01-01`,
      };
    }
    if (view === 'month') {
      const y = focus.getFullYear();
      const m = focus.getMonth();
      const first = new Date(y, m, 1);
      const next = new Date(y, m + 1, 1);
      return { from: dateKey(first), to: dateKey(next) };
    }
    // day
    const d = startOfDay(focus);
    return { from: dateKey(d), to: dateKey(addDays(d, 1)) };
  }, [view, focus]);

  // 客户端过滤：标题/描述子串（与列表页一致）
  const kindCsv = useMemo(() => {
    if (activeKinds.size === 0 || activeKinds.size === 3) return '';
    return Array.from(activeKinds).join(',');
  }, [activeKinds]);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const list = await requirementsApi.calendar({
        from: range.from,
        to: range.to,
        project_id: projectFilter || undefined,
        kind: kindCsv || undefined,
      });
      const term = search.trim().toLowerCase();
      setEvents(
        term
          ? list.filter(r =>
              (r.title || '').toLowerCase().includes(term) ||
              (r.description || '').toLowerCase().includes(term),
            )
          : list,
      );
    } catch (err: any) {
      showToast('加载失败：' + (err?.message || ''));
      setEvents([]);
    } finally {
      setLoading(false);
    }
  }, [range.from, range.to, projectFilter, kindCsv, search, showToast]);

  useEffect(() => {
    projectsApi.list().then(setProjects).catch(() => {});
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // 拖拽：月视图平移
  const moveEventDays = useCallback(async (id: string, dayDelta: number) => {
    const target = events.find(e => e.id === id);
    if (!target) return;
    const startISO = target.planned_start_at ?? target.created_at;
    const endISO = target.planned_end_at ?? target.planned_start_at ?? target.created_at;
    const newStart = addDays(new Date(startISO), dayDelta).toISOString();
    const newEnd = addDays(new Date(endISO), dayDelta).toISOString();
    const prev = events;
    setEvents(curr => curr.map(e => e.id === id ? {
      ...e,
      planned_start_at: newStart,
      planned_end_at: newEnd,
    } : e));
    try {
      const next = await requirementsApi.updateSchedule(id, {
        planned_start_at: newStart,
        planned_end_at: newEnd,
      });
      setEvents(curr => curr.map(e => e.id === id ? { ...e, ...next } : e));
    } catch (err: any) {
      setEvents(prev);
      showToast('拖拽失败：' + (err?.message || ''));
    }
  }, [events, showToast]);

  // 拖拽：日视图平移（snap 15min）
  const moveEventMinutes = useCallback(async (id: string, minutesDelta: number) => {
    const target = events.find(e => e.id === id);
    if (!target) return;
    const startISO = target.planned_start_at ?? target.created_at;
    const endISO = target.planned_end_at ?? target.planned_start_at ?? target.created_at;
    const newStart = addMinutes(new Date(startISO), minutesDelta).toISOString();
    const newEnd = addMinutes(new Date(endISO), minutesDelta).toISOString();
    const prev = events;
    setEvents(curr => curr.map(e => e.id === id ? {
      ...e,
      planned_start_at: newStart,
      planned_end_at: newEnd,
    } : e));
    try {
      const next = await requirementsApi.updateSchedule(id, {
        planned_start_at: newStart,
        planned_end_at: newEnd,
      });
      setEvents(curr => curr.map(e => e.id === id ? { ...e, ...next } : e));
    } catch (err: any) {
      setEvents(prev);
      showToast('拖拽失败：' + (err?.message || ''));
    }
  }, [events, showToast]);

  const shiftFocus = (delta: number) => {
    setFocus(curr => {
      if (view === 'year') return new Date(curr.getFullYear() + delta, curr.getMonth(), 1);
      if (view === 'month') return new Date(curr.getFullYear(), curr.getMonth() + delta, 1);
      return addDays(curr, delta);
    });
  };

  const gotoToday = () => setFocus(startOfDay(new Date()));

  const headerLabel = (() => {
    if (view === 'year') return `${focus.getFullYear()} 年`;
    if (view === 'month') return `${focus.getFullYear()} 年 ${MONTH_LABELS[focus.getMonth()]}`;
    return `${focus.getFullYear()}-${pad(focus.getMonth() + 1)}-${pad(focus.getDate())}`;
  })();

  const projectNameOf = (pid: string) => projects.find(p => p.id === pid)?.name || pid;

  return (
    <div className="cal-page">
      <header className="page-header cal-page-head">
        <h2>📅 需求日历</h2>
        <div className="cal-view-toggle desktop-only">
          {(['day', 'month', 'year'] as ViewMode[]).map(v => (
            <button
              key={v}
              className={`cal-view-toggle-btn ${view === v ? 'active' : ''}`}
              onClick={() => setView(v)}
            >
              {v === 'day' ? '日' : v === 'month' ? '月' : '年'}
            </button>
          ))}
        </div>
        <button
          className="btn btn-primary desktop-only"
          onClick={() => navigate('/requirements')}
        >
          📋 返回列表
        </button>
      </header>

      <div className="cal-toolbar">
        <button className="btn btn-sm" onClick={gotoToday}>今天</button>
        <button className="btn btn-icon" onClick={() => shiftFocus(-1)} aria-label="上一步">‹</button>
        <button className="btn btn-icon" onClick={() => shiftFocus(1)} aria-label="下一步">›</button>
        <span className="cal-toolbar-title">{headerLabel}</span>
        {loading && <span className="cal-toolbar-loading">加载中…</span>}
        {/* 移动端视图切换放工具栏 */}
        <div className="cal-view-toggle mobile-only">
          {(['day', 'month', 'year'] as ViewMode[]).map(v => (
            <button
              key={v}
              className={`cal-view-toggle-btn ${view === v ? 'active' : ''}`}
              onClick={() => setView(v)}
            >
              {v === 'day' ? '日' : v === 'month' ? '月' : '年'}
            </button>
          ))}
        </div>
      </div>

      <div className="req-filter-bar cal-filter-bar">
        <select
          className="req-filter-select"
          value={projectFilter}
          onChange={e => setProjectFilter(e.target.value)}
          aria-label="按项目过滤"
        >
          <option value="">全部项目</option>
          {projects.map(p => (
            <option key={p.id} value={p.id}>{p.name}</option>
          ))}
        </select>
        <div className="req-kind-chips" role="group" aria-label="按类型过滤">
          {KIND_FILTERS.map(k => (
            <button
              key={k.value}
              className={`req-kind-chip ${activeKinds.has(k.value) ? 'active' : ''}`}
              onClick={() => {
                setActiveKinds(prev => {
                  const next = new Set(prev);
                  if (next.has(k.value)) next.delete(k.value);
                  else next.add(k.value);
                  return next;
                });
              }}
            >
              {k.label}
            </button>
          ))}
        </div>
        <input
          className="req-filter-search"
          placeholder="搜索标题…"
          value={search}
          onChange={e => setSearch(e.target.value)}
        />
      </div>

      {view === 'month' && (
        <MonthView
          year={focus.getFullYear()}
          month0={focus.getMonth()}
          events={events}
          today={new Date()}
          onPickEvent={(id) => {
            const r = events.find(e => e.id === id);
            if (r) setModalFor(r);
          }}
          onMoveEvent={moveEventDays}
        />
      )}
      {view === 'day' && (
        <DayView
          day={focus}
          events={events}
          now={new Date()}
          onPickEvent={(id) => {
            const r = events.find(e => e.id === id);
            if (r) setModalFor(r);
          }}
          onMoveEvent={moveEventMinutes}
        />
      )}
      {view === 'year' && (
        <YearView
          year={focus.getFullYear()}
          events={events}
          today={new Date()}
          onPickDay={(d) => {
            setFocus(startOfDay(d));
            setView('day');
          }}
          onPickMonth={(y, m) => {
            setFocus(new Date(y, m, 1));
            setView('month');
          }}
        />
      )}

      {modalFor && (
        <RequirementModal
          open
          requirement={modalFor}
          onClose={() => setModalFor(null)}
          onSaved={(next) => {
            setEvents(curr => curr.map(e => e.id === next.id ? next : e));
            showToast('已保存');
          }}
        />
      )}

      {toast && <div className="cal-toast">{toast}</div>}

      {/* 用于语义化：当月没有需求时给出明确指引 */}
      {!loading && events.length === 0 && (
        <div className="cal-empty">
          <p>📭 这个时间段还没有需求。</p>
          <button className="btn btn-primary" onClick={() => navigate('/requirements')}>
            去新建一个需求
          </button>
        </div>
      )}
      {/* projectNameOf 暴露给可能的详情展示（暂未使用，预留给日历信息条） */}
      <span hidden>{projectNameOf('')}</span>
    </div>
  );
}

function pad(n: number) { return `${n}`.padStart(2, '0'); }