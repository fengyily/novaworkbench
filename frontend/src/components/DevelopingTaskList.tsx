/**
 * DevelopingTaskList — floating sidebar shown on the LEFT of the
 * developing stage. Renders a compact summary card per task (the main
 * coding job + each sub-task) plus a collapse/expand toggle that turns
 * the list into a narrow vertical rail.
 *
 * Behaviour
 *   - Click a card → onSelect(key). The parent (DevelopingStage) updates
 *     `selectedKey` and the shared JobLogView rerenders the chosen task's
 *     log lines.
 *   - The "⟷" toggle flips the collapsed state in the parent. Localstorage
 *     persistence is handled by the parent (this component is presentational).
 *   - Cards show a 16-char truncated title in expanded mode, the full
 *     title in the tooltip; in collapsed (rail) mode the title is rotated
 *     90° via writing-mode.
 *   - Status badges use the same chip classes as SubTaskCard
 *     (sub-card-status-*) so the visual treatment stays consistent.
 *
 * Sub-task selection is also forwarded into the SubTaskPanel (via the
 * `selectedKey / onSelect` props) so the per-card "is-selected" highlight
 * tracks the same source of truth.
 */

import { useTranslation } from 'react-i18next';
import type { SubTask, SubTaskStatus } from '../api/client';
import { IconListOrdered, IconChevronRight, IconArrowLeft } from './icons';

export type TaskKey = 'main' | string;

export interface MainTaskSummary {
  jobId: string | null;
  // Status drives the same chip palette the sub-task cards use.
  status: 'running' | 'done' | 'error' | 'pending' | 'idle' | 'stopped';
  // Display title for the main task card. Defaults to "主任务" when empty.
  title: string;
}

export interface DevelopingTaskListProps {
  main: MainTaskSummary;
  items: SubTask[];
  selectedKey: TaskKey;
  onSelect: (key: TaskKey) => void;
  collapsed: boolean;
  onToggleCollapsed: () => void;
}

// truncate keeps the card title at a predictable width in expanded mode so
// the floating list never wraps past its column boundary. Hover surfaces
// the full title via the native `title` attribute.
function truncate(s: string, max: number): string {
  if (!s) return '';
  if (s.length <= max) return s;
  return s.slice(0, max) + '…';
}

// statusChipClass mirrors the per-card chip styling inside SubTaskPanel so
// the floating sidebar renders the same color treatment without coupling
// to SubTaskPanel internals.
const statusChipClass: Record<SubTaskStatus, string> = {
  pending: 'sub-card-status-chip sub-card-status-pending',
  running: 'sub-card-status-chip sub-card-status-running',
  done:    'sub-card-status-chip sub-card-status-done',
  error:   'sub-card-status-chip sub-card-status-error',
  stopped: 'sub-card-status-chip sub-card-status-stopped',
};

const mainChipClass: Record<MainTaskSummary['status'], string> = {
  running: 'sub-card-status-chip sub-card-status-running',
  done:    'sub-card-status-chip sub-card-status-done',
  error:   'sub-card-status-chip sub-card-status-error',
  pending: 'sub-card-status-chip sub-card-status-pending',
  idle:    'sub-card-status-chip sub-card-status-pending',
  stopped: 'sub-card-status-chip sub-card-status-stopped',
};

export function DevelopingTaskList({
  main,
  items,
  selectedKey,
  onSelect,
  collapsed,
  onToggleCollapsed,
}: DevelopingTaskListProps) {
  const { t } = useTranslation();
  const totalTasks = 1 + items.length;
  return (
    <aside
      className={`dev-task-list${collapsed ? ' is-collapsed' : ''}`}
      aria-label={t('components.devStage.title')}
    >
      <header className="dev-task-list-header">
        {!collapsed && (
          <span className="dev-task-list-title">
            <IconListOrdered size={14} className="icon-mr" />
            {t('components.devStage.title')}
          </span>
        )}
        <button
          type="button"
          className="dev-task-list-toggle"
          onClick={onToggleCollapsed}
          title={collapsed ? t('components.devStage.expand') : t('components.devStage.collapse')}
          aria-label={collapsed ? t('components.devStage.expand') : t('components.devStage.collapse')}
          aria-expanded={!collapsed}
        >
          {collapsed ? <IconChevronRight size={14} /> : <IconArrowLeft size={14} />}
        </button>
      </header>

      {!collapsed && (
        <div className="dev-task-list-meta">
          {t('components.devStage.taskCount', { n: totalTasks })}
        </div>
      )}

      <ul className="dev-task-list-items" role="list">
        <li
          className={`dev-task-card dev-task-card-main${selectedKey === 'main' ? ' is-selected' : ''}`}
          title={main.title || t('components.devStage.mainTask')}
        >
          <button
            type="button"
            className="dev-task-card-btn"
            onClick={() => onSelect('main')}
            aria-pressed={selectedKey === 'main'}
          >
            <span className={mainChipClass[main.status]}>
              {collapsed ? '' : t('components.devStage.mainTask')}
            </span>
            <span className="dev-task-card-title">
              {collapsed
                ? truncate(main.title || t('components.devStage.mainTask'), 16)
                : truncate(main.title || t('components.devStage.mainTask'), 16)}
            </span>
          </button>
        </li>
        {items.length === 0 && !collapsed && (
          <li className="dev-task-list-empty">{t('components.devStage.empty')}</li>
        )}
        {items.map((st) => (
          <li
            key={st.id}
            className={`dev-task-card${selectedKey === st.id ? ' is-selected' : ''}`}
            title={st.title}
          >
            <button
              type="button"
              className="dev-task-card-btn"
              onClick={() => onSelect(st.id)}
              aria-pressed={selectedKey === st.id}
            >
              <span className={statusChipClass[st.status]}>
                {collapsed ? '' : t(`components.devStage.${st.status === 'running' ? 'running' : st.status}`)}
              </span>
              <span className="dev-task-card-title">
                {truncate(st.title || st.id, 16)}
              </span>
            </button>
          </li>
        ))}
      </ul>
    </aside>
  );
}

export default DevelopingTaskList;