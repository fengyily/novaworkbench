// 日历 —— 日视图。
//
// 顶部跨天延续条区（多日事件未结束的部分）+ 24 小时纵向网格 + 当前时
// 刻线。每条事件按时长定位。垂直 Pointer 拖动改时间（snap 15min），同时
// 保持时长不变。

import { useEffect, useRef } from 'react';
import type { DayBar } from './eventLayout';
import { DAY_PX_PER_HOUR, layoutDay, normalizeAll } from './eventLayout';
import type { Requirement } from '../../api/client';

interface Props {
  day: Date;
  events: Requirement[];
  now: Date;
  onPickEvent: (id: string) => void;
  onMoveEvent: (id: string, minutesDelta: number) => void;
}

export default function DayView({ day, events, now, onPickEvent, onMoveEvent }: Props) {
  const normalized = normalizeAll(events);
  const { bars, continuations } = layoutDay(normalized, day);
  const scrollerRef = useRef<HTMLDivElement>(null);

  // 滚动到当前时刻（8 点附近），每次切换日时重置
  useEffect(() => {
    if (!scrollerRef.current) return;
    scrollerRef.current.scrollTop = Math.max(0, (now.getHours() - 2) * DAY_PX_PER_HOUR);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [day]);

  const dragState = useRef<{ id: string; minutesDelta: number; pointerStartY: number } | null>(null);

  const onBarPointerDown = (e: React.PointerEvent<HTMLDivElement>, bar: DayBar) => {
    if (e.button !== 0) return;
    dragState.current = { id: bar.event.id, minutesDelta: 0, pointerStartY: e.clientY };
    (e.currentTarget as HTMLDivElement).setPointerCapture(e.pointerId);
  };
  const onBarPointerMove = (e: React.PointerEvent<HTMLDivElement>) => {
    if (!dragState.current) return;
    const dy = e.clientY - dragState.current.pointerStartY;
    // snap 15min → dy / (px/min = DAY_PX_PER_MIN) → snap
    const minutes = Math.round((dy / 1.2) / 15) * 15;
    dragState.current.minutesDelta = minutes;
  };
  const onBarPointerUp = (e: React.PointerEvent<HTMLDivElement>, bar: DayBar) => {
    if (!dragState.current) return;
    const delta = dragState.current.minutesDelta;
    dragState.current = null;
    try { (e.currentTarget as HTMLDivElement).releasePointerCapture(e.pointerId); } catch {}
    if (delta !== 0) onMoveEvent(bar.event.id, delta);
  };

  // 当前时刻线：只在「今天」显示
  const isToday = sameYMD(now, day);
  const nowTop = (now.getHours() * 60 + now.getMinutes()) * 1.2;

  return (
    <div className="cal-day">
      {continuations.length > 0 && (
        <div className="cal-day-cont">
          {continuations.map(c => (
            <div
              key={c.event.id}
              className={`cal-day-cont-bar status-${c.event.status}`}
              onClick={() => onPickEvent(c.event.id)}
              title={`${c.event.title} · 跨天（${c.daysBefore} 天前开始）`}
            >
              <span className="cal-bar-clock">🕐</span>
              <span className="cal-bar-title">{c.event.title}</span>
              <span className="cal-day-cont-tag">续</span>
            </div>
          ))}
        </div>
      )}
      <div className="cal-day-scroller" ref={scrollerRef}>
        <div className="cal-day-grid" style={{ height: 24 * DAY_PX_PER_HOUR }}>
          {Array.from({ length: 24 }).map((_, h) => (
            <div key={h} className="cal-day-hour" style={{ top: h * DAY_PX_PER_HOUR, height: DAY_PX_PER_HOUR }}>
              <span className="cal-day-hour-label">{h}:00</span>
            </div>
          ))}
          {bars.map((bar, i) => (
            <div
              key={bar.event.id + i}
              className={`cal-day-bar status-${bar.event.status}`}
              style={{ top: bar.topPx, height: bar.heightPx }}
              onPointerDown={(e) => onBarPointerDown(e, bar)}
              onPointerMove={onBarPointerMove}
              onPointerUp={(e) => onBarPointerUp(e, bar)}
              onClick={() => onPickEvent(bar.event.id)}
              title={`${bar.event.title}${bar.event.hasSchedule ? ' · 🕐 已定时' : ''}`}
            >
              {bar.event.hasSchedule && <span className="cal-bar-clock">🕐</span>}
              <span className="cal-bar-title">{bar.event.title}</span>
            </div>
          ))}
          {isToday && (
            <div className="cal-day-now" style={{ top: nowTop }}>
              <span className="cal-day-now-label">{pad(now.getHours())}:{pad(now.getMinutes())}</span>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

function sameYMD(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear()
    && a.getMonth() === b.getMonth()
    && a.getDate() === b.getDate();
}
function pad(n: number) { return `${n}`.padStart(2, '0'); }