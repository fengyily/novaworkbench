// "📋 总结转需求" 弹窗
//
// 想法 / issue 详情页触发，把整段讨论（描述 + 累积 acceptance_criteria +
// 分析师对话记录）一次性扔给 LLM 总结成可开发的需求。LLM 在 prompt 约束下
// 可以用空 markdown 表达"讨论还没收敛"，service 把这种情况当成 422
// NOT_CONVERGED 抛回，前端再翻译成"讨论还没达成共识"的友好错误，提示
// 用户先继续聊天再重试。
//
// 三种状态：idle（确认页）→ running（spinner）→ done（成功跳转 / 失败回退）。
// 跳转由 onCreated 回调负责——父组件接住新 requirement id 后用 navigate
// 跳到详情页。

import { useState } from 'react';
import { authedFetch, API_BASE } from '../api/client';

type Phase = 'idle' | 'running' | 'error';

interface Props {
  sourceId: string;
  sourceTitle: string;
  onClose: () => void;
  onCreated: (newId: string) => void;
}

export function SummarizeToRequirementModal({ sourceId, sourceTitle, onClose, onCreated }: Props) {
  const [phase, setPhase] = useState<Phase>('idle');
  const [errorMsg, setErrorMsg] = useState('');

  const handleConfirm = async () => {
    setPhase('running');
    setErrorMsg('');
    try {
      const res = await authedFetch(`${API_BASE}/api/requirements/${sourceId}/promote`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: '{}',
      });
      const json = await res.json();
      if (!res.ok || !json.success) {
        // 422 NOT_CONVERGED — 讨论还没有达成共识。把后端 error 信息透传给用户
        // （prompt 已经写好了"未达成共识"语义），并提示继续讨论再试。
        if (res.status === 422 || json.error?.code === 'NOT_CONVERGED') {
          setPhase('error');
          setErrorMsg('讨论还没有达成共识。请继续完善想法的讨论，待明确要实现哪些功能后再试。');
          return;
        }
        setPhase('error');
        setErrorMsg(json.error?.message || `请求失败 (${res.status})`);
        return;
      }
      // requirementsApi.promoteFromIdea 也可调用，但 modal 里已经手工处理了
      // 422；这里直接消费 json.data。
      const newReq = json.data as { id: string };
      onCreated(newReq.id);
    } catch (err: any) {
      setPhase('error');
      setErrorMsg(err?.message || '网络错误');
    }
  };

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal-card summarize-modal" onClick={e => e.stopPropagation()}>
        <div className="modal-header">
          <h3>📋 总结转需求</h3>
          <button className="btn btn-sm" onClick={onClose} disabled={phase === 'running'}>×</button>
        </div>

        <div className="modal-body">
          {phase === 'idle' && (
            <>
              <p>
                将把「<strong>{sourceTitle || '该想法'}</strong>」的描述、累积要点和与 AI 的完整对话记录总结成一份可开发的需求文档。
              </p>
              <ul className="modal-hint">
                <li>新需求会以 <code>kind = requirement</code> 创建，状态 <code>draft</code>，可进入分析 → 设计 → 开发完整流程。</li>
                <li>原想法的讨论、状态和历史<strong>保持不变</strong>，方便继续探讨或在不满意时重试。</li>
                <li>如果讨论还没有明确要做什么，AI 会拒绝转换，请先继续聊天再试。</li>
              </ul>
            </>
          )}

          {phase === 'running' && (
            <div className="modal-running">
              <div className="spinner" />
              <p>AI 正在总结讨论内容…</p>
              <small>这通常需要 5–15 秒，取决于讨论长度。</small>
            </div>
          )}

          {phase === 'error' && (
            <div className="modal-error" role="alert">
              <p>{errorMsg}</p>
            </div>
          )}
        </div>

        <div className="modal-actions">
          <button className="btn" onClick={onClose} disabled={phase === 'running'}>
            {phase === 'error' ? '关闭' : '取消'}
          </button>
          {phase === 'error' ? (
            <button className="btn btn-primary" onClick={() => setPhase('idle')}>🔄 重试</button>
          ) : (
            <button
              className="btn btn-primary"
              onClick={handleConfirm}
              disabled={phase === 'running'}
            >
              {phase === 'running' ? '总结中…' : '✨ 开始总结'}
            </button>
          )}
        </div>
      </div>
    </div>
  );
}