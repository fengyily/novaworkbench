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
  // Partial<Pick<…>> rather than Pick<…>: Pick made skip_design / design_docs
  // appear required even though Requirement declares them optional, which
  // blocked callers that only had the live `status` string (e.g. /schedules'
  // LEFT JOIN of requirements.status, no skip_design / design_docs in scope).
  // utils/statusChips.ts already handles every field as optional and falls
  // back to a single-chip default when they're missing, so Partial is the
  // accurate contract.
  req: Partial<
    Pick<Requirement, 'status' | 'skip_design' | 'design_docs' | 'dev_ended_at'>
  >;
  className?: string;
}

export function StatusChips({ req, className }: StatusChipsProps) {
  const { t } = useTranslation();
  // req.status is required by StatusChipsInput even though the outer Pick is
  // Partial; a missing status means there's nothing meaningful to render,
  // so short-circuit with null instead of feeding undefined to statusChips.
  if (!req.status) return null;
  // Spread into a fresh StatusChipsInput so TypeScript narrows req.status
  // to `string` here (it can't carry the narrowing across a function call).
  const status = req.status;
  const chips = statusChips({ ...req, status });
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