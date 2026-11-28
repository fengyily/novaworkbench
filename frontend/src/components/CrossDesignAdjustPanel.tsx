import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import MarkdownRender from './MarkdownRender';
import AtMentionTextarea from './AtMentionTextarea';
import { createEventStream, type EventStream } from '../api/stream';
import {
  crossRequirementsApi,
  type CrossPendingDecision,
  type CrossRequirementQATurn,
} from '../api/client';
import { errorMessage } from '../utils/errMsg';
import './CrossDesignAdjustPanel.css';

// 「方案调整对话」面板 —— 挂在跨项目需求的整体方案下面。
//
// 它解决的是这么一件事：整体方案跑完之后，方案里常常留着「待决策事项」
// （接口超时取多少、幂等键用哪种策略……）。用户在这里跟 AI 谈拢这些点，
// 谈完点「按讨论结论重新补全方案」，方案作业会带上这段讨论重跑一次，产出
// 一份把结论吃进去的完整方案。
//
// 与单项目需求页的「阶段答疑」面板（StageQAChat）同构，但更小：这里没有
// 问答索引、没有分栏视图、没有全屏、没有导出 —— 跨项目需求的整体方案只有
// 一条线程，够用就行。
//
// 两条约定照抄 StageQAChat：
//
//  1. **答案不从 SSE 重建**：流只用来显示进度，终帧之后重新拉一次线程 ——
//     落库的那份才是权威。
//  2. **自己持有自己的流**：详情页里的作业驱动（driverRef）是单实例，被
//     方案 / 拆分 / 验证三个作业占着，不能共享。

export interface CrossDesignAdjustPanelProps {
  xreqId: string;
  /** 方案里还没定、需要人拍板的点（详情接口现算出来的）。 */
  pendingDecisions: CrossPendingDecision[];
  /** 回收站 / 归档 → 整块置灰。 */
  disabled?: boolean;
  /** 「按讨论结论重新补全方案」—— 方案作业由页面起（它有作业驱动 / SSE / 恢复）。 */
  onApplyDiscussion?: () => void;
}

