// time (en-US) — must mirror modules/time.ts key-for-key.
export const time = {
  justNow: 'just now',
  minutesAgo: '{{n}} min ago',
  hoursAgo: '{{n}} h ago',
  daysAgo: '{{n}} d ago',
  monthsAgo: '{{n}} mo ago',
  yearsAgo: '{{n}} yr ago',
  soon: 'soon',
  inMinutes: 'in {{n}} min',
  inHours: 'in {{n}} h',
  inDays: 'in {{n}} d',
  inMonths: 'in {{n}} mo',
} as const;

export default time;
