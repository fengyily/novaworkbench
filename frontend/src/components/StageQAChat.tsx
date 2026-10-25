import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  API_BASE,
  authedFetch,
  qaChatApi,
  wizardApi,
  type QAStage,
  type RequirementQATurn,
} from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import AtMentionTextarea from './AtMentionTextarea';
import { FullscreenButton } from './FullscreenButton';
import MarkdownRender from './MarkdownRender';
import ModelSelect from './ModelSelect';
import { IconChat, IconClose, IconRefresh, IconRobot, IconTrash } from './icons';
import { errorMessage } from '../utils/errMsg';
import { exportDesignPdf } from '../utils/exportDesignPdf';
import { fmtDateTime } from '../utils/intl';
import { appendLogLine, type LogLine } from '../utils/logLines';
import {
  buildQAChatMarkdown,
  downloadQAChatMarkdown,
  exportableTurns,
  qaExportLabel,
  qaExportTitle,
} from '../utils/qaExport';
import { useFullscreen } from '../utils/useFullscreen';
import './StageQAChat.css';

// StageQAChat — a read-only Q&A thread about one production conversation.
//
// The thread asks the 方案设计会话 (design) or the 开发会话 (coding) "why did
// you do it this way". Three properties make it safe to have next to the
// artifacts it talks about:
//
//  1. It never touches them. The backend forks a DEDICATED QA session off the
//     stage session, so the production conversation and its jsonl transcript
//     stay byte-identical. A Q&A turn can never leak into the next
//     `--resume` of the real stage.
//  2. It is a thread, not a transcript blob. The backend persists one row per
//     turn, so the history survives a refresh and a backend restart. The
//     answer is re-read from the API on job_done — never reconstructed from
//     the SSE stream.
//  3. It degrades instead of failing. When the source session is gone the
//     backend re-answers from the plan document + repo code and marks the
//     turn `resumed: false`; the UI shows that badge so a shallower answer is
//     never mistaken for the real thing.
//
// The component owns its own SSE stream and state (it does NOT lift them
// into RequirementDetail — that page is already 6.7k lines).

interface Props {
  requirementId: string;
  /** Requirement title, used as the exported document's heading and
   *  filename stem — the thread itself never displays it. */
  requirementTitle?: string;
  /** Which production conversation this thread asks about. */
  stage: QAStage;
  /** The requirement's CURRENT stage session id. A turn whose
   *  `source_session_id` differs from the previous turn's means the stage
   *  was re-run in between — the UI draws a divider there. */
  stageSessionId?: string;
  /** Model override (the parent owns the selection so it can persist it). */
  model?: string;
  defaultModel?: string;
  onModelChange?: (model: string) => void;
  /** Claude config override, same ownership as `model`. */
  claudeConfigId?: string;
  onClaudeConfigChange?: (id: string) => void;
}

const MODEL_STAGE: Record<QAStage, 'architect' | 'developer'> = {
  design: 'architect',
  coding: 'developer',
};

