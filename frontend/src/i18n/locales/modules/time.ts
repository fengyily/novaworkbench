// time — relative/absolute time phrasing. Values are consumed by
// utils/intl.ts (fmtRelative / fmtDateTime); the interpolation variables are
// filled in code, not by i18next, because these helpers run outside React.
export const time = {
  justNow: '刚刚',
  minutesAgo: '{{n}} 分钟前',
  hoursAgo: '{{n}} 小时前',
  daysAgo: '{{n}} 天前',
  monthsAgo: '{{n}} 个月前',
  yearsAgo: '{{n}} 年前',
  soon: '即将',
  inMinutes: '{{n}} 分钟后',
  inHours: '{{n}} 小时后',
  inDays: '{{n}} 天后',
  inMonths: '{{n}} 个月后',
} as const;

export default time;
