// ReviewChat — the 追问 thread hanging off one report, plus the one action
// that turns that conversation back into a report.
//
// Two rules that shape the panel:
//
//  1. A 追问 never re-grades the review. The P0/P1/P3 counts beside it
//     always belong to the report above, which is why the thread is visually
//     quieter than the findings and never shows severity chips of its own.
//  2. The conversation is not a dead end. Once the user has corrected a
//     false positive or supplied the log line that proves a goal was met,
//     「基于对话重新生成」 folds all of it into a new version of the report —
//     the CTA at the bottom, highlighted exactly when there is something new
//     to fold in.

import { useEffect, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import type { CodeReview, CodeReviewFollowup } from '../../api/client';
import MarkdownRender from '../../components/MarkdownRender';
import { IconRobot } from '../../components/icons';
import { fmtDateTime } from '../../utils/intl';
import type { StreamLine } from './shared';

interface Props {
  review: CodeReview;
  followups: CodeReviewFollowup[];
  question: string;
  onQuestionChange: (v: string) => void;
  asking: boolean;
  askLines: StreamLine[];
  askError: string;
  onAsk: () => void;
  onDeleteFollowup: (fu: CodeReviewFollowup) => void;
  /** Answered turns not yet reflected in the report. */
  pendingFollowups: number;
  canRegenerate: boolean;
  regenerating: boolean;
  onRegenerate: () => void;
}

export default function ReviewChat({
  review, followups, question, onQuestionChange, asking, askLines, askError,
  onAsk, onDeleteFollowup, pendingFollowups, canRegenerate, regenerating, onRegenerate,
}: Props) {
  const { t } = useTranslation();
  const streamRef = useRef<HTMLDivElement>(null);
  const boxRef = useRef<HTMLTextAreaElement>(null);

  useEffect(() => {
    if (streamRef.current) streamRef.current.scrollTop = streamRef.current.scrollHeight;
  }, [askLines]);

  // The report has to exist before there is anything to ask about.
  if (!review.result_md) {
    return (
      <div className="cr-chat">
        <div className="cr-followups-head">
          <span className="cr-label">{t('projects.detail.reviewFollowupTitle')}</span>
        </div>
        <p className="cr-hint">{t('projects.detail.reviewFollowupNoReport')}</p>
      </div>
    );
  }

  // Starter questions. They exist because "问点什么" is a real blank-page
  // problem on a report you have just skimmed — and because each of these
  // three is a question the reviewer can answer well from its own session.
  const chips: string[] = [
    t('projects.detail.reviewChipRisk'),
    t('projects.detail.reviewChipGoal'),
    t('projects.detail.reviewChipFix'),
  ];

  const applyChip = (text: string) => {
    onQuestionChange(question.trim() ? `${question.trim()}\n${text}` : text);
    boxRef.current?.focus();
  };

  const disabled = asking || regenerating || review.status === 'running';

  return (
    <div className="cr-chat">
      <div className="cr-followups-head">
        <span className="cr-label">{t('projects.detail.reviewFollowupTitle')}</span>
        <span className="cr-hint">{t('projects.detail.reviewFollowupHint')}</span>
      </div>

      <div className="cr-followups">
        {followups.length === 0 && !asking && (
          <p className="cr-sev-empty">{t('projects.detail.reviewFollowupEmpty')}</p>
        )}

        {followups.map(fu => (
          <article key={fu.id} className={`cr-followup cr-followup-${fu.status}`}>
            <div className="cr-followup-q">
              <span className="cr-followup-tag">{t('projects.detail.reviewFollowupQ')}</span>
              <span className="cr-followup-qtext">{fu.question}</span>
              <button
                className="btn btn-sm cr-followup-del"
                onClick={() => onDeleteFollowup(fu)}
                disabled={fu.status === 'running'}
              >{t('projects.detail.reviewDeleteBtn')}</button>
            </div>
            {fu.status === 'running' ? (
              <p className="cr-followup-pending">{t('projects.detail.reviewFollowupRunning')}</p>
            ) : fu.status === 'error' ? (
              <p className="cr-followup-errtext">{t('projects.detail.reviewFollowupFailed')}：{fu.answer}</p>
            ) : (
              <div className="cr-followup-a md-prose">
                <span className="cr-followup-tag">{t('projects.detail.reviewFollowupA')}</span>
                <MarkdownRender content={fu.answer} />
              </div>
            )}
            <div className="cr-followup-meta">
              <span>{fmtDateTime(fu.created_at)}</span>
              {fu.model && <span><IconRobot size={12} /> {fu.model}</span>}
              {/* Only worth flagging the degraded case: a resumed answer is
                  the expectation, a fresh one explains why an answer may be
                  shallower than the report it talks about. */}
              {fu.status === 'done' && !fu.resumed && (
                <span className="cr-followup-fresh">{t('projects.detail.reviewFollowupFresh')}</span>
              )}
            </div>
          </article>
        ))}

        {asking && (
          <div className="coding-panel cr-followup-stream" ref={streamRef} style={{ maxHeight: 220 }}>
            {askLines.map((line, i) => (
              <div key={i} className={`coding-line coding-line-${line.type}`}>{line.content}</div>
            ))}
            <div className="coding-line coding-line-tool_call">{t('projects.detail.reviewFollowupRunning')}</div>
          </div>
        )}
      </div>

      {askError && <div className="cr-error">{askError}</div>}

      <div className="cr-chips">
        {chips.map(c => (
          <button key={c} className="cr-chip" onClick={() => applyChip(c)} disabled={disabled}>{c}</button>
        ))}
      </div>

      <div className="cr-followup-composer">
        <textarea
          ref={boxRef}
          className="form-input cr-desc"
          rows={3}
          placeholder={t('projects.detail.reviewFollowupPlaceholder')}
          value={question}
          onChange={e => onQuestionChange(e.target.value)}
          disabled={disabled}
        />
        <button
          className="btn btn-primary"
          onClick={onAsk}
          disabled={disabled || !question.trim()}
        >
          {asking ? t('projects.detail.reviewFollowupAsking') : t('projects.detail.reviewFollowupAsk')}
        </button>
      </div>

      {/* The payoff of the conversation. Highlighted only when there are
          answers the current report hasn't taken in yet — otherwise it is a
          plain secondary action, because regenerating with nothing new just
          spends tokens to say the same thing again. */}
      <div className="cr-regen">
        <button
          className={`btn btn-sm${pendingFollowups > 0 ? ' btn-primary' : ''}`}
          onClick={onRegenerate}
          disabled={regenerating || asking || !canRegenerate}
        >
          {regenerating
            ? t('projects.detail.reviewRegenerateBusy')
            : pendingFollowups > 0
              ? t('projects.detail.reviewRegenerateWithN', { n: pendingFollowups })
              : t('projects.detail.reviewRegenerate')}
        </button>
        <span className="cr-hint">
          {pendingFollowups > 0
            ? t('projects.detail.reviewRegeneratePending', { n: pendingFollowups })
            : t('projects.detail.reviewRegenerateHint')}
        </span>
      </div>
    </div>
  );
}
