// RequirementSplitModal — the 方案拆分 (requirement split) preview.
//
// The flow mirrors the wizard's three role-gated stages: the AI produces
// CANDIDATES, the human edits / deletes / reorders / wires up the
// prerequisites, and only then does anything hit the database. The model
// never gets to declare the split "done" on its own.
//
// What the user can change before confirming:
//   · the title / description / acceptance of each candidate
//   · the order (which is also the ordinal the dependency edges refer to)
//   · which candidates a candidate depends on (only EARLIER ones — a forward
//     reference would be a cycle)
//   · which candidates reference each other (context sharing, NOT a gate)
//
// The dependency ("前置任务") and the reference ("互相参考") are two
// different things and the UI keeps them apart:
//   · dependency = an execution-order gate. A dependent cannot start coding
//     until its prerequisite is done.
//   · reference  = context sharing. It only enriches the design prompt and
//     blocks nothing.
//
// The candidate list lives in memory (the backend's job result is
// in-process state), so a page refresh loses it — the modal says so rather
// than pretending otherwise.

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  ApiError,
  attachmentsApi,
  requirementsApi,
  requirementDirsApi,
  type Attachment,
  type RequirementDir,
  type RequirementReference,
  type SplitTaskCandidate,
} from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import { BranchPicker } from './BranchPicker';
import { ExecEnvSelect, type ExecEnvServer } from './ExecEnvSelect';
import { IconClose } from './icons';
import ModelSelect from './ModelSelect';
import './RequirementSplitModal.css';

/**
 * The modal runs in three stages:
 *
 *   config  → the user picks the model / execution environment / source
 *             branch. NOTHING is sent to the backend here.
 *   running → the AI is producing the candidates (SSE progress).
 *   review  → the candidate table; the user edits and confirms.
 *
 * The config stage exists because the split is the one entry point that used
 * to fire the AI the instant the dialog opened — the user never got a say in
 * which model ran, on which machine, or on top of which branch.
 */
type SplitStage = 'config' | 'running' | 'review';

export interface RequirementSplitModalProps {
  open: boolean;
  onClose: () => void;
  requirementId: string;
  requirementTitle: string;
  /** The master's CURRENT base — the default value of the source-branch
   *  picker, and what the stack-bottom branch is created from. */
  baseBranch: string;
  /** The master's own dev branch (feat/req_<master>) — the stack bottom every
   *  child develops on. */
  masterBranch: string;
  /** Branch candidates for the source-branch picker. */
  branches: string[];
  /** Ready Agent Servers (the parent already filters by status === 'ready'). */
  agentServers: ExecEnvServer[];
  /** Initial model / Claude config / execution environment, taken from the
   *  page's design toolbar. */
  initialModel?: string;
  initialConfigId?: string;
  initialAgentServerId?: string;
  /** The real model name shown next to the empty (default model) option. */
  initialModelName?: string;
  /** Fired after the candidates were persisted. */
  onConfirmed: () => void;
}

/** A candidate as the modal holds it: the API shape plus the editable bits. */
interface Row {
  title: string;
  description: string;
  scope: string[];
  acceptance: string[];
  boundary: string;
  /** 1-based ordinal within the batch — the dependency edges refer to it. */
  order: number;
  /** Ordinals (1-based) this candidate depends on. */
  dependsOn: number[];
}

/** The backend's candidate shape (the `result.tasks` payload on the SSE frame). */
interface RawCandidate {
  title?: string;
  description?: string;
  scope?: string[];
  acceptance?: string[];
  boundary?: string;
  order?: number;
  depends_on?: number[];
}

function toRow(raw: RawCandidate, index: number): Row {
  return {
    title: (raw.title ?? '').trim(),
    description: (raw.description ?? '').trim(),
    scope: Array.isArray(raw.scope) ? raw.scope : [],
    acceptance: Array.isArray(raw.acceptance) ? raw.acceptance : [],
    boundary: (raw.boundary ?? '').trim(),
    order: raw.order && raw.order > 0 ? raw.order : index + 1,
    dependsOn: Array.isArray(raw.depends_on) ? raw.depends_on : [],
  };
}

/** Splits a textarea's content into a trimmed, non-empty list. */
function lines(text: string): string[] {
  return text
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l !== '');
}

