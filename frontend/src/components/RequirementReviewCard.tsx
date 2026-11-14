// RequirementReviewCard — the requirement page's view of ONE Review round.
//
// It renders what the requirement-level Review is FOR, in the order the user
// acts in it:
//
//   verdict    — "P0/P1 clear?" answered once, in colour.
//   ledger     — the three severity counts (a zero bucket stays grey, so a
//                single P0 is findable at a glance).
//   findings   — the graded items, grouped by severity, each checkable.
//   actions    — turn the checked findings into fix sub-tasks, or start the
//                next round (the re-check button).
//
// The full report (follow-ups / regeneration / versions / export) lives on
// the review page (/reviews/:id) — this card links there rather than
// re-implementing those surfaces, so there is exactly one implementation of
// each.
//
// The report itself is fetched from GET /api/code-reviews/{rid}: the
// code_reviews row is the AUTHORITATIVE source (the sub-task's artifact is
// only a display copy), so the card and the review page can never disagree.

import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  codeReviewsApi,
  parseReviewFindings,
  requirementReviewsApi,
  type CodeReview,
  type ReviewFinding,
  type ReviewSeverity,
  type SubTask,
} from '../api/client';
import MarkdownRender from './MarkdownRender';
import { stripArtifactCodeFence, stripArtifactHeader } from '../utils/subTaskArtifact';
import { SEVERITIES, severityCount } from '../pages/codeReview/shared';

interface Props {
  requirementId: string;
  /** The code_reviews row id — st.review_id. */
  reviewId: string;
  /** The sub-task row that produced the round (used for the round number). */
  st: SubTask;
  /**
   * Is this the tip of the round chain? Only the latest round may start the
   * next one — the chain grows at its tip, so re-checking an OLD round would
   * mint a duplicate round number and fork the chain in two. The panel
   * computes it from the flat sub-task list.
   */
  isLatest?: boolean;
  /** Called after a fix batch or a re-check so the parent can refetch. */
  onChanged?: () => void;
}

