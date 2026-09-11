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
//
// The GPG fields mirror the PAT semantics: the server never echoes the
// ciphertext back, so we keep them empty in edit mode unless the user
// types something new — sending a non-empty value means "rotate". An
// explicit `gpg_enabled` flag captures the "un-check to wipe" intent that
// otherwise would be ambiguous (does empty = "keep" or "off"?).
interface FormState {
  name: string;
  platform: string;
  base_url: string;
  token: string;
  git_user_name: string;
  git_user_email: string;
  gpg_enabled: boolean;
  gpg_private_key: string;
  gpg_passphrase: string;
}

const emptyForm: FormState = {
  name: '',
  platform: 'github',
  base_url: '',
  token: '',
  git_user_name: '',
  git_user_email: '',
  gpg_enabled: false,
  gpg_private_key: '',
  gpg_passphrase: '',
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
  // Tracks the gpg_enabled value the row had when the modal opened. We need
  // it to disambiguate "the user re-saved with the toggle still on" (no
  // clear, no rotate) from "the user turned it off" (clear the stored
  // ciphertext).
  const [originalGpgEnabled, setOriginalGpgEnabled] = useState(false);

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
    setOriginalGpgEnabled(false);
    setForm(emptyForm);
    setError('');
    setShowModal(true);
  };

  const openEditModal = (tok: PlatformToken) => {
    setEditingId(tok.id);
    // Snapshot whether the row had GPG enabled BEFORE the user touched the
    // checkbox, so handleSave can detect "was on, now off" → clear_gpg.
    setOriginalGpgEnabled(!!tok.gpg_enabled);
    setForm({
      name: tok.name,
      platform: tok.platform,
      base_url: tok.base_url ?? '',
      // The PAT is never echoed back, so leave the rotation field blank —
      // the user only fills it when they want to rotate the secret.
      token: '',
      git_user_name: tok.git_user_name ?? '',
      git_user_email: tok.git_user_email ?? '',
      // Same secrecy story for the GPG material — we only get back the key
      // id, never the ciphertext. Editing the key requires re-uploading.
      gpg_enabled: !!tok.gpg_enabled,
      gpg_private_key: '',
      gpg_passphrase: '',
    });
    setError('');
    setShowModal(true);
  };

  const closeModal = () => {
    setShowModal(false);
    setEditingId('');
    setOriginalGpgEnabled(false);
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
    // GPG validation: enabling requires a key on create (server stores the
    // ciphertext on the same INSERT). On edit, a non-empty field means
    // "rotate"; empty means "keep whatever's stored".
    const enablingGpg = !originalGpgEnabled && form.gpg_enabled;
    const disablingGpg = originalGpgEnabled && !form.gpg_enabled;
    if (enablingGpg && !form.gpg_private_key.trim()) {
      setError(t('settings.tokens.errGpgKeyRequired'));
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
          gpg_enabled: form.gpg_enabled,
          gpg_private_key: form.gpg_private_key || undefined,
          gpg_passphrase: form.gpg_passphrase || undefined,
          // Wipe stored ciphertext + key id + flag when the user turns the
          // toggle off on an already-enabled row. Disabling on a row that
          // never had GPG on is a no-op — clear_gpg=false (the default).
          clear_gpg: disablingGpg || undefined,
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
          gpg_enabled: form.gpg_enabled,
          gpg_private_key: form.gpg_private_key || undefined,
          gpg_passphrase: form.gpg_passphrase || undefined,
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
  // Derive the token under edit from the live list so the modal can show its
  // current key id next to the rotation textarea. We avoid mirroring it in
  // form state — that field is read-only and the form-state copy would be
  // stale the moment a sibling update hits the list.
  const editingToken = isEdit ? tokens.find(tk => tk.id === editingId) : undefined;

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
              <th>{t('settings.tokens.colGpg')}</th>
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
                <td className="path-cell">
                  {tok.gpg_enabled ? (
                    tok.gpg_key_id
                      ? <span className="gpg-badge">{t('settings.tokens.gpgBadgeEnabled', { keyId: tok.gpg_key_id.slice(-8) })}</span>
                      : <span className="gpg-badge gpg-badge--pending">{t('settings.tokens.gpgBadgePending')}</span>
                  ) : '—'}
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

            {/* GPG signing block. The banner always shows so the AES-256-GCM
                warning is impossible to miss; the checkbox + fields appear
                below it. Editing an already-enabled row keeps the key
                textarea visible so the user can rotate the key without
                having to un-check + re-check the box. */}
            <div className="security-banner">
              {t('settings.tokens.modal.gpgSecurityHint')}
            </div>

            <div className="modal-field">
              <label className="checkbox-label">
                <input
                  type="checkbox"
                  checked={form.gpg_enabled}
                  onChange={e => setForm(f => ({ ...f, gpg_enabled: e.target.checked }))}
                />
                {' '}{t('settings.tokens.modal.gpgEnableLabel')}
              </label>
              <div className="form-hint">
                {t('settings.tokens.modal.gpgHint')}
              </div>
            </div>

            {(form.gpg_enabled || originalGpgEnabled) && (
              <>
                <div className="modal-field">
                  <label>
                    {t('settings.tokens.modal.gpgKeyLabel')}
                    {isEdit && <span className="hint">{t('settings.agentServersPage.keyKeepHint')}</span>}
                  </label>
                  <textarea
                    className="form-input gpg-key-input"
                    rows={6}
                    placeholder={t('settings.tokens.modal.gpgKeyPlaceholder')}
                    value={form.gpg_private_key}
                    onChange={e => setForm(f => ({ ...f, gpg_private_key: e.target.value }))}
                  />
                </div>

                <div className="modal-field">
                  <label>
                    {t('settings.tokens.modal.gpgPassphraseLabel')}
                    {isEdit && <span className="hint">{t('settings.agentServersPage.keyKeepHint')}</span>}
                  </label>
                  <input
                    className="form-input"
                    type="password"
                    placeholder={t('settings.tokens.modal.gpgPassphrasePlaceholder')}
                    value={form.gpg_passphrase}
                    onChange={e => setForm(f => ({ ...f, gpg_passphrase: e.target.value }))}
                  />
                </div>

                {isEdit && editingToken?.gpg_key_id && (
                  <div className="modal-field">
                    <label>{t('settings.tokens.modal.gpgKeyIdLabel')}</label>
                    <input
                      className="form-input"
                      readOnly
                      value={editingToken.gpg_key_id}
                    />
                  </div>
                )}

                {isEdit && originalGpgEnabled && !form.gpg_enabled && (
                  <div className="form-hint">
                    {t('settings.tokens.modal.gpgClearHint')}
                  </div>
                )}
              </>
            )}

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
