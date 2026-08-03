import { useState, useRef, useEffect, useCallback } from 'react';
import { API_BASE } from '../api/client';

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
  const chatRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (chatRef.current) chatRef.current.scrollTop = chatRef.current.scrollHeight;
  }, [messages]);

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
    const res = await fetch(`${API_BASE}/api/wizard/developer-chat`, {
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
          if (data.type === 'message') {
            aiText += data.content + '\n';
            setMessages(prev => {
              const next = [...prev];
              const idx = next.length - 1;
              if (idx >= 0) next[idx] = { role: 'ai', content: (next[idx].content || '') + data.content + '\n' };
              return next;
            });
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

      {hasConversation && (
        <div className="chat-panel" ref={chatRef}>
          {messages.map((msg, i) => (
            <div key={i} className={`chat-msg ${msg.role}${msg.isError ? ' error' : ''}`}>
              <span className="chat-role">{msg.role === 'ai' ? '🤖 AI' : '👤 你'}</span>
              <div className="chat-content" style={{ whiteSpace: 'pre-wrap' }}>{msg.content}</div>
            </div>
          ))}
          {thinking && (
            <div className="chat-msg ai">
              <span className="chat-role">🤖 AI</span>
              <div className="chat-content">⏳ 思考中...</div>
            </div>
          )}
        </div>
      )}

      <div className="chat-input-row" style={{ marginTop: hasConversation ? 8 : 0 }}>
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
    </div>
  );
}
