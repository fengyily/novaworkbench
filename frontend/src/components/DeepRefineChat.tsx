import { useState, useRef, useEffect, useCallback } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { API_BASE, authedFetch, kindChatPlaceholders, kindOf, requirementsApi, wizardApi, type Kind } from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import AtMentionTextarea from './AtMentionTextarea';
import { appendLogLine, type LogLine, type UsageInfo, computeUsage } from '../utils/logLines';
import { buildPhaseGroups, formatDuration, useTick } from '../utils/phaseGroups';
import ModelSelect from './ModelSelect';
import { FullscreenButton } from './FullscreenButton';
import { useFullscreen } from '../utils/useFullscreen';
import { ContextUsageBar } from './ContextUsageBar';

interface Props {
  reqId: string;
  projectPath: string;
  requirementTitle: string;
  currentAnalysis: string;
  analysisJobId: string;
  // Kind of the requirement — drives header title, input placeholder, default
  // first-turn prompt, and whether the "生成技术方案" CTA is hidden (kind=idea
  // is a discussion-only entry and never reaches the architect stage).
  kind?: Kind | string;
  // Last-persisted model for the analyst stage (req.analyst_model); empty =
  // never run. Used as the dropdown's default selection.
  model?: string;
  // Actual model that the empty "默认模型" selection resolves to for this stage
  // (角色模型 > 生效配置默认), shown next to "默认模型" before the stage runs.
  defaultModel?: string;
  onTurnDone?: () => void; // refresh req (sync status / clear analysis_job_id) after a turn
  // Reports the live turn state upward so the detail header can show an
  // accurate global "Claude 工作中" badge while an analyst turn runs (the
  // persisted analysis_job_id is only refreshed after the turn finishes).
  onWorkingChange?: (working: boolean) => void;
  // Controlled context-usage for the analyst session. The parent
  // (RequirementDetail) owns the live usage state so it can also drive the
  // always-on top strip AND seed it from requirements.usage_snapshots on
  // load — the bar thus survives page refresh / panel collapse instead of
  // dropping to 0%. The component reports each `usage` SSE event upward via
  // onUsage; the parent feeds the same value back in here for rendering.
  usage?: UsageInfo;
  onUsage?: (u: UsageInfo | undefined) => void;
  onGenerateDesign: () => void;
  onReset?: () => void;
}

interface ChatMessage { role: string; content: string; isError?: boolean; isStreaming?: boolean; }

