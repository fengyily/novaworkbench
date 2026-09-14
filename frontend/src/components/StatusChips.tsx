import { useTranslation } from 'react-i18next';
import { statusChipLabelKeys } from '../api/client';
import type { Requirement } from '../api/client';
import { tLabel } from '../i18n/label';
import { statusChips } from '../utils/statusChips';

/**
 * StatusChips renders one or more status badges for a single requirement row,
 * combining the underlying `status` code with its derived context
 * (skip_design / design_docs / dev_ended_at) into an ordered chip group:
 *
 *   [来源/方案 chip] → [进度 chip] → [待确认 chip]
 *
 * All derivation lives in utils/statusChips; this component is a pure
 * presentation wrapper that maps each chip's code to its i18n label and
 * applies the existing .status-badge.status-* color tokens — no new CSS.
 *
 * className is forwarded to every chip so callers (e.g. the detail header)
 * can layer effects like the wizard "working" pulse.
 */
export interface StatusChipsProps {
  req: Pick<
    Requirement,
    'status' | 'skip_design' | 'design_docs' | 'dev_ended_at'
  >;
  className?: string;
}

export function StatusChips({ req, className }: StatusChipsProps) {
  const { t } = useTranslation();
  const chips = statusChips(req);
  if (chips.length === 0) return null;
  // Wrap in a single inline-flex container so the chip group reads as one
  // ordered "tag string" (源/方案 → 进度 → 待确认) instead of loose chips
  // fighting for the same baseline. .status-chips in index.css owns the gap
  // + nowrap rules; individual chips keep the existing status-* palette.
  // className is forwarded to the container (not the chips) so callers can
  // layer effects on the whole group, e.g. claude-pulse on the detail header.
  return (
    <span
      className={`status-chips${className ? ' ' + className : ''}`}
      role="group"
      aria-label={tLabel(t, statusChipLabelKeys as Record<string, string>, 'status')}
    >
      {chips.map((c, i) => (
        <span
          key={`${c.code}-${i}`}
          className={`status-badge ${c.toneClass}`}
        >
          {tLabel(t, statusChipLabelKeys as Record<string, string>, c.code as string)}
        </span>
      ))}
    </span>
  );
}

export default StatusChips;