// 日历 —— 年视图。
//
// 12 个迷你月预览，每个迷你月是 6×7 色点网格。有需求的日子用状态色圆
// 点表示，点数与数量对应。无拖拽；点击「日」→ 切到日视图、点击月头
// → 切到月视图。

import { useMemo } from 'react';
import type { DayDot } from './eventLayout';
import { layoutYear, normalizeAll } from './eventLayout';
import { MONTH_LABELS, WEEK_LABELS, monthMatrix, sameDay } from '../../utils/time';
import type { Requirement } from '../../api/client';

interface Props {
  year: number;
  events: Requirement[];
  today: Date;
  onPickDay: (day: Date) => void;
  onPickMonth: (year: number, month0: number) => void;
}

export default function YearView({ year, events, today, onPickDay, onPickMonth }: Props) {
  const normalized = useMemo(() => normalizeAll(events), [events]);
  const dots = useMemo(() => layoutYear(normalized, year), [normalized, year]);
  const matrices = useMemo(() => {
    return Array.from({ length: 12 }, (_, m) => monthMatrix(year, m));
  }, [year]);

  return (
    <div className="cal-year">
      {Array.from({ length: 12 }, (_, m) => (
        <div key={m} className="cal-year-month">
          <button
            type="button"
            className="cal-year-month-head"
            onClick={() => onPickMonth(year, m)}
            title={`进入 ${MONTH_LABELS[m]} 月视图`}
          >
            {MONTH_LABELS[m]}
          </button>
          <div className="cal-year-grid">
            {WEEK_LABELS.map((w, i) => (
              <div key={`h-${i}`} className="cal-year-head">{w}</div>
            ))}
            {matrices[m].flat().map((day, idx) => {
              const inMonth = day.getMonth() === m;
              const dot: DayDot | undefined = dots[m]?.[day.getDate() - 1];
              const count = dot?.count ?? 0;
              const isToday = sameDay(day, today);
              return (
                <button
                  type="button"
                  key={idx}
                  className={[
                    'cal-year-cell',
                    inMonth ? '' : 'is-other-month',
                    isToday ? 'is-today' : '',
                    count > 0 ? 'has-events' : '',
                  ].filter(Boolean).join(' ')}
                  onClick={() => inMonth && onPickDay(day)}
                  disabled={!inMonth}
                  title={inMonth ? `${day.getMonth() + 1}/${day.getDate()} · ${count} 条` : ''}
                >
                  <span className="cal-year-num">{day.getDate()}</span>
                  {count > 0 && <span className="cal-year-dot" />}
                </button>
              );
            })}
          </div>
        </div>
      ))}
    </div>
  );
}