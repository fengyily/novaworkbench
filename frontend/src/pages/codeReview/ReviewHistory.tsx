// ReviewHistory — every review this project has run, newest first.
//
// Blank drafts are hidden rather than listed. Removing the manual
// 「创建草稿」 step means a row now exists as soon as someone picks a branch,
// so an abandoned composer leaves a draft with no description, no
// attachments and no report. Listing those would bury the real history under
// rows that say nothing; they are counted instead, with one button to clear
// them out (see shared.isBlankDraft).

import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import type { CodeReview } from '../../api/client';
import { fmtDateTime } from '../../utils/intl';
import { SEVERITIES, isBlankDraft, severityCount } from './shared';

interface Props {
  history: CodeReview[];
  selectedId: string;
  running: boolean;
  onOpen: (rv: CodeReview) => void;
  onRerun: (rv: CodeReview) => void;
  onDelete: (rv: CodeReview) => void;
  onPurgeBlankDrafts: (drafts: CodeReview[]) => void;
}

export default function ReviewHistory({
  history, selectedId, running, onOpen, onRerun, onDelete, onPurgeBlankDrafts,
}: Props) {
  const { t } = useTranslation();

  const { rows, blanks } = useMemo(() => ({
    rows: history.filter(rv => !isBlankDraft(rv)),
    blanks: history.filter(isBlankDraft),
  }), [history]);

  const statusLabel = (s: CodeReview['status']) =>
    t(`projects.detail.reviewStatus${s.charAt(0).toUpperCase()}${s.slice(1)}`);

  return (
    <div className="cr-history">
      <div className="review-panel-header">
        <span>{t('projects.detail.reviewHistoryTitle')}</span>
        {blanks.length > 0 && (
          <span className="cr-history-blanks">
            {t('projects.detail.reviewBlankDrafts', { n: blanks.length })}
            <button className="btn btn-sm" onClick={() => onPurgeBlankDrafts(blanks)} disabled={running}>
              {t('projects.detail.reviewBlankDraftsClear')}
            </button>
          </span>
        )}
      </div>
      {rows.length === 0 ? (
        <div className="tab-empty"><p>{t('projects.detail.reviewHistoryEmpty')}</p></div>
      ) : (
        <table className="pr-table table-cards">
          <thead>
            <tr>
              <th style={{ width: 90 }}>{t('projects.detail.reviewHistoryColType')}</th>
              <th>{t('projects.detail.reviewHistoryColTarget')}</th>
              <th style={{ width: 90 }}>{t('projects.detail.reviewHistoryColStatus')}</th>
              <th style={{ width: 150 }}>{t('projects.detail.reviewHistoryColFindings')}</th>
              <th style={{ width: 150 }}>{t('projects.detail.reviewHistoryColTime')}</th>
              <th style={{ width: 160 }}>{t('projects.detail.reviewHistoryColActions')}</th>
            </tr>
          </thead>
          <tbody>
            {rows.map(rv => (
              <tr key={rv.id} className={selectedId === rv.id ? 'pr-row-active' : ''}>
                <td data-label={t('projects.detail.reviewHistoryColType')}>
                  {rv.review_type === 'pr' ? t('projects.detail.reviewModePR') : t('projects.detail.reviewModeBranch')}
                </td>
                <td data-label={t('projects.detail.reviewHistoryColTarget')}>
                  {rv.review_type === 'pr' && rv.pr_number > 0 && <code className="pr-branch">#{rv.pr_number}</code>}{' '}
                  <code className="pr-branch">{rv.branch}</code>
                  {rv.base_branch ? <> ← <code className="pr-branch">{rv.base_branch}</code></> : null}
                  {rv.title ? <div className="cr-history-title">{rv.title}</div> : null}
                  {/* A review that has been rewritten says so, so "why does
                      this look different from what I read yesterday" has an
                      answer in the list itself. */}
                  {rv.revision_no > 1 && (
                    <span className="cr-history-rev">{t('projects.detail.reviewVersionBadge', { n: rv.revision_no })}</span>
                  )}
                </td>
                <td data-label={t('projects.detail.reviewHistoryColStatus')}>
                  <span className={`cr-status cr-status-${rv.status}`}>{statusLabel(rv.status)}</span>
                </td>
                <td data-label={t('projects.detail.reviewHistoryColFindings')}>
                  {/* Zero counts stay grey so a row with a single P0 reads
                      at a glance instead of blending into three chips. */}
                  {SEVERITIES.map(sev => {
                    const n = severityCount(rv, sev);
                    return (
                      <span key={sev} className={`cr-count cr-count-${sev}${n === 0 ? ' is-zero' : ''}`}>
                        {sev} {n}
                      </span>
                    );
                  })}
                </td>
                <td data-label={t('projects.detail.reviewHistoryColTime')}>{fmtDateTime(rv.created_at)}</td>
                <td data-label={t('projects.detail.reviewHistoryColActions')}>
                  <button className="btn btn-sm" onClick={() => onOpen(rv)}>
                    {t('projects.detail.reviewReportTitle')}
                  </button>
                  <button className="btn btn-sm" onClick={() => onRerun(rv)} disabled={running || rv.status === 'running'}>
                    {t('projects.detail.reviewRerunBtn')}
                  </button>
                  <button className="btn btn-sm" onClick={() => onDelete(rv)} disabled={rv.status === 'running'}>
                    {t('projects.detail.reviewDeleteBtn')}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
