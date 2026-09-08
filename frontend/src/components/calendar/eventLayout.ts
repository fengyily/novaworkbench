// 日历布局纯函数。
//
// 输入是日历数据（Requirement 瘦集合）+ 一个时间窗口 + 一个桶尺寸（行高
// 像素）。输出是已分配的、可直接渲染的「展示项」。所有计算都用原生 Date
// 与纯函数，便于单元测试与组件复用。
//
// 三类展示项：
//  - BarSegment   月视图的「周行内的一段」。跨周事件在每周行内被切成一
//                 段，端点带 roundCorner 标记用于圆角绘制。
//  - DayBar       日视图的「单日事件」。top/height 由时间推算。
//  - DayDot       年视图的「当月某天有需求」。只是色点 + 计数。
//  - OverflowChip 单日溢出 chip：「+N 更多」。
//
// 设计要点：
//  - 跨天泳道分配用贪心 + 按 start 排序：每段扫一遍已分配泳道，找
//    第一个不冲突的；全部冲突则开新泳道。
//  - 永远 42 格月矩阵：不论本月跨度（28/29/30/31）都铺 6×7，避免布局
//    在月切换时跳变（与 Google 日历一致）。
//  - 跨月/跨年的事件由上层组件按月/年分别调用本函数处理（把窗口
//    clamp 到当前月/年）。

import type { Requirement } from '../../api/client';
import {
  addDays,
  dateKey,
  startOfDay,
  minutesOfDay,
} from '../../utils/time';

// ---------- 基础类型 ----------

// 一个「展示锚点」事件：归一化后的 start/end（必填，与后端 planned_*
// 字段缺一不可）、跨天标记、来源 id。纯前端构造，不带任何 React/DOM 依赖。
export interface NormalizedEvent {
  id: string;
  title: string;
  status: string; // status → CSS class
  kind: string;   // kind  → CSS class
  priority: string;
  start: Date;    // 含时刻
  end: Date;      // 含时刻；与 start 一致表示零时长
  multiDay: boolean;
  hasSchedule: boolean; // scheduled_run_at != null → 🕐 icon
}

// 月视图周内的一段。
export interface BarSegment {
  eventId: string;
  // 在周内的「列索引」范围（inclusive）。left=colStart, right=colEnd+1。
  colStart: number; // 0..6
  colEnd: number;   // 0..6, inclusive
  lane: number;     // 该周内分配的泳道（0-based）
  // 真实端点（用于圆角）：是否在周内「这是真实起点 / 终点」。
  // 周一端/周日端即使两端是真实起止，仍可能因两端是周末而触发圆角；
  // 这里只标记「端点是否对应真实事件的开始/结束日」。
  realStart: boolean;
  realEnd: boolean;
  // 事件参考（便于组件跳转 / 弹窗）
  event: NormalizedEvent;
}

// 单日的「可见 N 条 + 溢出」。给月视图的单日单元格用。
export interface DayBucket {
  dayIndex: number;       // 月矩阵中的列下标（0..6）
  visibleSegments: BarSegment[];
  overflow: number;       // 折叠的条数
  // 该日是否至少有 1 条跨周/跨月跨段——若全空则不渲染「+N」。
  overflowEvents: NormalizedEvent[];
}

// 日视图的事件：纵向定位 + 高度。
export interface DayBar {
  event: NormalizedEvent;
  topPx: number;      // 距日视口顶部的像素
  heightPx: number;   // 高度像素
  // 是否跨天（start 不在本日）：置顶「跨天」区域。
  isContinuation: boolean;
}

// 日视图顶部「跨天」区域：跨进当日的多日事件（前一日开始的尾巴）。
export interface DayContinuation {
  event: NormalizedEvent;
  // 真实开始日在「今天之前的几天」，渲染时只显示「…」标题，避免占满当日。
  daysBefore: number;
}

// 年视图：单月单日仅 1 个色点 + 计数。
export interface DayDot {
  day: Date;
  count: number; // 当天有锚定事件的需求数（去重）
}

// ---------- 归一化 ----------

