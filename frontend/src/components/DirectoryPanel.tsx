// DirectoryPanel — directories the user has authorized Claude to read for one
// requirement. Sits next to AttachmentPanel on the requirement detail page.
//
// The distinction from attachments matters: an attachment is a FILE the user
// uploads, and NovaWorkbench copies its bytes into its own store
// (~/.novaworkbench/attachments/<req_id>/) before handing it to Claude. A
// granted directory stays exactly where it is on disk — the backend passes
// the path to the claude CLI as `--add-dir`, which is what lets a plan-mode
// stage read it. Without that grant, a read of a path outside the session's
// working directory needs a permission decision, and a non-interactive run
// has nobody to make it — the run stalls.
//
// The path is a SERVER-side path (the browser can't hand us a folder), so the
// input accepts an absolute path or a "~/" prefix and the server normalizes
// and validates it (must exist, must be a directory, must not be the
// filesystem root).
//
// Which machine the path belongs to depends on where claude runs: a local
// run resolves it on this host; an Agent-server run resolves it on the agent
// host. The hint line says so, because a local path silently does nothing
// on a remote host.

import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { requirementDirsApi, type RequirementDir } from '../api/client';
import { errorMessage } from '../utils/errMsg';
import './DirectoryPanel.css';

interface Props {
  /** The requirement id (req_*). */
  requirementId: string;
  /** Hide the header — for embedding into a compact form. */
  compact?: boolean;
}

/** The list payload adds an `exists` flag the backend computes per row. */
interface RequirementDirRow extends RequirementDir {
  exists: boolean;
}

export default function DirectoryPanel({ requirementId, compact = false }: Props) {
  const { t } = useTranslation();
  const [items, setItems] = useState<RequirementDirRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [path, setPath] = useState('');
  const [label, setLabel] = useState('');
  const [adding, setAdding] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const rows = await requirementDirsApi.list(requirementId);
      setItems(Array.isArray(rows) ? (rows as RequirementDirRow[]) : []);
    } catch (err) {
      setError(t('requirements.dirs.loadFailed', { msg: errorMessage(err) }));
    } finally {
      setLoading(false);
    }
  }, [requirementId, t]);

  useEffect(() => { void refresh(); }, [refresh]);

  const onAdd = async () => {
    const trimmed = path.trim();
    if (trimmed === '') return;
    setAdding(true);
    setError(null);
    try {
      await requirementDirsApi.add(requirementId, trimmed, label.trim());
      setPath('');
      setLabel('');
      await refresh();
    } catch (err) {
      setError(t('requirements.dirs.addFailed', { msg: errorMessage(err) }));
    } finally {
      setAdding(false);
    }
  };

  const onDelete = async (d: RequirementDirRow) => {
    if (!confirm(t('requirements.dirs.deleteConfirm', { path: d.path }))) return;
    try {
      await requirementDirsApi.delete(requirementId, d.id);
      setItems((cur) => cur.filter((x) => x.id !== d.id));
    } catch (err) {
      setError(t('requirements.dirs.deleteFailed', { msg: errorMessage(err) }));
    }
  };

  return (
    <div className="dir-panel">
      {!compact && (
        <div className="dir-panel__header">
          <div>
            <div className="dir-panel__title">{t('requirements.dirs.sectionTitle')}</div>
            <div className="dir-panel__hint">{t('requirements.dirs.sectionHint')}</div>
          </div>
        </div>
      )}

      {error && (
        <div className="dir-panel__error" role="alert" onClick={() => setError(null)}>
          {error}
        </div>
      )}

      {/* add row */}
      <div className="dir-panel__add">
        <input
          type="text"
          className="dir-panel__input"
          value={path}
          onChange={(e) => setPath(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); void onAdd(); } }}
          placeholder={t('requirements.dirs.pathPlaceholder')}
          spellCheck={false}
          autoComplete="off"
        />
        <input
          type="text"
          className="dir-panel__label-input"
          value={label}
          onChange={(e) => setLabel(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); void onAdd(); } }}
          placeholder={t('requirements.dirs.labelPlaceholder')}
          maxLength={200}
        />
        <button
          type="button"
          className="dir-panel__add-btn"
          onClick={() => void onAdd()}
          disabled={adding || path.trim() === ''}
        >
          {adding ? '…' : t('requirements.dirs.addBtn')}
        </button>
      </div>
      <div className="dir-panel__note">{t('requirements.dirs.hostNote')}</div>

      {/* list */}
      <div className="dir-panel__list">
        {loading && items.length === 0 ? (
          <div className="dir-panel__empty">…</div>
        ) : items.length === 0 ? (
          <div className="dir-panel__empty">{t('requirements.dirs.empty')}</div>
        ) : (
          items.map((d) => (
            <div className="dir-panel__row" key={d.id}>
              <div className="dir-panel__row-main">
                <span className="dir-panel__path" title={d.path}>{d.path}</span>
                {!d.exists && (
                  <span className="dir-panel__missing" title={t('requirements.dirs.missingHint')}>
                    {t('requirements.dirs.missing')}
                  </span>
                )}
              </div>
              {d.label && <div className="dir-panel__label">{d.label}</div>}
              <button
                type="button"
                className="dir-panel__delete"
                onClick={() => void onDelete(d)}
                title={t('requirements.dirs.delete')}
              >
                🗑
              </button>
            </div>
          ))
        )}
      </div>
    </div>
  );
}
