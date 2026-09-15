import { useState, useRef, useEffect, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { API_BASE, authedFetch, DefaultModelLabel, wizardApi } from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import { appendLogLine, coalesceLogLines, type LogLine, type UsageInfo, computeUsage } from '../utils/logLines';
import { buildPhaseGroups, formatDuration, useTick } from '../utils/phaseGroups';
import ModelSelect from './ModelSelect';
import AtMentionTextarea from './AtMentionTextarea';
import { FullscreenButton } from './FullscreenButton';
import { useFullscreen } from '../utils/useFullscreen';
import { ContextUsageBar } from './ContextUsageBar';
import { IconCheck } from './icons';

interface Props {
  reqId: string;
  projectPath: string;
  docType: 'design' | 'coding';
  currentDoc: string;
  // Effective model for this stage's refine/apply turns (server-persisted
  // stage model), used as the dropdown's default selection.
  model?: string;
  // Actual model that the empty DefaultModelLabel selection resolves to for
  // this stage (role default > active config default), shown next to
  // DefaultModelLabel before the stage runs.
  defaultModel?: string;
  // Active apply-doc JobStore job id (server truth, req.apply_job_id). When
  // set on mount we reconnect to the running apply so a page refresh mid-apply
  // resumes the stream instead of silently dropping it.
  applyJobId?: string;
  // Refresh the requirement after an apply completes (design_docs was
  // persisted server-side; refresh renders it and clears apply_job_id).
  onTurnDone?: () => void;
  // Reports the live turn state upward so the detail header can show an
  // accurate global "Claude working" badge while a refine / apply turn runs.
  // The persisted apply_job_id is only refreshed after the turn finishes
  // and the parent has reloaded the requirement, so it lags during the
  // turn — we mirror the DeepRefineChat.onWorkingChange pattern and let
  // the parent aggregate "is any turn in flight?" alongside the global
  // /api/wizard/active-jobs polling.
  onWorkingChange?: (working: boolean) => void;
  // Controlled context-usage for this stage's session. Parent owns the live
  // state so it can drive the always-on top strip AND seed from the persisted
  // requirements.usage_snapshots blob. The session key is derived from
  // docType (design→architect_design, coding→coding). We report each `usage`
  // SSE event upward via onUsage and read the value back in `usage`.
  usage?: UsageInfo;
  onUsage?: (u: UsageInfo | undefined) => void;
}

interface ChatMessage {
  role: 'user' | 'ai';
  content: string;
  isError?: boolean;
}

// docLabelKeys maps docType → wizard.docLabels.* (resolved at render via
// tLabel so the chip / header follow the active language).
const docLabelKeys: Record<'design' | 'coding', string> = {
  design: 'wizard.docLabels.design',
  coding: 'wizard.docLabels.coding',
};

export default function DocRefineChat({ reqId, projectPath, docType, currentDoc, model, defaultModel, applyJobId, onTurnDone, onWorkingChange, usage, onUsage }: Props) {
  const [expanded, setExpanded] = useState(false);
  const { isFullscreen, toggle: toggleFullscreen, exit: exitFullscreen } = useFullscreen();
  const { t } = useTranslation();
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState('');
  const [working, setWorking] = useState(false);
  const [refineLines, setRefineLines] = useState<LogLine[]>([]);
  const [refineComplete, setRefineComplete] = useState(false);
  const [applying, setApplying] = useState(false);
  const [applyLines, setApplyLines] = useState<LogLine[]>([]);
  const chatRef = useRef<HTMLDivElement>(null);
  const esRef = useRef<EventStream | null>(null);
  // streamDocJobRef holds the latest streamDocJob useCallback so
  // streamRefine can hand off to the Agent-bound JobStore flow without a
  // circular useCallback dependency (streamDocJob is defined later in the
  // file and would otherwise need to be in streamRefine's deps array).
  const streamDocJobRef = useRef<(jobId: string, kind: 'apply' | 'refine') => Promise<boolean>>(() => Promise.resolve(false));
  // Context-usage is CONTROLLED — the parent owns the live state (so the
  // always-on top strip shares it and persists across refresh / panel
  // collapse). We report `usage` SSE events upward via onUsage; the value
  // comes back in via the `usage` prop for rendering. The session key below
  // maps docType → wizard session for the report.
  const [compressing, setCompressing] = useState(false);
  const [compressedAt, setCompressedAt] = useState<string | null>(null);
  const [summaryModal, setSummaryModal] = useState<string | null>(null);

  // compressStep maps the docType (which the panel exposes) to the wizard
  // stage key that the backend's context-summary columns use:
  //   design  → architect_design  (design_docs is owned by this stage)
  //   coding  → coding            (coding instructions live here)
  // Computed once per render; the resulting string is what we send to
  // wizardApi.compressContext / getContextSummary.
  const compressStep = docType === 'design' ? 'architect_design' : 'coding';
  // Step label shown in the usage bar header — resolved through tLabel so it
  // follows the active language.
  const stepLabel = t(
    docType === 'design' ? 'wizard.docRefine.stepLabelDesign' : 'wizard.docRefine.stepLabelCoding',
  );
  // label: chip / header / apply-button suffix.
  const label = t(docLabelKeys[docType]);

  // Stage model for refine/apply turns. Seeded from the server-persisted
  // model; a user switch is sent with the next refine-doc / apply-doc POST
  // and stays local. Disabled while working.
  const [selectedModel, setSelectedModel] = useState('');
  const modelTouchedRef = useRef(false);
  useEffect(() => {
    if (!modelTouchedRef.current) {
      const v = model || '';
      setSelectedModel(v === DefaultModelLabel ? '' : v);
    }
  }, [model]);

  useEffect(() => {
    if (chatRef.current) chatRef.current.scrollTop = chatRef.current.scrollHeight;
  }, [messages, applyLines]);

  // Surface turn progress upward so the parent's claudeWorking aggregation
  // (RequirementDetail's status badge + claude-status row) can pulse
  // during a refine or apply turn. Mirrors DeepRefineChat's onWorkingChange
  // effect — apply runs through JobStore (so the global polling also picks
  // it up via listActiveJobs), but refine-doc streams straight to the
  // response without entering JobStore, so this callback is the only path
  // for the parent to know a refine turn is in flight.
  useEffect(() => {
    onWorkingChange?.(working || applying);
  }, [working, applying, onWorkingChange]);

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
  //
  // Two response shapes the backend may send, depending on whether the
  // requirement's design/coding stage was bound to an Agent server:
  //   1. SSE-direct (text/event-stream) — local exec path, the legacy
  //      handler streams phase / tool_call / message / done frames.
  //   2. JobStore JSON ({success, data:{job_id}}) — Agent-bound path,
  //      the handler returns a job id and the refine runs on context.
  //      Background() in a goroutine. We subscribe to the same
  //      /api/wizard/jobs/{id}/stream channel the apply path uses.
  // We branch on Content-Type right after authedFetch resolves, so the
  // local path stays a simple ReadableStream pump.
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

    const ctype = res.headers.get('content-type') || '';
    // Agent-bound branch: backend returned a JSON envelope with the JobStore
    // job id. Subscribe to the same /api/wizard/jobs/{id}/stream channel
    // apply-doc uses, and translate the job's terminal `done` log line back
    // into the refine_complete flag the local SSE-direct path used to
    // surface via the `done` SSE event. The job's `result` log line carries
    // the AI text; we splice it into the streaming ai placeholder the same
    // way SSE `message` events did.
    if (ctype.includes('application/json')) {
      const data = await res.json();
      const jobId = data?.data?.job_id || data?.job_id;
      if (!jobId) throw new Error(data?.error?.message || t('wizard.docRefine.noJobId'));
      const complete = await streamDocJobRef.current(jobId, 'refine');
      return complete;
    }

    const reader = res.body?.getReader();
    if (!reader) throw new Error(t('wizard.docRefine.noStream'));
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
            // Live activity feed — surfaces connection / tool labels during
            // the (often multi-minute) refine turn so the user has progress
            // feedback instead of just their echo + a static "thinking"
            // line. Use the backend-stamped `at` so phase timings stay
            // accurate; client-side Date.now() as fallback for old data.
            const at = typeof evt.at === 'number' ? evt.at : Date.now();
            setRefineLines(prev => appendLogLine(prev.slice(-80), { type: evt.type, content: evt.content ?? '', at }));
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
          // Usage snapshot emitted at the end of this refine-doc claude turn.
          // Same parsing as DeepRefineChat: backend marshals UsageInfo into
          // `content` as a JSON string; we parse + compute derived fields
          // (used, pct) here so the bar updates live.
          if (evt.type === 'usage') {
            try {
              const parsed = JSON.parse(evt.content ?? '{}');
              onUsage?.(computeUsage(parsed, compressStep));
            } catch { /* malformed payload — ignore */ }
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
  }, [reqId, projectPath, docType, selectedModel, compressStep, onUsage, t]);

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

  // Stream an apply-doc / refine-doc JobStore job: phase / tool_call / thinking
  // progress → applyLines; on job_done, refresh the requirement for `apply`
  // (design_docs was persisted server-side). The job replays its full history
  // first, so this works both for a freshly-started apply and for
  // reconnecting to an in-flight apply after a page refresh (applyJobId prop).
  // The apply runs on context.Background() in the backend, so it survives the
  // refresh — this SSE is just a progress view.
  //
  // The `kind` selector picks between the two flows the backend now exposes
  // for the wizard doc-refine endpoints:
  //   - 'apply'  → started by handleApply (design_docs was persisted, so
  //                onTurnDone fires on success to refresh the parent).
  //                State machine: applying=true while running; no separate
  //                refine-state set.
  //   - 'refine' → started by streamRefine when the backend returned an
  //                Agent-bound JSON handoff (see the `content-type`
  //                branch in streamRefine). The job's terminal `done` log
  //                line carries the same {type, history, refine_complete}
  //                payload the local SSE-direct path used to emit; we
  //                resolve the returned Promise with the `refine_complete`
  //                flag so streamRefine can flip refineComplete and tear
  //                down its own working state.
  // The error i18n key is reused from apply (wizard.docRefine.applyJobLost)
  // to avoid pulling in a new translation entry — the message is generic
  // enough to cover both flows ("apply job lost" reads naturally for a
  // refine job that got evicted mid-turn).
  const streamDocJob = useCallback((jobId: string, kind: 'apply' | 'refine'): Promise<boolean> => {
    if (esRef.current) esRef.current.close();
    if (kind === 'apply') {
      setApplying(true);
      setApplyLines([]);
    } else {
      setRefineLines([]);
    }
    return new Promise<boolean>((resolve) => {
      esRef.current = createEventStream(
        `/api/wizard/jobs/${jobId}/stream`,
        (evt) => {
          if (evt.type === 'job_done') {
            esRef.current?.close();
            esRef.current = null;
            const success = evt.status === 'done' || evt.exit_code === 0;
            if (kind === 'apply') {
              setApplying(false);
              if (success) {
                setApplyLines(prev => [...prev, { type: 'phase', content: t('wizard.docRefine.applyOk', { label }) }]);
                // design_docs was persisted server-side; refresh renders it and
                // clears apply_job_id (the doc-change reset effect tears down here).
                onTurnDone?.();
              } else {
                setApplyLines(prev => [...prev, { type: 'error', content: t('wizard.docRefine.applyFail') }]);
              }
            }
            resolve(success);
            return;
          }
          if (evt.type === 'error') {
            const line = { type: 'error' as const, content: '❌ ' + (evt.content ?? '') };
            if (kind === 'apply') {
              setApplyLines(prev => [...prev, line]);
            } else {
              setRefineLines(prev => appendLogLine(prev.slice(-80), line));
            }
            return;
          }
          if (evt.type === 'done') {
            // The Agent-bound refine-doc path emits the same terminal
            // {type:"done", history, refine_complete} envelope as the
            // SSE-direct path, packed into the `done` log line's
            // content. Pull it apart here so the caller's
            // refineComplete flip stays consistent across both
            // response shapes.
            try {
              const payload = JSON.parse(evt.content ?? '{}');
              if (typeof payload.refine_complete === 'boolean') {
                resolve(payload.refine_complete);
                return;
              }
            } catch { /* not JSON — fall through to log-only handling */ }
            // Job-level done without an inner refine payload — fall
            // back to treating it as a non-completion signal so the
            // user can decide to send another refinement.
            return;
          }
          if (evt.type === 'result') {
            // Mirror the SSE-direct `message` path: stream the final
            // AI text into the chat placeholder. The agent-bound
            // refine-doc handler always emits exactly one `result`
            // line with the trimmed finalResult before the `done`
            // log line, so this is the canonical AI reply.
            if (kind === 'refine') {
              setMessages(prev => {
                const next = [...prev];
                const idx = next.length - 1;
                if (idx >= 0) next[idx] = {
                  role: 'ai',
                  content: ((next[idx].content || '') + (evt.content ?? '') + '\n').replace('[REFINE_COMPLETE]', '').trim(),
                  isError: next[idx].isError,
                };
                return next;
              });
            } else {
              // Apply's result is the regenerated doc; it lands in
              // design_docs via the refresh, so don't pollute the
              // chat transcript with it.
            }
            return;
          }
          if (evt.type === 'phase' || evt.type === 'tool_call') {
            // Live activity feed — surfaces connection / tool labels
            // during the (often multi-minute) turn so the user has
            // progress feedback. Use the backend-stamped `at` so
            // phase timings stay accurate; client-side Date.now() as
            // fallback for old data.
            const at = typeof evt.at === 'number' ? evt.at : Date.now();
            if (kind === 'apply') {
              setApplyLines(prev => appendLogLine(prev.slice(-80), { type: evt.type, content: evt.content ?? '', at }));
            } else {
              setRefineLines(prev => appendLogLine(prev.slice(-80), { type: evt.type, content: evt.content ?? '', at }));
            }
            return;
          }
          // Usage snapshot for either turn. Same parsing as
          // DeepRefineChat: backend marshals UsageInfo into `content`
          // as a JSON string; both feeds target the same wizard
          // stage so the last write wins on the bar regardless of
          // which flow emitted it.
          if (evt.type === 'usage') {
            try {
              const parsed = JSON.parse(evt.content ?? '{}');
              onUsage?.(computeUsage(parsed, compressStep));
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
              if (kind === 'apply') {
                setApplying(false);
                setApplyLines(prev => [...prev, { type: 'error', content: t('wizard.docRefine.applyJobLost') }]);
              }
              resolve(false);
              return;
            }
            const { status, log } = json.data as { status: string; log: { type: string; content: string; at?: number }[] };
            const visible = coalesceLogLines((log || []).filter(l => l.type === 'phase' || l.type === 'tool_call' || l.type === 'error') as LogLine[]);
            if (visible.length > 0 && kind === 'apply') setApplyLines(visible);
            if (status === 'running') {
              streamDocJob(jobId, kind).then(resolve); // transient drop — re-arm the stream
            } else {
              if (kind === 'apply') setApplying(false);
              resolve(status !== 'done' && status !== 'success');
            }
          })
          .catch(() => {
            if (kind === 'apply') setApplying(false);
            resolve(false);
          });
        },
      );
    });
  }, [label, compressStep, onTurnDone, onUsage, t]);

  // Keep the ref pointed at the latest useCallback so streamRefine's
  // Agent-bound handoff can resolve to the up-to-date closure (the ref
  // sidesteps the otherwise-circular useCallback dep).
  useEffect(() => {
    streamDocJobRef.current = streamDocJob;
  }, [streamDocJob]);

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
      if (!jobId) throw new Error(json.error?.message || t('wizard.docRefine.noJobId'));
      await streamDocJob(jobId, 'apply');
    } catch (err: any) {
      setApplying(false);
      setApplyLines([{ type: 'error', content: '❌ ' + err.message }]);
    }
  };

  // Boot fetch of the persisted compression record. Mirrors DeepRefineChat:
  // loads the badge state once on mount and on reqId change so a refresh
  // surfaces the compressed badge without waiting for the user to click the
  // bar. The summary text itself is fetched lazily by handleShowSummary.
  useEffect(() => {
    if (!reqId) return;
    let cancelled = false;
    wizardApi.getContextSummary(reqId, compressStep)
      .then(data => { if (!cancelled && data) setCompressedAt(data.compressed_at ?? null); })
      .catch(() => { /* silent */ });
    return () => { cancelled = true; };
  }, [reqId, compressStep]);

  // Trigger claude to summarize the current design / coding doc conversation.
  // The backend writes the summary into the matching *{step}_context_summary
  // column, stamps the *_compressed_at timestamp, and clears the session_id
  // so subsequent refine/apply turns see the summary as their prepended
  // context (rather than the full — possibly stale — history).
  // request<T> throws on a non-2xx response; alert on the caught error
  // instead of checking a success field.
  const handleCompress = useCallback(async () => {
    if (!reqId || compressing) return;
    if (!confirm(t('wizard.docRefine.compressConfirm'))) return;
    setCompressing(true);
    try {
      const data = await wizardApi.compressContext(reqId, compressStep);
      setCompressedAt(data.compressed_at ?? null);
      // Reset usage so the bar doesn't keep reporting the soon-cleared
      // session's token counts; the next turn will push a fresh snapshot.
      onUsage?.(undefined);
      onTurnDone?.();
    } catch (err: any) {
      alert(t('wizard.docRefine.compressFailPrefix') + (err?.message || String(err)));
    } finally {
      setCompressing(false);
    }
  }, [reqId, compressing, compressStep, onTurnDone, t]);

  // Open the summary preview modal. Lazy fetch keeps the boot-time GET small.
  const handleShowSummary = useCallback(async () => {
    if (!reqId) return;
    try {
      const data = await wizardApi.getContextSummary(reqId, compressStep);
      setSummaryModal(data.summary || t('wizard.docRefine.summaryFallback'));
    } catch {
      setSummaryModal(t('wizard.docRefine.summaryLoadFail'));
    }
  }, [reqId, compressStep, t]);

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
        const { status, log } = json.data as { status: string; log: { type: string; content: string; at?: number }[] };
        const visible = coalesceLogLines((log || []).filter(l => l.type === 'phase' || l.type === 'tool_call' || l.type === 'error') as LogLine[]);
        if (visible.length > 0) setApplyLines(visible);
        if (status === 'running') {
          setExpanded(true);
          streamDocJob(applyJobId, 'apply');
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
    if (!confirm(t('wizard.docRefine.clearConfirm'))) return;
    setMessages([]);
    setRefineComplete(false);
    setApplyLines([]);
    setRefineLines([]);
  };

  if (!expanded) {
    return (
      <div style={{ marginTop: 16 }}>
        <button className="btn btn-sm" onClick={() => setExpanded(true)}>
          {t('wizard.docRefine.expandBtn', { label })}
        </button>
      </div>
    );
  }

  return (
    <div className="detail-section doc-refine-panel" style={{ marginTop: 16 }}>
      <div className="deep-refine-header">
        <h3>{t('wizard.docRefine.header', { label })}</h3>
        <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
          <ModelSelect
            value={selectedModel}
            onChange={m => { modelTouchedRef.current = true; setSelectedModel(m); }}
            disabled={working || applying}
            working={working || applying}
            label={t('wizard.docRefine.modelLabel', { label })}
            defaultModelName={defaultModel}
            title={(working || applying) ? t('wizard.docRefine.modelTitleBusy') : t('wizard.docRefine.modelTitle', { label })}
          />
          {messages.length > 0 && !working && (
            <button className="btn btn-sm" onClick={handleClear} title={t('wizard.docRefine.clearTitle')}>🗑</button>
          )}
          <button className="btn btn-sm" onClick={() => setExpanded(false)}>{t('wizard.docRefine.collapse')}</button>
          <FullscreenButton isFullscreen={isFullscreen} onClick={toggleFullscreen} />
        </div>
      </div>

      <div className={`chat-panel ${isFullscreen ? 'is-fullscreen' : ''}`} ref={chatRef}>
        {isFullscreen && (
          <FullscreenButton isFullscreen={true} onClick={exitFullscreen} variant="floating" />
        )}
        {messages.length === 0 && (
          <div className="chat-msg ai">
            <span className="chat-role">🤖 AI</span>
            <div className="chat-content">{t('wizard.docRefine.aiGreeting', { label })}</div>
          </div>
        )}
        {messages.map((msg, i) => (
          <div key={i} className={`chat-msg ${msg.role}${msg.isError ? ' error' : ''}`}>
            <span className="chat-role">{msg.role === 'ai' ? '🤖 AI' : '👤 ' + t('wizard.page.roleUser').replace('👤 ', '')}</span>
            <div className="chat-content" style={{ whiteSpace: 'pre-wrap' }}>{msg.content}</div>
          </div>
        ))}
        {working && (
          <div className="chat-msg ai">
            <span className="chat-role">🤖 AI</span>
            <div className="chat-content">{t('wizard.docRefine.aiThinking')}</div>
          </div>
        )}
        {refineLines.length > 0 && (
          <CodingPhasesPanel lines={refineLines} working={working} />
        )}
      </div>

      {/* Apply progress. Rendered as soon as `applying` flips true — the panel
          must NOT wait for the first SSE line, otherwise there is a window
          where both apply buttons have hidden (refineComplete && !applying /
          !refineComplete && !applying) and nothing on screen shows an in-flight
          state, which reads as "apply already finished" during a minutes-long
          regeneration. */}
      {(applying || applyLines.length > 0) && (
        <CodingPhasesPanel
          lines={applyLines}
          working={applying}
          trailingHint={applying ? t('wizard.docRefine.trailingHint', { label }) : ''}
          style={{ margin: '8px 0', maxHeight: 160 }}
        />
      )}

      <div className="chat-input-row composer-sticky">
        <AtMentionTextarea
          value={input}
          onChange={setInput}
          onKeyDown={e => {
            if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); handleSend(); }
          }}
          placeholder={t('wizard.docRefine.placeholder')}
          className="form-input chat-textarea"
          disabled={working || applying}
          rows={2}
        />
        <button className="btn btn-primary" onClick={handleSend} disabled={working || applying || !input.trim()}>
          {t('wizard.docRefine.send')}
        </button>
      </div>

      {refineComplete && !applying && (
        <div className="confirm-panel" style={{ marginTop: 8 }}>
          <div className="confirm-panel-icon"><IconCheck size={22} /></div>
          <div className="confirm-panel-body">
            <strong>{t('wizard.docRefine.confirmTitle')}</strong>
            <p>{t('wizard.docRefine.confirmBody', { label })}</p>
            <div className="confirm-panel-actions btn-row-2col">
              <button className="btn btn-primary" onClick={handleApply} disabled={applying}>
                {applying ? t('wizard.docRefine.applyBusy') : t('wizard.docRefine.applyBtn', { label })}
              </button>
              <button className="btn" onClick={() => setRefineComplete(false)}>{t('wizard.docRefine.continueBtn')}</button>
            </div>
          </div>
        </div>
      )}

      {!refineComplete && messages.length > 0 && !working && !applying && (
        <div className="deep-refine-actions">
          <button className="btn" onClick={handleApply} disabled={applying}>
            {t('wizard.docRefine.applyDirectBtn', { label })}
          </button>
        </div>
      )}

      {/* Live context-usage bar + compress-context entry point. Sits at the
          bottom of the chat panel so it stays visible while the user
          scrolls through messages / phase activity. Mirrors DeepRefineChat:
          disabled while a refine or apply turn is running; tap opens
          the summary modal when the stage has already been compressed. */}
      <ContextUsageBar
        usage={usage}
        onCompress={handleCompress}
        compressing={compressing}
        disabled={working || applying || compressing}
        stepLabel={stepLabel}
        compressedAt={compressedAt}
        onShowSummary={handleShowSummary}
        // The design stage is excluded from compression: the design is a
        // one-shot plan-mode artifact, the refine chat has no compression
        // value — keep the usage bar but hide the compress button.
        compressible={docType !== 'design'}
      />

      {/* Compressed-summary preview modal. Same shape as DeepRefineChat's
          modal so the visual treatment is consistent across stages. */}
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
              <h3>{t('wizard.docRefine.summaryTitle')}</h3>
              <button className="btn btn-sm" onClick={() => setSummaryModal(null)}>{t('wizard.docRefine.closeBtn')}</button>
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

