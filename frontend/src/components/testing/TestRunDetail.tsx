import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  authedFetch,
  testingApi,
  type TestAttachment,
  type TestRun,
  type TestRunCase,
} from '../../api/client';
import { errorMessage } from '../../utils/errMsg';

interface Props {
  runId: string;
  canManage: boolean;
  onChanged: () => void;
}

const STATUS_CLASS: Record<string, string> = {
  passed: 'status-succeeded',
  failed: 'status-failed',
  flaky: 'status-warning',
  skipped: 'status-pending',
  timedOut: 'status-failed',
  interrupted: 'status-error',
};

/** Render the status chip label; "timedOut" has no naive capitalization. */
function caseLabel(t: (k: string) => string, status: string): string {
  switch (status) {
    case 'timedOut': return t('testing.runTimedOut');
    case 'interrupted': return t('testing.runInterrupted');
    case 'flaky': return t('testing.runFlaky');
    case 'skipped': return t('testing.runSkipped');
    case 'failed': return t('testing.runFailed');
    default: return t('testing.runPassed');
  }
}

/**
 * useProtectedImage fetches an artifact through the authenticated API and
 * returns an object URL.
 *
 * Artifacts live behind the session-authenticated /api/testing/runs/... route,
 * so a plain <img src> would send no Authorization header and render broken.
 * The blob URL is revoked on unmount / src change to avoid leaking.
 */
function useProtectedImage(url: string | null): string | undefined {
  const [blobUrl, setBlobUrl] = useState<string | undefined>();
  const blobRef = useRef<string | undefined>(undefined);

  useEffect(() => {
    if (!url) return;
    let cancelled = false;
    authedFetch(url)
      .then((r) => (r.ok ? r.blob() : Promise.reject(new Error(String(r.status)))))
      .then((blob) => {
        if (cancelled) return;
        if (blobRef.current) URL.revokeObjectURL(blobRef.current);
        const next = URL.createObjectURL(blob);
        blobRef.current = next;
        setBlobUrl(next);
      })
      .catch(() => {
        // A missing artifact renders as a broken image, which is the honest
        // outcome — the run row itself still carries the failure detail.
      });
    return () => {
      cancelled = true;
      if (blobRef.current) {
        URL.revokeObjectURL(blobRef.current);
        blobRef.current = undefined;
      }
    };
  }, [url]);

  return blobUrl;
}

/** One evidence thumbnail; click opens the full-size lightbox. */
function ArtifactThumb({ runId, att, onOpen }: { runId: string; att: TestAttachment; onOpen: () => void }) {
  const { t } = useTranslation();
  const url = testingApi.artifactUrl(runId, att.path);
  const blob = useProtectedImage(att.content_type.startsWith('image/') ? url : null);

  const isTrace = att.name === 'trace' || att.path.endsWith('.zip');
  if (isTrace) {
    return (
      <a className="btn btn-sm" href={url} download title={t('testing.artifactTraceHint')}>
        {t('testing.artifactTrace')}
      </a>
    );
  }
  if (!blob) {
    return <span className="tp-muted">{att.name}</span>;
  }
  return <img className="tp-shot" src={blob} alt={att.name} onClick={onOpen} />;
}

/** Full-size overlay for a screenshot. */
function Lightbox({ url, onClose }: { url: string; onClose: () => void }) {
  const { t } = useTranslation();
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose]);
  return (
    <div className="tp-lightbox" onClick={onClose}>
      <button className="tp-lightbox-close" onClick={onClose} aria-label={t('testing.close')}>×</button>
      <img src={url} onClick={(e) => e.stopPropagation()} />
    </div>
  );
}

