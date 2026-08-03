import { useState, useRef, useEffect, useCallback } from 'react';
import { API_BASE } from '../api/client';

interface Props {
  reqId: string;
  projectPath: string;
  docType: 'spec' | 'design' | 'coding';
  currentDoc: string;
  onApplied: (newDoc: string) => void;
}

interface ChatMessage {
  role: 'user' | 'ai';
  content: string;
  isError?: boolean;
}

const LABEL = { spec: '需求文档', design: '技术方案', coding: '开发指令' };

export default function DocRefineChat({ reqId, projectPath, docType, currentDoc, onApplied }: Props) {
  const [expanded, setExpanded] = useState(false);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState('');
  const [working, setWorking] = useState(false);
  const [refineComplete, setRefineComplete] = useState(false);
  const [applying, setApplying] = useState(false);
  const [applyLines, setApplyLines] = useState<{ type: string; content: string }[]>([]);
  const chatRef = useRef<HTMLDivElement>(null);
  const label = LABEL[docType];

  useEffect(() => {
    if (chatRef.current) chatRef.current.scrollTop = chatRef.current.scrollHeight;
  }, [messages, applyLines]);

  // Reset when doc changes externally (e.g. after apply)
  useEffect(() => {
    setMessages([]);
    setRefineComplete(false);
    setApplyLines([]);
  }, [currentDoc]);

  // The authoritative conversation lives in the resumed claude session on the
  // server (keyed by requirement_id + doc_type). We only stream the user's new
  // message; no conversation_history / current_doc is re-fed.
  const streamRefine = useCallback(async (userMessage: string) => {
    const res = await fetch(`${API_BASE}/api/wizard/refine-doc`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        requirement_id: reqId,
        project_path: projectPath,
        doc_type: docType,
        user_message: userMessage,
      }),
    });

    const reader = res.body?.getReader();
    if (!reader) throw new Error('No stream');
    const decoder = new TextDecoder();
    let buffer = '';
    let aiText = '';
    let complete = false;

    // Add streaming placeholder
    setMessages(prev => [...prev, { role: 'ai', content: '' } as any]);

    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      const parts = buffer.split('\n\n');
      buffer = parts.pop() ?? '';
      for (const part of parts) {
        const dataLine = part.startsWith('data: ') ? part.slice(6) : part;
        if (!dataLine.trim()) continue;
        try {
          const evt = JSON.parse(dataLine);
          if (evt.type === 'done') {
            complete = evt.refine_complete || false;
            continue;
          }
          if (evt.type === 'message') {
            aiText += evt.content + '\n';
            setMessages(prev => {
              const next = [...prev];
              const idx = next.length - 1;
              if (idx >= 0) next[idx] = { role: 'ai', content: (next[idx].content || '') + evt.content + '\n' };
              return next;
            });
          }
        } catch { /* skip */ }
      }
    }

    // Finalize last message (strip [REFINE_COMPLETE] marker)
    setMessages(prev => {
      const next = [...prev];
      const idx = next.length - 1;
      if (idx >= 0) {
        next[idx] = {
          role: 'ai',
          content: (next[idx].content || aiText).replace('[REFINE_COMPLETE]', '').trim(),
        };
      }
      return next;
    });

    return complete;
  }, [reqId, projectPath, docType]);

  const handleSend = async () => {
    const msg = input.trim();
    if (!msg || working) return;
    setInput('');
    setWorking(true);
    setRefineComplete(false);
    setMessages(prev => [...prev, { role: 'user', content: msg }]);

    try {
      const complete = await streamRefine(msg);
      if (complete) setRefineComplete(true);
    } catch (err: any) {
      setMessages(prev => {
        const next = [...prev];
        const idx = next.length - 1;
        if (idx >= 0) next[idx] = { role: 'ai', content: '❌ ' + err.message, isError: true };
        return next;
      });
    } finally {
      setWorking(false);
    }
  };

  const handleApply = async () => {
    setApplying(true);
    setApplyLines([]);

    try {
      const res = await fetch(`${API_BASE}/api/wizard/apply-doc`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          requirement_id: reqId,
          project_path: projectPath,
          doc_type: docType,
        }),
      });

      const reader = res.body?.getReader();
      if (!reader) throw new Error('No stream');
      const decoder = new TextDecoder();
      let buffer = '';

      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        const parts = buffer.split('\n\n');
        buffer = parts.pop() ?? '';
        for (const part of parts) {
          const dataLine = part.startsWith('data: ') ? part.slice(6) : part;
          if (!dataLine.trim()) continue;
          try {
            const evt = JSON.parse(dataLine);
            if (evt.type === 'done') {
              if (evt.success && evt.result) {
                onApplied(evt.result);
                setMessages(prev => [...prev, { role: 'ai', content: `✅ ${label}已更新！` }]);
                setRefineComplete(false);
              }
              return;
            }
            if (evt.type === 'error') {
              setApplyLines(prev => [...prev, { type: 'error', content: '❌ ' + evt.content }]);
            } else if (evt.type === 'tool_call' || evt.type === 'phase') {
              setApplyLines(prev => [...prev, { type: evt.type, content: evt.content }]);
            }
          } catch { /* skip */ }
        }
      }
    } catch (err: any) {
      setApplyLines(prev => [...prev, { type: 'error', content: '❌ ' + err.message }]);
    } finally {
      setApplying(false);
    }
  };

  const handleClear = () => {
    if (!confirm('清除对话记录？')) return;
    setMessages([]);
    setRefineComplete(false);
    setApplyLines([]);
  };

  if (!expanded) {
    return (
      <div style={{ marginTop: 16 }}>
        <button className="btn btn-sm" onClick={() => setExpanded(true)}>
          💬 对话微调{label}
        </button>
      </div>
    );
  }

  return (
    <div className="detail-section" style={{ marginTop: 16 }}>
      <div className="deep-refine-header">
        <h3>💬 微调{label}</h3>
        <div style={{ display: 'flex', gap: 6 }}>
          {messages.length > 0 && !working && (
            <button className="btn btn-sm" onClick={handleClear} title="清除对话">🗑</button>
          )}
          <button className="btn btn-sm" onClick={() => setExpanded(false)}>收起</button>
        </div>
      </div>

      <div className="chat-panel" ref={chatRef}>
        {messages.length === 0 && (
          <div className="chat-msg ai">
            <span className="chat-role">🤖 AI</span>
            <div className="chat-content">告诉我你想修改或补充的内容，我会结合当前{label}给出建议。</div>
          </div>
        )}
        {messages.map((msg, i) => (
          <div key={i} className={`chat-msg ${msg.role}${msg.isError ? ' error' : ''}`}>
            <span className="chat-role">{msg.role === 'ai' ? '🤖 AI' : '👤 你'}</span>
            <div className="chat-content" style={{ whiteSpace: 'pre-wrap' }}>{msg.content}</div>
          </div>
        ))}
        {working && (
          <div className="chat-msg ai">
            <span className="chat-role">🤖 AI</span>
            <div className="chat-content">⏳ 思考中...</div>
          </div>
        )}
      </div>

      {/* Apply progress */}
      {applyLines.length > 0 && (
        <div className="coding-panel" style={{ margin: '8px 0', maxHeight: 120 }}>
          {applyLines.map((l, i) => (
            <div key={i} className={`coding-line coding-line-${l.type}`}>{l.content}</div>
          ))}
          {applying && <div className="coding-line coding-line-tool_call">⏳ 正在更新{label}...</div>}
        </div>
      )}

      <div className="chat-input-row">
        <textarea
          value={input}
          onChange={e => setInput(e.target.value)}
          onKeyDown={e => {
            if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); handleSend(); }
          }}
          placeholder={`描述你想修改或补充的内容...&#10;Enter 发送  ·  Shift+Enter 换行`}
          className="form-input chat-textarea"
          disabled={working || applying}
          rows={2}
        />
        <button className="btn btn-primary" onClick={handleSend} disabled={working || applying || !input.trim()}>
          发送
        </button>
      </div>

      {refineComplete && !applying && (
        <div className="confirm-panel" style={{ marginTop: 8 }}>
          <div className="confirm-panel-icon">✅</div>
          <div className="confirm-panel-body">
            <strong>修改内容已确认</strong>
            <p>点击「应用到{label}」，Claude 将把对话中的修改写入文档并保存。</p>
            <div className="confirm-panel-actions">
              <button className="btn btn-primary" onClick={handleApply} disabled={applying}>
                {applying ? '⏳ 应用中...' : `📝 应用到${label}`}
              </button>
              <button className="btn" onClick={() => setRefineComplete(false)}>继续对话</button>
            </div>
          </div>
        </div>
      )}

      {!refineComplete && messages.length > 0 && !working && !applying && (
        <div className="deep-refine-actions">
          <button className="btn" onClick={handleApply} disabled={applying}>
            📝 直接应用修改到{label}
          </button>
        </div>
      )}
    </div>
  );
}
