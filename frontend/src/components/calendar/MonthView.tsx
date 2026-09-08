// 日历 —— 月视图。
//
// 6×7 网格，事件按周切段、泳道分配。跨天事件渲染为连续横条（真实端点圆
// 角、中间段直通），与 Google 日历一致。单日条数超过 MAX_VISIBLE 时，
// 显示「+N 更多」chip，外部点击展开。拖拽支持：事件条 dragStart 携带
// eventId；日期格 drop 接收并平移 start/end（保持时长）。

import { useMemo } from 'react';
import type { BarSegment, DayBucket } from './eventLayout';
import {
  bucketPerDay,
  layoutWeeks,
  normalizeAll,
} from './eventLayout';
import { dateKey, monthMatrix, WEEK_LABELS } from '../../utils/time';
import type { Requirement } from '../../api/client';

interface Props {
  year: number;
  month0: number;
  events: Requirement[];
  today: Date;
  onPickEvent: (id: string) => void;
  onMoveEvent: (id: string, dayDelta: number) => void;
}

export default function MonthView({
  year, month0, events, today, onPickEvent, onMoveEvent,
}: Props) {
  const matrix = useMemo(() => monthMatrix(year, month0), [year, month0]);
  const normalized = useMemo(() => normalizeAll(events), [events]);
  const visibleStart = matrix[0][0];
  const visibleEnd = matrix[5][6];
  const segRows = useMemo(
    () => layoutWeeks(normalized, matrix, visibleStart, visibleEnd),
    [normalized, matrix, visibleStart, visibleEnd],
  );
  const bucketRows = useMemo(() => bucketPerDay(segRows), [segRows]);
  const todayKey = dateKey(today);
  const totalLanes = useMemo(
    () => Math.max(1, ...segRows.map(row => row.reduce((m, s) => Math.max(m, s.lane + 1), 0))),
    [segRows],
  );

  return (
    <div
      className="cal-month"
      style={{ ['--cal-month-lanes' as any]: totalLanes }}
    >
      <div className="cal-month-head">
        {WEEK_LABELS.map(label => (
          <div key={label} className="cal-month-head-cell">{label}</div>
        ))}
      </div>
      <div className="cal-month-body">
        {matrix.map((row, ri) => (
          <div key={ri} className="cal-month-row">
            {row.map((day, ci) => {
              const k = dateKey(day);
              const buckets = bucketRows[ri][ci];
              const isToday = k === todayKey;
              const isOtherMonth = day.getMonth() !== month0;
              return (
                <div
                  key={k}
                  className={[
                    'cal-month-cell',
                    isToday ? 'is-today' : '',
                    isOtherMonth ? 'is-other-month' : '',
                  ].filter(Boolean).join(' ')}
                  onDragOver={e => { e.preventDefault(); }}
                  onDrop={e => {
                    e.preventDefault();
                    const id = e.dataTransfer.getData('text/req-id');
                    if (!id) return;
                    // 计算天数差：drop cell day - 事件原 start day
                    const ev = normalized.find(x => x.id === id);
                    if (!ev) return;
                    const startDay = new Date(ev.start);
                    startDay.setHours(0, 0, 0, 0);
                    const targetDay = new Date(day);
                    targetDay.setHours(0, 0, 0, 0);
                    const dayDelta = Math.round((targetDay.getTime() - startDay.getTime()) / 86_400_000);
                    if (dayDelta !== 0) onMoveEvent(id, dayDelta);
                  }}
                >
                  <div className="cal-month-cell-head">
                    <span className="cal-month-cell-num">{day.getDate()}</span>
                    {isToday && <span className="cal-today-pill">今天</span>}
                  </div>
                  <div className="cal-month-cell-body">
                    {buckets.visibleSegments.map((seg, idx) => (
                      <BarChip
                      key={`${seg.eventId}-${seg.colStart}-${seg.colEnd}-${idx}`}
                      seg={seg}
                      dayIndex={ci}
                      onPickEvent={onPickEvent}
                    />
                    ))}
                    {buckets.overflow > 0 && (
                      <OverflowChip bucket={buckets} onPickEvent={onPickEvent} />
                    )}
                  </div>
                </div>
              );
            })}
          </div>
        ))}
      </div>
    </div>
  );
}