/** One case row: status, duration, expandable error, evidence, close-loop actions. */
function CaseRow({ c, runId, canManage, onEscalated }: {
  c: TestRunCase;
  runId: string;
  canManage: boolean;
  onEscalated: () => void;
}) {
  const { t } = useTranslation();
  const [showError, setShowError] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [lightbox, setLightbox] = useState<string | null>(null);

  const failing = c.status !== 'passed' && c.status !== 'skipped';

  const createIssue = async () => {
    setBusy(true); setError('');
    try { await testingApi.createCaseIssue(c.id); onEscalated(); }
    catch (err) { setError(errorMessage(err)); }
    finally { setBusy(false); }
  };

  const createRequirement = async () => {
    setBusy(true); setError('');
    try { await testingApi.createCaseRequirement(c.id); onEscalated(); }
    catch (err) { setError(errorMessage(err)); }
    finally { setBusy(false); }
  };

  return (
    <>
      <tr>
        <td>
          <span className={`status-badge ${STATUS_CLASS[c.status] ?? 'status-pending'}`}>
            {caseLabel(t, c.status)}
          </span>
        </td>
        <td>
          {c.title}
          {c.file && <div className="tp-row-sub">{c.file}</div>}
          {error && <div className="tp-error" style={{ marginTop: 4 }}>{error}</div>}
        </td>
        <td>{(c.duration_ms / 1000).toFixed(2)}s</td>
        <td style={{ maxWidth: 260 }}>
          {c.error && (
            <>
              <button className="btn btn-sm" onClick={() => setShowError((v) => !v)}>
                {showError ? t('testing.cancel') : t('testing.retry')}
              </button>
              {showError && <pre className="tp-case-error">{c.error}</pre>}
            </>
          )}
          {c.attachments.length > 0 && (
            <div className="tp-attachments">
              {c.attachments.map((att) => (
                <ArtifactThumb
                  key={att.path}
                  runId={runId}
                  att={att}
                  onOpen={() => {
                    const url = testingApi.artifactUrl(runId, att.path);
                    if (att.content_type.startsWith('image/')) setLightbox(url);
                  }}
                />
              ))}
            </div>
          )}
        </td>
        <td>
          {failing && canManage && (
            <div className="tp-row-actions">
              {c.issue_url ? (
                <a className="btn btn-sm" href={c.issue_url} target="_blank" rel="noreferrer">
                  #{c.issue_number}
                </a>
              ) : (
                <button className="btn btn-sm" disabled={busy} onClick={createIssue}>
                  {t('testing.createIssue')}
                </button>
              )}
              {c.requirement_id ? (
                <a className="btn btn-sm" href={`/requirements/${c.requirement_id}`}>
                  {t('testing.requirementCreated')}
                </a>
              ) : (
                <button className="btn btn-sm" disabled={busy} onClick={createRequirement}>
                  {t('testing.createRequirement')}
                </button>
              )}
            </div>
          )}
        </td>
      </tr>
      {lightbox && <Lightbox url={lightbox} onClose={() => setLightbox(null)} />}
    </>
  );
}

/**
 * TestRunDetail renders one run's per-case breakdown plus the evidence
 * gallery and the failure→Issue/requirement close-loop actions.
 */
export default function TestRunDetail({ runId, canManage, onChanged }: Props) {
  const { t } = useTranslation();
  const [run, setRun] = useState<TestRun | null>(null);
  const [cases, setCases] = useState<TestRunCase[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');

  const load = useCallback(async () => {
    try {
      const res = await testingApi.getRun(runId);
      setRun(res.run);
      setCases(res.cases ?? []);
      setError('');
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setLoading(false);
    }
  }, [runId]);

  useEffect(() => {
    void load();
  }, [load]);

  if (loading) return <div className="tp-plan-body tp-muted">{t('testing.loading')}</div>;
  if (error) return <div className="tp-plan-body"><div className="tp-error">{error}</div></div>;
  if (!run) return null;

  return (
    <div className="tp-plan-body">
      <table className="tp-cases">
        <thead>
          <tr>
            <th style={{ width: 90 }}>{t('testing.runCases')}</th>
            <th>{t('testing.planTitleLabel')}</th>
            <th style={{ width: 70 }}>{t('testing.runDuration')}</th>
            <th style={{ width: 260 }}>{t('testing.artifactTitle')}</th>
            <th style={{ width: 180 }}>{t('testing.close')}</th>
          </tr>
        </thead>
        <tbody>
          {cases.length === 0 ? (
            <tr><td colSpan={5} className="tp-muted">{t('testing.runNoCases')}</td></tr>
          ) : (
            cases.map((c) => (
              <CaseRow key={c.id} c={c} runId={runId} canManage={canManage} onEscalated={() => { void load(); onChanged(); }} />
            ))
          )}
        </tbody>
      </table>
    </div>
  );
}