// Shared rendering for the refine + apply progress panels: groups
// phase/tool_call lines into named phases with per-phase and per-tool-call
// durations, plus a top summary line.
function CodingPhasesPanel({
  lines: logLines,
  working,
  trailingHint,
  style,
}: {
  lines: LogLine[];
  working: boolean;
  trailingHint?: string;
  style?: React.CSSProperties;
}) {
  const { t } = useTranslation();
  useTick(working);
  const phases = buildPhaseGroups(logLines);
  const firstAt = phases[0]?.startedAt;
  const lastPhase = phases[phases.length - 1];
  const totalMs =
    phases.length > 0 && firstAt != null
      ? Math.max(0, (lastPhase.isActive ? Date.now() : lastPhase.finishedAt) - firstAt)
      : 0;
  return (
    <div className="coding-panel" style={style ?? { margin: '8px 0', maxHeight: 160 }}>
      <div className="coding-line-summary">
        {t('wizard.docRefine.toolLogSummary', { n: phases.length, total: formatDuration(totalMs) })}
      </div>
      {phases.map((p, i) => {
        const active = p.isActive && working;
        const displayMs = active ? Date.now() - p.startedAt : p.durationMs;
        return (
          <div key={i} className={`coding-line-phase-group${active ? ' active' : ''}`}>
            <div className="coding-line-phase-header">
              <span className="coding-line-phase-label">{p.label}</span>
              <span className="coding-line-phase-time">
                {formatDuration(displayMs)}
              </span>
            </div>
            {p.thinking && (
              <div className="coding-line coding-line-phase">{p.thinking.content}</div>
            )}
            {p.toolCalls.map((tc, j) => (
              <div key={j} className="coding-line coding-line-tool_call">
                <span>{tc.content}</span>
                {tc.durationMs != null && tc.durationMs > 0 && (
                  <span className="coding-line-entry-time">
                    {' · '}{formatDuration(tc.durationMs)}
                  </span>
                )}
              </div>
            ))}
          </div>
        );
      })}
      {trailingHint && (
        <div className="coding-line coding-line-tool_call">{trailingHint}</div>
      )}
    </div>
  );
}