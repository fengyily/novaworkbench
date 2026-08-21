import { useState, useRef, useEffect, useCallback } from 'react';
import { API_BASE, authedFetch } from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import { appendLogLine, coalesceLogLines } from '../utils/logLines';
import ModelSelect from './ModelSelect';
import AtMentionTextarea from './AtMentionTextarea';

interface Props {
  reqId: string;
  projectPath: string;
  docType: 'design' | 'coding';
  currentDoc: string;
  // Effective model for this stage's refine/apply turns (server-persisted
  // stage model), used as the dropdown's default selection.
  model?: string;
  // Actual model that the empty "默认模型" selection resolves to for this stage
  // (角色模型 > 生效配置默认), shown next to "默认模型" before the stage runs.
  defaultModel?: string;
  // Active apply-doc JobStore job id (server truth, req.apply_job_id). When
  // set on mount we reconnect to the running apply so a page refresh mid-apply
  // resumes the stream instead of silently dropping it.
  applyJobId?: string;
  // Refresh the requirement after an apply completes (design_docs was
  // persisted server-side; refresh renders it and clears apply_job_id).
  onTurnDone?: () => void;
}

interface ChatMessage {
  role: 'user' | 'ai';
  content: string;
  isError?: boolean;
}

const LABEL = { design: '技术方案', coding: '开发指令' };

