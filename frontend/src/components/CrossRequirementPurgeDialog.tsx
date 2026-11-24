// CrossRequirementPurgeDialog — the confirm step for permanently deleting a
// cross-project requirement.
//
// Shared by the list page and the detail page so both speak the SAME
// vocabulary: the same modal frame, the same risk panels, the same verbs.
// Duplicating it per page is how two screens drift apart (and the UI audit
// treats that as a defect, not a style choice).
//
// The two panels are separated on purpose, because they point in OPPOSITE
// directions:
//
//   - 「子需求会保留」 — the part that is NOT lost. Detached from the parent,
//     the children keep their code, branches and PRs.
//   - 「以下内容将被永久删除」 — the part that IS lost: the overall design
//     (including the cross-project interface contract), the integration
//     phases and their reports, the cross-project dependencies, the baseline
//     ledger and every attachment.
//
// The first panel is what makes the button safe to press: the user learns
// that the expensive part (the code) is not on the chopping block.
//
// The confirm button stays disabled until the risk is acknowledged — the same
// pattern BatchDeleteDialog uses for the sub-requirement risk, so "delete a
// batch of requirements" and "delete a cross-project requirement" are one
// action in this product, not two.

import { useTranslation } from 'react-i18next';

export default function CrossRequirementPurgeDialog({
  open,
  busy,
  title,
  childCount,
  ack,
  onAckChange,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  busy: boolean;
  /** The cross requirement's title — shown so the user knows what they are deleting. */
  title: string;
  /** How many sub-requirements survive the purge. */
  childCount: number;
  ack: boolean;
  onAckChange: (v: boolean) => void;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const { t } = useTranslation();
  if (!open) return null;
  return (
    <div className="modal-overlay" onClick={() => { if (!busy) onCancel(); }}>
      <div className="modal-box" role="dialog" aria-modal="true" onClick={e => e.stopPropagation()}>
        <h3 className="modal-title-danger">{t('crossRequirements.trash.purgeTitle')}</h3>
        <div className="modal-confirm-text">
          {t('crossRequirements.trash.purgeConfirmPrefix')} <strong>{title}</strong>
          {t('crossRequirements.trash.purgeConfirmSuffix')}
        </div>

        <div className="modal-risk-panel">
          <div className="modal-risk-title">{t('crossRequirements.trash.keepTitle')}</div>
          <div className="modal-risk-desc">
            {t('crossRequirements.trash.keepDesc', { n: childCount })}
          </div>
        </div>
        <div className="modal-risk-panel">
          <div className="modal-risk-title">{t('crossRequirements.trash.loseTitle')}</div>
          <div className="modal-risk-desc">{t('crossRequirements.trash.loseDesc')}</div>
        </div>

        <label className="modal-risk-check">
          <input
            type="checkbox"
            checked={ack}
            onChange={e => onAckChange(e.target.checked)}
          />
          <span>{t('crossRequirements.trash.purgeAck')}</span>
        </label>

        <div className="modal-actions btn-row-2col">
          <button className="btn" onClick={onCancel} disabled={busy}>
            {t('crossRequirements.trash.cancel')}
          </button>
          <button
            className="btn btn-danger"
            onClick={onConfirm}
            disabled={!ack || busy}
          >
            {busy ? t('crossRequirements.trash.purging') : t('crossRequirements.trash.purge')}
          </button>
        </div>
      </div>
    </div>
  );
}