export default function RequirementReviewCard({
  requirementId, reviewId, st, isLatest = true, onChanged,
}: Props) {
  const { t } = useTranslation();
  const [review, setReview] = useState<CodeReview | null>(null);
  const [loadError, setLoadError] = useState('');
  // The checked findings, keyed by their report ref ("P0-2"). Defaults to
  // "every P0 and P1" — those are the ones a user actually intends to fix;
  // P3 is a suggestion nobody files.
  const [checked, setChecked] = useState<Record<string, boolean>>({});
  const [fixBusy, setFixBusy] = useState(false);
  const [recheckBusy, setRecheckBusy] = useState(false);
  const [reparseBusy, setReparseBusy] = useState(false);
  const [actionError, setActionError] = useState('');
  const [reparseError, setReparseError] = useState('');

  const load = useCallback(() => {
    let alive = true;
    codeReviewsApi.get(reviewId)
      .then((rv) => { if (alive) { setReview(rv); setLoadError(''); } })
      .catch((e) => { if (alive) setLoadError(String(e?.message || e)); });
    return () => { alive = false; };
  }, [reviewId]);

  useEffect(() => { load(); }, [load]);

  const findings = useMemo(
    () => (review ? parseReviewFindings(review) : []),
    [review],
  );

  const grouped = useMemo(() => {
    const m = new Map<ReviewSeverity, ReviewFinding[]>(SEVERITIES.map((s) => [s, []]));
    for (const f of findings) m.get(f.severity)?.push(f);
    return m;
  }, [findings]);

  // The default selection: every P0 + P1. Computed once per findings change
  // so a newly-arrived report pre-selects the blocking items.
  useEffect(() => {
    const next: Record<string, boolean> = {};
    for (const sev of SEVERITIES) {
      if (sev === 'P3') continue;
      (grouped.get(sev) ?? []).forEach((_, i) => { next[`${sev}-${i + 1}`] = true; });
    }
    setChecked(next);
  }, [grouped]);

  const verdict = review
    ? (review.status === 'error' ? 'error'
      : (review.status === 'draft' || review.status === 'running') ? 'pending'
      : (!review.findings_json ? 'plain'
        : (review.p0_count > 0 || review.p1_count > 0) ? 'fail' : 'pass'))
    : 'pending';

  const selectedRefs = Object.keys(checked).filter((k) => checked[k]);

  const onCreateFixes = async () => {
    if (selectedRefs.length === 0) return;
    setFixBusy(true);
    setActionError('');
    try {
      // One request per finding: the endpoint takes a single finding_ref.
      // Doing it in sequence (rather than one batch call) keeps the failure
      // attributable to the finding that caused it.
      for (const ref of selectedRefs) {
        await requirementReviewsApi.createFixes(requirementId, reviewId, { finding_ref: ref });
      }
      onChanged?.();
    } catch (e) {
      setActionError(String((e as Error)?.message || e));
    } finally {
      setFixBusy(false);
    }
  };

  // Re-parse the stored report's contract block.
  //
  // WHY THIS EXISTS: the report prose and its structured half are decoded
  // separately, and the structured half can fail to decode while the prose
  // stays perfectly readable (a model writing `"line": 664` where the
  // contract asked for a string used to cost the whole report its grading).
  // The user is left with a readable report they cannot turn into fix
  // sub-tasks — and re-running the Review to get the grading back means
  // paying for (and receiving) a DIFFERENT report.
  //
  // So this re-reads the contract out of the report ALREADY on file. No
  // model runs; the report the user read is the report they keep.
  const onReparse = async () => {
    setReparseBusy(true);
    setReparseError('');
    try {
      await codeReviewsApi.reparse(reviewId);
      await load();
      onChanged?.();
    } catch (e) {
      setReparseError(String((e as Error)?.message || e));
    } finally {
      setReparseBusy(false);
    }
  };

  const onRecheck = async () => {
    setRecheckBusy(true);
    setActionError('');
    try {
      await requirementReviewsApi.recheck(requirementId, reviewId, {});
      onChanged?.();
    } catch (e) {
      setActionError(String((e as Error)?.message || e));
    } finally {
      setRecheckBusy(false);
    }
  };

  const round = st.review_round || 1;

  return (
    <section className={`sub-review-card sub-review-${verdict}`}>
      <header className="sub-review-head">
        <span className="sub-review-badge">🔍 {t('components.subTaskCard.reviewBadge', { round })}</span>
        {review?.model && <span className="sub-review-chip">🪙 {review.model}</span>}
        {review?.role_key && (
          <span className="sub-review-chip" title={t('components.subTaskCard.reviewIdentityChipHint')}>
            🎭 {review.role_key}
          </span>
        )}
        {/* The reasoning tier this round actually ran at — the same
            binding the model chip shows, so the reader can tell whether a
            thin report came from a thin model or a thin tier. */}
        {review?.effort && (
          <span className="sub-review-chip" title={t('components.subTaskCard.reviewEffortChipHint')}>
            🧠 {review.effort}
          </span>
        )}
        <span className="sub-review-spacer" />
        <a className="btn btn-sm" href={`/reviews/${reviewId}`}>
          {t('components.subTaskCard.reviewOpenFull')}
        </a>
      </header>

      {/* Verdict — the one answer the user came for. */}
      <div className="sub-review-verdict">
        <strong>{t(`components.subTaskCard.reviewVerdict_${verdict}`)}</strong>
        {verdict === 'fail' && review && (
          <span className="sub-review-verdict-sub">
            {t('components.subTaskCard.reviewVerdictFailSub', {
              p0: review.p0_count,
              p1: review.p1_count,
            })}
          </span>
        )}
        {verdict === 'plain' && (
          <>
            <span className="sub-review-verdict-sub">
              {t('components.subTaskCard.reviewVerdictPlainSub')}
            </span>
            {/* Recovery: the report body may still carry the JSON contract
                that carries the grading. Re-reading it costs nothing and
                runs no model, so it is offered before the user resorts to
                re-running the whole Review. */}
            <div className="sub-review-reparse">
              <button
                type="button"
                className="btn btn-sm"
                onClick={onReparse}
                disabled={reparseBusy}
                title={t('components.subTaskCard.reviewReparseHint')}
              >
                {reparseBusy
                  ? t('components.subTaskCard.reviewReparseBusy')
                  : t('components.subTaskCard.reviewReparse')}
              </button>
              {reparseError && <span className="sub-review-error">{reparseError}</span>}
            </div>
          </>
        )}
        {verdict === 'pending' && (
          <span className="sub-review-verdict-sub">
            {t('components.subTaskCard.reviewVerdictPendingSub')}
          </span>
        )}
      </div>

      {loadError && <div className="sub-review-error">{loadError}</div>}

      {/* Ledger — a zero bucket stays grey so the eye lands only on what
          actually needs attention. */}
      {review && review.findings_json && (
        <div className="sub-review-ledger">
          {SEVERITIES.map((sev) => {
            const n = severityCount(review, sev);
            return (
              <div key={sev} className={`sub-review-cell sub-review-cell-${sev}${n === 0 ? ' is-zero' : ''}`}>
                <span className="sub-review-cell-num">{n}</span>
                <span className="sub-review-cell-tag">{sev}</span>
              </div>
            );
          })}
        </div>
      )}

      {/* Findings — grouped by severity, each checkable so the user can pick
          exactly which ones to turn into fix sub-tasks. */}
      {findings.length > 0 && (
        <div className="sub-review-findings">
          {SEVERITIES.map((sev) => {
            const items = grouped.get(sev) ?? [];
            if (items.length === 0) return null;
            return (
              <section key={sev} className={`sub-review-sev sub-review-sev-${sev}`}>
                <h5 className="sub-review-sev-head">
                  <span className={`sub-review-sev-badge sub-review-sev-badge-${sev}`}>{sev}</span>
                  <span>{t(`components.subTaskCard.reviewSev${sev}`)}</span>
                  <span className="sub-review-sev-count">{items.length}</span>
                </h5>
                <ul className="sub-review-finding-list">
                  {items.map((f, i) => {
                    const ref = `${sev}-${i + 1}`;
                    return (
                      <li key={ref} className="sub-review-finding">
                        <label className="sub-review-finding-label">
                          <input
                            type="checkbox"
                            checked={!!checked[ref]}
                            onChange={(e) => setChecked((prev) => ({ ...prev, [ref]: e.target.checked }))}
                          />
                          <span className="sub-review-finding-idx">{ref}</span>
                          <span className="sub-review-finding-title">{f.title}</span>
                          {(f.file || f.line) && (
                            <code className="sub-review-finding-loc">{f.file}{f.line ? `:${f.line}` : ''}</code>
                          )}
                        </label>
                        {f.detail && <p className="sub-review-finding-detail">{f.detail}</p>}
                        {f.suggestion && (
                          <p className="sub-review-finding-suggestion">
                            <span className="sub-review-finding-suggestion-tag">
                              {t('components.subTaskCard.reviewFindingSuggestion')}
                            </span>
                            {f.suggestion}
                          </p>
                        )}
                      </li>
                    );
                  })}
                </ul>
              </section>
            );
          })}
        </div>
      )}

      {/* The full report. The code_reviews row is the authoritative copy and
          the card above renders its STRUCTURED view; this disclosure shows
          the prose so a reader who wants the reasoning does not have to
          leave the requirement page. On the degraded path (no structured
          findings) this is the only thing there is, so it opens by itself. */}
      {st.artifact && (
        <details className="sub-review-report" open={findings.length === 0}>
          <summary>{t('components.subTaskCard.reviewFullReport')}</summary>
          <div className="sub-review-report-body md-prose">
            <MarkdownRender content={stripArtifactCodeFence(stripArtifactHeader(st.artifact))} />
          </div>
        </details>
      )}

      {/* Actions. The "generate fix sub-tasks" button is disabled when the
          round produced no structured findings — the report itself is not
          lost (it renders as Markdown on the review page), but there is
          nothing to convert. */}
      <div className="sub-review-actions">
        {findings.length > 0 && (
          <button
            type="button"
            className="btn btn-sm btn-primary"
            onClick={onCreateFixes}
            disabled={fixBusy || selectedRefs.length === 0}
            title={t('components.subTaskCard.reviewCreateFixesHint')}
          >
            {fixBusy
              ? t('components.subTaskCard.reviewCreateFixesBusy')
              : t('components.subTaskCard.reviewCreateFixes', { n: selectedRefs.length })}
          </button>
        )}
        {isLatest && (
          <button
            type="button"
            className="btn btn-sm"
            onClick={onRecheck}
            disabled={recheckBusy}
            title={t('components.subTaskCard.reviewRecheckHint')}
          >
            {recheckBusy
              ? t('components.subTaskCard.reviewRecheckBusy')
              : t('components.subTaskCard.reviewRecheck')}
          </button>
        )}
        {actionError && <span className="sub-review-error">{actionError}</span>}
      </div>
    </section>
  );
}