export default function DocRefineChat({ reqId, projectPath, docType, currentDoc, model, defaultModel, applyJobId, onTurnDone }: Props) {
  const [expanded, setExpanded] = useState(false);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState('');
  const [working, setWorking] = useState(false);
  const [refineLines, setRefineLines] = useState<{ type: string; content: string }[]>([]);
  const [refineComplete, setRefineComplete] = useState(false);
  const [applying, setApplying] = useState(false);
  const [applyLines, setApplyLines] = useState<{ type: string; content: string }[]>([]);
  const chatRef = useRef<HTMLDivElement>(null);
  const esRef = useRef<EventStream | null>(null);
  const label = LABEL[docType];

  // Stage model for refine/apply turns. Seeded from the server-persisted
  // model (default = 已设置模型); a user switch is sent with the next
  // refine-doc / apply-doc POST and stays local. Disabled while working.
  const [selectedModel, setSelectedModel] = useState('');
  const modelTouchedRef = useRef(false);
  useEffect(() => {
    if (!modelTouchedRef.current) {
      const v = model || '';
      setSelectedModel(v === '默认模型' ? '' : v);
    }
  }, [model]);

  useEffect(() => {
    if (chatRef.current) chatRef.current.scrollTop = chatRef.current.scrollHeight;
  }, [messages, applyLines]);

  // Reset when the doc changes externally (e.g. after an apply refresh).
  useEffect(() => {
    setMessages([]);
    setRefineComplete(false);
    setApplyLines([]);
    setRefineLines([]);
  }, [currentDoc]);

  // The authoritative conversation lives in the resumed claude session on the
  // server (keyed by requirement_id + doc_type). We only stream the user's new
  // message; no conversation_history / current_doc is re-fed.
  const streamRefine = useCallback(async (userMessage: string) => {
    const res = await authedFetch(`${API_BASE}/api/wizard/refine-doc`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        requirement_id: reqId,
        project_path: projectPath,
        doc_type: docType,
        user_message: userMessage,
        // Per-request model override — empty means the role's configured model.
        ...(selectedModel ? { model: selectedModel } : {}),
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
          if (evt.type === 'error') {
            // Backend failures (missing/stale session, claude exit) arrive as
            // error events — render them, otherwise the user sees only their
            // own echo with no explanation.
            aiText += '❌ ' + evt.content + '\n';
            setMessages(prev => {
              const next = [...prev];
              const idx = next.length - 1;
              if (idx >= 0) next[idx] = { role: 'ai', content: (next[idx].content || '') + '❌ ' + evt.content + '\n', isError: true };
              return next;
            });
            continue;
          }
          if (evt.type === 'phase' || evt.type === 'tool_call') {
            // Live activity feed — surfaces "🤖 Claude 已连接" and tool labels
            // during the (often multi-minute) refine turn so the user has
            // progress feedback instead of just their echo + a static
            // "思考中…" line.
            setRefineLines(prev => appendLogLine(prev.slice(-49), { type: evt.type, content: evt.content ?? '' }));
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
          isError: next[idx].isError,
        };
      }
      return next;
    });

    return complete;
  }, [reqId, projectPath, docType, selectedModel]);

  const handleSend = async () => {
    const msg = input.trim();
    if (!msg || working) return;
    setInput('');
    setWorking(true);
    setRefineComplete(false);
    setMessages(prev => [...prev, { role: 'user', content: msg }]);
    setRefineLines([]);

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

  // Stream an apply-doc JobStore job: phase / tool_call / thinking progress →
  // applyLines; on job_done, refresh the requirement (design_docs was persisted
  // server-side). The job replays its full history first, so this works both for
  // a freshly-started apply and for reconnecting to an in-flight apply after a
  // page refresh (applyJobId prop). The apply runs on context.Background() in
  // the backend, so it survives the refresh — this SSE is just a progress view.
  const streamApplyJob = useCallback((jobId: string) => {
    if (esRef.current) esRef.current.close();
    setApplying(true);
    setApplyLines([]);
    esRef.current = createEventStream(
      `/api/wizard/jobs/${jobId}/stream`,
      (evt) => {
        if (evt.type === 'job_done') {
          esRef.current?.close();
          esRef.current = null;
          setApplying(false);
          if (evt.status === 'done' || evt.exit_code === 0) {
            setApplyLines(prev => [...prev, { type: 'phase', content: `✅ ${label}已更新！` }]);
            // design_docs was persisted server-side; refresh renders it and
            // clears apply_job_id (the doc-change reset effect tears down here).
            onTurnDone?.();
          } else {
            setApplyLines(prev => [...prev, { type: 'error', content: '❌ 应用失败，请重试' }]);
          }
          return;
        }
        if (evt.type === 'error') {
          setApplyLines(prev => [...prev, { type: 'error', content: '❌ ' + (evt.content ?? '') }]);
          return;
        }
        // Surface phase / tool_call progress (incl. the thinking_tokens
        // heartbeat). Skip "message" lines — the regenerated doc is large and
        // lands in design_docs via the refresh, not in this thin progress panel.
        // Coalesce consecutive thinking-tokens phase lines into one updatable
        // row instead of stacking one per heartbeat.
        if (evt.type === 'phase' || evt.type === 'tool_call') {
          setApplyLines(prev => appendLogLine(prev.slice(-49), { type: evt.type, content: evt.content ?? '' }));
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
            setApplying(false);
            setApplyLines(prev => [...prev, { type: 'error', content: '⚠️ 任务已丢失（服务可能重启）' }]);
            return;
          }
          const { status, log } = json.data as { status: string; log: { type: string; content: string }[] };
          const visible = coalesceLogLines((log || []).filter(l => l.type === 'phase' || l.type === 'tool_call' || l.type === 'error'));
          if (visible.length > 0) setApplyLines(visible);
          if (status === 'running') {
            streamApplyJob(jobId); // transient drop — re-arm the stream
          } else {
            setApplying(false);
          }
        })
        .catch(() => { setApplying(false); });
      },
    );
  }, [label, onTurnDone]);

  const handleApply = async () => {
    setApplyLines([]);
    setApplying(true);
    try {
      const res = await authedFetch(`${API_BASE}/api/wizard/apply-doc`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          requirement_id: reqId,
          project_path: projectPath,
          doc_type: docType,
          // Per-request model override — empty means the role's configured model.
          ...(selectedModel ? { model: selectedModel } : {}),
        }),
      });
      const json = await res.json();
      const jobId = json.data?.job_id;
      if (!jobId) throw new Error(json.error?.message || '未获取到任务 ID');
      streamApplyJob(jobId);
    } catch (err: any) {
      setApplying(false);
      setApplyLines([{ type: 'error', content: '❌ ' + err.message }]);
    }
  };

  // Boot: reconnect to an in-flight apply job (page refresh mid-apply). The
  // requirement carries apply_job_id (server truth); if the job is still
  // running we resume its stream, otherwise (server restarted, job evicted)
  // we drop into the idle state so the apply button shows again.
  useEffect(() => {
    if (!applyJobId) return;
    let cancelled = false;
    authedFetch(`${API_BASE}/api/wizard/jobs/${applyJobId}`)
      .then(r => r.json())
      .then(json => {
        if (cancelled || !json.success) return;
        const { status, log } = json.data as { status: string; log: { type: string; content: string }[] };
        const visible = coalesceLogLines((log || []).filter(l => l.type === 'phase' || l.type === 'tool_call' || l.type === 'error'));
        if (visible.length > 0) setApplyLines(visible);
        if (status === 'running') {
          setExpanded(true);
          streamApplyJob(applyJobId);
        } else {
          setApplying(false);
        }
      })
      .catch(() => {});
    return () => { cancelled = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [applyJobId]);

  // Tear down the EventSource if the component unmounts mid-apply.
  useEffect(() => () => { if (esRef.current) { esRef.current.close(); esRef.current = null; } }, []);

  const handleClear = () => {
    if (!confirm('清除对话记录？')) return;
    setMessages([]);
    setRefineComplete(false);
    setApplyLines([]);
    setRefineLines([]);
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
        <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
          <ModelSelect
            value={selectedModel}
            onChange={m => { modelTouchedRef.current = true; setSelectedModel(m); }}
            disabled={working || applying}
            working={working || applying}
            label={`${label}模型`}
            defaultModelName={defaultModel}
            title={(working || applying) ? 'Claude 工作中，暂不能切换模型' : `微调${label}使用的模型`}
          />
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
        {working && refineLines.length > 0 && (
          <div className="coding-panel" style={{ margin: '8px 0', maxHeight: 120 }}>
            {refineLines.map((l, i) => (
              <div key={i} className={`coding-line coding-line-${l.type}`}>{l.content}</div>
            ))}
          </div>
        )}
      </div>

      {/* Apply progress. Rendered as soon as `applying` flips true — the panel
          must NOT wait for the first SSE line, otherwise there is a window
          where both apply buttons have hidden (refineComplete && !applying /
          !refineComplete && !applying) and nothing on screen shows an in-flight
          state, which reads as "apply already finished" during a minutes-long
          regeneration. */}
      {(applying || applyLines.length > 0) && (
        <div className="coding-panel" style={{ margin: '8px 0', maxHeight: 160 }}>
          {applyLines.map((l, i) => (
            <div key={i} className={`coding-line coding-line-${l.type}`}>{l.content}</div>
          ))}
          {applying && <div className="coding-line coding-line-tool_call">⏳ 正在更新{label}，预计需要几分钟…</div>}
        </div>
      )}

      <div className="chat-input-row composer-sticky">
        <AtMentionTextarea
          value={input}
          onChange={setInput}
          onKeyDown={e => {
            if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); handleSend(); }
          }}
          placeholder={`描述你想修改或补充的内容... 输入 @ 引用 Skill&#10;Enter 发送  ·  Shift+Enter 换行`}
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
            <div className="confirm-panel-actions btn-row-2col">
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