// 后端给的 planned_* 为空时，回退到 created_at；零时长；时区安全。
export function normalizeEvent(r: Requirement): NormalizedEvent {
  const startStr = r.planned_start_at ?? r.created_at;
  const endStr = r.planned_end_at ?? r.planned_start_at ?? r.created_at;
  const start = new Date(startStr);
  // end 允许为空（与 start 共享时刻 = 零时长事件）；fallback 处理让
  // null/undefined/无效都收敛到 start 时刻。
  let end = new Date(endStr);
  if (Number.isNaN(end.getTime())) end = start;
  if (end.getTime() < start.getTime()) end = start;
  const startDay = startOfDay(start);
  const endDay = startOfDay(end);
  const multiDay = startDay.getTime() !== endDay.getTime();
  return {
    id: r.id,
    title: r.title,
    status: r.status,
    kind: r.kind || 'requirement',
    priority: r.priority || 'medium',
    start,
    end,
    multiDay,
    hasSchedule: !!r.scheduled_run_at,
  };
}

export function normalizeAll(rs: Requirement[]): NormalizedEvent[] {
  return rs.map(normalizeEvent);
}

// ---------- 月视图：跨天泳道分配 ----------

// 把事件 clamp 进 [weekStart, weekEnd+1) 后，再切成最多 7 段（每段
// 落在周内 1..N 列）。返回的每个 BarSegment 已经按「周行」归类。
export function layoutWeeks(
  events: NormalizedEvent[],
  weekRows: Date[][], // monthMatrix 输出：6×7
  visibleStart: Date,
  visibleEnd: Date, // 闭区间最后一天（24:00 前）
): BarSegment[][] {
  // weekRows[i] = 第 i 周的 7 天
  const rows: BarSegment[][] = weekRows.map(() => []);
  if (events.length === 0) return rows;

  // 事件按 start 升序；同 start 按 duration 降序（长的先占泳道），保证
  // 视觉稳定（短事件不会把长事件挤下去）。
  const sorted = [...events].sort((a, b) => {
    const d = a.start.getTime() - b.start.getTime();
    if (d !== 0) return d;
    return b.end.getTime() - a.end.getTime();
  });

  for (const ev of sorted) {
    const evStart = ev.start < visibleStart ? visibleStart : ev.start;
    const evEnd = ev.end > visibleEnd ? visibleEnd : ev.end;
    if (evEnd < evStart) continue;

    // 跨哪些周？
    for (let r = 0; r < weekRows.length; r++) {
      const week = weekRows[r];
      const weekStartDay = week[0];
      const weekEndDay = week[6];
      // 不相交就跳过
      if (evEnd < weekStartDay) continue;
      if (evStart > addDays(weekEndDay, 1)) continue;

      // clamp 到本周
      const segStart = evStart > weekStartDay ? evStart : weekStartDay;
      const segEnd = evEnd < weekEndDay ? evEnd : weekEndDay;
      const segStartDay = startOfDay(segStart);
      const segEndDay = startOfDay(segEnd);

      const colStart = week.findIndex(d => dateKey(d) === dateKey(segStartDay));
      const colEnd = week.findIndex(d => dateKey(d) === dateKey(segEndDay));
      if (colStart === -1 || colEnd === -1) continue;

      // 找泳道：扫已分配 lane 的 maxEnd，< segStart 即可复用。
      const existing = rows[r];
      const lanes: Date[] = []; // 每泳道的「最后一个事件的结束日」
      for (const seg of existing) {
        while (lanes.length <= seg.lane) {
          lanes.push(new Date(0));
        }
        const segEndMax = week[seg.colEnd];
        lanes[seg.lane] = segEndMax > lanes[seg.lane] ? segEndMax : lanes[seg.lane];
      }
      let lane = 0;
      while (lane < lanes.length && lanes[lane] >= segStartDay) {
        lane++;
      }

      const realStart = dateKey(evStart) === dateKey(segStartDay);
      const realEnd = dateKey(evEnd) === dateKey(segEndDay);

      rows[r].push({
        eventId: ev.id,
        colStart,
        colEnd,
        lane,
        realStart,
        realEnd,
        event: ev,
      });
    }
  }

  return rows;
}

