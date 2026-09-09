// 时间格式化共用工具。

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