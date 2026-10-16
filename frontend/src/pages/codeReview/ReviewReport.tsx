// ReviewReport — the graded result of one review.
//
// Reading order is fixed and deliberate:
//
//   toolbar   — what you can DO with this report (export / regenerate /
//               re-run / switch version), pinned so it stays reachable while
//               reading a long one.
//   verdict   — the "can this merge?" answer, stated once, in colour, before
//               any reading is required.
//   ledger    — three count tiles, coloured only when non-zero so a single
//               P0 is findable at a glance.
//   goals     — the 目标达成 evidence table: which acceptance point was
//               checked, the verdict, and what it rests on. This is the half
//               of the product requirement that attachments exist for, so it
//               sits above the findings rather than inside the prose.
//   diagrams  — the report's Mermaid blocks, hoisted out of the collapsed
//               prose (a picture nobody expands is a picture nobody drew).
//   findings  — the graded items, grouped by severity, each with a 追问 entry
//               point so a question can carry its own reference.
//   prose     — result_md behind a disclosure, rendered with the shared
//               `.md-prose` typography.
//
// Findings rendering degrades on purpose: when the model honoured the
// trailing-JSON contract we group by P0/P1/P3 with counts; when it didn't,
// findings_json is empty, the verdict is 'plain', and the Markdown
// disclosure opens by itself (the report's own ## P0/P1/P3 sections mean
// nothing is lost).

import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  parseGoalChecks, parseReviewFindings,
  type CodeReview, type CodeReviewRevision, type GoalMet, type ReviewFinding, type ReviewSeverity,
} from '../../api/client';
import MarkdownRender from '../../components/MarkdownRender';
import { IconArchitect, IconBranch, IconClock, IconFileText, IconRobot } from '../../components/icons';
import { fmtDateTime } from '../../utils/intl';
import { SEVERITIES, deriveVerdict, extractMermaidBlocks, severityCount, titleCase } from './shared';

interface Props {
  review: CodeReview;
  revisions: CodeReviewRevision[];
  /** Any run (review / regeneration) is in flight — actions are disabled. */
  busy: boolean;
  /** Follow-ups not yet folded into the report; drives the regenerate CTA. */
  pendingFollowups: number;
  canRegenerate: boolean;
  onExportMarkdown: () => void;
  onExportPdf: () => void;
  exportingPdf: boolean;
  onRegenerate: () => void;
  onRerun: () => void;
  onSelectRevision: (revNo: number) => void;
  /** Pre-fill the 追问 box with a question about one finding. */
  onAskAbout: (finding: ReviewFinding, ref: string) => void;
  /** Turn this finding into a tracked issue-kind requirement. The handler is
      expected to surface the new requirement id (e.g. via a toast or inline
      chip) so the user can jump straight to /requirements/:new_id. */
  onCreateIssue: (finding: ReviewFinding, ref: string) => void;
  /** Per-finding ref strings currently being turned into issues (keyed by
      ref, value true). The container owns this state so a clicked button
      stays in its busy state through the round-trip and re-renders after
      success don't reset it. */
  creatingIssues: Record<string, boolean>;
  /** Inline success state: ref → newly-created requirement id. Rendered as a
      "✓ 已创建 →" chip with a link to /requirements/:new_id. Cleared on the
      next report reload so it doesn't leak across runs. */
  createdIssues: Record<string, string>;
  /** Convert every graded finding in one shot. Mirrors the per-card button
      but at the report level — the typical "I've read the report, action
      it all" gesture after a verdict comes back. */
  onBulkCreateIssues: () => void;
  /** True while a batch creation round-trip is in flight. The button is
      disabled during this window so a second click can't mint duplicates. */
  bulkCreating: boolean;
}

