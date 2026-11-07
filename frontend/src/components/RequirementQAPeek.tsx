// RequirementQAPeek — a read-only window onto a requirement's stage Q&A.
//
// The stage Q&A thread (requirement_qa_turns) lives on the requirement
// DETAIL page, in two panels buried in a long vertical page (方案设计 and
// 开发实现 blocks, no tabs). Asking "why was it designed this way?" from a
// list therefore meant: open the row, scroll a screen or two, find the
// panel, expand it. This dialog answers the same question from the list.
//
// It is deliberately READ-ONLY. Asking, deleting and clearing a thread are
// the detail page's job (StageQAChat owns the SSE stream, the model picker
// and the export buttons); duplicating them here would give the user two
// places to mutate the same thread.
//
// Both stages are fetched concurrently and independently: a failure on one
// stage must not blank the other.

import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  qaChatApi,
  type QAStage,
  type RequirementQATurn,
} from '../api/client';
import { IconChat, IconClose } from './icons';
import MarkdownRender from './MarkdownRender';
import { fmtDateTime } from '../utils/intl';
import { questionHeadline } from '../utils/qaQuestion';
import { qaStageLabel } from '../utils/qaExport';
import './RequirementQAPeek.css';

interface Props {
  requirementId: string;
  requirementTitle: string;
  onClose: () => void;
  /** Jump to the requirement's detail page (the deep view). */
  onOpenDetail?: () => void;
}

const STAGES: QAStage[] = ['design', 'coding'];

export function RequirementQAPeek({
  requirementId,
  requirementTitle,
  onClose,
  onOpenDetail,
}: Props) {
  const { t } = useTranslation();
  const [turns, setTurns] = useState<Record<QAStage, RequirementQATurn[]>>({
    design: [],
    coding: [],
  });
  const [loading, setLoading] = useState(true);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setFailed(false);
    Promise.all(
      STAGES.map(stage =>
        qaChatApi.list(requirementId, stage)
          .then(list => ({ stage, list }))
          .catch(() => ({ stage, list: null as RequirementQATurn[] | null })),
      ),
    ).then(results => {
      if (cancelled) return;
      const next = { design: [] as RequirementQATurn[], coding: [] as RequirementQATurn[] };
      let anyFailed = false;
      for (const r of results) {
        if (r.list === null) anyFailed = true;
        else next[r.stage] = r.list;
      }
      setTurns(next);
      setFailed(anyFailed);
      setLoading(false);
    });
    return () => { cancelled = true; };
  }, [requirementId]);

  // Esc closes the dialog.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [onClose]);

  const total = turns.design.length + turns.coding.length;

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal-box qa-peek-box" onClick={e => e.stopPropagation()}>
        <div className="modal-header">
          <h3>
            <IconChat size={14} className="icon-mr" />
            {t('requirements.qaPeek.title', { title: requirementTitle })}
          </h3>
          <button
            type="button"
            className="qa-peek-close"
            onClick={onClose}
            aria-label={t('requirements.qaPeek.close')}
            title={t('requirements.qaPeek.close')}
          >
            <IconClose size={14} />
          </button>
        </div>

        <div className="modal-body qa-peek-body">
          {loading && <div className="qa-peek-empty">{t('requirements.qaPeek.loading')}</div>}
          {!loading && total === 0 && (
            <div className="qa-peek-empty">
              {failed ? t('requirements.qaPeek.loadFailed') : t('requirements.qaPeek.empty')}
            </div>
          )}
          {!loading && total > 0 && STAGES.map(stage => {
            const list = turns[stage];
            if (list.length === 0) return null;
            return (
              <section key={stage} className="qa-peek-stage">
                <div className="qa-peek-stage-head">
                  <span className="qa-peek-stage-name">{qaStageLabel(stage)}</span>
                  <span className="qa-peek-stage-count">
                    {t('requirements.qaPeek.turnCount', { count: list.length })}
                  </span>
                </div>
                {list.map((turn, i) => (
                  // Each turn is a <details open>: the expanded form is what
                  // the user asked for ("quickly view the Q&A"), and the
                  // native summary still lets them collapse a long thread
                  // down to its question headlines.
                  <details key={turn.id} className="qa-peek-turn" open>
                    <summary className="qa-peek-q">
                      <span className="qa-peek-tag">{t('requirements.qaChat.questionTag')}</span>
                      <span className="qa-peek-qtext">
                        <span className="qa-peek-qnum">{`Q${i + 1}`}</span>
                        {questionHeadline(turn.question)}
                      </span>
                    </summary>
                    <div className="qa-peek-turn-body">
                      {turn.question !== questionHeadline(turn.question) && (
                        <p className="qa-peek-qfull">{turn.question}</p>
                      )}
                      {turn.status === 'running' ? (
                        <p className="qa-peek-pending">{t('requirements.qaChat.running')}</p>
                      ) : turn.status === 'error' ? (
                        <p className="qa-peek-errtext">
                          {t('requirements.qaChat.failed')}：{turn.answer}
                        </p>
                      ) : (
                        <div className="qa-peek-a">
                          <span className="qa-peek-tag">{t('requirements.qaChat.answerTag')}</span>
                          <div className="qa-peek-abody md-prose">
                            <MarkdownRender content={turn.answer} />
                          </div>
                        </div>
                      )}
                      <div className="qa-peek-meta">
                        <span>{fmtDateTime(turn.created_at)}</span>
                        {turn.model && <span>{turn.model}</span>}
                        {turn.status === 'done' && !turn.resumed && (
                          <span className="qa-peek-fresh">{t('requirements.qaChat.freshBadge')}</span>
                        )}
                      </div>
                    </div>
                  </details>
                ))}
              </section>
            );
          })}
        </div>

        <div className="modal-actions">
          {onOpenDetail && (
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => { onOpenDetail(); onClose(); }}
            >
              {t('requirements.qaPeek.openDetail')}
            </button>
          )}
          <button type="button" className="btn btn-primary btn-sm" onClick={onClose}>
            {t('requirements.qaPeek.close')}
          </button>
        </div>
      </div>
    </div>
  );
}

export default RequirementQAPeek;
