import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { testingApi, type TestEnvironment } from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import { errorMessage } from '../utils/errMsg';
import { useAuth } from '../utils/auth';
import TestEnvForm from '../components/testing/TestEnvForm';
import TestPlanList from '../components/testing/TestPlanList';
import TestRunList from '../components/testing/TestRunList';
import './ProjectTesting.css';

interface Props {
  projectId: string;
  project: { id: string; name: string; remote_url?: string } | null;
}

/** One frame's worth of job log, rendered in the shared stream panel. */
interface StreamLine {
  type: string;
  content: string;
}

/**
 * ProjectTesting is the automated-testing tab: an environment bar on top,
 * then the plan list and the run list.
 *
 * Everything expensive (init, planning, generating, running) is a JobStore
 * job that streams over ONE shared SSE route, so `stream` is lifted here and
 * handed to whichever child started the work.
 */
export default function ProjectTesting({ projectId }: Props) {
  const { t } = useTranslation();
  const { hasPermission } = useAuth();
  const canManage = hasPermission('testing.manage');

  const [envs, setEnvs] = useState<TestEnvironment[]>([]);
  const [envsLoading, setEnvsLoading] = useState(true);
  const [envError, setEnvError] = useState('');
  const [selectedEnvId, setSelectedEnvId] = useState('');
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<TestEnvironment | null>(null);

  const [stream, setStream] = useState<{ title: string; lines: StreamLine[] } | null>(null);
  const esRef = useRef<EventStream | null>(null);

  const loadEnvs = useCallback(async () => {
    setEnvsLoading(true);
    try {
      const list = (await testingApi.listEnvs(projectId)) ?? [];
      setEnvs(list);
      setEnvError('');
      // Keep a selection if it still exists; otherwise fall back to the first
      // environment so the tab is usable without an extra click.
      setSelectedEnvId((prev) =>
        prev && list.some((e) => e.id === prev) ? prev : list[0]?.id ?? '',
      );
    } catch (err) {
      setEnvError(errorMessage(err));
    } finally {
      setEnvsLoading(false);
    }
  }, [projectId]);

  useEffect(() => {
    void loadEnvs();
  }, [loadEnvs]);

  // Close the stream on unmount / project switch so a live SSE connection
  // isn't left dangling against the previous project.
  useEffect(() => {
    return () => {
      esRef.current?.close();
      esRef.current = null;
    };
  }, []);

  const selectedEnv = envs.find((e) => e.id === selectedEnvId) ?? null;

  /**
   * Subscribe to a job's SSE stream. `onDone` fires on the terminal frame so
   * the caller can refresh whatever the job produced.
   */
  const subscribe = useCallback(
    (jobId: string, title: string, onDone?: () => void) => {
      esRef.current?.close();
      setStream({ title, lines: [] });
      esRef.current = createEventStream(
        testingApi.streamJobUrl(projectId, jobId),
        (data) => {
          if (data.type === 'job_done') {
            esRef.current?.close();
            esRef.current = null;
            setStream((prev) =>
              prev
                ? {
                    ...prev,
                    lines: [
                      ...prev.lines,
                      {
                        type: data.status === 'done' ? 'done' : 'error',
                        content:
                          data.status === 'done'
                            ? t('testing.close')
                            : `${t('testing.runFailed')} (${data.exit_code ?? '?'})`,
                      },
                    ],
                  }
                : prev,
            );
            onDone?.();
            void loadEnvs();
            return;
          }
          if (data.content) {
            setStream((prev) =>
              prev
                ? { ...prev, lines: [...prev.lines, { type: data.type, content: data.content }] }
                : prev,
            );
          }
        },
        () => {
          setStream((prev) =>
            prev
              ? { ...prev, lines: [...prev.lines, { type: 'error', content: t('testing.sseInterrupted') }] }
              : prev,
          );
          esRef.current = null;
          void loadEnvs();
        },
      );
    },
    [projectId, t, loadEnvs],
  );

  const dismissStream = useCallback(() => {
    esRef.current?.close();
    esRef.current = null;
    setStream(null);
  }, []);

  const handleEnvSaved = useCallback(() => {
    setFormOpen(false);
    setEditing(null);
    void loadEnvs();
  }, [loadEnvs]);

  const handleEnvDeleted = useCallback(() => {
    setFormOpen(false);
    setEditing(null);
    void loadEnvs();
  }, [loadEnvs]);

  return (
    <div className="tp-root">
      {/* ── environment bar ── */}
      <div className="tp-env-bar">
        <div className="tp-env-left">
          <span className="tp-label">{t('testing.envTitle')}</span>
          {envsLoading ? (
            <span className="tp-muted">{t('testing.loading')}</span>
          ) : envs.length === 0 ? (
            <span className="tp-muted">{t('testing.envEmpty')}</span>
          ) : (
            <>
              <select
                className="form-input tp-env-select"
                value={selectedEnvId}
                onChange={(e) => setSelectedEnvId(e.target.value)}
              >
                {envs.map((e) => (
                  <option key={e.id} value={e.id}>
                    {e.name}
                  </option>
                ))}
              </select>
              {selectedEnv && (
                <InitStatusBadge env={selectedEnv} />
              )}
            </>
          )}
        </div>
        {canManage && (
          <div className="tp-env-actions">
            {selectedEnv && (
              <>
                <button className="btn btn-sm" onClick={() => { setEditing(selectedEnv); setFormOpen(true); }}>
                  {t('testing.edit')}
                </button>
                <button className="btn btn-sm btn-danger" onClick={() => { setEditing(selectedEnv); setFormOpen(true); }}>
                  {t('testing.delete')}
                </button>
              </>
            )}
            <button className="btn btn-sm btn-primary" onClick={() => { setEditing(null); setFormOpen(true); }}>
              {t('testing.envCreate')}
            </button>
          </div>
        )}
      </div>
      {envError && <div className="tp-error">{envError}</div>}

      {formOpen && (
        <TestEnvForm
          projectId={projectId}
          env={editing}
          onClose={() => { setFormOpen(false); setEditing(null); }}
          onSaved={handleEnvSaved}
          onDeleted={handleEnvDeleted}
          onStartInit={(jobId, envId) => {
            // Attach to the init stream and follow the badge as it moves.
            subscribe(jobId, t('testing.initLogTitle'), () => void loadEnvs());
            setSelectedEnvId(envId);
          }}
        />
      )}

      {/* ── job stream ── */}
      {stream && (
        <div className="tp-stream">
          <div className="tp-stream-head">
            <span>{stream.title}</span>
            <button className="btn btn-sm" onClick={dismissStream}>
              {t('testing.close')}
            </button>
          </div>
          <div className="tp-stream-body">
            {stream.lines.map((line, i) => (
              <div key={i} className={`tp-stream-line tp-line-${line.type}`}>
                {line.content}
              </div>
            ))}
          </div>
        </div>
      )}

      {/* ── plans + runs ── */}
      {selectedEnv ? (
        <>
          <TestPlanList
            projectId={projectId}
            env={selectedEnv}
            canManage={canManage}
            subscribe={subscribe}
            onChanged={loadEnvs}
          />
          <TestRunList
            projectId={projectId}
            env={selectedEnv}
            canManage={canManage}
            subscribe={subscribe}
          />
        </>
      ) : (
        !envsLoading && <div className="tp-empty">{t('testing.envEmpty')}</div>
      )}
    </div>
  );
}

/** Init status pill. Reuses the global .status-badge palette. */
function InitStatusBadge({ env }: { env: TestEnvironment }) {
  const { t } = useTranslation();
  const map: Record<string, { cls: string; label: string }> = {
    pending: { cls: 'status-pending', label: t('testing.initPending') },
    initializing: { cls: 'status-running', label: t('testing.initInitializing') },
    ready: { cls: 'status-succeeded', label: t('testing.initReady') },
    failed: { cls: 'status-failed', label: t('testing.initFailed') },
  };
  const s = map[env.init_status] ?? map.pending;
  return (
    <span
      className={`status-badge ${s.cls}`}
      title={env.init_status === 'failed' ? env.init_error : undefined}
    >
      {s.label}
    </span>
  );
}
