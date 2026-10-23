// RequirementBatch — the multi-select + batch-delete surface shared by the
// two requirement lists (cross-project /requirements and the project detail
// tab). It exists as one component because the two lists must speak the SAME
// vocabulary: the same tray, the same count, the same confirm dialog, the same
// words for the destructive action. Duplicating it per page is how those two
// screens drift apart in the first place.
//
// Three pieces, all layout-agnostic so each host decides where they land:
//
//   RowSelectBox / SelectAllBox — the checkbox itself. Every host row here
//     navigates on click and is draggable (drag-to-nest), so the box has to
//     defeat three separate behaviours; see the stopPropagation notes below.
//   BatchBar — the tray. The one deliberately loud element on the page.
//   BatchDeleteDialog — the confirm step.
//
// Selection state lives next door in useRowSelection.ts (a hook in a
// component file would break Vite fast refresh for this module).

import { useEffect, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import { IconTrash } from '../icons';
import './RequirementBatch.css';

/**
 * One checkbox renderer shared by every host (desktop row, desktop header,
 * mobile card). All three hosts behave differently but all three fight back:
 *
 *  - the row / card navigates on click  → stopPropagation on change + click
 *  - the desktop row is draggable        → stopPropagation on mousedown, so
 *    pressing the box never starts a drag-to-nest
 *  - the input itself is a drag handle   → draggable={false}
 */
export function RowSelectBox({
  checked,
  label,
  onToggle,
}: {
  checked: boolean;
  label: string;
  onToggle: () => void;
}) {
  return (
    <input
      type="checkbox"
      className="rb-check"
      checked={checked}
      aria-label={label}
      title={label}
      draggable={false}
      onChange={(e) => { e.stopPropagation(); onToggle(); }}
      onClick={(e) => e.stopPropagation()}
      onMouseDown={(e) => e.stopPropagation()}
    />
  );
}

/** Header checkbox. `indeterminate` is a DOM property, not an attribute. */
export function SelectAllBox({
  checked,
  indeterminate,
  label,
  onToggle,
}: {
  checked: boolean;
  indeterminate: boolean;
  label: string;
  onToggle: () => void;
}) {
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (ref.current) ref.current.indeterminate = indeterminate;
  }, [indeterminate]);
  return (
    <input
      ref={ref}
      type="checkbox"
      className="rb-check"
      checked={checked}
      aria-label={label}
      title={label}
      draggable={false}
      onChange={(e) => { e.stopPropagation(); onToggle(); }}
      onClick={(e) => e.stopPropagation()}
      onMouseDown={(e) => e.stopPropagation()}
    />
  );
}

/**
 * The staging tray — the page's one loud element.
 *
 * Everything else in these lists stays at hairline level: rows are separated by
 * 1px rules, chips are muted, the tree tint is 4% indigo. When a selection
 * exists the bar goes INK: the only dark object on the page. That contrast is
 * the whole point — "armed" has to read in peripheral vision, because the
 * click that follows it destroys data.
 *
 * The count is set in tabular numerals and names BOTH numbers (selected and
 * filtered total). A bare "3 selected" is unverifiable when the list is
 * paginated or tree-collapsed; `3 / 14` is checkable at a glance.
 */
export function BatchBar({
  selectedCount,
  total,
  busy,
  allSelected,
  onSelectAll,
  onClear,
  onDelete,
}: {
  selectedCount: number;
  total: number;
  busy: boolean;
  allSelected: boolean;
  onSelectAll: () => void;
  onClear: () => void;
  onDelete: () => void;
}) {
  const { t } = useTranslation();
  if (selectedCount === 0) return null;
  return (
    <div className="rb-tray" role="region" aria-label={t('requirements.batch.trayLabel')}>
      <div className="rb-tray-count">
        <span className="rb-tray-n">{selectedCount}</span>
        <span className="rb-tray-word">{t('requirements.batch.traySelected')}</span>
        <span className="rb-tray-sep" aria-hidden />
        <span className="rb-tray-total">{t('requirements.batch.trayScope', { total })}</span>
      </div>
      <div className="rb-tray-actions">
        <button type="button" className="rb-tray-btn" onClick={onSelectAll} disabled={busy}>
          {allSelected ? t('requirements.batch.unselectAll') : t('requirements.batch.selectAll')}
        </button>
        <button type="button" className="rb-tray-btn" onClick={onClear} disabled={busy}>
          {t('requirements.batch.clear')}
        </button>
        <button type="button" className="rb-tray-btn rb-tray-btn-danger" onClick={onDelete} disabled={busy}>
          <IconTrash size={13} />
          {busy ? t('requirements.batch.deleting') : t('requirements.batch.delete', { n: selectedCount })}
        </button>
      </div>
    </div>
  );
}

/**
 * Confirm step. Structure is the app-wide modal vocabulary (.modal-overlay /
 * .modal-box / .modal-risk-panel / .btn-row-2col) so "delete a batch of
 * requirements" looks like every other confirmation the user has seen.
 *
 * `childRiskCount` is the number of SUB-requirements that will survive the
 * operation: deleting a master detaches its children and promotes them to top
 * level instead of cascading. That is the most surprising thing these two
 * lists can do, so when the selection touches a parent it gets said out loud
 * and the confirm button stays disabled until it is acknowledged.
 */
export function BatchDeleteDialog({
  open,
  busy,
  count,
  titles,
  childRiskCount,
  ack,
  onAckChange,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  busy: boolean;
  count: number;
  titles: string[];
  childRiskCount: number;
  ack: boolean;
  onAckChange: (v: boolean) => void;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const { t } = useTranslation();
  if (!open) return null;
  const preview = titles.slice(0, 5);
  const rest = count - preview.length;
  return (
    <div className="modal-overlay" onClick={() => { if (!busy) onCancel(); }}>
      <div className="modal-box rb-dialog" role="dialog" aria-modal="true" onClick={e => e.stopPropagation()}>
        <h3 className="modal-title-danger">{t('requirements.batch.dialog.title', { n: count })}</h3>
        <div className="modal-confirm-text">{t('requirements.batch.dialog.body')}</div>

        <ul className="rb-dialog-list">
          {preview.map((title, i) => (
            <li key={`${i}-${title}`} className="rb-dialog-list-item">{title}</li>
          ))}
          {rest > 0 && (
            <li className="rb-dialog-list-more">{t('requirements.batch.dialog.moreTitles', { n: rest })}</li>
          )}
        </ul>

        {childRiskCount > 0 && (
          <div className="modal-risk-panel">
            <div className="modal-risk-title">
              {t('requirements.batch.dialog.childRiskTitle', { n: childRiskCount })}
            </div>
            <div className="modal-risk-desc">{t('requirements.batch.dialog.childRiskDesc')}</div>
          </div>
        )}
        {childRiskCount > 0 && (
          <label className="modal-risk-check">
            <input type="checkbox" checked={ack} onChange={e => onAckChange(e.target.checked)} />
            <span>{t('requirements.batch.dialog.childRiskAck')}</span>
          </label>
        )}

        <div className="modal-actions btn-row-2col">
          <button className="btn" onClick={onCancel} disabled={busy}>
            {t('requirements.batch.dialog.cancel')}
          </button>
          <button
            className="btn btn-danger"
            onClick={onConfirm}
            disabled={busy || (childRiskCount > 0 && !ack)}
          >
            {busy
              ? t('requirements.batch.dialog.deleting')
              : t('requirements.batch.dialog.confirm', { n: count })}
          </button>
        </div>
      </div>
    </div>
  );
}