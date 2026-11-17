// RequirementMobileCard — the shared phone rendering of a requirement row.
//
// Two surfaces collapse a wide requirement table into a vertical stack of
// cards below 768px: the cross-project list (/requirements) and the project
// detail tab. Both used to carry their own copy of the markup, which is
// exactly how two cards that "look the same" end up not looking the same.
// The card lives here so both hosts share one definition.
//
// The root is a <div role="button">, NOT a <button>: the project tab puts
// the "create a sub-requirement" trigger (a real <button>) inside the card
// title row, and a <button> nested in a <button> is invalid HTML whose
// click the outer button swallows. The root therefore
//   • handles Enter / Space itself (keyboard activation, like a button),
//   • ignores clicks that land on an interactive descendant
//     (button / a / input / textarea / [role=button]), so a nested trigger
//     needs no stopPropagation of its own.
import type { MouseEvent, KeyboardEvent, ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import {
  kindOf, kindLabelKeys, kindShortLabelKeys,
  MARK_PRESETS, parseMarks,
  type Requirement,
} from '../../api/client';
import { tLabel } from '../../i18n/label';
import { StatusChips } from '../StatusChips';
import { fmtDateTime, fmtRelative, fmtRunAtRelative } from '../../utils/intl';
import { IconClock } from '../icons';
import RequirementMarkChips from '../RequirementMarkChips/RequirementMarkChips';
import './RequirementMobileCard.css';

/**
 * Circled digits for the "which child am I" badge. Past ten the glyphs
 * stop being legible, so the badge falls back to a plain number.
 */
const CIRCLED_DIGITS = ['①', '②', '③', '④', '⑤', '⑥', '⑦', '⑧', '⑨', '⑩'];

function ordinalGlyph(n: number): string {
  if (n >= 1 && n <= CIRCLED_DIGITS.length) return CIRCLED_DIGITS[n - 1];
  return `#${n}`;
}

// MarksStrip renders the preset mark chip strip (重要 / 跟进 / 阻塞 / 风险)
// inline with the requirement title. Empty marks → nothing rendered, so
// unmarked cards don't pick up extra spacing. Colors come from
// MARK_PRESETS so the detail-page editor stays in sync.
//
// Exported because the desktop table rows render the same strip inline
// with their title — one helper, both surfaces.
export function MarksStrip({ marks }: { marks: string | undefined }) {
  const { t } = useTranslation();
  const rowMarks = parseMarks(marks);
  if (rowMarks.length === 0) return null;
  return (
    <div className="req-row-marks">
      {rowMarks.map(code => {
        const p = MARK_PRESETS.find(x => x.code === code);
        if (!p) return null;
        const label = t(`requirements.detail2.marksPreset.${p.code}`);
        return (
          <span
            key={code}
            className={`req-row-mark-tag ${p.code}`}
            style={{ color: p.color, background: p.bg, borderColor: p.color }}
            title={label}
          >
            <span aria-hidden>{p.icon}</span>
            <span>{label}</span>
          </span>
        );
      })}
    </div>
  );
}

// ScheduleChip renders the "this requirement is scheduled" pill. Exported
// for the desktop table rows, which show the same affordance inline with
// their title.
export function ScheduleChip({ scheduledRunAt }: { scheduledRunAt?: string }) {
  const { t } = useTranslation();
  if (!scheduledRunAt) return null;
  return (
    <span
      className="schedule-chip"
      title={t('requirements.list.scheduledTooltip', {
        time: fmtDateTime(scheduledRunAt),
        rel: fmtRunAtRelative(scheduledRunAt),
      })}
      aria-label={t('requirements.list.scheduledAria')}
    >
      <IconClock size={12} className="schedule-chip-icon" />
      {fmtRunAtRelative(scheduledRunAt)}
    </span>
  );
}

export interface RequirementMobileCardProps {
  // The requirement the card renders.
  req: Requirement;
  // 0 = top-level row, 1 = sub-requirement. Drives the indent, the group
  // rail and the tinted group background.
  depth?: number;
  // Batch-selection highlight.
  selected?: boolean;
  // Called when the card body itself is activated (tap / Enter / Space).
  onOpen?: (req: Requirement) => void;
  // Called after a preset mark was toggled inside the card and the server
  // confirmed the change. The host patches its own list row so the desktop
  // table — which renders the SAME requirement — stays in sync with the
  // card. Both surfaces mount their own copy of the strip, so neither can
  // see the other's optimistic state.
  onMarkChange?: (updated: Requirement) => void;
  // Sibling rendered BEFORE the card (selection checkbox, tree toggle).
  // Deliberately not a child of the card — see the note at the top.
  leading?: ReactNode;
  // Extra badges rendered BEFORE the kind badge in the header row (the
  // priority dot lives there on the desktop table, next to the badge).
  headLeading?: ReactNode;
  // Extra badges for the header row (split marker, dev-delegated marker…).
  headExtra?: ReactNode;
  // Extra controls for the title row (the "create sub-requirement" trigger).
  titleExtra?: ReactNode;
  // Footer left slot (project name on the cross-project list).
  footStart?: ReactNode;
  // Footer right slot (agent server / tokens / cost on the project tab).
  footExtra?: ReactNode;
  // Relative-time label; defaults to the requirement's updated_at.
  timeLabel?: string;
  // 1-based position of a sub-requirement inside its parent's child list.
  // Rendered as a badge in the header — only meaningful at depth 1.
  childOrdinal?: number;
  // How many lines the title may occupy. The cross-project list keeps the
  // historical 3 lines; the project tab asks for 2 so a screen shows six
  // to eight cards instead of three.
  titleLines?: 2 | 3;
  // Title of the master requirement, rendered as a footer line. Only set
  // when the master is NOT on the current page, so an orphaned child still
  // says who it belongs to.
  parentTitle?: string;
  // id for aria-controls wiring (the tree toggle points at it).
  cardId?: string;
  // Extra modifier classes.
  className?: string;
}

export default function RequirementMobileCard({
  req,
  depth = 0,
  selected = false,
  onOpen,
  onMarkChange,
  leading,
  headLeading,
  headExtra,
  titleExtra,
  footStart,
  footExtra,
  timeLabel,
  childOrdinal,
  parentTitle,
  cardId,
  className,
  titleLines = 3,
}: RequirementMobileCardProps) {
  const { t } = useTranslation();
  const k = kindOf(req);

  const title = req.title || t('requirements.list.noTitle');

  // Clicks on an interactive descendant belong to that descendant: the
  // card must not navigate when the user hits the quick-create "+" or a
  // nested link.
  //
  // The card root itself carries role="button", so `closest()` has to be
  // compared against the root: a click on the title text resolves to the
  // card (the nearest [role=button] ancestor) and MUST still navigate,
  // while a click on a nested control resolves to that control and must
  // not.
  const handleClick = (e: MouseEvent<HTMLDivElement>) => {
    const target = e.target as HTMLElement | null;
    const interactive = target?.closest('button, a, input, textarea, select, [role="button"]');
    if (interactive && interactive !== e.currentTarget) return;
    onOpen?.(req);
  };

  // Keyboard activation only when the card itself has focus — otherwise
  // Enter inside a nested control would both activate that control and
  // navigate away.
  const handleKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.target !== e.currentTarget) return;
    if (e.key !== 'Enter' && e.key !== ' ') return;
    e.preventDefault();
    onOpen?.(req);
  };

  const childOrdinalLabel = childOrdinal
    ? t('requirements.tree.childOrdinal', { n: childOrdinal })
    : '';

  return (
    <div
      className={`req-card-mobile-wrap req-tree-row${depth > 0 ? ' req-card-mobile-child' : ''}${selected ? ' rb-selected' : ''}${className ? ' ' + className : ''}`}
      id={cardId}
    >
      {leading}
      <div
        className={`req-card-mobile kind-${k}${titleLines === 2 ? ' is-title-2line' : ''}`}
        role="button"
        tabIndex={0}
        onClick={handleClick}
        onKeyDown={handleKeyDown}
        aria-label={t('requirements.list.openCardAria', { title })}
      >
        <div className="req-card-mobile-head">
          {headLeading}
          <span className={`kind-badge kind-${k}`} title={tLabel(t, kindLabelKeys, k)}>
            {tLabel(t, kindShortLabelKeys, k)}
          </span>
          <StatusChips req={req} />
          {headExtra}
          {/* Which child of its master is this? The ordinal makes a long
              child list scannable. */}
          {childOrdinal ? (
            <span className="req-card-mobile-ordinal" title={childOrdinalLabel} aria-label={childOrdinalLabel}>
              {ordinalGlyph(childOrdinal)}
            </span>
          ) : null}
        </div>
        {/* Preset marks + schedule chip render inline with the title so a
            marked card stays on one line. The mark strip is INTERACTIVE
            here (tap a chip to remove it, "＋" to add one) — the same
            shared component the desktop table rows use, so the two
            surfaces cannot drift apart. */}
        <div className={`req-card-mobile-title${titleExtra ? ' has-extra' : ''}`}>
          <RequirementMarkChips
            reqId={req.id}
            marks={req.marks}
            status={req.status}
            onChange={onMarkChange}
          />
          <ScheduleChip scheduledRunAt={req.scheduled_run_at} />
          <span className="req-row-title-text-label">{title}</span>
          {titleExtra ? <span className="req-card-mobile-title-extra">{titleExtra}</span> : null}
        </div>
        {/* The master lives on another page (pagination splits the tree) —
            the card has to name its owner or it reads as a top-level
            requirement. */}
        {parentTitle ? (
          <div className="req-card-mobile-parent" title={parentTitle}>
            {t('requirements.tree.parentOf', { title: parentTitle })}
          </div>
        ) : null}
        <div className="req-card-mobile-foot">
          {footStart}
          <span className="req-card-mobile-spacer" />
          {footExtra}
          <span className="req-card-mobile-time">
            {timeLabel ?? fmtRelative(req.updated_at)}
          </span>
        </div>
      </div>
    </div>
  );
}
