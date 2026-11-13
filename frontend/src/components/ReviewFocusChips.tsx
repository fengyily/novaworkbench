// ReviewFocusChips — the palette of things to point the reviewer at.
//
// The Review composer's box used to be an empty text field the user had to
// fill from scratch, even though the requirement already carried the goal.
// This component is the other half of that fix: the system offers the focus
// points it can find on its own, and the user curates them.
//
// Two kinds, deliberately different in weight:
//
//   验收点 (criterion) — what the change was supposed to achieve, extracted
//     from the requirement's own description. Quiet: a plain chip with a
//     neutral border, because these are the expected shape of the work.
//
//   疑似缺陷 (defect) — what the project's static checks (go vet / tsc)
//     already flagged. Loud: a monospace file:line in the error colour,
//     because a compiler error is a fact, not a suggestion.
//
// Clicking a chip stamps its text into the box; clicking it again removes
// it. The box stays editable throughout — the chips are a shortcut, not a
// form.

import { useTranslation } from 'react-i18next';
import type { ReviewCandidate } from '../api/client';

interface Props {
  criteria: ReviewCandidate[];
  defects: ReviewCandidate[];
  /** candidate id → true when its text is currently in the box. */
  applied: Record<string, boolean>;
  onToggle: (candidate: ReviewCandidate) => void;
  /** Runs the static check. Null hides the button (already scanned / running). */
  onScan?: () => void;
  scanning?: boolean;
  /** Shown when a scan finished and found nothing. */
  scanEmpty?: boolean;
}

export default function ReviewFocusChips({
  criteria, defects, applied, onToggle, onScan, scanning, scanEmpty,
}: Props) {
  const { t } = useTranslation();

  const hasAny = criteria.length > 0 || defects.length > 0;

  return (
    <div className="sub-review-focus">
      <div className="sub-review-focus-head">
        <span className="sub-review-focus-label">
          {t('components.subTaskPanel.reviewFocusLabel')}
        </span>
        <span className="sub-review-focus-hint" role="note">
          {t('components.subTaskPanel.reviewFocusHint')}
        </span>
        {onScan && (
          <button
            type="button"
            className="btn btn-sm sub-review-focus-scan"
            onClick={onScan}
            disabled={scanning}
          >
            {scanning
              ? t('components.subTaskPanel.reviewScanBusy')
              : t('components.subTaskPanel.reviewScanBtn')}
          </button>
        )}
      </div>

      {criteria.length > 0 && (
        <div className="sub-review-focus-group">
          <span className="sub-review-focus-group-tag">{t('components.subTaskPanel.reviewFocusCriteria')}</span>
          <div className="sub-review-focus-chips">
            {criteria.map((c) => (
              <button
                key={c.id}
                type="button"
                className={`sub-review-chip sub-review-chip-criterion${applied[c.id] ? ' is-applied' : ''}`}
                onClick={() => onToggle(c)}
                title={c.title}
                aria-pressed={!!applied[c.id]}
              >
                {c.title}
              </button>
            ))}
          </div>
        </div>
      )}

      {defects.length > 0 && (
        <div className="sub-review-focus-group">
          <span className="sub-review-focus-group-tag is-defect">
            {t('components.subTaskPanel.reviewFocusDefects')}
          </span>
          <div className="sub-review-focus-chips">
            {defects.map((c) => (
              <button
                key={c.id}
                type="button"
                className={`sub-review-chip sub-review-chip-defect${applied[c.id] ? ' is-applied' : ''}`}
                onClick={() => onToggle(c)}
                title={c.detail ? `${c.detail} — ${c.title}` : c.title}
                aria-pressed={!!applied[c.id]}
              >
                {c.detail && <code className="sub-review-chip-loc">{c.detail}</code>}
                <span className="sub-review-chip-msg">{c.title}</span>
              </button>
            ))}
          </div>
        </div>
      )}

      {scanEmpty && (
        <span className="sub-review-focus-empty" role="note">
          {t('components.subTaskPanel.reviewScanEmpty')}
        </span>
      )}

      {!hasAny && !scanning && !scanEmpty && (
        <span className="sub-review-focus-empty" role="note">
          {t('components.subTaskPanel.reviewFocusNone')}
        </span>
      )}
    </div>
  );
}
