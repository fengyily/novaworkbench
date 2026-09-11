// 时间格式化与日历运算共用工具。
//
// 三个家族：
//  1. 文本渲染：relativeTime / formatDateTime，给列表与详情页用。
//  2. 提交流转：toRFC3339Local，给 datetime-local 提交的「带时区偏移」安全
//     模式（见 ScheduleModal 注释的历史 bug）。
//  3. 日历工具：startOfDay / addDays / sameDay / dateKey / monthMatrix /
//     minutesOfDay / fromUTCKey / toUTCKey —— 给月/日/年三视图与拖拽共用。
//
// 所有日期运算都用原生 Date。月份一律 0-based（Date 默认），渲染时加 1。

// ─────────── 文本渲染 ───────────

// relativeTime 把 ISO 时间字符串渲染成「刚刚 / N 分钟前 / N 小时前 / N 天
// 前 / N 个月前 / N 年前」之类的相对表达，桌面端 + 移动端列表都用。
// 最初从 RequirementsList.tsx 抽出，多页复用（RequirementsList +
// SchedulesPage 等）。
export function relativeTime(s: string): string {
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

// formatDateTime 渲染一个完整的本地时间字符串（YYYY/MM/DD HH:MM）。SchedulesPage
// 的「计划时间」列需要它，比 toLocaleString() 输出更紧凑。
export function formatDateTime(s: string): string {
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return s;
  const pad = (n: number) => `${n}`.padStart(2, '0');
  return `${d.getFullYear()}/${pad(d.getMonth() + 1)}/${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// ─────────── 提交流转 ───────────

// toRFC3339Local: 把 datetime-local 字符串 ("YYYY-MM-DDTHH:MM",
// user-local, no offset) 转成带本地时区偏移的 RFC3339（如
// "...T23:30:00+08:00"）。只有这样后端才不会在 UTC 服务器上把 CST 用户
// 输入的 "9-7 23:30" 解释成 "9-8 7:30"。ScheduleModal 与日历页面的
// RequirementModal 都依赖它。
export function toRFC3339Local(local: string): string {
  const d = new Date(local);
  if (Number.isNaN(d.getTime())) {
    // 原样回传给后端，让解析错误冒出来，避免静默吞掉空 body。
    return local;
  }
  const pad = (n: number) => `${n}`.padStart(2, '0');
  const yyyy = d.getFullYear();
  const mm = pad(d.getMonth() + 1);
  const dd = pad(d.getDate());
  const hh = pad(d.getHours());
  const mi = pad(d.getMinutes());
  const ss = pad(d.getSeconds());
  // getTimezoneOffset() 返回「UTC 西侧分钟数」(CST 为正)。翻号写成
  // 常规的 +HH:MM 形式。
  const offMin = -d.getTimezoneOffset();
  const sign = offMin >= 0 ? '+' : '-';
  const abs = Math.abs(offMin);
  const offH = pad(Math.floor(abs / 60));
  const offM = pad(abs % 60);
  return `${yyyy}-${mm}-${dd}T${hh}:${mi}:${ss}${sign}${offH}:${offM}`;
}

// datetime-local input 需要的本地字符串回写（YYYY-MM-DDTHH:MM）。
// toRFC3339Local 的反向操作，给编辑态回填使用。
export function fromLocalDateTime(d: Date): string {
  const pad = (n: number) => `${n}`.padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// ─────────── 日历工具 ───────────

// 一天的开始（00:00:00.000 本地）。
export function startOfDay(d: Date): Date {
  const x = new Date(d);
  x.setHours(0, 0, 0, 0);
  return x;
}

// d + n 天（n 可负）。返回新 Date。
export function addDays(d: Date, n: number): Date {
  const x = new Date(d);
  x.setDate(x.getDate() + n);
  return x;
}

// d + n 分钟（n 可负）。返回新 Date。给日视图拖拽用。
export function addMinutes(d: Date, n: number): Date {
  return new Date(d.getTime() + n * 60_000);
}

// 同一天（本地日界）。用于比较两个 Date 是否落在同一格。
export function sameDay(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear()
    && a.getMonth() === b.getMonth()
    && a.getDate() === b.getDate();
}

// 一天 0 时至 24 时的分钟偏移。给日视图的纵向定位用。
export function minutesOfDay(d: Date): number {
  return d.getHours() * 60 + d.getMinutes();
}

// "YYYY-MM-DD" 键。日历比较与后端 from/to 参数的统一形式。
export function dateKey(d: Date): string {
  const pad = (n: number) => `${n}`.padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

// 把 dateKey 反解回本地 00:00:00 Date。日历数据可空时（创建时间
// 仅到天）需要给后端补一个本地 00:00；正常时间字符串走 new Date() 即可。
export function fromUTCKey(key: string): Date {
  const [y, m, d] = key.split('-').map(Number);
  return new Date(y, m - 1, d);
}

// 月视图网格：返回 6 周 × 7 天的本地 Date 数组（永远是 42 单元格，便于布局
// 计算）。每周以周一起始。包含给定月份的全部日子 + 首尾填充。
export function monthMatrix(year: number, month0: number): Date[][] {
  const first = new Date(year, month0, 1);
  const start = startOfDay(first);
  // 周一为 1 ... 周日为 0（Date.getDay()）。转换为「周一为 0」：周一=0, ..., 周日=6。
  const weekStartOffset = (start.getDay() + 6) % 7;
  const gridStart = addDays(start, -weekStartOffset);
  const rows: Date[][] = [];
  for (let r = 0; r < 6; r++) {
    const row: Date[] = [];
    for (let c = 0; c < 7; c++) {
      row.push(addDays(gridStart, r * 7 + c));
    }
    rows.push(row);
  }
  return rows;
}

// 一年 12 个月，每月 6 周的首日（用于年视图的迷你月预览）。
export function yearMatrix(year: number): { month: number; matrix: Date[][] }[] {
  const out: { month: number; matrix: Date[][] }[] = [];
  for (let m = 0; m < 12; m++) {
    out.push({ month: m, matrix: monthMatrix(year, m) });
  }
  return out;
}

// 周内 7 天的中文短标签（周一为首）。给日历表头用。
export const WEEK_LABELS = ['一', '二', '三', '四', '五', '六', '日'] as const;

// 中文月份标签（一月/二月/...）。
export const MONTH_LABELS = ['一月', '二月', '三月', '四月', '五月', '六月',
  '七月', '八月', '九月', '十月', '十一月', '十二月'] as const;

// 月份 ≤ 12 / ≤ 31 的天数（闰年 2 月特判）。给日视图跨天滚动与年视图
// 计算用。
export function daysInMonth(year: number, month0: number): number {
  return new Date(year, month0 + 1, 0).getDate();
}