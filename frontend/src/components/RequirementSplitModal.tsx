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
import { ApiError, requirementsApi, type SplitTaskCandidate } from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import { IconClose } from './icons';
import './RequirementSplitModal.css';

export interface RequirementSplitModalProps {
  open: boolean;
  onClose: () => void;
  requirementId: string;
  requirementTitle: string;
  /** The master's base branch — every child inherits it. */
  baseBranch: string;
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

export function RequirementSplitModal({
  open,
  onClose,
  requirementId,
  requirementTitle,
  baseBranch,
  onConfirmed,
}: RequirementSplitModalProps) {
  const { t } = useTranslation();

  const [rows, setRows] = useState<Row[]>([]);
  const [phase, setPhase] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [running, setRunning] = useState(false);
  /** Candidate pairs the user ticked for mutual reference: [a, b] ordinals. */
  const [references, setReferences] = useState<[number, number][]>([]);

  const streamRef = useRef<EventStream | null>(null);

  const close = useCallback(() => {
    streamRef.current?.close();
    streamRef.current = null;
    setRows([]);
    setPhase('');
    setError('');
    setRunning(false);
    setReferences([]);
    onClose();
  }, [onClose]);

  // Start the split as soon as the modal opens.
  useEffect(() => {
    if (!open) return;
    let cancelled = false;

    const start = async () => {
      setRows([]);
      setPhase(t('requirements.split.generating'));
      setError('');
      setRunning(true);
      setReferences([]);
      try {
        const { job_id } = await requirementsApi.split(requirementId);
        if (cancelled) return;
        const stream = createEventStream(
          requirementsApi.splitStreamUrl(requirementId, job_id),
          (data: any) => {
            if (cancelled) return;
            if (data?.type === 'phase' || data?.type === 'message' || data?.type === 'tool_call') {
              if (data.content) setPhase(String(data.content));
              return;
            }
            if (data?.type === 'job_done') {
              if (data.status === 'error') {
                setError(t('requirements.split.errGeneric', { msg: data.error_kind || '' }));
                setRunning(false);
                return;
              }
              const tasks: RawCandidate[] = Array.isArray(data?.result?.tasks)
                ? data.result.tasks
                : [];
              if (tasks.length === 0) {
                // The candidate list is in-process state — a backend restart
                // (or an eviction from the job ring buffer) loses it.
                setError(t('requirements.split.errExpired'));
                setRunning(false);
                return;
              }
              const next = tasks.map(toRow).sort((a, b) => a.order - b.order);
              setRows(next.map((r, i) => ({ ...r, order: i + 1 })));
              setPhase(t('requirements.split.generated', { count: next.length }));
              setRunning(false);
              return;
            }
          },
          () => {
            if (cancelled) return;
            setError(t('requirements.split.errGeneric', { msg: '' }));
            setRunning(false);
          },
        );
        streamRef.current = stream;
      } catch (err) {
        if (cancelled) return;
        setError(err instanceof ApiError ? err.message : t('requirements.split.errGeneric', { msg: String(err) }));
        setRunning(false);
      }
    };

    void start();
    return () => {
      cancelled = true;
      streamRef.current?.close();
      streamRef.current = null;
    };
  }, [open, requirementId, t]);

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
      const res = await requirementsApi.confirmSplit(requirementId, { tasks, references });
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

  const canConfirm = !busy && !running && rows.length > 0 && rows.every((r) => r.title.trim() !== '');

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
          <div className="req-split-hint">
            {t('requirements.split.baseHint', { base: baseBranch || 'main' })}
          </div>
          <div className="req-split-hint req-split-hint-muted">{t('requirements.split.parallelHint')}</div>

          {running && (
            <div className="req-split-progress">
              <span className="req-spinner" aria-hidden />
              {phase || t('requirements.split.generating')}
            </div>
          )}

          {error && <div className="req-split-error">{error}</div>}

          {!running && rows.length > 0 && (
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
          <button type="button" className="btn btn-primary btn-sm" onClick={() => void confirm()} disabled={!canConfirm}>
            {busy
              ? t('requirements.split.confirming')
              : t('requirements.split.confirm', { count: rows.length })}
          </button>
        </div>
      </div>
    </div>
  );
}

export default RequirementSplitModal;
