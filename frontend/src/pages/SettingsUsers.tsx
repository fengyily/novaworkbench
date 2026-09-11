import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { aclApi, projectsApi, type User, type ACLRole, type Project } from '../api/client';
import { errorMessage } from '../utils/errMsg';
import { fmtDateTime } from '../utils/intl';

interface UserForm {
  username: string;
  display_name: string;
  password: string;
  is_admin: boolean;
  status: string;
  role_ids: string[];
  project_ids: string[];
}

const emptyForm: UserForm = {
  username: '',
  display_name: '',
  password: '',
  is_admin: false,
  status: 'active',
  role_ids: [],
  project_ids: [],
};

export default function SettingsUsers() {
  const { t } = useTranslation();
  const [users, setUsers] = useState<User[]>([]);
  const [roles, setRoles] = useState<ACLRole[]>([]);
  const [projects, setProjects] = useState<Project[]>([]);
  const [loading, setLoading] = useState(true);
  const [showModal, setShowModal] = useState(false);
  const [editingId, setEditingId] = useState('');
  const [saving, setSaving] = useState(false);
  const [busyId, setBusyId] = useState('');
  const [error, setError] = useState('');
  const [form, setForm] = useState<UserForm>(emptyForm);

  const load = async () => {
    setLoading(true);
    try {
      const [u, r, p] = await Promise.all([aclApi.listUsers(), aclApi.listRoles(), projectsApi.list()]);
      setUsers(u ?? []);
      setRoles(r ?? []);
      setProjects(p ?? []);
    } catch {
      setUsers([]);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    load();
  }, []);

  const openCreate = () => {
    setEditingId('');
    setForm(emptyForm);
    setError('');
    setShowModal(true);
  };

  const openEdit = (u: User) => {
    setEditingId(u.id);
    setForm({
      username: u.username,
      display_name: u.display_name,
      password: '',
      is_admin: u.is_admin,
      status: u.status,
      role_ids: u.role_ids ?? [],
      project_ids: u.project_ids ?? [],
    });
    setError('');
    setShowModal(true);
  };

  const toggle = (key: 'role_ids' | 'project_ids', id: string) => {
    setForm((f) => {
      const set = new Set(f[key]);
      if (set.has(id)) set.delete(id);
      else set.add(id);
      return { ...f, [key]: Array.from(set) };
    });
  };

  const handleSave = async () => {
    if (!form.username || (!editingId && !form.password)) {
      setError(t('settings.users.errMissing'))
      return;
    }
    setSaving(true);
    setError('');
    try {
      if (editingId) {
        await aclApi.updateUser(editingId, {
          display_name: form.display_name,
          password: form.password,
          status: form.status,
          is_admin: form.is_admin,
          role_ids: form.role_ids,
          project_ids: form.project_ids,
        });
      } else {
        await aclApi.createUser({
          username: form.username,
          password: form.password,
          display_name: form.display_name,
          status: form.status,
          is_admin: form.is_admin,
          role_ids: form.role_ids,
          project_ids: form.project_ids,
        });
      }
      setShowModal(false);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const handleDelete = async (id: string) => {
    if (!confirm(t('settings.users.deleteConfirm'))) return;
    setBusyId(id);
    try {
      await aclApi.deleteUser(id);
      await load();
    } catch (err) {
      alert(errorMessage(err))
    } finally {
      setBusyId('');
    }
  };

  return (
    <div className="settings-section">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">{t('settings.users.title')}</h3>
          <p className="settings-section-desc">
            {t('settings.users.desc')}
          </p>
        </div>
        <button className="btn btn-primary" onClick={openCreate}>{t('settings.users.addButton')}</button>
      </div>

      {loading && <div className="settings-empty">{t('settings.users.loading')}</div>}

      {!loading && users.length === 0 && (
        <div className="settings-empty"><p>{t('settings.users.empty')}</p></div>
      )}

      {users.length > 0 && (
        <table className="project-table">
          <thead>
            <tr>
              <th>{t('settings.users.table.username')}</th>
              <th>{t('settings.users.table.displayName')}</th>
              <th>{t('settings.users.table.roles')}</th>
              <th>{t('settings.users.table.projectCount')}</th>
              <th>{t('settings.users.table.status')}</th>
              <th>{t('settings.users.table.lastLogin')}</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {users.map((u) => {
              const roleNames = (u.role_ids ?? [])
                .map((rid) => roles.find((r) => r.id === rid)?.name)
                .filter(Boolean) as string[];
              return (
                <tr key={u.id}>
                  <td className="project-name">
                    {u.username}
                    {u.is_admin && <span className="admin-badge">{t('settings.users.adminBadge')}</span>}
                  </td>
                  <td>{u.display_name || '—'}</td>
                  <td className="path-cell">{roleNames.length ? roleNames.join('、') : '—'}</td>
                  <td>{u.project_ids?.length ?? 0}</td>
                  <td>
                    <span className={`status-badge ${u.status === 'active' ? 'status-active' : 'status-disabled'}`}>
                      {u.status === 'active' ? t('settings.users.statusActive') : t('settings.users.statusDisabled')}
                    </span>
                  </td>
                  <td>{u.last_login_at ? fmtDateTime(u.last_login_at) : '—'}</td>
                  <td>
                    <button className="btn-link" onClick={() => openEdit(u)}>{t('settings.users.edit')}</button>
                    <button
                      className="btn-link btn-danger-link"
                      onClick={() => handleDelete(u.id)}
                      disabled={busyId === u.id}
                    >
                      {busyId === u.id ? t('settings.users.deleteBusy') : t('settings.users.delete')}
                    </button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}

      {showModal && (
        <div className="modal-overlay" onClick={() => setShowModal(false)}>
          <div className="modal-box" style={{ maxWidth: 560 }} onClick={(e) => e.stopPropagation()}>
            <h3>{editingId ? t('settings.users.modal.edit') : t('settings.users.modal.add')}</h3>
            {error && <div className="form-error">{error}</div>}

            <div className="form-group">
              <label>{t('settings.users.modal.usernameLabel')}</label>
              <input
                className="form-input"
                value={form.username}
                onChange={(e) => setForm((f) => ({ ...f, username: e.target.value }))}
                disabled={!!editingId}
                placeholder={t('settings.users.modal.usernamePlaceholder')}
              />
            </div>

            <div className="form-group">
              <label>{t('settings.users.modal.displayLabel')}</label>
              <input
                className="form-input"
                value={form.display_name}
                onChange={(e) => setForm((f) => ({ ...f, display_name: e.target.value }))}
                placeholder={t('settings.users.modal.displayPlaceholder')}
              />
            </div>

            <div className="form-group">
              <label>{t('settings.users.modal.passwordLabel')} {editingId && <span className="form-hint">{t('settings.users.modal.passwordKeepHint')}</span>}</label>
              <input
                className="form-input"
                type="password"
                value={form.password}
                onChange={(e) => setForm((f) => ({ ...f, password: e.target.value }))}
                placeholder={editingId ? t('settings.users.modal.passwordEditPlaceholder') : t('settings.users.modal.passwordNewPlaceholder')}
              />
            </div>

            <div className="form-group">
              <label className="checkbox-row">
                <input
                  type="checkbox"
                  checked={form.is_admin}
                  onChange={(e) => setForm((f) => ({ ...f, is_admin: e.target.checked }))}
                />
                <span>{t('settings.users.modal.adminCheckbox')}</span>
              </label>
            </div>

            {!form.is_admin && (
              <>
                <div className="form-group">
                  <label>{t('settings.users.modal.rolesLabel')}</label>
                  <div className="checkbox-grid">
                    {roles.map((r) => (
                      <label key={r.id} className="checkbox-row">
                        <input
                          type="checkbox"
                          checked={form.role_ids.includes(r.id)}
                          onChange={() => toggle('role_ids', r.id)}
                        />
                        <span>{r.name}{r.is_builtin ? t('settings.users.modal.rolesBuiltin') : ''}</span>
                      </label>
                    ))}
                    {roles.length === 0 && <span className="form-hint">{t('settings.users.modal.rolesEmpty')}</span>}
                  </div>
                </div>

                <div className="form-group">
                  <label>{t('settings.users.modal.projectsLabel')}</label>
                  <div className="checkbox-grid">
                    {projects.map((p) => (
                      <label key={p.id} className="checkbox-row">
                        <input
                          type="checkbox"
                          checked={form.project_ids.includes(p.id)}
                          onChange={() => toggle('project_ids', p.id)}
                        />
                        <span>{p.name}</span>
                      </label>
                    ))}
                    {projects.length === 0 && <span className="form-hint">{t('settings.users.modal.projectsEmpty')}</span>}
                  </div>
                </div>
              </>
            )}

            <div className="form-actions">
              <button className="btn btn-secondary" onClick={() => setShowModal(false)}>{t('settings.users.modal.cancel')}</button>
              <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
                {saving ? t('settings.users.modal.saving') : t('settings.users.modal.save')}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
