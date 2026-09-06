import { useState, useRef, useEffect, useCallback } from 'react';
import { API_BASE, authedFetch, wizardApi } from '../api/client';
import { type UsageInfo } from '../utils/logLines';
import { ContextUsageBar } from './ContextUsageBar';

interface Props {
  reqId: string;
  projectPath: string;
  requirementTitle: string;
  onStartCoding: (desc: string) => void;
}

interface ChatMessage {
  role: 'user' | 'ai';
  content: string;
  isError?: boolean;
}

export default function CodingChat({ reqId, projectPath, requirementTitle, onStartCoding }: Props) {
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState('');
  const [thinking, setThinking] = useState(false);
  const [lastRequest, setLastRequest] = useState('');
  const chatRef = useRef<HTMLDivElement>(null);
  // Live context-usage snapshot for the developer-chat SSE stream. Updated
  // on every `usage` event; mirrors DeepRefineChat's parsing rules. Undefined
  // until the first claude turn of this session finishes.
  const [usage, setUsage] = useState<UsageInfo | undefined>(undefined);
  const [compressing, setCompressing] = useState(false);
  const [compressedAt, setCompressedAt] = useState<string | null>(null);
  const [summaryModal, setSummaryModal] = useState<string | null>(null);

  useEffect(() => {
    if (chatRef.current) chatRef.current.scrollTop = chatRef.current.scrollHeight;
  }, [messages]);

  // Load persisted developer-chat history on mount so the conversation
  // survives a page refresh. Best-effort: a failed load leaves the panel
  // empty (the same as "no prior conversation").
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await authedFetch(`${API_BASE}/api/requirements/${reqId}/coding-chat`);
        if (!res.ok) return;
        const raw = await res.text();
        const parsed = JSON.parse(raw);
        if (cancelled) return;
        if (Array.isArray(parsed)) {
          const loaded: ChatMessage[] = parsed
            .filter((m: any) => (m?.role === 'user' || m?.role === 'ai') && typeof m?.content === 'string')
            .map((m: any) => ({ role: m.role, content: m.content }));
          if (loaded.length > 0) setMessages(loaded);
        }
      } catch { /* ignore — empty panel is fine */ }
    })();
    return () => { cancelled = true; };
  }, [reqId]);

  // Persist the full conversation after every change. Best-effort: a save
  // failure is logged but does not break the in-flight chat turn. We save
  // on a debounce window so typing/thinking doesn't spam the endpoint.
  const saveMessages = useCallback(async (msgs: ChatMessage[]) => {
    try {
      const body = JSON.stringify({
        messages: JSON.stringify(msgs.map(m => ({ role: m.role, content: m.content }))),
      });
      await authedFetch(`${API_BASE}/api/requirements/${reqId}/coding-chat`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body,
      });
    } catch (err) {
      console.warn('[coding-chat] persist failed', err);
    }
  }, [reqId]);
  useEffect(() => {
    if (messages.length === 0) return;
    const t = setTimeout(() => { saveMessages(messages); }, 400);
    return () => clearTimeout(t);
  }, [messages, saveMessages]);

  // Boot fetch of the persisted compression record for the coding stage.
  // Mirrors the other two chat components: loads the badge state once on
  // mount so the bar's "📦 已压缩" label is correct after a page refresh,
  // even before the user clicks anything.
  useEffect(() => {
    if (!reqId) return;
    let cancelled = false;
    wizardApi.getContextSummary(reqId, 'coding')
      .then(data => { if (!cancelled && data) setCompressedAt(data.compressed_at ?? null); })
      .catch(() => { /* silent */ });
    return () => { cancelled = true; };
  }, [reqId]);

  // Trigger claude to summarize the current developer-chat conversation.
  // Backend writes the summary into requirements.coding_context_summary,
  // stamps coding_compressed_at, and clears coding_session_id so the next
  // developer-chat turn sees the summary as its prepended context rather
  // than the full session history.
  // request<T> throws on a non-2xx response; alert on the caught error
  // instead of checking a success field.
  const handleCompress = useCallback(async () => {
    if (!reqId || compressing) return;
    if (!confirm('让 Claude 总结当前对话并压缩上下文？\n\n该操作会清空当前会话 ID,下次对话将看到压缩摘要而不是完整历史。')) return;
    setCompressing(true);
    try {
      const data = await wizardApi.compressContext(reqId, 'coding');
      setCompressedAt(data.compressed_at ?? null);
      // Reset usage so the bar doesn't keep reporting the soon-cleared
      // session's token counts; the next turn will push a fresh snapshot.
      setUsage(undefined);
    } catch (err: any) {
      alert('压缩失败:' + (err?.message || String(err)));
    } finally {
      setCompressing(false);
    }
  }, [reqId, compressing]);

  // Lazy fetch of the persisted summary text for the modal preview.
  const handleShowSummary = useCallback(async () => {
    if (!reqId) return;
    try {
      const data = await wizardApi.getContextSummary(reqId, 'coding');
      setSummaryModal(data.summary || '(暂无压缩摘要)');
    } catch {
      setSummaryModal('(加载摘要失败)');
    }
  }, [reqId]);

  // Build the desc to pass to onStartCoding: last user message + full conversation context
  const buildCodingDesc = useCallback(() => {
    if (messages.length === 0) return input.trim();
    return messages
      .map(m => (m.role === 'user' ? '用户: ' : 'AI: ') + m.content)
      .join('\n');
  }, [messages, input]);

  // The authoritative conversation lives in the resumed claude session
  // (coding_session_id, keyed by requirement_id). We only send the new user
  // message; no conversation_history is re-fed. This hits the DEVELOPER role
  // (developer-chat), not the analyst — the 追加调整 panel adjusts the
  // already-implemented code, so it must talk to the developer, not refine the
  // requirement.
  const streamReply = useCallback(async (userMessage: string) => {
    const res = await authedFetch(`${API_BASE}/api/wizard/developer-chat`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        requirement_id: reqId,
        project_path: projectPath,
        requirement_title: requirementTitle,
        user_message: userMessage,
      }),
    });

    const reader = res.body?.getReader();
    if (!reader) throw new Error('No stream');

    const decoder = new TextDecoder();
    let aiText = '';

    // Add streaming placeholder
    setMessages(prev => [...prev, { role: 'ai', content: '' }]);

    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      const text = decoder.decode(value, { stream: true });
      for (const line of text.split('\n')) {
        if (!line.startsWith('data: ')) continue;
        try {
          const data = JSON.parse(line.substring(6));
          if (data.type === 'user_input') {
            // Backend echoes the just-sent adjustment as a separate event so
            // the output box can render a "📝 调整请求" strip confirming what
            // the backend actually received. This is the same string that is
            // persisted into token_usage.meta.summary for the token-stats
            // table — the SSE echo lets the user verify that content.
            setLastRequest(data.content || '');
          } else if (data.type === 'message') {
            aiText += data.content + '\n';
            setMessages(prev => {
              const next = [...prev];
              const idx = next.length - 1;
              if (idx >= 0) next[idx] = { role: 'ai', content: (next[idx].content || '') + data.content + '\n' };
              return next;
            });
          } else if (data.type === 'usage') {
            // Usage snapshot emitted at the end of this developer-chat turn.
            // Same parsing as DeepRefineChat / DocRefineChat: backend serializes
            // UsageInfo into `content` as a JSON string; we parse + compute
            // derived fields here so the bar updates live.
            try {
              const parsed = JSON.parse(data.content || '{}') as UsageInfo;
              const used = parsed.input_tokens + parsed.cache_creation_tokens + parsed.cache_read_tokens;
              const cw = parsed.context_window || 200000;
              const pct = cw > 0 ? (used / cw) * 100 : 0;
              setUsage({ ...parsed, used, pct });
            } catch { /* malformed payload — ignore */ }
          }
        } catch { /* skip malformed */ }
      }
    }

    // Finalize: last AI message is the final content
    setMessages(prev => {
      const next = [...prev];
      const idx = next.length - 1;
      if (idx >= 0) next[idx] = { role: 'ai', content: (next[idx].content || '').trim() };
      return next;
    });

    return aiText.trim();
  }, [reqId, projectPath, requirementTitle]);

  const handleSend = async () => {
    const msg = input.trim();
    if (!msg || thinking) return;
    setInput('');
    setMessages(prev => [...prev, { role: 'user', content: msg }]);
    setLastRequest('');
    setThinking(true);

    try {
      await streamReply(msg);
    } catch (err: any) {
      setMessages(prev => {
        const next = [...prev];
        const idx = next.length - 1;
        if (idx >= 0) next[idx] = { role: 'ai', content: '❌ ' + err.message, isError: true };
        return next;
      });
    } finally {
      setThinking(false);
    }
  };

  const handleConfirm = () => {
    const desc = buildCodingDesc();
    onStartCoding(desc);
  };

  const hasConversation = messages.length > 0;

  return (
    <div className="detail-section coding-chat-panel">
      <div className="deep-refine-header">
        <h3>💬 追加调整</h3>
      </div>

      {hasConversation && (() => {
        // Split history into completed rounds (user + ai) and the current
        // round — the most recent trailing user message (and any stream
        // placeholder). The current round stays unfolded so the user can see
        // the in-flight AI reply without expanding history; prior rounds are
        // collapsed by default to avoid burying the input box.
        type Round = { user: ChatMessage; ai?: ChatMessage };
        const rounds: Round[] = [];
        for (let i = 0; i < messages.length; i++) {
          const m = messages[i];
          if (m.role === 'user') {
            const next = messages[i + 1];
            if (next && next.role === 'ai') {
              rounds.push({ user: m, ai: next });
              i += 1;
            } else {
              rounds.push({ user: m }); // trailing user w/o ai yet
            }
          } else if (m.role === 'ai') {
            // Standalone AI (e.g. error fallback). Attach to last round's ai
            // or surface as its own partial round.
            const last = rounds[rounds.length - 1];
            if (last && !last.ai) last.ai = m;
            else rounds.push({ user: { role: 'user', content: '' }, ai: m });
          }
        }
        const currentIdx = rounds.findIndex(r => !r.ai || (r.ai && !r.ai.content.trim())) ?? -1;
        const isCurrent = (i: number) => i === currentIdx;
        const truncate = (s: string, n: number) => s.length > n ? s.slice(0, n) + '…' : s;
        return (
          <div className="chat-panel" ref={chatRef}>
            {lastRequest && (
              <div
                className="chat-msg user user-request"
                title="本次已发送到后端的调整请求"
                style={{
                  background: '#FFFBEB',
                  border: '2px solid #F59E0B',
                  borderLeft: '4px solid #F59E0B',
                  borderRadius: 8,
                  padding: '10px 12px',
                  margin: '8px 0',
                }}
              >
                <span className="chat-role" style={{ color: '#B45309', fontWeight: 700, fontSize: 12, display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                  📤 已发送至后端的调整请求
                </span>
                <div
                  className="chat-content"
                  style={{
                    background: '#FFFFFF',
                    border: '1px dashed #F59E0B',
                    borderRadius: 6,
                    padding: '8px 10px',
                    marginTop: 6,
                    color: '#1E293B',
                    fontFamily: 'var(--font-mono)',
                    fontSize: 12,
                    lineHeight: 1.6,
                    whiteSpace: 'pre-wrap',
                    wordBreak: 'break-word',
                  }}
                >
                  {lastRequest}
                </div>
              </div>
            )}
            {rounds.map((r, i) => {
              const userLabel = r.user.content ? truncate(r.user.content, 30) : '（空）';
              return (
                <details
                  key={i}
                  open={isCurrent(i)}
                  style={{
                    background: '#FFFFFF',
                    border: '1px solid #E2E8F0',
                    borderRadius: 6,
                    padding: '6px 10px',
                    margin: '6px 0',
                  }}
                >
                  <summary style={{ cursor: 'pointer', fontSize: 12, color: '#475569', fontWeight: 600, listStyle: 'none' }}>
                    <span style={{ display: 'inline-block', width: 10, color: '#94A3B8', marginRight: 4 }}>{isCurrent(i) ? '▼' : '▶'}</span>
                    💬 #{i + 1}：{userLabel}
                  </summary>
                  <div style={{ marginTop: 8 }}>
                    {r.user.content && (
                      <div
                        style={{
                          background: '#EEF2FF',
                          border: '1px solid #C7D2FE',
                          borderLeft: '4px solid #4F46E5',
                          borderRadius: 6,
                          padding: '8px 10px',
                          marginBottom: 6,
                          fontSize: 12,
                          color: '#1E293B',
                          whiteSpace: 'pre-wrap',
                          wordBreak: 'break-word',
                        }}
                      >
                        <strong style={{ color: '#4F46E5' }}>你追问：</strong> {r.user.content}
                      </div>
                    )}
                    {r.ai && (
                      <div
                        className={`chat-msg ai${r.ai.isError ? ' error' : ''}`}
                        style={{
                          background: r.ai.isError ? '#FEF2F2' : '#F8FAFC',
                          border: '1px solid #E2E8F0',
                          borderLeft: `4px solid ${r.ai.isError ? '#EF4444' : '#4F46E5'}`,
                          borderRadius: 6,
                          padding: '8px 10px',
                          fontSize: 12,
                          color: '#1E293B',
                          whiteSpace: 'pre-wrap',
                          wordBreak: 'break-word',
                        }}
                      >
                        <strong style={{ color: r.ai.isError ? '#EF4444' : '#4F46E5' }}>🤖 AI：</strong> {r.ai.content || '⏳ 思考中…'}
                      </div>
                    )}
                  </div>
                </details>
              );
            })}
          </div>
        );
      })()}

      <div className="chat-input-row composer-sticky" style={{ marginTop: hasConversation ? 8 : 0 }}>
        <textarea
          value={input}
          onChange={e => setInput(e.target.value)}
          onKeyDown={e => {
            if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); handleSend(); }
          }}
          placeholder={`描述需要调整的内容，AI 会先确认理解再开始修改...\nEnter 发送  ·  Shift+Enter 换行`}
          className="form-input chat-textarea"
          disabled={thinking}
          rows={2}
        />
        <button className="btn btn-primary" onClick={handleSend} disabled={thinking || !input.trim()}>
          发送
        </button>
      </div>

      {hasConversation && !thinking && (
        <div style={{ marginTop: 8 }}>
          <button className="btn btn-primary" onClick={handleConfirm}>
            ✅ 确认，开始修改
          </button>
        </div>
      )}

      {/* Live context-usage bar + 压缩上下文 entry point. Sits at the bottom
          of the chat panel so it's always reachable; mirrors the other two
          chat components (DeepRefineChat, DocRefineChat). The disabled state
          suppresses the click while a turn is in flight or a compression is
          already running. */}
      <ContextUsageBar
        usage={usage}
        onCompress={handleCompress}
        compressing={compressing}
        disabled={thinking || compressing}
        stepLabel="开发调整"
        compressedAt={compressedAt}
        onShowSummary={handleShowSummary}
      />

      {/* Compressed-summary preview modal. Same shape as the other chat
          components' modals so users get the same affordance wherever they
          invoke a compression. */}
      {summaryModal !== null && (
        <div
          className="modal-overlay"
          onClick={() => setSummaryModal(null)}
          role="dialog"
          aria-modal="true"
        >
          <div
            className="modal-box"
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
    </div>
  );
}
