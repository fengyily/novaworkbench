// RequirementAcceptancePanel — the acceptance-check block on a master
// requirement's detail page.
//
// A master requirement whose development was delegated to its split
// children has no code of its own to review — but it still has a contract,
// and the only question that matters about it is "did the children
// actually deliver it?". This panel is where the user asks that question.
//
// What it shows, in the order the user acts:
//
//   verdict  — 已达成 / 部分达成 / 未达成, in colour, with the per-status
//              counts so a "3 of 5 passed" is visible without counting.
//   checks   — one row per acceptance point of the master, with the verdict
//              and the evidence behind it, and which child owns it.
//   gaps     — what the children did NOT implement.
//   proposals— whether the master should be split further.
//
// The whole thing is READ-ONLY with respect to the code: the backend runs
// the check with Write / Edit denied, and the panel never edits a
// requirement.
//
// The check is triggered manually (the backend refuses to run one
// automatically — it costs a full model run over N directories), and the
// panel only nudges when every child is already done: that is the moment
// the answer becomes actionable.

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import {
  acceptanceCheckApi,
  parseAcceptanceSummary,
  type AcceptanceCheck,
  type AcceptanceSummary,
  type CodeReview,
  type Requirement,
} from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import { errorMessage } from '../utils/errMsg';
import { appendLogLine, type LogLine } from '../utils/logLines';
import MarkdownRender from './MarkdownRender';
import ModelSelect from './ModelSelect';
import './RequirementAcceptancePanel.css';

export interface RequirementAcceptancePanelProps {
  requirementId: string;
  /**
   * The master's child requirements — the parent already fetched them for
   * the children block, so the panel does not ask for them again.
   *
   * They are used for ONE thing: to know when every child is done, which
   * is when the panel shows the "run a check" nudge. Passing them down
   * keeps the two blocks consistent (the panel can never claim "all
   * children are done" while the list above shows one still in progress).
   *
   * NOT named `children`: that name belongs to React's own render prop,
   * and a component that takes it as DATA confuses every reader (and the
   * lint rule that guards the real one).
   */
  childRequirements?: Requirement[];
}

type PanelState = 'idle' | 'running' | 'failed';