/** Human-readable file size — mirrors the backend's formatSize. */
function formatSize(n: number): string {
  if (!n || n < 1024) return `${n || 0} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

export function RequirementSplitModal({
  open,
  onClose,
  requirementId,
  requirementTitle,
  baseBranch,
  masterBranch,
  branches,
  agentServers,
  initialModel,
  initialConfigId,
  initialAgentServerId,
  initialModelName,
  onConfirmed,
}: RequirementSplitModalProps) {
  const { t } = useTranslation();

  const [rows, setRows] = useState<Row[]>([]);
  const [stage, setStage] = useState<SplitStage>('config');
  /** The latest progress line pushed over SSE. */
  const [progress, setProgress] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  /** Candidate pairs the user ticked for mutual reference: [a, b] ordinals. */
  const [references, setReferences] = useState<[number, number][]>([]);
  /** The branch the master's own dev branch is created from. */
  const [sourceBranch, setSourceBranch] = useState<string>(baseBranch || '');
  /** The model this split run uses (empty = the architect persona default). */
  const [model, setModel] = useState<string>(initialModel || '');
  /** The claude_configs row this split run uses (empty = the active config). */
  const [configId, setConfigId] = useState<string>(initialConfigId || '');
  /** The execution environment (empty = this machine). */
  const [agentServerId, setAgentServerId] = useState<string>(() => {
    // Guard against an AgentServer id that has dropped off the ready list
    // since the page last fetched — same fallback DesignCodingImmediateModal
    // uses. A stale id would send the split to a target that no longer
    // exists.
    if (!initialAgentServerId) return '';
    return agentServers.some((s) => s.id === initialAgentServerId)
      ? initialAgentServerId
      : '';
  });

  /**
   * The material this split will hand to the AI: the uploaded attachments,
   * the directories the user authorized Claude to read, and the requirements
   * this one references.
   *
   * Purely informational — the backend injects all of it into the split
   * prompt on its own. The card exists so the user can SEE what the model
   * will see before spending a run on it: a split that silently ignores the
   * attachments is the difference between grounded children and invented
   * interfaces.
   */
  const [ctxAttachments, setCtxAttachments] = useState<Attachment[]>([]);
  const [ctxDirs, setCtxDirs] = useState<RequirementDir[]>([]);
  const [ctxRefs, setCtxRefs] = useState<RequirementReference[]>([]);
  const [ctxLoading, setCtxLoading] = useState(false);

  const streamRef = useRef<EventStream | null>(null);
  /** True once the modal unmounted — guards the async start() against
   *  touching state that no longer exists. */
  const cancelledRef = useRef(false);

  const close = useCallback(() => {
    streamRef.current?.close();
    streamRef.current = null;
    setRows([]);
    setStage('config');
    setProgress('');
    setError('');
    setReferences([]);
    onClose();
  }, [onClose]);

  // Seed the source-branch picker from the master's current base whenever
  // the modal opens (the parent's base can change between two splits).
  useEffect(() => {
    if (!open) return;
    setSourceBranch(baseBranch || '');
  }, [open, baseBranch]);

  // Load the requirement's material for the context card. Three independent
  // reads — one of them failing (e.g. an old database without the dirs
  // table) must not hide the other two, so each result settles on its own.
  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setCtxLoading(true);
    void Promise.allSettled([
      attachmentsApi.list('requirements', requirementId),
      requirementDirsApi.list(requirementId),
      requirementsApi.getReferences(requirementId),
    ]).then(([atts, dirs, refs]) => {
      if (cancelled) return;
      setCtxAttachments(atts.status === 'fulfilled' ? atts.value ?? [] : []);
      // The synthetic "auto" row is the requirement's attachment directory —
      // not a grant the user made, so it does not belong in the grant list.
      setCtxDirs(dirs.status === 'fulfilled' ? (dirs.value ?? []).filter((d) => !d.auto) : []);
      setCtxRefs(refs.status === 'fulfilled' ? refs.value ?? [] : []);
      setCtxLoading(false);
    });
    return () => {
      cancelled = true;
    };
  }, [open, requirementId]);

  // The split no longer starts on its own: the user configures the run
  // (model / execution environment / source branch) first and then presses
  // the start button. This effect only tears the SSE stream down on unmount.
  useEffect(() => {
    cancelledRef.current = false;
    return () => {
      cancelledRef.current = true;
      streamRef.current?.close();
      streamRef.current = null;
    };
  }, []);

  // start kicks off one split run with the configuration the user picked.
  // The SSE handling below is the original one, moved verbatim — only the
  // trigger changed.
  const start = useCallback(async () => {
    setRows([]);
    setProgress(t('requirements.split.generating'));
    setError('');
    setReferences([]);
    setStage('running');
    try {
      const { job_id } = await requirementsApi.split(requirementId, {
        model: model || undefined,
        claude_config_id: configId || undefined,
        agent_server_id: agentServerId || undefined,
      });
      if (cancelledRef.current) return;
      const stream = createEventStream(
        requirementsApi.splitStreamUrl(requirementId, job_id),
        (data: any) => {
          if (cancelledRef.current) return;
          if (data?.type === 'phase' || data?.type === 'message' || data?.type === 'tool_call') {
            if (data.content) setProgress(String(data.content));
            return;
          }
          if (data?.type === 'job_done') {
            if (data.status === 'error') {
              setError(t('requirements.split.errGeneric', { msg: data.error_kind || '' }));
              setStage('config');
              return;
            }
            const tasks: RawCandidate[] = Array.isArray(data?.result?.tasks)
              ? data.result.tasks
              : [];
            if (tasks.length === 0) {
              // The candidate list is in-process state — a backend restart
              // (or an eviction from the job ring buffer) loses it.
              setError(t('requirements.split.errExpired'));
              setStage('config');
              return;
            }
            const next = tasks.map(toRow).sort((a, b) => a.order - b.order);
            setRows(next.map((r, i) => ({ ...r, order: i + 1 })));
            setProgress(t('requirements.split.generated', { count: next.length }));
            setStage('review');
            return;
          }
        },
        () => {
          if (cancelledRef.current) return;
          setError(t('requirements.split.errGeneric', { msg: '' }));
          setStage('config');
        },
      );
      streamRef.current = stream;
    } catch (err) {
      if (cancelledRef.current) return;
      setError(err instanceof ApiError ? err.message : t('requirements.split.errGeneric', { msg: String(err) }));
      setStage('config');
    }
  }, [requirementId, model, configId, agentServerId, t]);

  const update = (index: number, patch: Partial<Row>) => {
    setRows((prev) => prev.map((r, i) => (i === index ? { ...r, ...patch } : r)));
  };

  const remove = (index: number) => {
    setRows((prev) => {
      const next = prev.filter((_, i) => i !== index);
      // Re-number and drop the dangling dependency ordinals — the ordinals
      // are positional, so removing a row shifts everything below it.
      const remap = new Map<number, number>();
      next.forEach((r, i) => remap.set(r.order, i + 1));
      return next.map((r, i) => ({
        ...r,
        order: i + 1,
        dependsOn: r.dependsOn
          .map((d) => remap.get(d))
          .filter((d): d is number => d !== undefined && d < i + 1),
      }));
    });
    setReferences([]);
  };

  const move = (index: number, delta: number) => {
    setRows((prev) => {
      const target = index + delta;
      if (target < 0 || target >= prev.length) return prev;
      const next = [...prev];
      const [moved] = next.splice(index, 1);
      next.splice(target, 0, moved);
      // Ordinals are positional — renumber and fix the edges.
      const remap = new Map<number, number>();
      next.forEach((r, i) => remap.set(r.order, i + 1));
      return next.map((r, i) => ({
        ...r,
        order: i + 1,
        dependsOn: r.dependsOn
          .map((d) => remap.get(d))
          .filter((d): d is number => d !== undefined && d < i + 1),
      }));
    });
    setReferences([]);
  };

  const toggleDependency = (index: number, dep: number) => {
    setRows((prev) =>
      prev.map((r, i) => {
        if (i !== index) return r;
        const has = r.dependsOn.includes(dep);
        return {
          ...r,
          dependsOn: has ? r.dependsOn.filter((d) => d !== dep) : [...r.dependsOn, dep].sort((a, b) => a - b),
        };
      }),
    );
  };

  const toggleReference = (a: number, b: number) => {
    setReferences((prev) => {
      const exists = prev.some((p) => p[0] === a && p[1] === b);
      if (exists) return prev.filter((p) => !(p[0] === a && p[1] === b));
      // A reference is undirected — store it once, in order.
      const key: [number, number] = a < b ? [a, b] : [b, a];
      return [...prev, key];
    });
  };

  const confirm = async () => {
    setBusy(true);
    setError('');
    try {
      const tasks: SplitTaskCandidate[] = rows.map((r, i) => ({
        title: r.title.trim(),
        description: r.description.trim(),
        scope: r.scope,
        acceptance: r.acceptance,
        boundary: r.boundary,
        order: i + 1,
        depends_on: r.dependsOn,
      }));
      const res = await requirementsApi.confirmSplit(requirementId, {
        tasks,
        references,
        source_branch: sourceBranch.trim() || undefined,
      });
      const warn = res.warnings && res.warnings.length > 0 ? res.warnings.join('；') : '';
      close();
      if (warn) {
        // Surface the soft failure — the children landed, only the references
        // did not.
        setError(warn);
      }
      onConfirmed();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : t('requirements.split.errGeneric', { msg: String(err) }));
    } finally {
      setBusy(false);
    }
  };

  // The candidates a row may depend on: only the EARLIER ones. A forward
  // reference would be a cycle.
  const possibleDeps = useMemo(
    () => (index: number) => rows.slice(0, index).map((_, i) => i + 1),
    [rows],
  );

  const referencePairs = useMemo(() => {
    const pairs: [number, number][] = [];
    for (let a = 1; a <= rows.length; a++) {
      for (let b = a + 1; b <= rows.length; b++) pairs.push([a, b]);
    }
    return pairs;
  }, [rows.length]);

  if (!open) return null;

  const canConfirm = !busy && stage === 'review' && rows.length > 0 && rows.every((r) => r.title.trim() !== '');

  return (
    <div className="modal-overlay" onClick={() => !busy && close()}>
      <div className="modal-box req-split-modal" onClick={(e) => e.stopPropagation()}>
        <div className="modal-header">
          <h3>
            {t('requirements.split.modalTitle')}
            {requirementTitle && <span className="req-split-modal-subject"> · {requirementTitle}</span>}
          </h3>
          <button
            type="button"
            className="btn btn-sm"
            onClick={close}
            disabled={busy}
            aria-label={t('requirements.split.cancel')}
            title={t('requirements.split.cancel')}
          >
            <IconClose size={12} />
          </button>
        </div>

        <div className="req-split-body">
          {/* ── Config stage ──
              The model, the execution environment and the source branch are
              picked BEFORE the AI is asked for candidates. Nothing is sent
              to the backend from this block. */}
          {stage === 'config' && (
            <div className="req-split-config">
              <div className="req-split-config-title">{t('requirements.split.configTitle')}</div>

              <div className="req-split-config-field">
                <span className="req-split-config-label">{t('requirements.split.modelLabel')}</span>
                <ModelSelect
                  value={model}
                  onChange={setModel}
                  stage="architect"
                  defaultModelName={initialModelName || undefined}
                  configId={configId || undefined}
                  onConfigChange={setConfigId}
                />
              </div>

              <div className="req-split-config-field">
                <span className="req-split-config-label">{t('requirements.split.execEnvLabel')}</span>
                <div className="preflight-field-card preflight-field-card--exec">
                  <span className="preflight-field-chip" aria-hidden="true">ENV</span>
                  <ExecEnvSelect
                    className="preflight-field-input"
                    servers={agentServers}
                    value={agentServerId}
                    onChange={setAgentServerId}
                    localOptionLabel={t('requirements.detail2.preflightLocalExec')}
                  />
                </div>
                {agentServers.length === 0 && (
                  <small className="req-split-hint req-split-hint-muted">
                    {t('requirements.split.execEnvEmptyHint')}
                  </small>
                )}
                {agentServerId !== '' && (
                  <small className="req-split-hint req-split-hint-muted">
                    {t('requirements.split.execEnvRemoteHint')}
                  </small>
                )}
              </div>

              <div className="req-split-hint req-split-hint-muted">
                {t('requirements.split.configScopeHint')}
              </div>

              {/* ── The material this split carries ──
                  Read-only. The backend injects all of it into the split
                  prompt; this card only shows the user what the model will
                  see. */}
              <div className="req-split-context">
                <div className="req-split-context-title">{t('requirements.split.contextTitle')}</div>

                {ctxLoading ? (
                  <div className="req-split-context-empty">{t('requirements.split.contextLoading')}</div>
                ) : (
                  <>
                    <div className="req-split-context-group">
                      <div className="req-split-context-label">
                        📎 {t('requirements.split.contextAttachments', { count: ctxAttachments.length })}
                      </div>
                      {ctxAttachments.length === 0 ? (
                        <div className="req-split-context-empty">{t('requirements.split.contextNone')}</div>
                      ) : (
                        <ul className="req-split-context-list">
                          {ctxAttachments.map((a) => (
                            <li key={a.id}>
                              <span className="req-split-context-name">{a.filename}</span>
                              <span className="req-split-context-meta">{formatSize(a.size_bytes)}</span>
                              {a.description && (
                                <span className="req-split-context-desc">{a.description}</span>
                              )}
                            </li>
                          ))}
                        </ul>
                      )}
                    </div>

                    <div className="req-split-context-group">
                      <div className="req-split-context-label">
                        📂 {t('requirements.split.contextDirs', { count: ctxDirs.length })}
                      </div>
                      {ctxDirs.length === 0 ? (
                        <div className="req-split-context-empty">{t('requirements.split.contextNone')}</div>
                      ) : (
                        <ul className="req-split-context-list">
                          {ctxDirs.map((d) => (
                            <li key={d.id}>
                              <span className="req-split-context-name">{d.path}</span>
                              {d.label && <span className="req-split-context-desc">{d.label}</span>}
                            </li>
                          ))}
                        </ul>
                      )}
                    </div>

                    <div className="req-split-context-group">
                      <div className="req-split-context-label">
                        🔗 {t('requirements.split.contextRefs', { count: ctxRefs.length })}
                      </div>
                      {ctxRefs.length === 0 ? (
                        <div className="req-split-context-empty">{t('requirements.split.contextNone')}</div>
                      ) : (
                        <ul className="req-split-context-list">
                          {ctxRefs.map((r) => (
                            <li key={r.requirement_id}>
                              <span className="req-split-context-name">{r.title || r.requirement_id}</span>
                              {r.note && <span className="req-split-context-desc">{r.note}</span>}
                            </li>
                          ))}
                        </ul>
                      )}
                    </div>

                    {ctxAttachments.length === 0 && ctxDirs.length === 0 && ctxRefs.length === 0 && (
                      <div className="req-split-context-empty">{t('requirements.split.contextEmptyHint')}</div>
                    )}
                  </>
                )}
              </div>
            </div>
          )}

          {/* The source branch: the master's own dev branch is created from
              it (or aligned to it) and pushed to the remote. Every child
              then develops on that branch — stacked PRs. */}
          <div className="req-split-source">
            <BranchPicker
              inputId="split-source-branch-input"
              datalistId="split-source-branch-suggestions"
              value={sourceBranch}
              onChange={setSourceBranch}
              disabled={busy}
              placeholder={baseBranch || 'main'}
              options={branches}
              chipLabel="BASE 主需求"
              defaultName={baseBranch || 'main'}
              emptyWarnText={t('requirements.detail2.designBranchPickerEmpty')}
            />
            <div className="req-split-hint req-split-hint-muted">
              {t('requirements.split.sourceBranchHint')}
            </div>
          </div>
          <div className="req-split-hint">
            {t('requirements.split.baseHint', { base: masterBranch || baseBranch || 'main' })}
          </div>
          <div className="req-split-hint req-split-hint-muted">{t('requirements.split.parallelHint')}</div>

          {stage === 'running' && (
            <div className="req-split-progress">
              <span className="req-spinner" aria-hidden />
              {progress || t('requirements.split.generating')}
            </div>
          )}

          {error && <div className="req-split-error">{error}</div>}

          {stage === 'review' && rows.length > 0 && (
            <>
              <div className="req-split-table">
                <div className="req-split-row req-split-row-head">
                  <span className="req-split-col-idx">#</span>
                  <span className="req-split-col-title">{t('requirements.split.colTitle')}</span>
                  <span className="req-split-col-desc">{t('requirements.split.colDesc')}</span>
                  <span className="req-split-col-dep">{t('requirements.split.colDepends')}</span>
                  <span className="req-split-col-act">{t('requirements.split.colActions')}</span>
                </div>
                {rows.map((r, i) => (
                  <div className="req-split-row" key={i}>
                    <span className="req-split-col-idx">{i + 1}</span>
                    <span className="req-split-col-title">
                      <input
                        className="form-input"
                        value={r.title}
                        placeholder={t('requirements.split.noTitle')}
                        onChange={(e) => update(i, { title: e.target.value })}
                      />
                    </span>
                    <span className="req-split-col-desc">
                      <textarea
                        className="form-input"
                        rows={2}
                        value={r.description}
                        onChange={(e) => update(i, { description: e.target.value })}
                      />
                      <textarea
                        className="form-input"
                        rows={2}
                        placeholder={t('requirements.split.acceptancePlaceholder')}
                        value={r.acceptance.join('\n')}
                        onChange={(e) => update(i, { acceptance: lines(e.target.value) })}
                      />
                    </span>
                    <span className="req-split-col-dep">
                      {possibleDeps(i).length === 0 ? (
                        <span className="req-split-muted">{t('requirements.split.dependsNone')}</span>
                      ) : (
                        possibleDeps(i).map((d) => (
                          <label key={d} className="req-split-check">
                            <input
                              type="checkbox"
                              checked={r.dependsOn.includes(d)}
                              onChange={() => toggleDependency(i, d)}
                            />
                            {d}
                          </label>
                        ))
                      )}
                      <span className="req-split-muted req-split-dep-hint">{t('requirements.split.dependsHint')}</span>
                    </span>
                    <span className="req-split-col-act">
                      <button
                        type="button"
                        className="btn btn-sm"
                        onClick={() => move(i, -1)}
                        disabled={i === 0 || busy}
                        aria-label={t('requirements.split.moveUp')}
                        title={t('requirements.split.moveUp')}
                      >
                        ↑
                      </button>
                      <button
                        type="button"
                        className="btn btn-sm"
                        onClick={() => move(i, 1)}
                        disabled={i === rows.length - 1 || busy}
                        aria-label={t('requirements.split.moveDown')}
                        title={t('requirements.split.moveDown')}
                      >
                        ↓
                      </button>
                      <button
                        type="button"
                        className="btn btn-sm"
                        onClick={() => remove(i)}
                        disabled={busy}
                        aria-label={t('requirements.split.remove')}
                        title={t('requirements.split.remove')}
                      >
                        ✕
                      </button>
                    </span>
                  </div>
                ))}
              </div>

              {referencePairs.length > 0 && (
                <div className="req-split-refs">
                  <div className="req-split-refs-title">{t('requirements.split.referencesTitle')}</div>
                  <div className="req-split-refs-hint">{t('requirements.split.referencesHint')}</div>
                  <div className="req-split-refs-grid">
                    {referencePairs.map(([a, b]) => {
                      const on = references.some((p) => p[0] === a && p[1] === b);
                      return (
                        <label key={`${a}-${b}`} className="req-split-check">
                          <input
                            type="checkbox"
                            checked={on}
                            onChange={() => toggleReference(a, b)}
                          />
                          {a}↔{b}
                        </label>
                      );
                    })}
                  </div>
                </div>
              )}

              <div className="req-split-hint req-split-hint-muted">{t('requirements.split.refreshHint')}</div>
            </>
          )}
        </div>

        <div className="modal-actions">
          <button type="button" className="btn btn-sm" onClick={close} disabled={busy}>
            {t('requirements.split.cancel')}
          </button>
          {stage === 'config' && (
            <button type="button" className="btn btn-primary btn-sm" onClick={() => void start()}>
              {t('requirements.split.start')}
            </button>
          )}
          {stage === 'running' && (
            <button type="button" className="btn btn-primary btn-sm" disabled>
              {t('requirements.split.generating')}
            </button>
          )}
          {stage === 'review' && (
            <>
              <button
                type="button"
                className="btn btn-sm"
                onClick={() => setStage('config')}
                disabled={busy}
              >
                {t('requirements.split.regenerate')}
              </button>
              <button type="button" className="btn btn-primary btn-sm" onClick={() => void confirm()} disabled={!canConfirm}>
                {busy
                  ? t('requirements.split.confirming')
                  : t('requirements.split.confirm', { count: rows.length })}
              </button>
            </>
          )}
        </div>
      </div>
    </div>
  );
}

export default RequirementSplitModal;