export default function ReviewReport({
  review, revisions, busy, pendingFollowups, canRegenerate,
  onExportMarkdown, onExportPdf, exportingPdf, onRegenerate, onRerun, onSelectRevision, onAskAbout,
  onCreateIssue, creatingIssues, createdIssues,
  onBulkCreateIssues, bulkCreating,
}: Props) {
  const { t } = useTranslation();
  const [exportOpen, setExportOpen] = useState(false);
  const [reportOpen, setReportOpen] = useState(false);

  const findings = useMemo(() => parseReviewFindings(review), [review]);
  const goalChecks = useMemo(() => parseGoalChecks(review), [review]);
  const diagrams = useMemo(() => extractMermaidBlocks(review.result_md || ''), [review.result_md]);
  const grouped = useMemo(() => {
    const m = new Map<ReviewSeverity, ReviewFinding[]>(SEVERITIES.map(s => [s, []]));
    for (const f of findings) m.get(f.severity)?.push(f);
    return m;
  }, [findings]);

  // Bulk-conversion candidate count: P0 + P1 findings only (P3 are nits
  // nobody plans to file). When the model didn't honour the trailing-JSON
  // contract there are no findings to convert at all and the bulk button
  // is disabled by the `hasBulkFindings` flag below.
  const bulkCount = useMemo(() => {
    let n = 0;
    for (const f of findings) if (f.severity === 'P0' || f.severity === 'P1') n++;
    return n;
  }, [findings]);
  const hasBulkFindings = bulkCount > 0;

  const verdict = deriveVerdict(review);
  const verdictKey = titleCase(verdict);

  // The full Markdown report is a disclosure: when the findings are graded
  // they lead and the prose is backup, but on the degraded path the prose is
  // all there is, so it opens by itself.
  useEffect(() => {
    setReportOpen(!review.findings_json);
  }, [review.id, review.findings_json]);

  const goalLabel = (g: GoalMet | string) => {
    switch (g) {
      case 'yes': return t('projects.detail.reviewGoalYes');
      case 'partial': return t('projects.detail.reviewGoalPartial');
      case 'no': return t('projects.detail.reviewGoalNo');
      default: return t('projects.detail.reviewGoalUnknown');
    }
  };

  return (
    <article className={`cr-report cr-report-${verdict}`}>
      {/* ── Action bar ── Sticky: a long report scrolls past the point where
          you decide to export it or ask for a rewrite. */}
      <div className="cr-toolbar">
        {revisions.length > 1 && (
          <div className="cr-versions" role="group" aria-label={t('projects.detail.reviewVersionsLabel')}>
            {revisions.map(rev => (
              <button
                key={rev.rev_no}
                className={`cr-version${rev.rev_no === review.revision_no ? ' active' : ''}`}
                onClick={() => onSelectRevision(rev.rev_no)}
                disabled={busy || rev.rev_no === review.revision_no}
                title={t('projects.detail.reviewVersionTitle', {
                  n: rev.rev_no,
                  time: fmtDateTime(rev.created_at),
                  count: rev.followup_count,
                })}
              >v{rev.rev_no}</button>
            ))}
          </div>
        )}

        <div className="cr-toolbar-spacer" />

        {/* Export is a two-item menu rather than two buttons: Markdown is the
            common case, PDF is the "send it to someone" case, and both are
            built from the same reassembled document. */}
        <div className="cr-export">
          <button
            className="btn btn-sm"
            onClick={() => setExportOpen(v => !v)}
            disabled={!review.result_md || exportingPdf}
            aria-expanded={exportOpen}
          >
            {exportingPdf ? t('projects.detail.reviewExportBusy') : t('projects.detail.reviewExport')} ▾
          </button>
          {exportOpen && (
            <>
              <div className="cr-export-backdrop" onClick={() => setExportOpen(false)} />
              <div className="cr-export-menu" role="menu">
                <button role="menuitem" onClick={() => { setExportOpen(false); onExportMarkdown(); }}>
                  {t('projects.detail.reviewExportMd')}
                </button>
                <button role="menuitem" onClick={() => { setExportOpen(false); onExportPdf(); }}>
                  {t('projects.detail.reviewExportPdf')}
                </button>
              </div>
            </>
          )}
        </div>

        <button
          className={`btn btn-sm${pendingFollowups > 0 ? ' btn-primary' : ''}`}
          onClick={onRegenerate}
          disabled={busy || !canRegenerate}
          title={t('projects.detail.reviewRegenerateHint')}
        >
          {pendingFollowups > 0
            ? t('projects.detail.reviewRegenerateWithN', { n: pendingFollowups })
            : t('projects.detail.reviewRegenerate')}
        </button>
        <button className="btn btn-sm" onClick={onRerun} disabled={busy}>
          {t('projects.detail.reviewRerunBtn')}
        </button>
        {/* Convert every graded finding into a tracked issue in one shot.
            Lives at the report level because the user has just read the
            whole report and decided to action it; the per-card button
            above handles the "I just want this one" case. Disabled while
            a batch is in flight or any per-card creation is mid-RTT, so
            a double-click can't mint duplicate rows. */}
        <button
          className="btn btn-sm cr-bulk-issue"
          onClick={onBulkCreateIssues}
          disabled={busy || bulkCreating || !hasBulkFindings || Object.values(creatingIssues).some(Boolean)}
          title={t('projects.detail.reviewBulkCreateHint')}
        >
          {bulkCreating
            ? t('projects.detail.reviewBulkCreateBusy')
            : t('projects.detail.reviewBulkCreate', { n: bulkCount })}
        </button>
      </div>

      {/* ── Verdict band ── */}
      <header className="cr-verdict">
        <span className="cr-verdict-mark" aria-hidden="true" />
        <div className="cr-verdict-text">
          <h3 className="cr-verdict-title">{t(`projects.detail.reviewVerdict${verdictKey}`)}</h3>
          <p className="cr-verdict-sub">{t(`projects.detail.reviewVerdictHint${verdictKey}`)}</p>
        </div>
        <div className="cr-verdict-meta">
          <span className={`cr-goal cr-goal-${review.goal_met || 'unknown'}`}>
            {t('projects.detail.reviewGoalLabel')}：{goalLabel(review.goal_met)}
          </span>
          <span className="cr-meta-chip">
            <IconBranch size={12} />
            <code>{review.review_type === 'pr' && review.pr_number > 0 ? `#${review.pr_number}` : review.branch}</code>
            {review.base_branch ? <> ← <code>{review.base_branch}</code></> : null}
          </span>
          {review.model && (
            <span className="cr-meta-chip" title={t('projects.detail.reviewModelLine', { model: review.model })}>
              <IconRobot size={12} />{review.model}
            </span>
          )}
          <span className="cr-meta-chip">
            <IconClock size={12} />{fmtDateTime(review.finished_at || review.created_at)}
          </span>
        </div>
      </header>

      {/* ── Severity ledger ── Zero stays grey so the eye lands only on what
          actually needs attention. */}
      {review.findings_json ? (
        <div className="cr-ledger">
          {SEVERITIES.map(sev => {
            const n = severityCount(review, sev);
            return (
              <div key={sev} className={`cr-ledger-cell cr-ledger-${sev}${n === 0 ? ' is-zero' : ''}`}>
                <span className="cr-ledger-num">{n}</span>
                <span className="cr-ledger-label">
                  <span className="cr-ledger-tag">{sev}</span>
                  {t(`projects.detail.reviewSevShort${sev}`)}
                </span>
              </div>
            );
          })}
        </div>
      ) : null}

      {/* ── 目标达成清单 ── The evidence behind the one-word verdict above.
          Optional by contract, so its absence is silent. */}
      {goalChecks.length > 0 && (
        <section className="cr-goals">
          <h4 className="cr-goals-head">
            <span className="cr-goals-title">{t('projects.detail.reviewGoalChecksTitle')}</span>
            <span className="cr-hint">{t('projects.detail.reviewGoalChecksHint')}</span>
          </h4>
          <ul className="cr-goal-list">
            {goalChecks.map((c, i) => (
              <li key={i} className={`cr-goal-item cr-goal-item-${c.status}`}>
                <span className={`cr-goal-flag cr-goal-${c.status}`}>{goalLabel(c.status)}</span>
                <div className="cr-goal-body">
                  <p className="cr-goal-criterion">{c.criterion}</p>
                  {c.evidence && (
                    <p className="cr-goal-evidence">
                      <span className="cr-goal-evidence-tag">{t('projects.detail.reviewGoalChecksColEvidence')}</span>
                      {c.evidence}
                    </p>
                  )}
                </div>
              </li>
            ))}
          </ul>
        </section>
      )}

      {/* ── 改动概览图 ── Hoisted above the findings so the shape of the
          change is visible before the list of what is wrong with it. The
          blocks stay in the prose too; duplicating a picture that is one
          click away costs less than burying it. */}
      {diagrams.length > 0 && (
        <section className="cr-diagrams">
          <h4 className="cr-diagrams-head">
            <IconArchitect size={13} />
            <span className="cr-diagrams-title">{t('projects.detail.reviewDiagrams')}</span>
            <span className="cr-hint">{t('projects.detail.reviewDiagramsHint')}</span>
          </h4>
          <div className="cr-diagrams-body md-prose">
            {diagrams.map((src, i) => (
              <MarkdownRender key={i} content={'```mermaid\n' + src + '\n```'} />
            ))}
          </div>
        </section>
      )}

      {findings.length > 0 && (
        <div className="cr-findings">
          {SEVERITIES.map(sev => {
            const items = grouped.get(sev) ?? [];
            // An empty bucket collapses to one quiet line — rendering a
            // full empty section for each would triple the noise on a
            // report that only found P3s.
            if (items.length === 0) {
              return (
                <p key={sev} className="cr-sev-empty">
                  <span className={`cr-sev-badge cr-sev-badge-${sev}`}>{sev}</span>
                  {t('projects.detail.reviewSevEmpty')}
                </p>
              );
            }
            return (
              <section key={sev} className={`cr-sev cr-sev-${sev}`}>
                <h4 className="cr-sev-head">
                  <span className={`cr-sev-badge cr-sev-badge-${sev}`}>{sev}</span>
                  <span className="cr-sev-name">{t(`projects.detail.reviewSev${sev}`)}</span>
                  <span className="cr-sev-count">{items.length}</span>
                </h4>
                <ol className="cr-finding-list">
                  {items.map((f, i) => {
                    const ref = `${sev}-${i + 1}`;
                    return (
                      <li key={i} className="cr-finding">
                        <div className="cr-finding-head">
                          <span className="cr-finding-idx">{ref}</span>
                          <span className="cr-finding-title">{f.title}</span>
                          {(f.file || f.line) && (
                            <code className="cr-finding-loc">{f.file}{f.line ? `:${f.line}` : ''}</code>
                          )}
                          {/* Asking about a specific finding is the most
                              common follow-up there is; without this the
                              user retypes "P0 的第 2 条…" by hand and the
                              model has to guess which item that was. */}
                          <button
                            className="btn btn-sm cr-finding-ask"
                            onClick={() => onAskAbout(f, ref)}
                            title={t('projects.detail.reviewAskAboutHint')}
                          >{t('projects.detail.reviewAskAbout')}</button>
                          {/* Turn this finding into a tracked issue. Lives
                              next to Ask About because both are per-card
                              actions on the same finding. After success the
                              container renders the new requirement id as a
                              link, so the button itself can stay quiet. */}
                          {createdIssues[ref] ? (
                            <a
                              className="btn btn-sm cr-finding-issue-link"
                              href={`/requirements/${createdIssues[ref]}`}
                              title={t('projects.detail.reviewCreatedIssueTitle', { id: createdIssues[ref] })}
                            >✓ {t('projects.detail.reviewCreatedIssue')}</a>
                          ) : (
                            <button
                              className="btn btn-sm cr-finding-issue"
                              onClick={() => onCreateIssue(f, ref)}
                              disabled={busy || !!creatingIssues[ref]}
                              title={t('projects.detail.reviewCreateIssueHint')}
                            >
                              {creatingIssues[ref]
                                ? t('projects.detail.reviewCreateIssueBusy')
                                : t('projects.detail.reviewCreateIssue')}
                            </button>
                          )}
                        </div>
                        {f.detail && <p className="cr-finding-detail">{f.detail}</p>}
                        {f.suggestion && (
                          <p className="cr-finding-suggestion">
                            <span className="cr-finding-suggestion-tag">{t('projects.detail.reviewFindingSuggestion')}</span>
                            {f.suggestion}
                          </p>
                        )}
                      </li>
                    );
                  })}
                </ol>
              </section>
            );
          })}
        </div>
      )}

      {/* Full Markdown report. Collapsed behind a disclosure when the
          findings above already carry the verdict; open by default when
          they don't (degraded path), so nothing is ever hidden. */}
      {review.result_md && (
        <details className="cr-report-md" open={reportOpen} onToggle={e => setReportOpen(e.currentTarget.open)}>
          <summary className="cr-report-md-summary">
            <IconFileText size={13} />
            {t('projects.detail.reviewFullReport')}
          </summary>
          <div className="cr-report-md-body md-prose">
            <MarkdownRender content={review.result_md} />
          </div>
        </details>
      )}
    </article>
  );
}
