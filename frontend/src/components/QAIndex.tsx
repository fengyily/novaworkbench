import { useTranslation } from 'react-i18next';
import type { RequirementQATurn } from '../api/client';
import { IconMagnifier, IconTrash } from './icons';

// QAIndex — the question index of one stage Q&A thread.
//
// A thread of questions and answers reads like a document once it grows past
// a handful of turns, and a document without a table of contents forces the
// reader to scroll to find "the one about the turn table". This component IS
// that table of contents.
//
// It is ONE component with TWO presentations, not two components:
//
//   • `drawer` — sits above the thread in global mode. Clicking an entry
//     scrolls the thread to that turn.
//   • `rail`   — the permanent left column in split mode. Clicking an entry
//     swaps the detail pane.
//
// Both presentations read the same `entries`, so the two can never drift.
// The parent owns the data (it already has the full turn list and knows how
// to filter it); this file owns only the rendering.
//
// Everything shown here comes from data the backend already stores: the
// question text is `requirement_qa_turns.question`, written once when the
// turn is created and never touched again. No new field, no new endpoint.

/** One row of the index. */
export interface QAIndexEntry {
  turn: RequirementQATurn;
  /** 1-based position in the FULL thread. Deliberately not the position in
   *  the filtered list: filtering the index must not renumber it, or "Q3"
   *  would mean a different question depending on what you typed. */
  no: number;
  /** True when this turn starts a new fork segment — the stage was re-run
   *  between it and the previous turn, so the answers below it were produced
   *  against a different conversation. Mirrors the divider the thread draws. */
  rotated: boolean;
}

interface Props {
  /** The rows to show, already filtered by the parent. */
  entries: QAIndexEntry[];
  /** Size of the FULL thread — the count badge must not shrink when the
   *  filter narrows the list. */
  total: number;
  /** Which turn is selected (split mode). */
  selectedId?: string;
  onSelect: (turn: RequirementQATurn) => void;
  /** Omitted when deleting is not offered from this presentation. */
  onDelete?: (turn: RequirementQATurn) => void;
  filter: string;
  onFilterChange: (value: string) => void;
  /** Which presentation to render. */
  variant: 'drawer' | 'rail';
}

/**
 * The headline of a question: its first non-empty line. A question can be
 * several lines long (the composer allows them), and a table of contents
 * wants a label, not the whole paragraph.
 */
function questionPreview(question: string): string {
  for (const line of question.split('\n')) {
    const trimmed = line.trim();
    if (trimmed) return trimmed;
  }
  return question.trim();
}

export default function QAIndex({
  entries,
  total,
  selectedId,
  onSelect,
  onDelete,
  filter,
  onFilterChange,
  variant,
}: Props) {
  const { t } = useTranslation();

  return (
    <nav className={`qa-index qa-index--${variant}`} aria-label={t('requirements.qaChat.indexTitle')}>
      <div className="qa-index-head">
        <span className="qa-index-title">{t('requirements.qaChat.indexTitle')}</span>
        <span className="qa-index-count">{t('requirements.qaChat.indexCount', { count: total })}</span>
      </div>

      {total > 0 && (
        <div className="qa-index-filter-wrap">
          <IconMagnifier size={12} className="icon-mr" />
          <input
            type="text"
            className="qa-index-filter"
            value={filter}
            placeholder={t('requirements.qaChat.indexFilterPlaceholder')}
            onChange={(e) => onFilterChange(e.target.value)}
          />
        </div>
      )}

      <div className="qa-index-list">
        {entries.length === 0 && (
          <p className="qa-index-empty">
            {filter.trim()
              ? t('requirements.qaChat.indexFilterEmpty')
              : t('requirements.qaChat.indexEmpty')}
          </p>
        )}
        {entries.map((entry) => {
          const { turn, no, rotated } = entry;
          const selected = turn.id === selectedId;
          return (
            <div key={turn.id}>
              {rotated && (
                <div className="qa-index-rotated">{t('requirements.qaChat.threadRotated')}</div>
              )}
              <div className={`qa-index-row${selected ? ' is-selected' : ''}`}>
                <button
                  type="button"
                  className="qa-index-item"
                  onClick={() => onSelect(turn)}
                  title={questionPreview(turn.question)}
                >
                  <span className="qa-index-num">
                    {t('requirements.qaChat.indexNum', { n: no })}
                  </span>
                  <span className="qa-index-q">{questionPreview(turn.question)}</span>
                  {turn.status === 'running' && (
                    <span
                      className="qa-index-dot is-running"
                      title={t('requirements.qaChat.indexRunning')}
                    />
                  )}
                  {turn.status === 'error' && (
                    <span
                      className="qa-index-dot is-error"
                      title={t('requirements.qaChat.indexError')}
                    />
                  )}
                  {turn.status === 'done' && !turn.resumed && (
                    <span
                      className="qa-index-dot is-fresh"
                      title={t('requirements.qaChat.freshBadge')}
                    />
                  )}
                </button>
                {onDelete && turn.status !== 'running' && (
                  <button
                    type="button"
                    className="btn btn-sm qa-index-del"
                    onClick={() => onDelete(turn)}
                    title={t('requirements.qaChat.deleteTitle')}
                  >
                    <IconTrash size={12} />
                  </button>
                )}
              </div>
            </div>
          );
        })}
      </div>
    </nav>
  );
}