export default function StageQAChat({
  requirementId,
  requirementTitle = '',
  stage,
  stageSessionId,
  model,
  defaultModel,
  onModelChange,
  claudeConfigId,
  onClaudeConfigChange,
}: Props) {
  const { t } = useTranslation();
  const [turns, setTurns] = useState<RequirementQATurn[]>([]);
  const [loading, setLoading] = useState(true);
  const [asking, setAsking] = useState(false);
  const [question, setQuestion] = useState('');
  const [error, setError] = useState('');
  const [expanded, setExpanded] = useState(false);
  const [liveLines, setLiveLines] = useState<LogLine[]>([]);
  const [exportOpen, setExportOpen] = useState(false);
  const [exportingPdf, setExportingPdf] = useState(false);
  // CSS-only fullscreen (same mechanism as the other SSE panels): the
  // EventStream, refs and jobs keep running untouched while the panel
  // becomes viewport-fixed. Esc exits via the hook.
  const { isFullscreen, toggle: toggleFullscreen, exit: exitFullscreen } = useFullscreen();

  const esRef = useRef<EventStream | null>(null);
  // Guards against a late stream callback writing into an unmounted tree.
  const mountedRef = useRef(true);
  const threadRef = useRef<HTMLDivElement | null>(null);

  const closeStream = useCallback(() => {
    esRef.current?.close();
    esRef.current = null;
  }, []);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      esRef.current?.close();
      esRef.current = null;
    };
  }, []);

  const reload = useCallback(async () => {
    try {
      const rows = await qaChatApi.list(requirementId, stage);
      if (!mountedRef.current) return;
      setTurns(Array.isArray(rows) ? rows : []);
    } catch (err) {
      if (!mountedRef.current) return;
      setError(errorMessage(err));
    } finally {
      if (mountedRef.current) setLoading(false);
    }
  }, [requirementId, stage]);

  useEffect(() => {
    setLoading(true);
    setError('');
    void reload();
  }, [reload]);

  // Re-attach to a still-running turn after a page refresh. The JobStore
  // replays its log on subscribe, so the user sees the live progress again
  // instead of a dead spinner.
  useEffect(() => {
    const running = turns.find((x) => x.status === 'running');
    if (!running || !running.job_id || asking) return;
    let cancelled = false;
    void (async () => {
      try {
        const snap = await wizardApi.getJob(running.job_id);
        if (cancelled || !mountedRef.current) return;
        if (snap?.status === 'running') {
          subscribe(running.job_id);
        } else {
          // The job is gone (backend restarted / evicted) — the turn row is
          // the truth; re-read it.
          await reload();
        }
      } catch {
        await reload();
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [turns, asking]);

  // subscribe wires the SSE for a job. The answer itself is NOT taken from
  // the stream — on job_done the thread is re-read from the API, which is
  // the authoritative source.
  const subscribe = useCallback(
    (id: string) => {
      closeStream();
      setAsking(true);
      setLiveLines([]);
      esRef.current = createEventStream(
        qaChatApi.streamJobUrl(id),
        (evt) => {
          if (!mountedRef.current) return;
          if (evt.type === 'job_done') {
            closeStream();
            setAsking(false);
            void reload();
            return;
          }
          if (evt.type === 'error') {
            closeStream();
            setAsking(false);
            setError(String(evt.content ?? t('requirements.qaChat.execError')));
            void reload();
            return;
          }
          if (evt.type === 'phase' || evt.type === 'tool_call') {
            const at = typeof evt.at === 'number' ? evt.at : Date.now();
            setLiveLines((prev) =>
              appendLogLine(prev.slice(-60), { type: evt.type, content: evt.content ?? '', at }),
            );
            return;
          }
          if (evt.type === 'message') {
            const at = typeof evt.at === 'number' ? evt.at : Date.now();
            setLiveLines((prev) =>
              appendLogLine(prev.slice(-60), { type: 'message', content: evt.content ?? '', at }),
            );
          }
        },
        () => {
          // The stream dropped. Poll the job snapshot once: a still-running
          // job means a transient drop (re-arm), a finished one means we
          // simply missed job_done (re-read the thread).
          void (async () => {
            if (!mountedRef.current) return;
            try {
              const snap = await authedFetch(`${API_BASE}/api/wizard/jobs/${id}`).then((r) => r.json());
              const status = snap?.data?.status;
              if (status === 'running') {
                subscribe(id);
                return;
              }
            } catch {
              /* fall through to the reload below */
            }
            closeStream();
            setAsking(false);
            void reload();
          })();
        },
      );
    },
    [closeStream, reload, t],
  );

  const ask = useCallback(
    async (text: string) => {
      const q = text.trim();
      if (!q || asking) return;
      setQuestion('');
      setError('');
      setAsking(true);
      setLiveLines([]);
      try {
        const res = await qaChatApi.ask(requirementId, stage, q, {
          ...(model ? { model } : {}),
          ...(claudeConfigId ? { claude_config_id: claudeConfigId } : {}),
        });
        // Show the question immediately — the persisted thread only gets the
        // row once the backend has created it.
        setTurns((prev) => [
          ...prev,
          {
            id: res.turn_id,
            requirement_id: requirementId,
            stage,
            question: q,
            answer: '',
            status: 'running',
            job_id: res.job_id,
            model: model ?? '',
            session_id: '',
            source_session_id: '',
            resumed: false,
            created_by: '',
            created_at: new Date().toISOString(),
            updated_at: new Date().toISOString(),
          },
        ]);
        subscribe(res.job_id);
      } catch (err) {
        if (!mountedRef.current) return;
        setAsking(false);
        setError(errorMessage(err));
      }
    },
    [asking, requirementId, stage, model, claudeConfigId, subscribe],
  );

  // Keep the newest turn in view when the panel is open. In fullscreen the
  // thread is the only scrolling region, so without this a new question would
  // be appended below the fold.
  useEffect(() => {
    if (!expanded) return;
    const el = threadRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [expanded, turns]);

  const removeTurn = useCallback(
    async (turn: RequirementQATurn) => {
      if (!window.confirm(t('requirements.qaChat.deleteConfirm'))) return;
      try {
        await qaChatApi.removeTurn(requirementId, stage, turn.id);
        await reload();
      } catch (err) {
        if (mountedRef.current) setError(errorMessage(err));
      }
    },
    [requirementId, stage, reload, t],
  );

  const resetThread = useCallback(async () => {
    if (!window.confirm(t('requirements.qaChat.resetConfirm'))) return;
    try {
      await qaChatApi.reset(requirementId, stage);
      setTurns([]);
      setLiveLines([]);
      setError('');
    } catch (err) {
      if (mountedRef.current) setError(errorMessage(err));
    }
  }, [requirementId, stage, t]);

  // ── Export ───────────────────────────────────────────────────────────────
  //
  // Both formats come from the SAME Markdown string: the `.md` is the thread
  // as-is, and the PDF goes through exportDesignPdf, which pre-bakes mermaid
  // so a diagram inside an answer survives into the file.
  const canExport = exportableTurns(turns).length > 0;

  const handleExportMd = useCallback(() => {
    downloadQAChatMarkdown({ requirementTitle, requirementId, stage, turns });
  }, [requirementTitle, requirementId, stage, turns]);

  const handleExportPdf = useCallback(async () => {
    if (exportingPdf) return;
    setExportingPdf(true);
    setError('');
    try {
      await exportDesignPdf({
        title: qaExportTitle({ requirementTitle, requirementId, stage }),
        meta: requirementId,
        markdown: buildQAChatMarkdown({ requirementTitle, requirementId, stage, turns }),
        filename: requirementTitle.trim() || requirementId,
        pdfLabel: qaExportLabel(stage),
      });
    } catch (err) {
      // Surface inline, in the same panel the user is looking at — a failed
      // export must not look like a dead button.
      if (mountedRef.current) {
        setError(`${t('requirements.qaChat.exportFailed')}${errorMessage(err)}`);
      }
    } finally {
      if (mountedRef.current) setExportingPdf(false);
    }
  }, [exportingPdf, requirementTitle, requirementId, stage, turns, t]);

  // Collapsing must also leave fullscreen: a fixed, empty panel with only its
  // header on screen would look like a broken overlay.
  const collapse = useCallback(() => {
    exitFullscreen();
    setExpanded(false);
  }, [exitFullscreen]);

  // The stage was re-run: the current stage session no longer matches the
  // one the thread was forked from. Flag it so the panel can say so.
  const drifted =
    !!stageSessionId &&
    turns.some((x) => x.source_session_id && x.source_session_id !== stageSessionId);

  const hasContent = turns.length > 0;

  return (
    <div className={`qa-panel${expanded ? ' is-expanded' : ''}${isFullscreen ? ' is-fullscreen' : ''}`}>
      <div className="qa-header">
        <button
          type="button"
          className="qa-header-toggle"
          onClick={() => (expanded ? collapse() : setExpanded(true))}
          title={expanded ? t('requirements.qaChat.collapseBtn') : t('requirements.qaChat.expandBtn')}
        >
          <IconChat size={15} className="icon-mr" />
          <span>{t('requirements.qaChat.title')}</span>
          {hasContent && <span className="qa-count">{turns.length}</span>}
        </button>
        <div className="qa-header-actions">
          {drifted && (
            <span className="qa-drift-badge" title={t('requirements.qaChat.driftHint')}>
              {t('requirements.qaChat.driftBadge')}
            </span>
          )}
          <ModelSelect
            value={model ?? ''}
            onChange={(m) => onModelChange?.(m)}
            disabled={asking}
            working={asking}
            stage={MODEL_STAGE[stage]}
            defaultModelName={defaultModel}
            configId={claudeConfigId || undefined}
            onConfigChange={(id) => onClaudeConfigChange?.(id)}
            style={{ minWidth: 150 }}
          />
          {hasContent && (
            <div className="qa-export">
              <button
                type="button"
                className="btn btn-sm"
                onClick={() => setExportOpen((v) => !v)}
                disabled={!canExport || exportingPdf}
                aria-expanded={exportOpen}
                title={
                  canExport
                    ? t('requirements.qaChat.exportTitleAttr')
                    : t('requirements.qaChat.exportDisabledTitle')
                }
              >
                {exportingPdf ? t('requirements.qaChat.exportBusy') : `${t('requirements.qaChat.exportBtn')} ▾`}
              </button>
              {exportOpen && (
                <>
                  <div className="qa-export-backdrop" onClick={() => setExportOpen(false)} />
                  <div className="qa-export-menu" role="menu">
                    <button
                      type="button"
                      role="menuitem"
                      onClick={() => {
                        setExportOpen(false);
                        handleExportMd();
                      }}
                    >
                      {t('requirements.qaChat.exportMd')}
                    </button>
                    <button
                      type="button"
                      role="menuitem"
                      onClick={() => {
                        setExportOpen(false);
                        void handleExportPdf();
                      }}
                    >
                      {t('requirements.qaChat.exportPdf')}
                    </button>
                  </div>
                </>
              )}
            </div>
          )}
          {hasContent && !asking && (
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => void resetThread()}
              title={t('requirements.qaChat.resetTitle')}
            >
              <IconTrash size={13} className="icon-mr" />
              {t('requirements.qaChat.resetBtn')}
            </button>
          )}
          {expanded && (
            <FullscreenButton isFullscreen={isFullscreen} onClick={toggleFullscreen} />
          )}
          {expanded && (
            <button
              type="button"
              className="btn btn-sm"
              onClick={collapse}
              title={t('requirements.qaChat.collapseBtn')}
            >
              <IconClose size={13} />
            </button>
          )}
        </div>
      </div>

      {expanded && (
        <>
          <p className="qa-hint">{t('requirements.qaChat.hint')}</p>

          <div className="qa-thread" ref={threadRef}>
            {loading && <p className="qa-empty">{t('requirements.qaChat.loading')}</p>}
            {!loading && turns.length === 0 && (
              <p className="qa-empty">{t('requirements.qaChat.empty')}</p>
            )}
            {turns.map((turn, i) => {
              const prev = i > 0 ? turns[i - 1] : null;
              // A change of the fork source between two turns means the
              // stage was re-run in between: the answers below the divider
              // were produced against a different (older) conversation.
              const rotated = !!prev && prev.source_session_id !== turn.source_session_id;
              return (
                <div key={turn.id}>
                  {rotated && (
                    <div className="qa-rotate-divider">
                      <span>{t('requirements.qaChat.threadRotated')}</span>
                    </div>
                  )}
                  <article className={`qa-turn qa-turn-${turn.status}`}>
                    <div className="qa-turn-q">
                      <span className="qa-turn-tag">{t('requirements.qaChat.questionTag')}</span>
                      <span className="qa-turn-qtext">{turn.question}</span>
                      {turn.status !== 'running' && (
                        <button
                          type="button"
                          className="btn btn-sm qa-turn-del"
                          onClick={() => void removeTurn(turn)}
                          title={t('requirements.qaChat.deleteTitle')}
                        >
                          <IconTrash size={12} />
                        </button>
                      )}
                    </div>
                    {turn.status === 'running' ? (
                      <p className="qa-turn-pending">{t('requirements.qaChat.running')}</p>
                    ) : turn.status === 'error' ? (
                      <p className="qa-turn-errtext">
                        {t('requirements.qaChat.failed')}：{turn.answer}
                      </p>
                    ) : (
                      <div className="qa-turn-a">
                        <span className="qa-turn-tag">
                          <IconRobot size={12} className="icon-mr" />
                          {t('requirements.qaChat.answerTag')}
                        </span>
                        <div className="qa-turn-abody md-prose">
                          <MarkdownRender content={turn.answer} />
                        </div>
                      </div>
                    )}
                    <div className="qa-turn-meta">
                      <span>{fmtDateTime(turn.created_at)}</span>
                      {turn.model && <span>{turn.model}</span>}
                      {turn.status === 'done' && !turn.resumed && (
                        <span className="qa-turn-fresh">{t('requirements.qaChat.freshBadge')}</span>
                      )}
                    </div>
                  </article>
                </div>
              );
            })}
          </div>

          {liveLines.length > 0 && (
            <div className="qa-live">
              {liveLines.map((line, i) => (
                <div key={i} className={`qa-live-line qa-live-${line.type}`}>
                  {line.content}
                </div>
              ))}
            </div>
          )}

          {error && (
            <div className="qa-error">
              {error}
              <button type="button" className="qa-error-close" onClick={() => setError('')}>
                <IconClose size={12} />
              </button>
            </div>
          )}

          <div className="qa-composer">
            <AtMentionTextarea
              value={question}
              onChange={setQuestion}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && !e.shiftKey) {
                  e.preventDefault();
                  void ask(question);
                }
              }}
              placeholder={t('requirements.qaChat.placeholder')}
              className="form-input qa-textarea"
              disabled={asking}
              rows={2}
            />
            <button
              type="button"
              className="btn btn-primary"
              onClick={() => void ask(question)}
              disabled={asking || !question.trim()}
            >
              {asking ? t('requirements.qaChat.asking') : t('requirements.qaChat.askBtn')}
            </button>
          </div>
          {asking && (
            <button
              type="button"
              className="btn btn-sm qa-refresh"
              onClick={() => void reload()}
              title={t('requirements.qaChat.refreshTitle')}
            >
              <IconRefresh size={12} className="icon-mr" />
              {t('requirements.qaChat.refreshBtn')}
            </button>
          )}
        </>
      )}
    </div>
  );
}
