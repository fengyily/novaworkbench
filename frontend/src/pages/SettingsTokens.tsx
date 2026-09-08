import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { platformApi, type PlatformToken } from '../api/client';
import { errorMessage } from '../utils/errMsg';
import './Settings.css';

const platformLabels: Record<string, string> = {
  github: 'GitHub',
  gitlab: 'GitLab',
  gitea: 'Gitea',
};

const platformColors: Record<string, string> = {
  github: '#24292e',
  gitlab: '#FC6D26',
  gitea: '#609926',
};

// One form is reused for both the add and edit modals. The
// `editingId` state is empty in create mode and carries the token id in
// edit mode, which swaps the title / button label and the role of the
// Token input (required PAT vs. optional rotation).
interface FormState {
  name: string;
  platform: string;
  base_url: string;
  token: string;
  git_user_name: string;
  git_user_email: string;
}

const emptyForm: FormState = {
  name: '',
  platform: 'github',
  base_url: '',
  token: '',
  git_user_name: '',
  git_user_email: '',
};

export default function SettingsTokens() {
  const { t } = useTranslation();
  const [tokens, setTokens] = useState<PlatformToken[]>([]);
  const [loading, setLoading] = useState(true);
  const [showModal, setShowModal] = useState(false);
  const [editingId, setEditingId] = useState('');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [deleteId, setDeleteId] = useState('');

  const [form, setForm] = useState<FormState>(emptyForm);

  const reload = async () => {
    try {
      const data = await platformApi.list();
      setTokens(data ?? []);
    } catch {
      setTokens([]);
    }
  };

  useEffect(() => {
    reload().finally(() => setLoading(false));
  }, []);

  const openCreateModal = () => {
    setEditingId('');
    setForm(emptyForm);
    setError('');
    setShowModal(true);
  };

  const openEditModal = (tok: PlatformToken) => {
    setEditingId(tok.id);
    setForm({
      name: tok.name,
      platform: tok.platform,
      base_url: tok.base_url ?? '',
      // The PAT is never echoed back, so leave the rotation field blank —
      // the user only fills it when they want to rotate the secret.
      token: '',
      git_user_name: tok.git_user_name ?? '',
      git_user_email: tok.git_user_email ?? '',
    });
    setError('');
    setShowModal(true);
  };

  const closeModal = () => {
    setShowModal(false);
    setEditingId('');
    setError('');
  };

  const handleSave = async () => {
    if (!form.name) {
      setError(t('settings.tokens.errNameRequired'));
      return;
    }
    if (!editingId && !form.token) {
      // New token rows must carry a PAT; edits can leave it blank to keep
      // the existing secret.
      setError(t('settings.tokens.errTokenRequired'));
      return;
    }
    if (!editingId && (form.platform === 'gitea') && !form.base_url) {
      setError(t('settings.tokens.errGiteaNeedsUrl'));
      return;
    }

    setSaving(true);
    setError('');
    try {
      if (editingId) {
        const updated = await platformApi.update(editingId, {
          name: form.name,
          base_url: form.base_url,
          git_user_name: form.git_user_name,
          git_user_email: form.git_user_email,
          new_token: form.token || undefined,
        });
        setTokens(prev => prev.map(t => (t.id === updated.id ? updated : t)));
      } else {
        const tok = await platformApi.create({
          name: form.name,
          platform: form.platform,
          base_url: form.base_url,
          token: form.token,
          git_user_name: form.git_user_name,
          git_user_email: form.git_user_email,
        });
        setTokens(prev => [tok, ...prev]);
      }
      closeModal();
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  const handleDelete = async (id: string) => {
    setDeleteId(id);
    try {
      await platformApi.delete(id);
      setTokens(prev => prev.filter(t => t.id !== id));
    } catch (err: unknown) {
      alert(err instanceof Error ? err.message : String(err));
    } finally {
      setDeleteId('');
    }
  };

  const needsBaseUrl = !editingId && (form.platform === 'gitea' || form.platform === 'gitlab');
  const isEdit = !!editingId;

  return (
    <div className="settings-section">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">{t('settings.tokens.title')}</h3>
          <p className="settings-section-desc">
            {t('settings.tokens.desc')}
          </p>
        </div>
        <button className="btn btn-primary" onClick={openCreateModal}>{t('settings.tokens.add')}</button>
      </div>

      {loading && <div className="settings-empty">{t('settings.tokens.loading')}</div>}

      {!loading && tokens.length === 0 && (
        <div className="settings-empty">
          <p>{t('settings.tokens.empty')}</p>
        </div>
      )}

      {tokens.length > 0 && (
        <table className="project-table">
          <thead>
            <tr>
              <th>{t('settings.tokens.colName')}</th>
              <th>{t('settings.tokens.colPlatform')}</th>
              <th>Base URL</th>
              <th>{t('settings.tokens.colGitIdentity')}</th>
              <th>{t('settings.tokens.colCreatedAt')}</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {tokens.map(tok => (
              <tr key={tok.id}>
                <td className="project-name">{tok.name}</td>
                <td>
                  <span className="platform-badge" style={{ background: platformColors[tok.platform] ?? '#64748b' }}>
                    {platformLabels[tok.platform] ?? tok.platform}
                  </span>
                </td>
                <td className="path-cell">{tok.base_url || '—'}</td>
                <td className="path-cell">
                  {tok.git_user_name || tok.git_user_email
                    ? `${tok.git_user_name} <${tok.git_user_email}>`
                    : '—'}
                </td>
                <td>{new Date(tok.created_at).toLocaleDateString('zh-CN')}</td>
                <td className="row-actions">
                  <button
                    className="btn-link"
                    onClick={() => openEditModal(tok)}
                  >
                    {t('settings.tokens.edit')}
                  </button>
                  <button
                    className="btn-link btn-danger-link"
                    onClick={() => handleDelete(tok.id)}
                    disabled={deleteId === tok.id}
                  >
                    {deleteId === tok.id ? t('settings.tokens.deleting') : t('settings.tokens.delete')}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {showModal && (
        <div className="modal-overlay" onClick={closeModal}>
          <div className="modal-box" onClick={e => e.stopPropagation()}>
            <h3>{isEdit ? t('settings.tokens.modal.editTitle') : t('settings.tokens.modal.createTitle')}</h3>

            {error && <div className="form-error">{error}</div>}

            <div className="modal-field">
              <label>{t('settings.tokens.modal.nameLabel')}</label>
              <input
                className="form-input"
                placeholder={t('settings.tokens.modal.namePlaceholder')}
                value={form.name}
                onChange={e => setForm(f => ({ ...f, name: e.target.value }))}
              />
            </div>

            {!isEdit && (
              <>
                <div className="modal-field">
                  <label>{t('settings.tokens.modal.platformLabel')}</label>
                  <select
                    className="form-input"
                    value={form.platform}
                    onChange={e => setForm(f => ({ ...f, platform: e.target.value }))}
                  >
                    <option value="github">GitHub</option>
                    <option value="gitlab">GitLab</option>
                    <option value="gitea">{t('settings.tokens.modal.platformGitea')}</option>
                  </select>
                </div>

                {needsBaseUrl && (
                  <div className="modal-field">
                    <label>Base URL</label>
                    <input
                      className="form-input"
                      placeholder={form.platform === 'gitlab' ? t('settings.tokens.modal.baseUrlPlaceholderGitlab') : t('settings.tokens.modal.baseUrlPlaceholderGitea')}
                      value={form.base_url}
                      onChange={e => setForm(f => ({ ...f, base_url: e.target.value }))}
                    />
                  </div>
                )}
              </>
            )}

            {isEdit && form.base_url && (
              <div className="modal-field">
                <label>Base URL</label>
                <input
                  className="form-input"
                  value={form.base_url}
                  onChange={e => setForm(f => ({ ...f, base_url: e.target.value }))}
                />
              </div>
            )}

            <div className="modal-field">
              <label>{isEdit ? t('settings.tokens.modal.tokenLabelNew') : t('settings.tokens.modal.tokenLabel')}</label>
              <input
                className="form-input"
                type="password"
                placeholder={isEdit ? t('settings.tokens.modal.tokenPlaceholderEdit') : t('settings.tokens.modal.tokenPlaceholderNew')}
                value={form.token}
                onChange={e => setForm(f => ({ ...f, token: e.target.value }))}
              />
            </div>

            <div className="modal-field">
              <label>{t('settings.tokens.modal.gitUserLabel')}</label>
              <input
                className="form-input"
                placeholder={t('settings.tokens.modal.gitUserPlaceholder')}
                value={form.git_user_name}
                onChange={e => setForm(f => ({ ...f, git_user_name: e.target.value }))}
              />
            </div>

            <div className="modal-field">
              <label>{t('settings.tokens.modal.gitEmailLabel')}</label>
              <input
                className="form-input"
                placeholder={t('settings.tokens.modal.gitEmailPlaceholder')}
                value={form.git_user_email}
                onChange={e => setForm(f => ({ ...f, git_user_email: e.target.value }))}
              />
              <div className="form-hint">
                {t('settings.tokens.modal.gitIdentityHint')}
              </div>
            </div>

            <div className="modal-actions btn-row-2col">
              <button className="btn" onClick={closeModal}>{t('settings.tokens.modal.cancel')}</button>
              <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
                {saving ? t('settings.tokens.modal.saving') : t('settings.tokens.modal.save')}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