// 单格内的一条事件。负责把它做成「跨天泳道」：left=colStart/7, right=colEnd/7+1/7。
function BarChip({
  seg, dayIndex, onPickEvent,
}: { seg: BarSegment; dayIndex: number; onPickEvent: (id: string) => void }) {
  // 用 CSS Grid 列绝对定位。7 列网格，每条事件在每格渲染时决定是否显示：
  // 只在「seg.colStart === dayIndex」或「seg.colStart < dayIndex && seg.colEnd >= dayIndex」
  // 且不是最后一段时，绘制起点段；中间段绘制直通段；最后一段绘制圆角。
  const isStart = seg.colStart === dayIndex;
  const isEnd = seg.colEnd === dayIndex;
  const isMid = !isStart && !isEnd;
  // 我们用相对格子容器 col grid：[colStart, colEnd+1)，由 gridColumn 直接铺。
  const span = seg.colEnd - seg.colStart + 1;
  // 只在起点日渲染（最左那格画一整条；后续天在它们自己的格子里画相同的
  //「泳道高条」形成 Google 式直通效果）。
  if (!isStart && !isMid && !isEnd) return null;

  // 计算泳道 top：lane * (CAL_LANE_HEIGHT + gap)
  const top = `calc(${seg.lane} * var(--cal-lane-h, 18px))`;
  const classes = [
    'cal-bar',
    `status-${seg.event.status}`,
    isStart ? 'is-start' : '',
    isEnd ? 'is-end' : '',
    isMid ? 'is-mid' : '',
    seg.event.multiDay ? 'is-multiday' : '',
    seg.event.hasSchedule ? 'has-schedule' : '',
  ].filter(Boolean).join(' ');

  return (
    <div
      className={classes}
      style={{
        gridColumn: `${seg.colStart + 1} / span ${span}`,
        top,
        borderRadius: !seg.event.multiDay
          ? 'var(--radius-sm)'
          : (isStart
              ? 'var(--radius-sm) 0 0 var(--radius-sm)'
              : isEnd
                ? '0 var(--radius-sm) var(--radius-sm) 0'
                : '0'),
      }}
      onClick={(e) => { e.stopPropagation(); onPickEvent(seg.eventId); }}
      draggable
      onDragStart={(e) => {
        e.dataTransfer.setData('text/req-id', seg.eventId);
        e.dataTransfer.effectAllowed = 'move';
      }}
      title={`${seg.event.title}${seg.event.hasSchedule ? ' · 🕐 已定时' : ''}`}
    >
      <span className="cal-bar-dot" />
      {seg.event.hasSchedule && <span className="cal-bar-clock" aria-label="已定时">🕐</span>}
      <span className="cal-bar-title">{seg.event.title}</span>
    </div>
  );
}

// 「+N 更多」chip + popover。popover 用原生 <details> 不引外部依赖。
function OverflowChip({
  bucket, onPickEvent,
}: { bucket: DayBucket; onPickEvent: (id: string) => void }) {
  return (
    <details className="cal-overflow">
      <summary className="cal-overflow-summary">+{bucket.overflow} 更多</summary>
      <div className="cal-overflow-list">
        {bucket.overflowEvents.map(ev => (
          <div
            key={ev.id}
            className={`cal-overflow-row status-${ev.status}`}
            onClick={() => onPickEvent(ev.id)}
          >
            <span className="cal-bar-dot" />
            <span className="cal-overflow-title">{ev.title}</span>
            {ev.hasSchedule && <span className="cal-bar-clock">🕐</span>}
          </div>
        ))}
      </div>
    </details>
  );
}