export default function DeepRefineChat({
  reqId, projectPath, requirementTitle, currentAnalysis, analysisJobId, kind, model, defaultModel, onTurnDone, onWorkingChange, usage, onUsage, onGenerateDesign, onReset,
}: Props) {
  const [expanded, setExpanded] = useState(true);
  const { isFullscreen, toggle: toggleFullscreen, exit: exitFullscreen } = useFullscreen();
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState('');
  const [chatting, setChatting] = useState(false);
  // toolLog: live activity feed (phase + tool-call labels)
  const [toolLog, setToolLog] = useState<LogLine[]>([]);
  const [retryMsg, setRetryMsg] = useState('');
  const chatRef = useRef<HTMLDivElement>(null);
  // Context-usage snapshot is now CONTROLLED — the parent owns the live state
  // so it can also feed the always-on top strip and seed from the persisted
  // requirements.usage_snapshots blob. We report each `usage` SSE event upward
  // via onUsage and read the value back from the `usage` prop below.
  // True while POST /api/wizard/compress-context is in flight. Disables the
  // compress button and shows the "⏳ 压缩中…" label to prevent duplicate
  // requests during the (potentially long) claude summarization run.
  const [compressing, setCompressing] = useState(false);
  // compressedAt: ISO timestamp persisted on requirements.analyst_compressed_at
  // after a successful compression. Drives the "📦 已压缩" badge in the bar.
  // summaryModal: when non-null, shows a modal with the persisted Chinese
  // summary text so the user can review what claude distilled.
  const [compressedAt, setCompressedAt] = useState<string | null>(null);
  const [summaryModal, setSummaryModal] = useState<string | null>(null);
  // Guard against auto-start firing twice (StrictMode / concurrent renders).
  const bootedRef = useRef(false);

  // Per-turn analyst model. Seeded from the server-persisted model (default
  // selection = 已设置的模型); once the user switches it stays local and is
  // sent with the next analyst-chat POST. Disabled while a turn is running.
  const [selectedModel, setSelectedModel] = useState('');
  const modelTouchedRef = useRef(false);
  useEffect(() => {
    if (!modelTouchedRef.current) {
      const v = model || '';
      setSelectedModel(v === '默认模型' ? '' : v);
    }
  }, [model]);

  // Mirror of `messages` for use inside async callbacks (POST / stream) where
  // the state closure would otherwise be stale.
  const messagesRef = useRef<ChatMessage[]>([]);
  useEffect(() => { messagesRef.current = messages; }, [messages]);

  // Surface the live turn state to the parent (RequirementDetail header badge).
  useEffect(() => { onWorkingChange?.(chatting); }, [chatting, onWorkingChange]);

  // Accumulated AI text for the turn currently being streamed. Rebuilt from the
  // job's "message" log lines — on a page refresh the job replays its history,
  // so this reconstructs the full turn output even mid-flight.
  const aiTextRef = useRef('');
  const esRef = useRef<EventStream | null>(null);

  const saveMessages = useCallback(async (msgs: ChatMessage[]) => {
    if (!reqId) return;
    try {
      await authedFetch(`${API_BASE}/api/requirements/${reqId}/chat-history`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ messages: JSON.stringify(msgs) }),
      });
    } catch { /* silent */ }
  }, [reqId]);

  const loadMessages = useCallback(async (): Promise<ChatMessage[] | null> => {
    if (!reqId) return null;
    try {
      const res = await authedFetch(`${API_BASE}/api/requirements/${reqId}/chat-history`);
      const json = await res.json();
      if (json.data && json.data !== '[]') {
        const parsed = JSON.parse(json.data);
        if (Array.isArray(parsed) && parsed.length > 0) return parsed as ChatMessage[];
      }
    } catch { /* ignore */ }
    return null;
  }, [reqId]);

  const handleClear = async () => {
    if (!confirm('确定要清除对话记录吗？需求将回到草稿状态，可手动重新触发分析。')) return;
    if (esRef.current) { esRef.current.close(); esRef.current = null; }
    setMessages([]);
    setRetryMsg('');
    setToolLog([]);
    setChatting(false);
    await saveMessages([]);
    try { await requirementsApi.clearAnalysisSession(reqId); } catch { /* silent */ }
    try { await requirementsApi.updateStatus(reqId, 'draft'); } catch { /* silent */ }
    onReset?.();
  };

  // Ensure the last message is a streaming AI placeholder; returns the index.
  const ensureStreamingPlaceholder = useCallback((): number => {
    let idx = -1;
    setMessages(prev => {
      const next = [...prev];
      const last = next[next.length - 1];
      if (last && last.role === 'ai' && last.isStreaming) {
        idx = next.length - 1;
        return next;
      }
      next.push({ role: 'ai', content: '', isStreaming: true });
      idx = next.length - 1;
      return next;
    });
    return idx;
  }, []);

  // Stream a JobStore job's log lines via SSE. The job replays its full history
  // first, then pushes live lines until job_done — so this works both for a
  // freshly-started turn and for reconnecting to an in-flight turn after a page
  // refresh. AI text is accumulated from "message" lines into the streaming
  // placeholder; on job_done the placeholder is finalized and persisted.
  const streamAnalystJob = useCallback((jobId: string) => {
    if (esRef.current) esRef.current.close();
    ensureStreamingPlaceholder();
    setChatting(true);
    setToolLog([]);
    aiTextRef.current = '';

    esRef.current = createEventStream(
      `/api/wizard/jobs/${jobId}/stream`,
      (evt) => {
        if (evt.type === 'job_done') {
          esRef.current?.close();
          esRef.current = null;
          setChatting(false);
          // Keep toolLog visible so the user can see phase timings for the
          // just-finished turn. `isWorking` will drop, and the render block
          // still shows the phase view as long as toolLog is non-empty.
          const finalText = aiTextRef.current.trim();
          setMessages(prev => {
            const next = [...prev];
            const idx = next.length - 1;
            if (idx >= 0 && next[idx]?.isStreaming) {
              next[idx] = { role: 'ai', content: finalText || '(无回复)' };
            }
            const saved = next.filter(m => !m.isError && !m.isStreaming);
            saveMessages(saved);
            return next;
          });
          onTurnDone?.();
          return;
        }
        if (evt.type === 'error') {
          esRef.current?.close();
          esRef.current = null;
          setChatting(false);
          // Keep toolLog visible — same reason as job_done above.
          setMessages(prev => {
            const next = [...prev];
            const idx = next.length - 1;
            if (idx >= 0 && next[idx]?.isStreaming) {
              next[idx] = { role: 'ai', content: '❌ ' + (evt.content || 'Claude 执行出错'), isError: true };
            }
            return next;
          });
          return;
        }
        if (evt.type === 'tool_call' || evt.type === 'phase') {
          // Coalesce consecutive "模型思考中… (N tokens)" phase lines into a
          // single updatable row instead of stacking one per heartbeat. Use
          // the backend-stamped `at` (or Date.now() as a client-side fallback
          // if the server didn't send one) so phaseGroups can compute
          // accurate per-phase + per-tool-call durations.
          const at = typeof evt.at === 'number' ? evt.at : Date.now();
          setToolLog(prev => appendLogLine(prev.slice(-60), { type: evt.type, content: evt.content ?? '', at }));
          return;
        }
        if (evt.type === 'message') {
          aiTextRef.current += evt.content ?? '';
          const snapshot = aiTextRef.current;
          setMessages(prev => {
            const next = [...prev];
            const idx = next.length - 1;
            if (idx >= 0 && next[idx]?.isStreaming) {
              next[idx] = { role: 'ai', content: snapshot, isStreaming: true };
            }
            return next;
          });
        }
        // Usage snapshot emitted at the end of every claude turn. Report the
        // raw payload upward (parent owns the state) — computeUsage derives
        // used + pct client-side so the bar fills before the backend stamps
        // them. Multiple events per turn are fine — the parent just overwrites.
        if (evt.type === 'usage') {
          try {
            const parsed = JSON.parse(evt.content ?? '{}');
            onUsage?.(computeUsage(parsed, 'analyst_chat'));
          } catch { /* malformed payload — ignore */ }
        }
      },
      () => {
        // The stream dropped (or the job is gone — backend restarted, ring
        // evicted). Poll the snapshot once; if it's gone, drop to idle so the
        // user can retry.
        esRef.current = null;
        authedFetch(`${API_BASE}/api/wizard/jobs/${jobId}`)
          .then(r => r.json())
          .then(json => {
            if (!json.success) {
              // Job evicted (backend restart) — surface a recoverable error.
              setChatting(false);
              setToolLog([]);
              setMessages(prev => {
                const next = [...prev];
                const idx = next.length - 1;
                if (idx >= 0 && next[idx]?.isStreaming) {
                  next[idx] = { role: 'ai', content: '⚠️ 任务已丢失（服务可能重启）。点击重试重新开始。', isError: true };
                }
                return next;
              });
              return;
            }
            const { status, log } = json.data as { status: string; log: { type: string; content: string; at?: number }[] };
            if (status === 'running') {
              // transient drop — re-arm the stream
              streamAnalystJob(jobId);
            } else {
              // finished but we missed job_done — reconstruct from the snapshot.
              aiTextRef.current = '';
              const replayLines: LogLine[] = [];
              for (const l of log || []) {
                if (l.type === 'message') aiTextRef.current += l.content;
                // Restore the phase/tool_call activity feed so the user can
                // still see phase timings after a missed job_done frame.
                if (l.type === 'phase' || l.type === 'tool_call') {
                  replayLines.push({ type: l.type, content: l.content, at: l.at });
                }
              }
              setChatting(false);
              setToolLog(replayLines);
              const finalText = aiTextRef.current.trim();
              setMessages(prev => {
                const next = [...prev];
                const idx = next.length - 1;
                if (idx >= 0 && next[idx]?.isStreaming) {
                  next[idx] = { role: 'ai', content: finalText || '(无回复)' };
                }
                const saved = next.filter(m => !m.isError && !m.isStreaming);
                saveMessages(saved);
                return next;
              });
              onTurnDone?.();
            }
          })
          .catch(() => { setChatting(false); });
      },
    );
  }, [ensureStreamingPlaceholder, saveMessages, onTurnDone]);

  // Start a new analyst turn: POST to create the job, then stream it. The
  // user message (+ a streaming placeholder) is persisted BEFORE the POST so a
  // refresh mid-turn keeps the user's message in chat-history.
  const runTurn = useCallback(async (userMessage: string) => {
    const base = messagesRef.current.filter(m => !m.isError && !m.isStreaming);
    const withUser: ChatMessage[] = userMessage
      ? [...base, { role: 'user', content: userMessage }]
      : base;
    const withPlaceholder = [...withUser, { role: 'ai', content: '', isStreaming: true }];
    setMessages(withPlaceholder);
    messagesRef.current = withPlaceholder;
    setRetryMsg(userMessage || '__init__');
    setChatting(true);
    setToolLog([]);
    // Persist the user message before the turn starts so a refresh preserves it.
    saveMessages(withUser);

    try {
      const res = await authedFetch(`${API_BASE}/api/wizard/analyst-chat`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          project_path: projectPath,
          requirement_id: reqId,
          requirement_title: requirementTitle,
          current_analysis: currentAnalysis,
          user_message: userMessage,
          // Per-request model override — empty means the role's configured model.
          ...(selectedModel ? { model: selectedModel } : {}),
        }),
      });
      const json = await res.json();
      const jobId = json.data?.job_id;
      if (!jobId) throw new Error(json.error?.message || '未获取到任务 ID');
      streamAnalystJob(jobId);
    } catch (err: any) {
      setChatting(false);
      setToolLog([]);
      setMessages(prev => {
        const next = [...prev];
        const idx = next.length - 1;
        if (idx >= 0 && next[idx]?.isStreaming) {
          next[idx] = { role: 'ai', content: '❌ ' + err.message, isError: true };
        }
        return next;
      });
    }
  }, [projectPath, reqId, requirementTitle, currentAnalysis, selectedModel, saveMessages, streamAnalystJob]);

  const handleSend = async () => {
    if (!input.trim() || chatting) return;
    const msg = input.trim();
    setInput('');
    await runTurn(msg);
  };

  const handleRetry = async () => {
    if (chatting) return;
    const msg = retryMsg === '__init__' ? '' : retryMsg;
    // Drop the trailing error placeholder before retrying.
    setMessages(prev => {
      const next = [...prev];
      const idx = next.length - 1;
      if (idx >= 0 && next[idx]?.isError) next.pop();
      return next;
    });
    setRetryMsg('');
    await runTurn(msg);
  };

  // Boot-time fetch of the persisted compression record. Drives the "📦 已压缩"
  // badge on the usage bar so a page refresh still shows the user that this
  // stage has been summarized — and lets the bar's "查看摘要" link open the
  // modal without a second round-trip after the user clicks the button.
  useEffect(() => {
    if (!reqId) return;
    let cancelled = false;
    wizardApi.getContextSummary(reqId, 'analyst_chat')
      .then(data => { if (!cancelled && data) setCompressedAt(data.compressed_at ?? null); })
      .catch(() => { /* silent */ });
    return () => { cancelled = true; };
  }, [reqId]);

  // Trigger claude to summarize the current analyst conversation. The backend
  // runs a one-shot `--resume` turn with a fixed Chinese prompt, then writes
  // the summary into requirements.analyst_context_summary + stamps
  // analyst_compressed_at + clears analysis_session_id. On success we
  // refresh the requirement (so the detail header badge updates) and reload
  // the compressedAt so the bar switches to "已压缩" state.
  // request<T> throws on a non-2xx response; alert on the caught error
  // instead of checking a success field.
  const handleCompress = useCallback(async () => {
    if (!reqId || compressing) return;
    if (!confirm('让 Claude 总结当前对话并压缩上下文？\n\n该操作会清空当前会话 ID,下次对话将看到压缩摘要而不是完整历史。')) return;
    setCompressing(true);
    try {
      const data = await wizardApi.compressContext(reqId, 'analyst_chat');
      setCompressedAt(data.compressed_at ?? null);
      // Reset the usage bar so it doesn't lie about context usage based on
      // the soon-to-be-cleared session — the next turn will refresh it.
      onUsage?.(undefined);
      onTurnDone?.();
    } catch (err: any) {
      alert('压缩失败:' + (err?.message || String(err)));
    } finally {
      setCompressing(false);
    }
  }, [reqId, compressing, onTurnDone]);

  // Open the summary preview modal. Lazily fetches the persisted summary so
  // the boot-time fetch stays cheap — the modal is the only place that needs
  // the full Chinese text, not the bar itself.
  const handleShowSummary = useCallback(async () => {
    if (!reqId) return;
    try {
      const data = await wizardApi.getContextSummary(reqId, 'analyst_chat');
      setSummaryModal(data.summary || '(暂无压缩摘要)');
    } catch {
      setSummaryModal('(加载摘要失败)');
    }
  }, [reqId]);

  useEffect(() => {
    if (chatRef.current) chatRef.current.scrollTop = chatRef.current.scrollHeight;
  }, [messages, toolLog]);

  // Boot: load saved chat; reconnect to an in-flight job if one is active
  // (page refresh), otherwise auto-start the first turn when there's no
  // prior history.
  useEffect(() => {
    if (bootedRef.current) return;
    bootedRef.current = true;
    (async () => {
      const saved = await loadMessages();
      if (saved && saved.length > 0) setMessages(saved);
      if (analysisJobId) {
        // Reconnect to the running turn — the job replays its history so the
        // user sees the full in-flight output and continues receiving live
        // events. The turn survives the refresh because it runs in a backend
        // goroutine decoupled from this request.
        streamAnalystJob(analysisJobId);
      } else if (!saved || saved.length === 0) {
        runTurn(defaultInitMessage);
      }
    })();
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Reply-template helper: pre-fill the input with numbered question slots.
  useEffect(() => {
    const lastMsg = messages[messages.length - 1];
    if (!lastMsg || lastMsg.role !== 'ai' || chatting || input || lastMsg.isError || lastMsg.isStreaming) return;
    const template = buildReplyTemplate(lastMsg.content);
    if (template) setInput(template);
  }, [messages, chatting, input]);

  if (!expanded) {
    return (
      <div className="deep-refine-toggle">
        <button className="btn btn-sm" onClick={() => setExpanded(true)}>
          💬 展开对话
        </button>
      </div>
    );
  }

  const isWorking = chatting;
  // Block advancing to the architect stage when the last analyst turn ended in
  // an error — advancing would fork an empty/failed analysis session and leave
  // the requirement stuck in "designing" with no design and no way back. The
  // user must retry (and succeed) before 生成技术方案 is allowed.
  const lastIsError = messages.length > 0 && messages[messages.length - 1]?.isError === true;
  // The streaming placeholder carries empty content until the first
  // "message" event arrives. While it's empty, the standalone spinner
  // block below represents the "starting" state — so we must not also
  // render the empty placeholder (that would produce two "🤖 AI" rows).
  const lastMsg = messages[messages.length - 1];
  const placeholderEmpty = !!lastMsg?.isStreaming && !lastMsg?.content;
  const showSpinner = isWorking && toolLog.length === 0 && placeholderEmpty;

  // Kind-aware copy: idea → discussion thread, no architect handoff;
  // issue → bug-investigation framing; requirement → standard 3-stage flow.
  const reqKind: Kind = kindOf({ kind } as any);
  const isIdea = reqKind === 'idea';
  const chatHeaderTitle = isIdea
    ? '💡 想法讨论 — 探索可行方案'
    : reqKind === 'issue'
      ? '🐞 问题分析 — 排查根因并修复'
      : '🔍 深入分析 — 确认具体改动点';
  const chatPlaceholder = kindChatPlaceholders[reqKind];
  // First-turn kickoff prompt tailored per kind. The backend's prompt blocks
  // (analyst-tail) carry the detailed instructions; this is just the user-
  // facing seed message so the AI has context to react to.
  const defaultInitMessage = isIdea
    ? '请阅读相关代码并和我一起讨论这个想法的可行方案（不进入开发）。'
    : reqKind === 'issue'
      ? '请阅读相关代码，帮我定位这个问题的根因并提出修复方案。'
      : '';

  return (
    <div className="detail-section deep-refine-panel">
      <div className="deep-refine-header">
        <div>
          <h3>{chatHeaderTitle}</h3>
        </div>
        <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
          {/* Per-turn analyst model; disabled while a turn is running. The
              model list comes from the active claude config; default = 已设置模型. */}
          <ModelSelect
            value={selectedModel}
            onChange={m => { modelTouchedRef.current = true; setSelectedModel(m); }}
            disabled={isWorking}
            working={isWorking}
            label="分析模型"
            defaultModelName={defaultModel}
            title={isWorking ? 'Claude 工作中，暂不能切换模型' : '需求分析使用的模型'}
          />
          {messages.length > 0 && !isWorking && (
            <button className="btn btn-sm" onClick={handleClear} title="清除对话记录">🗑</button>
          )}
          <button className="btn btn-sm" onClick={() => setExpanded(false)}>收起</button>
          <FullscreenButton isFullscreen={isFullscreen} onClick={toggleFullscreen} />
        </div>
      </div>

      <div className={`chat-panel ${isFullscreen ? 'is-fullscreen' : ''}`} ref={chatRef}>
        {isFullscreen && (
          <FullscreenButton isFullscreen={true} onClick={exitFullscreen} variant="floating" />
        )}
        {messages.map((msg, i) => {
          // Skip the empty streaming placeholder — the spinner block below
          // renders the "starting" indicator. Once content arrives this
          // message renders normally and the spinner hides.
          if (msg.isStreaming && !msg.content) return null;
          return (
          <div key={i} className={`chat-msg ${msg.role}${msg.isError ? ' error' : ''}`}>
            <span className="chat-role">{msg.role === 'ai' ? '🤖 AI' : '👤 你'}</span>
            {msg.role === 'ai' && !msg.isError
              ? <div className="chat-content chat-content-md"><ReactMarkdown remarkPlugins={[remarkGfm]}>{msg.content}</ReactMarkdown></div>
              : <div className="chat-content" style={{ whiteSpace: 'pre-wrap' }}>{msg.content}</div>
            }
          </div>
          );
        })}

        {/* Live / finished tool-call activity feed, grouped by phase.
            Stays mounted after the job ends so the user can review phase
            timings for the just-finished turn. */}
        {toolLog.length > 0 && <ToolLogPhases toolLog={toolLog} isWorking={isWorking} />}

        {/* Spinner when working but no activity yet */}
        {showSpinner && (
          <div className="chat-msg ai">
            <span className="chat-role">🤖 AI</span>
            <div className="chat-content">⏳ Claude 正在启动...</div>
          </div>
        )}

        {/* Retry button below the last error message */}
        {retryMsg && !isWorking && (
          <div style={{ display: 'flex', justifyContent: 'center', padding: '8px 0' }}>
            <button className="btn btn-primary" onClick={handleRetry}>
              🔄 重试
            </button>
          </div>
        )}
      </div>

      <div className="chat-input-row composer-sticky">
        <AtMentionTextarea
          value={input}
          onChange={setInput}
          onKeyDown={e => {
            if (e.key === 'Enter' && !e.shiftKey) {
              e.preventDefault();
              handleSend();
            }
          }}
          placeholder={chatPlaceholder + '\nEnter 发送  ·  Shift+Enter 换行'}
          className="form-input chat-textarea"
          disabled={isWorking}
          rows={2}
        />
        <button className="btn btn-primary" onClick={handleSend} disabled={isWorking || !input.trim()}>发送</button>
      </div>

      {/* Live token-usage bar + 压缩上下文 entry point. Sits between the
          composer and the action row so it stays visible while typing.
          Disabled while no session has been started yet (chatting is false,
          usage is undefined, and no compressedAt); the button is still
          tappable once a turn has produced a usage snapshot. */}
      <ContextUsageBar
        usage={usage}
        onCompress={handleCompress}
        compressing={compressing}
        disabled={isWorking || compressing}
        stepLabel="需求分析师"
        compressedAt={compressedAt}
        onShowSummary={handleShowSummary}
      />

      {/* Compressed-summary preview modal. Rendered as a simple overlay
          rather than reusing a generic modal library — kept inline so the
          DeepRefineChat remains self-contained. */}
      {summaryModal !== null && (
        <div
          className="modal-backdrop"
          onClick={() => setSummaryModal(null)}
          role="dialog"
          aria-modal="true"
        >
          <div
            className="modal"
            onClick={e => e.stopPropagation()}
            style={{ maxWidth: 640 }}
          >
            <div className="modal-header">
              <h3>📦 已压缩上下文摘要</h3>
              <button className="btn btn-sm" onClick={() => setSummaryModal(null)}>关闭</button>
            </div>
            <div
              className="modal-body"
              style={{ whiteSpace: 'pre-wrap', lineHeight: 1.6, maxHeight: '60vh', overflowY: 'auto' }}
            >
              {summaryModal}
            </div>
          </div>
        </div>
      )}

      <div className="deep-refine-actions">
        {!isIdea && (
          <button
            className="btn btn-primary"
            onClick={onGenerateDesign}
            disabled={isWorking || messages.length === 0 || lastIsError}
            title={lastIsError ? '当前分析回合已出错，请先重试成功后再生成技术方案' : undefined}
          >
            📐 生成技术方案
          </button>
        )}
        {lastIsError && !isWorking && (
          <span style={{ color: '#B91C1C', fontSize: 12 }}>⚠️ 上一次分析出错，请先重试</span>
        )}
      </div>
    </div>
  );
}

/** Extract numbered questions from AI message and build a reply template */
function buildReplyTemplate(content: string): string {
  const patterns = [
    /(?:^|\n)\s*(\d+)[.)、]\s+.+/g,
    /\*\*(\d+)[.]\*\*\s+.+/g,
  ];
  let maxNum = 0;
  for (const re of patterns) {
    let m;
    while ((m = re.exec(content)) !== null) {
      const n = parseInt(m[1], 10);
      if (n > maxNum) maxNum = n;
    }
  }
  if (maxNum < 2 || maxNum > 8) return '';
  const lines: string[] = [];
  for (let i = 1; i <= maxNum; i++) lines.push(`${i}. `);
  return lines.join('\n');
}