export default function RequirementAcceptancePanel({
  requirementId,
  childRequirements,
}: RequirementAcceptancePanelProps) {
  const { t } = useTranslation();
  const [checks, setChecks] = useState<CodeReview[]>([]);
  const [loadError, setLoadError] = useState('');
  const [actionError, setActionError] = useState('');
  const [state, setState] = useState<PanelState>('idle');
  const [logs, setLogs] = useState<LogLine[]>([]);
  const [model, setModel] = useState('');
  const [configId, setConfigId] = useState('');
  const streamRef = useRef<EventStream | null>(null);

  const load = useCallback(async (): Promise<CodeReview[]> => {
    try {
      const rows = await acceptanceCheckApi.list(requirementId);
      setChecks(rows ?? []);
      setLoadError('');
      return rows ?? [];
    } catch (e) {
      setLoadError(errorMessage(e));
      return [];
    }
  }, [requirementId]);

  useEffect(() => {
    // Reset before the fetch: the panel must not show the PREVIOUS
    // requirement's verdict while the new one's is still loading.
    setChecks([]);
    void load();
  }, [load]);

  // Close the stream when the panel unmounts — a run that outlives the
  // page must not keep writing into a dead component.
  useEffect(() => () => { streamRef.current?.close(); }, []);

  const latest = checks[0] ?? null;
  const summary = useMemo(
    () => (latest ? parseAcceptanceSummary(latest) : null),
    [latest],
  );

  // The nudge: every child is done, and no check has ever run. After the
  // first check the user knows the feature exists, so the nudge stops.
  const allChildrenDone = useMemo(() => {
    if (!childRequirements || childRequirements.length === 0) return false;
    return childRequirements.every((c) => c.status === 'done');
  }, [childRequirements]);
  const showReadyHint = allChildrenDone && checks.length === 0 && state !== 'running';

  const start = async () => {
    setActionError('');
    setLogs([]);
    setState('running');
    try {
      const res = await acceptanceCheckApi.start(requirementId, {
        model: model || undefined,
        claude_config_id: configId || undefined,
      });
      const jobId = res?.job_id ?? '';
      if (!jobId) {
        setState('idle');
        setActionError(t('components.acceptanceCheck.errNoJob'));
        return;
      }
      const stream = createEventStream(
        acceptanceCheckApi.streamUrl(requirementId, jobId),
        (data: any) => {
          const type = String(data?.type ?? '');
          if (type === 'job_done') {
            stream.close();
            setState(data?.status === 'error' ? 'failed' : 'idle');
            if (data?.status === 'error') {
              setActionError(String(data?.error_message || t('components.acceptanceCheck.errFailed')));
            }
            void load();
            return;
          }
          setLogs((prev) => appendLogLine(prev, {
            type,
            content: String(data?.content ?? ''),
            at: data?.at,
          } as LogLine));
        },
        () => {
          // The stream ended without a terminal frame. That happens when
          // the backend dies mid-run — but it also happens when a proxy
          // swallows the LAST frame of a run that actually finished, so the
          // PERSISTED result decides the state, not the stream.
          void load().then((rows) => {
            const head = rows[0];
            if (head && head.status === 'done') {
              setState('idle');
              return;
            }
            setState((prev) => (prev === 'running' ? 'failed' : prev));
            setActionError((prev) => prev || t('components.acceptanceCheck.errFailed'));
          });
        },
      );
      streamRef.current = stream;
    } catch (e) {
      setState('idle');
      setActionError(errorMessage(e));
    }
  };

  const busy = state === 'running';

  return (
    <div className="acc-panel">
      <div className="acc-head">
        <span className="acc-title">{t('requirements.detail2.acceptance.title')}</span>
        <span className={`acc-badge acc-badge-${badgeKind(latest, state)}`}>
          {t(`requirements.detail2.acceptance.badge.${badgeKind(latest, state)}`)}
        </span>
        <div className="acc-head-actions">
          <ModelSelect
            value={model}
            onChange={setModel}
            configId={configId}
            onConfigChange={setConfigId}
            disabled={busy}
            label={t('requirements.detail2.acceptance.modelLabel')}
          />
          {checks.length > 0 ? (
            <button
              type="button"
              className="btn btn-sm"
              disabled={busy}
              onClick={() => void start()}
            >
              {busy
                ? t('requirements.detail2.acceptance.running')
                : t('requirements.detail2.acceptance.btnAgain')}
            </button>
          ) : (
            <button
              type="button"
              className="btn btn-primary btn-sm"
              disabled={busy}
              onClick={() => void start()}
            >
              {busy
                ? t('requirements.detail2.acceptance.running')
                : t('requirements.detail2.acceptance.btnStart')}
            </button>
          )}
        </div>
      </div>

      <p className="acc-hint">{t('requirements.detail2.acceptance.hint')}</p>

      {showReadyHint && (
        <div className="acc-ready">
          <span>{t('requirements.detail2.acceptance.readyHint')}</span>
          <button
            type="button"
            className="btn btn-sm btn-primary"
            disabled={busy}
            onClick={() => void start()}
          >
            {t('requirements.detail2.acceptance.btnStart')}
          </button>
        </div>
      )}

      {loadError && <div className="acc-error">{loadError}</div>}
      {actionError && <div className="acc-error">{actionError}</div>}

      {busy && (
        <div className="acc-log">
          {logs.length === 0 ? (
            <div className="acc-log-empty">{t('components.acceptanceCheck.logWaiting')}</div>
          ) : (
            logs.map((l, i) => (
              <div key={i} className={`acc-log-line acc-log-${l.type}`}>
                {l.content}
              </div>
            ))
          )}
        </div>
      )}

      {/* A failed run explains itself in the row it left behind (the
          backend stores the reason in result_md), so the panel shows THAT
          rather than a second copy of the same message. */}
      {!busy && latest?.status === 'error' && (
        <div className="acc-error">
          {latest.result_md || t('components.acceptanceCheck.errFailed')}
        </div>
      )}

      {summary && (
        <>
          <VerdictCard summary={summary} />
          <CheckList checks={summary.checks} />
          <GapList gaps={summary.gaps} />
          <ProposalList proposals={summary.proposals} />
        </>
      )}

      {/* The raw report. It is the fallback when the structured half is
          missing (a model that broke the contract), so it is always
          rendered when there is one. */}
      {latest?.result_md && (
        <details className="acc-report">
          <summary>{t('components.acceptanceCheck.reportTitle')}</summary>
          <div className="md-prose acc-report-body">
            <MarkdownRender content={latest.result_md} />
          </div>
        </details>
      )}

      {checks.length > 1 && (
        <details className="acc-history">
          <summary>{t('components.acceptanceCheck.historyTitle', { count: checks.length })}</summary>
          <ul className="acc-history-list">
            {checks.map((c) => (
              <li key={c.id}>
                <span className="acc-history-time">{fmtWhen(c.created_at)}</span>
                <span className={`acc-history-verdict acc-history-${c.goal_met || 'none'}`}>
                  {t(`requirements.detail2.acceptance.badge.${
                    c.status === 'error' ? 'failed' : verdictBadge(c.goal_met)
                  }`)}
                </span>
              </li>
            ))}
          </ul>
        </details>
      )}
    </div>
  );
}

