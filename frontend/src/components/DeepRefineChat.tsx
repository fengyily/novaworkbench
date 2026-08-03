import { useState, useRef, useEffect, useCallback } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { API_BASE, requirementsApi } from '../api/client';

interface Props {
  reqId: string;
  projectPath: string;
  requirementTitle: string;
  currentAnalysis: string;
  onGenerateDesign: () => void;
  onReset?: () => void;
}

interface ChatMessage { role: string; content: string; isError?: boolean; }

export default function DeepRefineChat({ reqId, projectPath, requirementTitle, currentAnalysis, onGenerateDesign, onReset }: Props) {
  const [expanded, setExpanded] = useState(true);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState('');
  const [chatting, setChatting] = useState(false);
  const [loading, setLoading] = useState(false);
  // toolLog: list of tool-call labels shown as a live activity feed
  const [toolLog, setToolLog] = useState<string[]>([]);
  const [retryMsg, setRetryMsg] = useState(''); // last user message that can be retried
  const chatRef = useRef<HTMLDivElement>(null);
  // Guard against React StrictMode double-invoking the auto-start effect in dev,
  // which would otherwise mint two session ids and resume a not-yet-persisted one.
  const startedRef = useRef(false);

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

  const loadMessages = useCallback(async () => {
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
    setMessages([]);
    setRetryMsg('');
    setToolLog([]);
    await saveMessages([]);
    try {
      await requirementsApi.clearAnalysisSession(reqId);
    } catch { /* silent */ }
    try {
      await requirementsApi.updateStatus(reqId, 'draft');
    } catch { /* silent */ }
    onReset?.();
  };

  // Core streaming function — used by both startChat and handleSend.
  // The backend owns the conversation session (claude --session-id/--resume),
  // so we no longer thread conversation_history; we only send the new user
  // message. Returns { aiText, doneHistory } or throws on error.
  const streamDeepRefine = useCallback(async (
    userMessage: string,
    signal: AbortSignal,
    onToolCall: (label: string) => void,
    onTextChunk: (line: string) => void,
  ): Promise<{ aiText: string; doneHistory: string }> => {
    const res = await fetch(`${API_BASE}/api/wizard/analyst-chat`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      signal,
      body: JSON.stringify({
        project_path: projectPath,
        requirement_id: reqId,
        requirement_title: requirementTitle,
        current_analysis: currentAnalysis,
        user_message: userMessage,
      }),
    });

    const reader = res.body?.getReader();
    if (!reader) throw new Error('No stream');

    const decoder = new TextDecoder();
    let aiText = '';
    let doneHistory = '';
    let sseBuffer = '';

    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      sseBuffer += decoder.decode(value, { stream: true });
      // SSE frames are delimited by \n\n; process all complete frames
      const parts = sseBuffer.split('\n\n');
      sseBuffer = parts.pop() ?? '';
      for (const part of parts) {
        const dataLine = part.startsWith('data: ') ? part.slice(6) : part.replace(/^.*data: /s, '');
        if (!dataLine.trim()) continue;
        try {
          const data = JSON.parse(dataLine);
          if (data.type === 'tool_call') onToolCall(data.content);
          if (data.type === 'message') {
            aiText += data.content;
            onTextChunk(data.content);
          }
          if (data.type === 'done' && data.history) doneHistory = data.history;
        } catch { /* ignore malformed SSE */ }
      }
    }

    return { aiText, doneHistory };
  }, [projectPath, reqId, requirementTitle, currentAnalysis]);

  const startChat = async () => {
    if (startedRef.current) return; // dedupe StrictMode double-fire / concurrent starts
    startedRef.current = true;
    setExpanded(true);
    if (messages.length > 0) return;

    setLoading(true);
    setToolLog([]);

    const saved = await loadMessages();
    if (saved) {
      setMessages(saved);
      setLoading(false);
      return;
    }

    // The backend builds the full first-turn prompt itself (from the DB requirement
    // record + pre-read project context). We just trigger the first turn with an
    // empty user_message so the backend uses its own prompt without duplication.

    // Add a streaming AI placeholder so the first-round text streams live
    // (the backend emits message events as Claude types); without this the
    // whole response would only render once at the end.
    const streamingIdx = { current: -1 };
    setMessages(prev => {
      streamingIdx.current = prev.length;
      return [...prev, { role: 'ai', content: '', isStreaming: true } as any];
    });

    const controller = new AbortController();
    const timeoutId = setTimeout(() => controller.abort(), 600000); // 10 min — first-turn project read via proxy can be long

    try {
      const { aiText } = await streamDeepRefine(
        '',
        controller.signal,
        (label) => setToolLog(prev => [...prev.slice(-19), label]),
        (line) => {
          // Update the streaming placeholder in real time.
          setMessages(prev => {
            const next = [...prev];
            const idx = next.length - 1;
            if (idx >= 0 && (next[idx] as any).isStreaming) {
              next[idx] = { role: 'ai', content: (next[idx].content || '') + line };
            }
            return next;
          });
        },
      );
      clearTimeout(timeoutId);
      setToolLog([]);

      setMessages(prev => {
        const next = [...prev];
        const idx = next.length - 1;
        if (idx >= 0) next[idx] = { role: 'ai', content: aiText.trim() };
        return next;
      });
    } catch (err: any) {
      clearTimeout(timeoutId);
      setToolLog([]);
      const isTimeout = err.name === 'AbortError';
      const errContent = isTimeout
        ? '⏱ Claude 初始分析超时。点击重试按钮重新开始。'
        : '❌ ' + err.message;
      setMessages([{ role: 'ai', content: errContent, isError: true }]);
      setRetryMsg('__init__'); // special marker for retry of startChat
    } finally {
      setLoading(false);
    }
  };

  const doSend = useCallback(async (msg: string) => {
    setChatting(true);
    setToolLog([]);
    setRetryMsg('');

    // Add a live streaming message placeholder
    const streamingIdx = { current: -1 };
    setMessages(prev => {
      streamingIdx.current = prev.length;
      return [...prev, { role: 'ai', content: '', isStreaming: true } as any];
    });

    const controller = new AbortController();
    const timeoutId = setTimeout(() => controller.abort(), 600000); // 10 min — proxy-backed turns can be slow

    try {
      const { aiText } = await streamDeepRefine(
        msg,
        controller.signal,
        (label) => setToolLog(prev => [...prev.slice(-19), label]),
        (line) => {
          // Update the streaming placeholder in real time
          setMessages(prev => {
            const next = [...prev];
            const idx = next.length - 1;
            if (idx >= 0 && (next[idx] as any).isStreaming) {
              next[idx] = { role: 'ai', content: (next[idx].content || '') + line };
            }
            return next;
          });
        },
      );
      clearTimeout(timeoutId);
      setToolLog([]);

      const raw = aiText.trim();
      const cleanResponse = raw;

      // Replace streaming placeholder with final content
      setMessages(prev => {
        const next = [...prev];
        const idx = next.length - 1;
        if (idx >= 0) next[idx] = { role: 'ai', content: cleanResponse };
        const saved = next.filter(m => !m.isError);
        saveMessages(saved);
        return next;
      });
    } catch (err: any) {
      clearTimeout(timeoutId);
      setToolLog([]);
      const isTimeout = err.name === 'AbortError';
      const errContent = isTimeout
        ? '⏱ Claude 响应超时（3分钟），请点击重试。'
        : '❌ ' + err.message;
      // Replace placeholder with error
      setMessages(prev => {
        const next = [...prev];
        const idx = next.length - 1;
        if (idx >= 0) next[idx] = { role: 'ai', content: errContent, isError: true };
        return next;
      });
      setRetryMsg(msg); // allow retry
    } finally {
      setChatting(false);
    }
  }, [streamDeepRefine, saveMessages]);

  const handleSend = async () => {
    if (!input.trim() || chatting) return;
    const msg = input.trim();
    setInput('');
    setMessages(prev => [...prev, { role: 'user', content: msg }]);
    await doSend(msg);
  };

  const handleRetry = async () => {
    if (retryMsg === '__init__') {
      // Retry the initial analysis — clear messages and restart
      setMessages([]);
      setRetryMsg('');
      startedRef.current = false; // allow startChat to run again
      await startChat();
      return;
    }
    if (!retryMsg) return;
    const msg = retryMsg;
    // Remove the last error message before retrying
    setMessages(prev => prev.filter((_, i) => i < prev.length - 1));
    await doSend(msg);
  };

  useEffect(() => {
    if (messages.length > 0) saveMessages(messages.filter(m => !m.isError));
  }, [messages, saveMessages]);

  // Auto-start on mount
  useEffect(() => {
    startChat();
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    const lastMsg = messages[messages.length - 1];
    if (!lastMsg || lastMsg.role !== 'ai' || chatting || input || lastMsg.isError) return;
    const template = buildReplyTemplate(lastMsg.content);
    if (template) setInput(template);
  }, [messages, chatting, input]);

  useEffect(() => {
    if (chatRef.current) chatRef.current.scrollTop = chatRef.current.scrollHeight;
  }, [messages, toolLog]);

  if (!expanded) {
    return (
      <div className="deep-refine-toggle">
        <button className="btn btn-sm" onClick={() => setExpanded(true)}>
          💬 展开对话
        </button>
      </div>
    );
  }

  const isWorking = loading || chatting;

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
          <div key={i} className={`chat-msg ${msg.role}${(msg as any).isError ? ' error' : ''}`}>
            <span className="chat-role">{msg.role === 'ai' ? '🤖 AI' : '👤 你'}</span>
            {msg.role === 'ai' && !(msg as any).isError
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

        {/* Spinner when working but no tool calls yet */}
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