// Renders the analyst turn's phase + tool-call activity as collapsible-style
// rows with per-phase and per-tool-call elapsed time. The active (trailing)
// phase ticks live via useTick; finished phases show their frozen duration.
function ToolLogPhases({ toolLog, isWorking }: { toolLog: LogLine[]; isWorking: boolean }) {
  // Re-render every 500ms while working so the active phase's elapsed time
  // updates in place. The counter value is unused.
  useTick(isWorking);
  const phases = buildPhaseGroups(toolLog);
  if (phases.length === 0) return null;
  const firstAt = phases[0].startedAt;
  const lastPhase = phases[phases.length - 1];
  const lastAt = lastPhase.isActive ? Date.now() : lastPhase.finishedAt;
  const totalMs = Math.max(0, lastAt - firstAt);
  return (
    <div className="tool-log">
      <div className="tool-log-summary">
        工具调用 · {phases.length} 个阶段 · 总计 {formatDuration(totalMs)}
      </div>
      {phases.map((p, i) => {
        const active = p.isActive && isWorking;
        const displayMs = active ? Date.now() - p.startedAt : p.durationMs;
        return (
          <div key={i} className={`tool-log-phase${active ? ' active' : ''}`}>
            <div className="tool-log-phase-header">
              <span className="tool-log-phase-label">{p.label}</span>
              <span className="tool-log-phase-time">
                {active ? `已用 ${formatDuration(displayMs)}` : formatDuration(displayMs)}
              </span>
            </div>
            {p.thinking && (
              <div className="tool-log-thinking">{p.thinking.content}</div>
            )}
            {p.toolCalls.map((tc, j) => (
              <div key={j} className="tool-log-entry">
                <span>{tc.content}</span>
                {tc.durationMs != null && tc.durationMs > 0 && (
                  <span className="tool-log-entry-time">
                    {' · '}{formatDuration(tc.durationMs)}
                  </span>
                )}
              </div>
            ))}
          </div>
        );
      })}
    </div>
  );
}