// badgeKind maps the panel's state onto a badge vocabulary.
function badgeKind(latest: CodeReview | null, state: PanelState): string {
  if (state === 'running') return 'running';
  if (state === 'failed') return 'failed';
  if (!latest) return 'none';
  if (latest.status === 'error') return 'failed';
  if (latest.status !== 'done') return 'running';
  return verdictBadge(latest.goal_met);
}

// verdictBadge maps the backend's yes/partial/no verdict onto the badge
// vocabulary.
function verdictBadge(goalMet: string): string {
  if (goalMet === 'yes') return 'yes';
  if (goalMet === 'partial') return 'partial';
  if (goalMet === 'no') return 'no';
  return 'none';
}

// VerdictCard — the headline conclusion plus the per-status counts.
function VerdictCard({ summary }: { summary: AcceptanceSummary }) {
  const { t } = useTranslation();
  const counts = useMemo(() => {
    const c = { yes: 0, partial: 0, no: 0, unknown: 0 };
    for (const ck of summary.checks) {
      if (ck.status === 'yes') c.yes += 1;
      else if (ck.status === 'partial') c.partial += 1;
      else if (ck.status === 'no') c.no += 1;
      else c.unknown += 1;
    }
    return c;
  }, [summary.checks]);

  const total = summary.checks.length;
  // The completion ratio counts only the points that were actually
  // checkable — an "unknown" is not a failure, so counting it as one
  // would make an unreadable implementation look like a broken one.
  const checked = counts.yes + counts.partial + counts.no;
  const ratio = checked > 0 ? Math.round((counts.yes / checked) * 100) : null;

  return (
    <div className={`acc-verdict acc-verdict-${summary.verdict || 'none'}`}>
      <div className="acc-verdict-head">
        <span className="acc-verdict-label">
          {t(`requirements.detail2.acceptance.verdict.${summary.verdict || 'none'}`)}
        </span>
        {ratio !== null && (
          <span className="acc-verdict-ratio">{t('components.acceptanceCheck.ratio', { pct: ratio })}</span>
        )}
      </div>
      {summary.summary && <p className="acc-verdict-summary">{summary.summary}</p>}
      {total > 0 && (
        <div className="acc-verdict-counts">
          <span className="acc-count acc-count-yes">{t('components.acceptanceCheck.countYes', { n: counts.yes })}</span>
          <span className="acc-count acc-count-partial">{t('components.acceptanceCheck.countPartial', { n: counts.partial })}</span>
          <span className="acc-count acc-count-no">{t('components.acceptanceCheck.countNo', { n: counts.no })}</span>
          <span className="acc-count acc-count-unknown">{t('components.acceptanceCheck.countUnknown', { n: counts.unknown })}</span>
        </div>
      )}
    </div>
  );
}

