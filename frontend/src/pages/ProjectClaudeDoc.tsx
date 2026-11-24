import { useState, useEffect, useCallback, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import {
  projectClaudeDocApi,
  type ProjectClaudeDoc,
  type ProjectClaudeDocRevision,
} from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import MarkdownRender from '../components/MarkdownRender';
import { errorMessage } from '../utils/errMsg';
import { fmtDateTime } from '../utils/intl';
import './ProjectClaudeDoc.css';

// Project-level CLAUDE.md panel.
//
// The document is a MANAGED project asset: it lives in the DB (never in the
// user's working tree), it is shared by every requirement of the project,
// and the backend injects it into the analysis / design / coding / sub-task
// stages. This panel is the only place a user sees or edits it.
//
// Three rules the UI must not break:
//  - a manual edit does NOT change the "last generated" stamp, so the badge
//    keeps showing the machine-written time;
//  - a failed generation must NOT wipe the document that is already there —
//    the old content stays on screen while the error is shown;
//  - a destructive replace (regenerate / import) must NOT silently drop
//    hand-edited content: the user is asked first, and the backend refuses
//    without that confirmation (409 MANUAL_EDIT_PROTECTED). Every such
//    replace snapshots the outgoing version, so it can be restored.
export default function ProjectClaudeDoc({ projectId }: { projectId: string }) {
  const { t } = useTranslation();

  const [loading, setLoading] = useState(true);
  const [doc, setDoc] = useState<ProjectClaudeDoc | null>(null);
  const [error, setError] = useState('');
  const [info, setInfo] = useState('');

  // Editing
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState('');
  const [saving, setSaving] = useState(false);

  // Rendered view vs raw source
  const [rawView, setRawView] = useState(false);

  // Version history — the safety net for the destructive replaces.
  const [revisions, setRevisions] = useState<ProjectClaudeDocRevision[]>([]);
  const [historyOpen, setHistoryOpen] = useState(false);
  const [restoringId, setRestoringId] = useState('');

  // Generation
  const [generating, setGenerating] = useState(false);
  const [focus, setFocus] = useState('');
  const [genLines, setGenLines] = useState<{ type: string; content: string }[]>([]);
  const esRef = useRef<EventStream | null>(null);

  const load = useCallback(() => {
    setLoading(true);
    projectClaudeDocApi.get(projectId)
      .then(data => { setDoc(data); setError(''); })
      .catch(err => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, [projectId]);

  const loadRevisions = useCallback(() => {
    projectClaudeDocApi.listRevisions(projectId)
      .then(data => setRevisions(data ?? []))
      .catch(() => setRevisions([]));
  }, [projectId]);

  // The history is fetched on load too: the button label carries the
  // version count, and it must not read "0" until the user opens the list.
  useEffect(() => { load(); loadRevisions(); }, [load, loadRevisions]);

  // Close the stream on unmount / project switch.
  useEffect(() => () => { esRef.current?.close(); }, []);

  // The document carries hand-edited content → a destructive replace has to
  // ask first. Declared before the handlers so they can read it.
  const hasManualEdits = !!doc?.manual_edited_at;

  const startEdit = () => {
    setDraft(doc?.content ?? '');
    setEditing(true);
    setInfo('');
  };

  const cancelEdit = () => {
    setEditing(false);
    setDraft('');
  };

  const handleSave = async () => {
    if (!draft.trim()) return;
    setSaving(true);
    setError('');
    try {
      await projectClaudeDocApi.save(projectId, draft);
      setEditing(false);
      setInfo(t('claudeDoc.savedOk'));
      load();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  const handleRemove = async () => {
    if (!window.confirm(t('claudeDoc.removeConfirm'))) return;
    setError('');
    try {
      await projectClaudeDocApi.remove(projectId);
      setEditing(false);
      setInfo('');
      load();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const handleImport = async (overwriteManual = false) => {
    setError('');
    try {
      await projectClaudeDocApi.importFromDisk(projectId, overwriteManual);
      setInfo(t('claudeDoc.savedOk'));
      load();
      loadRevisions();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  // A hand-edited document is the one thing here a human wrote on purpose.
  // Replacing it wholesale needs their explicit yes — the backend refuses
  // without it, and the old version is snapshotted so the yes is undoable.
  const confirmOverwrite = (): boolean =>
    !hasManualEdits || window.confirm(t('claudeDoc.overwriteManualConfirm'));

  const handleRestore = async (revId: string) => {
    if (!window.confirm(t('claudeDoc.restoreConfirm'))) return;
    setRestoringId(revId);
    setError('');
    try {
      await projectClaudeDocApi.restoreRevision(projectId, revId);
      setInfo(t('claudeDoc.restoredOk'));
      load();
      loadRevisions();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setRestoringId('');
    }
  };

  const handleGenerate = async (overwriteManual = false) => {
    setError('');
    setInfo('');
    setGenLines([]);
    setGenerating(true);
    try {
      const { job_id } = await projectClaudeDocApi.generate(projectId, focus, overwriteManual);
      esRef.current = createEventStream(
        projectClaudeDocApi.streamJobUrl(projectId, job_id),
        (data) => {
          if (data.type === 'job_done') {
            setGenerating(false);
            // Close explicitly: the stream also ends naturally, and the
            // natural end would fire onError and overwrite the terminal
            // state with a bogus error.
            esRef.current?.close();
            esRef.current = null;
            if (data.status === 'done') {
              setInfo(t('claudeDoc.savedOk'));
              load();
              loadRevisions();
            } else {
              setError(t('claudeDoc.generateFailed'));
            }
          } else if (data.content) {
            setGenLines(prev => [...prev, { type: data.type, content: data.content }]);
          }
        },
        () => {
          setGenerating(false);
          setGenLines(prev => [...prev, { type: 'error', content: t('claudeDoc.generateFailed') }]);
        },
      );
    } catch (err) {
      setGenerating(false);
      setError(errorMessage(err));
    }
  };

  if (loading) {
    return <div className="tab-empty"><p>{t('claudeDoc.loading')}</p></div>;
  }

  const exists = !!doc?.exists;
  const content = doc?.content ?? '';

  // The disk-vs-managed hint has three states (see the render block below).
  // The divergent one is the important one: the user edited the file on
  // disk, the managed copy stayed behind, and every later stage kept
  // injecting the stale conventions — invisible from the panel otherwise.
  // The disk mtime is a separate fragment so the sentence stays intact when
  // the timestamp is unavailable (an omitted field would otherwise leave a
  // dangling empty parenthesis in the middle of the hint).
  const diskAt = doc?.disk?.modified_at
    ? t('claudeDoc.diskModifiedAt', { time: fmtDateTime(doc.disk.modified_at) })
    : '';
  const diskHint = !doc
    ? ''
    : doc.disk?.exists
      ? (doc.disk.differs
          ? t('claudeDoc.diskDiffers', { at: diskAt })
          : t('claudeDoc.diskExists'))
      : (exists ? t('claudeDoc.diskMissing') : '');

  return (
    <div className="claude-doc-panel">
      {error && <div className="claude-doc-error">{error}</div>}
      {info && !error && <div className="claude-doc-info">{info}</div>}

      <div className="claude-doc-header">
        <h3 className="claude-doc-title">{t('claudeDoc.title')}</h3>
        {exists && (
          <div className="claude-doc-badges">
            <span className="status-badge">{sourceLabel(t, doc?.source)}</span>
            <span className="status-badge">
              {doc?.generated_at
                ? t('claudeDoc.lastGenerated', { time: fmtDateTime(doc.generated_at) })
                : t('claudeDoc.neverGenerated')}
            </span>
            <span className="status-badge">{t('claudeDoc.sizeLabel', { n: doc?.bytes ?? 0 })}</span>
            {/* Which channel the document travels through. The user can act
                on it: in native mode the repository's own CLAUDE.md is kept
                out of the context, so an edit there has no effect until it
                is imported — a fact the panel has to state. */}
            <span
              className="status-badge claude-doc-badge-channel"
              title={t('claudeDoc.injectionChannelLabel')}
            >
              {doc?.injection_channel === 'prompt'
                ? t('claudeDoc.injectionChannelPrompt')
                : t('claudeDoc.injectionChannelNative')}
            </span>
            {hasManualEdits && (
              <span className="status-badge claude-doc-badge-manual">
                {t('claudeDoc.manualEdited')}
              </span>
            )}
          </div>
        )}
      </div>

      {/* Disk hint — the CLI auto-loads a CLAUDE.md that sits in the project
          directory, so the user has to know that there are two documents.

          Three states, because "the disk copy and the managed copy drifted
          apart" is the one the user cannot see on their own: they edited
          the file on disk, the managed copy stayed as it was, and every
          later stage kept injecting the stale conventions. The reverse
          state (a managed copy but no file on disk) gets its own line so
          the panel never implies a disk file that isn't there. */}
      {diskHint && (
        <div className={`claude-doc-disk${doc?.disk?.differs ? ' claude-doc-disk-changed' : ''}`}>
          <span>{diskHint}</span>
          {doc?.disk?.exists && (
            <button
              className="btn btn-sm"
              onClick={() => { if (confirmOverwrite()) handleImport(hasManualEdits); }}
            >
              {t('claudeDoc.importFromDisk')}
            </button>
          )}
        </div>
      )}

      {/* Single-source hint — only meaningful in native mode, where the
          repository's own copy is excluded from the CLI's load set. */}
      {exists && doc?.injection_channel !== 'prompt' && (
        <div className="claude-doc-disk">
          <span>{t('claudeDoc.singleSourceHint')}</span>
        </div>
      )}

      {!exists && !generating && (
        <div className="claude-doc-empty">
          <p>{t('claudeDoc.emptyTitle')}</p>
          <p className="claude-doc-empty-hint">{t('claudeDoc.emptyHint')}</p>
          <button className="btn btn-primary" onClick={() => handleGenerate(false)}>
            {t('claudeDoc.generate')}
          </button>
        </div>
      )}

      {generating && (
        <div className="claude-doc-progress">
          <div className="claude-doc-progress-head">
            <span>{t('claudeDoc.generating')}</span>
            <button className="btn btn-sm" onClick={() => setGenLines([])}>{t('claudeDoc.clearLog')}</button>
          </div>
          <div className="claude-doc-log">
            {genLines.length === 0 && <div className="claude-doc-log-line">{t('claudeDoc.loading')}</div>}
            {genLines.map((l, i) => (
              <div key={i} className={`claude-doc-log-line claude-doc-log-${l.type}`}>{l.content}</div>
            ))}
          </div>
        </div>
      )}

      {exists && !editing && (
        <div className="claude-doc-body">
          {rawView ? (
            <pre className="claude-doc-source">{content}</pre>
          ) : (
            <div className="md-prose">
              <MarkdownRender content={content} />
            </div>
          )}
        </div>
      )}

      {editing && (
        <div className="claude-doc-editor">
          <textarea
            className="form-input claude-doc-textarea"
            value={draft}
            onChange={e => setDraft(e.target.value)}
            spellCheck={false}
          />
          <div className="claude-doc-edit-hint">{t('claudeDoc.editHint')}</div>
        </div>
      )}

      {!editing && (
        <div className="claude-doc-focus">
          <label className="claude-doc-focus-label">{t('claudeDoc.focusLabel')}</label>
          <input
            className="form-input"
            value={focus}
            onChange={e => setFocus(e.target.value)}
            placeholder={t('claudeDoc.focusPlaceholder')}
            disabled={generating}
          />
        </div>
      )}

      {/* Version history — the recovery path for the destructive replaces.
          Every regeneration / import snapshots the document it replaced,
          so a hand-edited version is always one click away. */}
      {(exists || revisions.length > 0) && (
        <div className="claude-doc-history">
          <button
            className="btn btn-sm"
            onClick={() => setHistoryOpen(v => !v)}
            aria-expanded={historyOpen}
          >
            {historyOpen ? t('claudeDoc.historyHide') : t('claudeDoc.historyShow', { n: revisions.length })}
          </button>
          {historyOpen && (
            <div className="claude-doc-history-list">
              {revisions.length === 0 && (
                <div className="claude-doc-history-empty">{t('claudeDoc.historyEmpty')}</div>
              )}
              {revisions.map(rev => (
                <div key={rev.id} className="claude-doc-history-row">
                  <div className="claude-doc-history-meta">
                    <span className="claude-doc-history-time">{fmtDateTime(rev.created_at)}</span>
                    <span className="claude-doc-history-reason">{revisionReasonLabel(t, rev.reason)}</span>
                    <span className="claude-doc-history-size">{t('claudeDoc.sizeLabel', { n: rev.bytes })}</span>
                  </div>
                  <button
                    className="btn btn-sm"
                    disabled={restoringId === rev.id}
                    onClick={() => handleRestore(rev.id)}
                  >
                    {t('claudeDoc.historyRestore')}
                  </button>
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      <div className="claude-doc-actions">
        {editing ? (
          <>
            <button className="btn btn-primary" onClick={handleSave} disabled={saving || !draft.trim()}>
              {saving ? t('claudeDoc.saving') : t('claudeDoc.save')}
            </button>
            <button className="btn" onClick={cancelEdit} disabled={saving}>{t('claudeDoc.cancel')}</button>
          </>
        ) : (
          <>
            {/* The empty state owns the "generate" CTA; once a document
                exists the same slot becomes "regenerate". */}
            {exists && (
              <>
                <button
                  className="btn btn-primary"
                  disabled={generating}
                  onClick={() => { if (confirmOverwrite()) handleGenerate(hasManualEdits); }}
                >
                  {t('claudeDoc.regenerate')}
                </button>
                <button className="btn" onClick={startEdit}>{t('claudeDoc.edit')}</button>
                <button className="btn" onClick={() => setRawView(v => !v)}>
                  {rawView ? t('claudeDoc.viewRendered') : t('claudeDoc.viewSource')}
                </button>
                <button className="btn btn-danger" onClick={handleRemove}>{t('claudeDoc.remove')}</button>
              </>
            )}
          </>
        )}
      </div>
    </div>
  );
}

// revisionReasonLabel maps the backend's snapshot reason to a display label.
function revisionReasonLabel(t: (k: string) => string, reason: string): string {
  switch (reason) {
    case 'regenerate': return t('claudeDoc.reasonRegenerate');
    case 'import': return t('claudeDoc.reasonImport');
    case 'restore': return t('claudeDoc.reasonRestore');
    default: return '—';
  }
}

// sourceLabel maps the backend's source literal to a display label.
function sourceLabel(t: (k: string) => string, source?: string): string {
  switch (source) {
    case 'generated': return t('claudeDoc.sourceGenerated');
    case 'manual': return t('claudeDoc.sourceManual');
    case 'imported': return t('claudeDoc.sourceImported');
    default: return '—';
  }
}