// 单日桶：把周行 BarSegment 摊到 7 个 dayIndex 上，给单格「+N 更多」用。
export function bucketPerDay(weekRows: BarSegment[][]): DayBucket[][] {
  return weekRows.map(row => {
    const buckets: DayBucket[] = [];
    for (let i = 0; i < 7; i++) {
      buckets.push({ dayIndex: i, visibleSegments: [], overflow: 0, overflowEvents: [] });
    }
    // 排序：按 lane 升序 → 短的先排
    const sorted = [...row].sort((a, b) => a.lane - b.lane);
    for (const seg of sorted) {
      for (let c = seg.colStart; c <= seg.colEnd; c++) {
        const b = buckets[c];
        if (b.visibleSegments.length < MAX_VISIBLE) {
          b.visibleSegments.push(seg);
        } else {
          b.overflow += 1;
          // 仅在该段「唯一身份」上累加，避免连续段被重复计数
          if (!b.overflowEvents.some(e => e.id === seg.eventId)) {
            b.overflowEvents.push(seg.event);
          }
        }
      }
    }
    return buckets;
  });
}

// 单格内最多展示的事件条数。超出后渲染「+N 更多」chip。
export const MAX_VISIBLE = 3;

// ---------- 日视图 ----------

// 输入：NormalizedEvent 集合 + 焦点日；输出：当天的事件（按 start
// 排序）+ 跨天延续（昨日开始、今仍在）。
export function layoutDay(events: NormalizedEvent[], focusDay: Date): {
  bars: DayBar[];
  continuations: DayContinuation[];
} {
  const dayStart = startOfDay(focusDay);
  const dayEnd = addDays(dayStart, 1);
  const bars: DayBar[] = [];
  const continuations: DayContinuation[] = [];
  // 像素比例：1 分钟 = DAY_PX_PER_MIN（外部传或默认）。
  const pxPerMin = DAY_PX_PER_MIN;
  for (const ev of events) {
    const startsBefore = ev.start < dayStart;
    const endsAfter = ev.end >= dayEnd;
    const startsIn = ev.start >= dayStart && ev.start < dayEnd;
    const endsIn = ev.end >= dayStart && ev.end < dayEnd;

    if (!startsIn && !endsIn && !startsBefore && !endsAfter) continue;

    if (startsBefore) {
      // 跨天延续：从昨日开始的尾巴，只渲染在顶部「跨天」区。
      const daysBefore = Math.round((dayStart.getTime() - startOfDay(ev.start).getTime()) / 86_400_000);
      continuations.push({ event: ev, daysBefore });
      continue;
    }

    const barStart = ev.start < dayStart ? dayStart : ev.start;
    const barEnd = ev.end > dayEnd ? dayEnd : ev.end;
    const topPx = (minutesOfDay(barStart) - 0) * pxPerMin;
    const heightPx = Math.max(20, (barEnd.getTime() - barStart.getTime()) / 60_000 * pxPerMin);
    bars.push({
      event: ev,
      topPx,
      heightPx,
      isContinuation: startsBefore && endsAfter,
    });
  }
  // 排序：先按 top，再按 lane 不需要
  bars.sort((a, b) => a.topPx - b.topPx);
  continuations.sort((a, b) => b.daysBefore - a.daysBefore);
  return { bars, continuations };
}

// 日视口每分钟像素。72px/小时 = 1.2 px/min；与「连续」条 24h 纵向刚好。
export const DAY_PX_PER_MIN = 1.2;
export const DAY_PX_PER_HOUR = DAY_PX_PER_MIN * 60;

// ---------- 年视图 ----------

// 12 个迷你月，每天计数。返回二维 [monthIndex][dayIndex] = 计数。
export function layoutYear(events: NormalizedEvent[], year: number): DayDot[][] {
  const out: DayDot[][] = [];
  for (let m = 0; m < 12; m++) {
    const days = new Date(year, m + 1, 0).getDate();
    const dots: DayDot[] = [];
    for (let d = 1; d <= days; d++) {
      dots.push({ day: new Date(year, m, d), count: 0 });
    }
    out.push(dots);
  }
  for (const ev of events) {
    const s = startOfDay(ev.start < new Date(year, 0, 1) ? new Date(year, 0, 1) : ev.start);
    const e = startOfDay(ev.end > new Date(year, 11, 31) ? new Date(year, 11, 31) : ev.end);
    const cursor = new Date(s);
    while (cursor <= e) {
      const m = cursor.getMonth();
      const d = cursor.getDate();
      if (out[m] && out[m][d - 1]) {
        out[m][d - 1].count += 1;
      }
      cursor.setDate(cursor.getDate() + 1);
    }
  }
  return out;
}