// CheckList — one row per acceptance point of the master requirement.
function CheckList({ checks }: { checks: AcceptanceCheck[] }) {
  const { t } = useTranslation();
  if (checks.length === 0) return null;
  return (
    <section className="acc-section">
      <h4 className="acc-section-title">{t('components.acceptanceCheck.checksTitle')}</h4>
      <ul className="acc-list">
        {checks.map((c, i) => (
          <li key={i} className={`acc-item acc-item-${c.status}`}>
            <span className={`acc-flag acc-flag-${c.status}`}>
              {t(`components.acceptanceCheck.status.${c.status}`)}
            </span>
            <div className="acc-item-body">
              <p className="acc-item-title">{c.criterion}</p>
              {c.evidence && (
                <p className="acc-item-evidence">
                  <span className="acc-item-evidence-tag">
                    {t('components.acceptanceCheck.evidenceTag')}
                  </span>
                  {c.evidence}
                </p>
              )}
              {c.child_title && (
                <p className="acc-item-child">
                  <span className="acc-item-child-tag">
                    {t('components.acceptanceCheck.childTag')}
                  </span>
                  {c.child_id ? (
                    <Link to={`/requirements/${c.child_id}`}>{c.child_title}</Link>
                  ) : (
                    c.child_title
                  )}
                </p>
              )}
            </div>
          </li>
        ))}
      </ul>
    </section>
  );
}

// GapList — what the children did NOT implement.
function GapList({ gaps }: { gaps: AcceptanceSummary['gaps'] }) {
  const { t } = useTranslation();
  if (gaps.length === 0) return null;
  return (
    <section className="acc-section">
      <h4 className="acc-section-title">{t('components.acceptanceCheck.gapsTitle')}</h4>
      <ul className="acc-list">
        {gaps.map((g, i) => (
          <li key={i} className="acc-item acc-item-no">
            <span className="acc-flag acc-flag-no">{t('components.acceptanceCheck.gapFlag')}</span>
            <div className="acc-item-body">
              <p className="acc-item-title">{g.title}</p>
              {g.detail && <p className="acc-item-evidence">{g.detail}</p>}
              {g.child_title && (
                <p className="acc-item-child">
                  <span className="acc-item-child-tag">
                    {t('components.acceptanceCheck.childTag')}
                  </span>
                  {g.child_id ? (
                    <Link to={`/requirements/${g.child_id}`}>{g.child_title}</Link>
                  ) : (
                    g.child_title
                  )}
                </p>
              )}
            </div>
          </li>
        ))}
      </ul>
    </section>
  );
}

// ProposalList — the "split further" advice.
//
// Read-only on purpose: the acceptance check never creates requirements —
// changing the requirement tree is the user's call.
function ProposalList({ proposals }: { proposals: AcceptanceSummary['proposals'] }) {
  const { t } = useTranslation();
  if (proposals.length === 0) return null;
  return (
    <section className="acc-section">
      <h4 className="acc-section-title">{t('components.acceptanceCheck.proposalsTitle')}</h4>
      <div className="acc-proposals">
        {proposals.map((p, i) => (
          <div key={i} className="acc-proposal">
            <div className="acc-proposal-title">{p.title}</div>
            {p.goal && (
              <div className="acc-proposal-row">
                <span className="acc-proposal-tag">{t('components.acceptanceCheck.goalTag')}</span>
                <span>{p.goal}</span>
              </div>
            )}
            {p.acceptance.length > 0 && (
              <div className="acc-proposal-row">
                <span className="acc-proposal-tag">{t('components.acceptanceCheck.acceptanceTag')}</span>
                <ul className="acc-proposal-list">
                  {p.acceptance.map((a, j) => <li key={j}>{a}</li>)}
                </ul>
              </div>
            )}
            {p.depends_on.length > 0 && (
              <div className="acc-proposal-row">
                <span className="acc-proposal-tag">{t('components.acceptanceCheck.dependsOnTag')}</span>
                <span>{p.depends_on.join('、')}</span>
              </div>
            )}
          </div>
        ))}
      </div>
    </section>
  );
}

// fmtWhen renders a timestamp for the history list.
function fmtWhen(raw: string): string {
  if (!raw) return '';
  const d = new Date(raw);
  if (Number.isNaN(d.getTime())) return raw;
  return d.toLocaleString();
}
