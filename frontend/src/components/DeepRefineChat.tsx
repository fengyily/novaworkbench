import { useState, useRef, useEffect, useCallback } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { API_BASE, requirementsApi } from '../api/client';

interface Props {
  reqId: string;
  projectPath: string;
  requirementTitle: string;
  currentAnalysis: string;
  analysisJobId: string;
  onTurnDone?: () => void; // refresh req (sync status / clear analysis_job_id) after a turn
  onGenerateDesign: () => void;
  onReset?: () => void;
}

interface ChatMessage { role: string; content: string; isError?: boolean; isStreaming?: boolean; }

export default function DeepRefineChat({
  reqId, projectPath, requirementTitle, currentAnalysis, analysisJobId, onTurnDone, onGenerateDesign, onReset,
}: Props) {
  const [expanded, setExpanded] = useState(true);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState('');
  const [chatting, setChatting] = useState(false);
  // toolLog: live activity feed (phase + tool-call labels)
  const [toolLog, setToolLog] = useState<string[]>([]);
  const [retryMsg, setRetryMsg] = useState('');
  const chatRef = useRef<HTMLDivElement>(null);
  // Guard against auto-start firing twice (StrictMode / concurrent renders).
  const bootedRef = useRef(false);

  // Mirror of `messages` for use inside async callbacks (POST / stream) where
  // the state closure would otherwise be stale.
  const messagesRef = useRef<ChatMessage[]>([]);
  useEffect(() => { messagesRef.current = messages; }, [messages]);

  // Accumulated AI text for the turn currently being streamed. Rebuilt from the
  // job's "message" log lines — on a page refresh the job replays its history,
  // so this reconstructs the full turn output even mid-flight.
  const aiTextRef = useRef('');
  const esRef = useRef<EventSource | null>(null);

  const saveMessages = useCallback(async (msgs: ChatMessage[]) => {
    if (!reqId) return;
    try {
      await fetch(`${API_BASE}/api/requirements/${reqId}/chat-history`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ messages: JSON.stringify(msgs) }),
      });
    } catch { /* silent */ }
  }, [reqId]);

  const loadMessages = useCallback(async (): Promise<ChatMessage[] | null> => {
    if (!reqId) return null;
    try {
      const res = await fetch(`${API_BASE}/api/requirements/${reqId}/chat-history`);
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

    const es = new EventSource(`${API_BASE}/api/wizard/jobs/${jobId}/stream`);
    esRef.current = es;

    es.onmessage = (e) => {
      try {
        const evt = JSON.parse(e.data);
        if (evt.type === 'job_done') {
          es.close();
          esRef.current = null;
          setChatting(false);
          setToolLog([]);
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
          es.close();
          esRef.current = null;
          setChatting(false);
          setToolLog([]);
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
          setToolLog(prev => [...prev.slice(-19), evt.content ?? '']);
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
      } catch { /* skip malformed SSE */ }
    };

    es.onerror = () => {
      // EventSource auto-reconnects on transient drops; if the job is gone
      // (backend restarted, ring evicted) the stream errors repeatedly. Poll
      // the snapshot once; if it's gone, drop to idle so the user can retry.
      es.close();
      esRef.current = null;
      fetch(`${API_BASE}/api/wizard/jobs/${jobId}`)
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
          const { status, log } = json.data as { status: string; log: { type: string; content: string }[] };
          if (status === 'running') {
            // transient drop — re-arm the stream
            streamAnalystJob(jobId);
          } else {
            // finished but we missed job_done — reconstruct from the snapshot.
            aiTextRef.current = '';
            for (const l of log || []) {
              if (l.type === 'message') aiTextRef.current += l.content;
            }
            setChatting(false);
            setToolLog([]);
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
    };
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
      const res = await fetch(`${API_BASE}/api/wizard/analyst-chat`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          project_path: projectPath,
          requirement_id: reqId,
          requirement_title: requirementTitle,
          current_analysis: currentAnalysis,
          user_message: userMessage,
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
  }, [projectPath, reqId, requirementTitle, currentAnalysis, saveMessages, streamAnalystJob]);

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
        runTurn('');
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

  return (
    <div className="detail-section deep-refine-panel">
      <div className="deep-refine-header">
        <div>
          <h3>🔍 深入分析 — 确认具体改动点</h3>
        </div>
        <div style={{ display: 'flex', gap: 6 }}>
          {messages.length > 0 && !isWorking && (
            <button className="btn btn-sm" onClick={handleClear} title="清除对话记录">🗑</button>
          )}
          <button className="btn btn-sm" onClick={() => setExpanded(false)}>收起</button>
        </div>
      </div>

      <div className="chat-panel" ref={chatRef}>
        {messages.map((msg, i) => (
          <div key={i} className={`chat-msg ${msg.role}${msg.isError ? ' error' : ''}`}>
            <span className="chat-role">{msg.role === 'ai' ? '🤖 AI' : '👤 你'}</span>
            {msg.role === 'ai' && !msg.isError
              ? <div className="chat-content chat-content-md"><ReactMarkdown remarkPlugins={[remarkGfm]}>{msg.content}</ReactMarkdown></div>
              : <div className="chat-content" style={{ whiteSpace: 'pre-wrap' }}>{msg.content}</div>
            }
          </div>
        ))}

        {/* Live tool-call activity feed */}
        {isWorking && toolLog.length > 0 && (
          <div className="tool-log">
            {toolLog.map((entry, i) => (
              <div key={i} className={`tool-log-entry${i === toolLog.length - 1 ? ' active' : ''}`}>
                {entry}
              </div>
            ))}
          </div>
        )}

        {/* Spinner when working but no activity yet */}
        {isWorking && toolLog.length === 0 && (
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

      <div className="chat-input-row">
        <textarea
          value={input}
          onChange={e => setInput(e.target.value)}
          onKeyDown={e => {
            if (e.key === 'Enter' && !e.shiftKey) {
              e.preventDefault();
              handleSend();
            }
          }}
          placeholder="贴URL、描述页面元素、或回复AI的问题...&#10;Enter 发送  ·  Shift+Enter 换行"
          className="form-input chat-textarea"
          disabled={isWorking}
          rows={2}
        />
        <button className="btn btn-primary" onClick={handleSend} disabled={isWorking || !input.trim()}>发送</button>
      </div>

      <div className="deep-refine-actions">
        <button className="btn btn-primary" onClick={onGenerateDesign} disabled={isWorking || messages.length === 0}>
          📐 生成技术方案
        </button>
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