export default function CrossDesignAdjustPanel({
  xreqId,
  pendingDecisions,
  disabled,
  onApplyDiscussion,
}: CrossDesignAdjustPanelProps) {
  const { t } = useTranslation();

  const [turns, setTurns] = useState<CrossRequirementQATurn[]>([]);
  const [loading, setLoading] = useState(true);
  const [asking, setAsking] = useState(false);
  const [question, setQuestion] = useState('');
  const [error, setError] = useState('');
  const [expanded, setExpanded] = useState(false);

  const esRef = useRef<EventStream | null>(null);
  // streamToken — 每次订阅都递增。token 过期的流（被重连取代，或页面已卸载）
  // 不得再碰状态。
  const streamTokenRef = useRef(0);
  const mountedRef = useRef(true);
  const reconnectTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const pollTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // subscribeRef — subscribe 与 pollState 互相调用（重连 ↔ 降级），用 ref
  // 打破循环依赖。
  const subscribeRef = useRef<((jobId: string, attempt: number) => void) | null>(null);

  useEffect(() => () => {
    // 让所有还在飞的回调失效，然后拆掉流与定时器。
    mountedRef.current = false;
    streamTokenRef.current++;
    esRef.current?.close();
    esRef.current = null;
    if (reconnectTimerRef.current) clearTimeout(reconnectTimerRef.current);
    if (pollTimerRef.current) clearTimeout(pollTimerRef.current);
  }, []);

  const reload = useCallback(async () => {
    try {
      const state = await crossRequirementsApi.adjustState(xreqId);
      if (!mountedRef.current) return;
      setTurns(Array.isArray(state?.turns) ? state.turns : []);
      setError('');
    } catch (err) {
      if (!mountedRef.current) return;
      setError(errorMessage(err));
    } finally {
      if (mountedRef.current) setLoading(false);
    }
  }, [xreqId]);

  useEffect(() => {
    setLoading(true);
    void reload();
  }, [reload]);

  // pollState — 断线重连也失败之后的降级：每 3 秒拉一次线程状态。
  const pollState = useCallback(() => {
    const token = streamTokenRef.current;
    const tick = async () => {
      if (!mountedRef.current || streamTokenRef.current !== token) return;
      try {
        const state = await crossRequirementsApi.adjustState(xreqId);
        if (!mountedRef.current || streamTokenRef.current !== token) return;
        if (state?.running && state.job_id) {
          // 还在跑 → 回到流上（服务端会把历史日志重放一遍，面板自己重建）。
          subscribeRef.current?.(state.job_id, 0);
          return;
        }
        setAsking(false);
        await reload();
      } catch {
        if (!mountedRef.current || streamTokenRef.current !== token) return;
        pollTimerRef.current = setTimeout(() => { void tick(); }, 3000);
      }
    };
    void tick();
  }, [xreqId, reload]);

  // subscribe — 订阅一轮调整的 SSE。
  //
  // 断线不是失败：作业在服务端还在跑，所以按 1s→2s→4s→8s 退避重连；连不上
  // 就降级为轮询状态端点。
  const subscribe = useCallback((jobId: string, attempt: number) => {
    esRef.current?.close();
    esRef.current = null;
    const token = ++streamTokenRef.current;

    const es = createEventStream(
      crossRequirementsApi.streamAdjustUrl(xreqId, jobId),
      (data) => {
        if (!mountedRef.current || streamTokenRef.current !== token) return;
        if (data?.type === 'job_done') {
          es.close();
          if (esRef.current === es) esRef.current = null;
          setAsking(false);
          void reload();
          return;
        }
        if (data?.type === 'error') {
          es.close();
          if (esRef.current === es) esRef.current = null;
          setAsking(false);
          setError(String(data.content ?? t('crossRequirements.adjustChat.failed')));
          void reload();
        }
      },
      () => {
        if (!mountedRef.current || streamTokenRef.current !== token) return;
        // 断线了。作业还在服务端跑 —— 重连，不是失败。
        if (attempt < 4) {
          const delay = Math.min(1000 * Math.pow(2, attempt), 10000);
          reconnectTimerRef.current = setTimeout(() => {
            if (streamTokenRef.current !== token) return;
            subscribeRef.current?.(jobId, attempt + 1);
          }, delay);
          return;
        }
        pollState();
      },
    );
    esRef.current = es;
  }, [xreqId, reload, pollState, t]);

  subscribeRef.current = subscribe;

  // 刷新之后接回正在跑的那一轮。
  useEffect(() => {
    const running = turns.find((x) => x.status === 'running');
    if (!running || !running.job_id || asking) return;
    if (!expanded) return;
    setAsking(true);
    subscribeRef.current?.(running.job_id, 0);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [turns, asking, expanded]);

  const ask = useCallback(async (text: string) => {
    const q = text.trim();
    if (!q || asking || disabled) return;
    setQuestion('');
    setError('');
    setAsking(true);
    try {
      const res = await crossRequirementsApi.askAdjust(xreqId, q);
      // 立刻把这一轮显示出来 —— 落库的线程要等后端建好行才有。
      setTurns((prev) => [
        ...prev,
        {
          id: res.turn_id,
          cross_requirement_id: xreqId,
          question: q,
          answer: '',
          status: 'running',
          job_id: res.job_id,
          model: '',
          session_id: '',
          source_session_id: '',
          resumed: false,
          error_kind: '',
          created_by: '',
          created_at: new Date().toISOString(),
          updated_at: new Date().toISOString(),
        },
      ]);
      subscribeRef.current?.(res.job_id, 0);
    } catch (err) {
      if (!mountedRef.current) return;
      setAsking(false);
      setError(errorMessage(err));
    }
  }, [xreqId, asking, disabled]);

  const removeTurn = useCallback(async (turn: CrossRequirementQATurn) => {
    try {
      await crossRequirementsApi.removeAdjustTurn(xreqId, turn.id);
      await reload();
    } catch (err) {
      if (!mountedRef.current) return;
      setError(errorMessage(err));
    }
  }, [xreqId, reload]);

  const resetThread = useCallback(async () => {
    try {
      await crossRequirementsApi.resetAdjust(xreqId);
      await reload();
    } catch (err) {
      if (!mountedRef.current) return;
      setError(errorMessage(err));
    }
  }, [xreqId, reload]);

  // 点 chip = 把一个模板问题填进输入框（用户还能改）。不直接发出去 ——
  // 那等于替用户拍板。
  const fillFromDecision = useCallback((d: CrossPendingDecision) => {
    setExpanded(true);
    setQuestion((prev) => {
      const line = t('crossRequirements.adjustChat.chipTemplate', { title: d.title });
      return prev ? `${prev}\n${line}` : line;
    });
  }, [t]);

  const answeredCount = turns.filter((x) => x.status === 'done').length;
  // 「按讨论结论重新补全方案」只在**真的有结论**时才有意义：只有提问、没
  // 拍板，讨论记录带进方案只是让模型重说一遍没定的点。
  const hasDiscussion = turns.some((x) => x.status === 'done' && !!x.answer?.trim());

  return (
    <div className={`xqa-panel${expanded ? ' is-expanded' : ''}`}>
      <div className="xqa-head">
        <button
          type="button"
          className="xqa-head-toggle"
          onClick={() => setExpanded((v) => !v)}
          disabled={disabled}
        >
          <span className="xqa-caret">{expanded ? '▾' : '▸'}</span>
          {t('crossRequirements.adjustChat.title')}
          {answeredCount > 0 && <span className="xqa-count">{answeredCount}</span>}
        </button>
        <div className="xqa-head-actions">
          {turns.length > 0 && (
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => { void resetThread(); }}
              disabled={disabled || asking}
              title={t('crossRequirements.adjustChat.resetHint')}
            >
              {t('crossRequirements.adjustChat.reset')}
            </button>
          )}
          {hasDiscussion && onApplyDiscussion && (
            <button
              type="button"
              className="btn btn-sm btn-primary"
              onClick={() => { onApplyDiscussion(); }}
              disabled={disabled || asking}
              title={t('crossRequirements.adjustChat.applyHint')}
            >
              {t('crossRequirements.adjustChat.apply')}
            </button>
          )}
        </div>
      </div>

      {expanded && (
        <div className="xqa-body">
          <p className="xqa-hint">{t('crossRequirements.adjustChat.hint')}</p>

          {pendingDecisions.length > 0 && (
            <div className="xqa-chips">
              <span className="xqa-chips-label">{t('crossRequirements.adjustChat.pending')}</span>
              {pendingDecisions.map((d, i) => (
                <button
                  key={i}
                  type="button"
                  className="xreq-chip xqa-chip"
                  title={d.detail}
                  onClick={() => { fillFromDecision(d); }}
                  disabled={disabled}
                >
                  {d.title}
                </button>
              ))}
            </div>
          )}

          {loading ? (
            <p className="xqa-empty">{t('crossRequirements.adjustChat.loading')}</p>
          ) : turns.length === 0 ? (
            <p className="xqa-empty">{t('crossRequirements.adjustChat.empty')}</p>
          ) : (
            <div className="xqa-thread">
              {turns.map((turn) => (
                <article key={turn.id} className={`xqa-turn xqa-turn-${turn.status}`}>
                  <div className="xqa-turn-q">
                    <span className="xqa-turn-tag">{t('crossRequirements.adjustChat.questionTag')}</span>
                    <span className="xqa-turn-qtext">{turn.question}</span>
                    {turn.status !== 'running' && (
                      <button
                        type="button"
                        className="btn btn-sm xqa-turn-del"
                        onClick={() => { void removeTurn(turn); }}
                        disabled={disabled}
                        title={t('crossRequirements.adjustChat.deleteTurn')}
                      >
                        ✕
                      </button>
                    )}
                  </div>
                  {turn.status === 'running' ? (
                    <p className="xqa-turn-pending">{t('crossRequirements.adjustChat.running')}</p>
                  ) : turn.status === 'error' ? (
                    <p className="xqa-turn-errtext">
                      {t('crossRequirements.adjustChat.failed')}：{turn.answer}
                    </p>
                  ) : (
                    <div className="xqa-turn-a">
                      <div className="xqa-turn-abody md-prose">
                        <MarkdownRender content={turn.answer} />
                      </div>
                    </div>
                  )}
                  {turn.status === 'error' && (
                    <div className="xqa-turn-meta">
                      <button
                        type="button"
                        className="btn btn-sm xqa-turn-retry"
                        onClick={() => { void ask(turn.question); }}
                        disabled={asking || disabled}
                      >
                        {t('crossRequirements.adjustChat.retry')}
                      </button>
                    </div>
                  )}
                </article>
              ))}
            </div>
          )}

          {error && (
            <div className="xqa-error">
              <span>{error}</span>
              <button type="button" className="xqa-error-close" onClick={() => setError('')}>✕</button>
            </div>
          )}

          <div className="xqa-composer">
            <AtMentionTextarea
              value={question}
              onChange={setQuestion}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && !e.shiftKey) {
                  e.preventDefault();
                  void ask(question);
                }
              }}
              placeholder={t('crossRequirements.adjustChat.placeholder')}
              rows={3}
              className="xqa-textarea"
              disabled={disabled || asking}
            />
            <button
              type="button"
              className="btn btn-primary"
              onClick={() => { void ask(question); }}
              disabled={disabled || asking || !question.trim()}
            >
              {asking ? t('crossRequirements.adjustChat.asking') : t('crossRequirements.adjustChat.send')}
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
