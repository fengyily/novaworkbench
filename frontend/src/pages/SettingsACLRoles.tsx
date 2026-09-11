import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { aclApi, type ACLRole, type Permission } from '../api/client';
import { errorMessage } from '../utils/errMsg';

interface RoleForm {
  key: string;
  name: string;
  description: string;
  enabled: boolean;
  permission_ids: string[];
}

export default function SettingsACLRoles() {
  const { t } = useTranslation();
  const [roles, setRoles] = useState<ACLRole[]>([]);
  const [permissions, setPermissions] = useState<Permission[]>([]);
  const [loading, setLoading] = useState(true);
  const [showModal, setShowModal] = useState(false);
  const [editingId, setEditingId] = useState('');
  const [saving, setSaving] = useState(false);
  const [busyId, setBusyId] = useState('');
  const [error, setError] = useState('');
  const [form, setForm] = useState<RoleForm>({ key: '', name: '', description: '', enabled: true, permission_ids: [] });

  const load = async () => {
    setLoading(true);
    try {
      const [r, p] = await Promise.all([aclApi.listRoles(), aclApi.listPermissions()]);
      setRoles(r ?? []);
      setPermissions(p ?? []);
    } catch {
      setRoles([]);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    load();
  }, []);

  const openCreate = () => {
    setEditingId('');
    setForm({ key: '', name: '', description: '', enabled: true, permission_ids: [] });
    setError('');
    setShowModal(true);
  };

  const openEdit = (r: ACLRole) => {
    setEditingId(r.id);
    setForm({
      key: r.key,
      name: r.name,
      description: r.description,
      enabled: r.enabled,
      permission_ids: r.permission_ids ?? [],
    });
    setError('');
    setShowModal(true);
  };

  const togglePerm = (id: string) => {
    setForm((f) => {
      const set = new Set(f.permission_ids);
      if (set.has(id)) set.delete(id);
      else set.add(id);
      return { ...f, permission_ids: Array.from(set) };
    });
  };

  const handleSave = async () => {
    if (!form.name) {
      setError(t('settings.aclRoles.errNameRequired'));
      return;
    }
    if (!editingId && !form.key) {
      setError(t('settings.aclRoles.errKeyRequired'));
      return;
    }
    setSaving(true);
    setError('');
    try {
      if (editingId) {
        await aclApi.updateRole(editingId, {
          name: form.name,
          description: form.description,
          enabled: form.enabled,
          permission_ids: form.permission_ids,
        });
      } else {
        await aclApi.createRole({
          key: form.key,
          name: form.name,
          description: form.description,
          enabled: form.enabled,
          permission_ids: form.permission_ids,
        });
      }
      setShowModal(false);
      await load();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  const handleDelete = async (r: ACLRole) => {
    if (r.is_builtin) return;
    if (!confirm(t('settings.aclRoles.deleteConfirm', { name: r.name }))) return;
    setBusyId(r.id);
    try {
      await aclApi.deleteRole(r.id);
      await load();
    } catch (err) {
      alert(errorMessage(err));
    } finally {
      setBusyId('');
    }
  };

  // Group permissions by module for the matrix UI.
  const modules = Array.from(new Set(permissions.map((p) => p.module))).sort();
  const permsByModule: Record<string, Permission[]> = {};
  for (const m of modules) permsByModule[m] = permissions.filter((p) => p.module === m);

  return (
    <div className="settings-section">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">{t('settings.aclRoles.title')}</h3>
          <p className="settings-section-desc">
            {t('settings.aclRoles.desc')}
          </p>
        </div>
        <button className="btn btn-primary" onClick={openCreate}>{t('settings.aclRoles.add')}</button>
      </div>

      {loading && <div className="settings-empty">{t('settings.aclRoles.loading')}</div>}

      {!loading && roles.length === 0 && (
        <div className="settings-empty"><p>{t('settings.aclRoles.empty')}</p></div>
      )}

      {roles.length > 0 && (
        <div className="role-grid">
          {roles.map((r) => (
            <div key={r.id} className="role-card">
              <div className="role-card-header">
                <div>
                  <span className="role-name">{r.name}</span>
                  {r.is_builtin && <span className="builtin-badge">{t('settings.aclRoles.builtin')}</span>}
                  {!r.enabled && <span className="disabled-badge">{t('settings.aclRoles.disabled')}</span>}
                  <span className="role-key">{r.key}</span>
                </div>
                <div className="role-card-actions">
                  <button className="btn-link" onClick={() => openEdit(r)}>{t('settings.aclRoles.edit')}</button>
                  {!r.is_builtin && (
                    <button
                      className="btn-link btn-danger-link"
                      onClick={() => handleDelete(r)}
                      disabled={busyId === r.id}
                    >
                      {busyId === r.id ? t('settings.aclRoles.deleting') : t('settings.aclRoles.delete')}
                    </button>
                  )}
                </div>
              </div>
              <p className="role-desc">{r.description || '—'}</p>
              <div className="role-perms">
                {(r.permission_keys ?? []).length === 0 ? (
                  <span className="form-hint">{t('settings.aclRoles.noPermissions')}</span>
                ) : (
                  (r.permission_keys ?? []).map((k) => {
                    const p = permissions.find((pp) => pp.key === k);
                    return (
                      <span key={k} className="perm-chip" title={k}>
                        {p?.name ?? k}
                      </span>
                    );
                  })
                )}
              </div>
              <div className="role-meta">{t('settings.aclRoles.userCount', { n: r.user_count ?? 0 })}</div>
            </div>
          ))}
        </div>
      )}

      {showModal && (
        <div className="modal-overlay" onClick={() => setShowModal(false)}>
          <div className="modal-box" style={{ maxWidth: 640 }} onClick={(e) => e.stopPropagation()}>
            <h3>{editingId ? t('settings.aclRoles.modal.edit') : t('settings.aclRoles.modal.add')}</h3>
            {error && <div className="form-error">{error}</div>}

            <div className="form-group">
              <label>{t('settings.aclRoles.modal.keyLabel')} {editingId && <span className="form-hint">{t('settings.aclRoles.modal.keyLocked')}</span>}</label>
              <input
                className="form-input"
                value={form.key}
                onChange={(e) => setForm((f) => ({ ...f, key: e.target.value }))}
                disabled={!!editingId}
                placeholder={t('settings.aclRoles.modal.keyPlaceholder')}
              />
            </div>

            <div className="form-group">
              <label>{t('settings.aclRoles.modal.nameLabel')}</label>
              <input
                className="form-input"
                value={form.name}
                onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
                placeholder={t('settings.aclRoles.modal.namePlaceholder')}
              />
            </div>

            <div className="form-group">
              <label>{t('settings.aclRoles.modal.descLabel')}</label>
              <input
                className="form-input"
                value={form.description}
                onChange={(e) => setForm((f) => ({ ...f, description: e.target.value }))}
              />
            </div>

            <div className="form-group">
              <label className="checkbox-row">
                <input
                  type="checkbox"
                  checked={form.enabled}
                  onChange={(e) => setForm((f) => ({ ...f, enabled: e.target.checked }))}
                />
                <span>{t('settings.aclRoles.modal.enabled')}</span>
              </label>
            </div>

            <div className="form-group">
              <label>{t('settings.aclRoles.modal.permissionsLabel')}</label>
              {modules.map((m) => (
                <div key={m} className="perm-module">
                  <div className="perm-module-title">{moduleLabel(t, m)}</div>
                  <div className="checkbox-grid">
                    {permsByModule[m].map((p) => (
                      <label key={p.id} className="checkbox-row">
                        <input
                          type="checkbox"
                          checked={form.permission_ids.includes(p.id)}
                          onChange={() => togglePerm(p.id)}
                        />
                        <span>{p.name}<span className="perm-key-hint">{p.key}</span></span>
                      </label>
                    ))}
                  </div>
                </div>
              ))}
            </div>

            <div className="form-actions">
              <button className="btn btn-secondary" onClick={() => setShowModal(false)}>{t('settings.aclRoles.modal.cancel')}</button>
              <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
                {saving ? t('settings.aclRoles.modal.saving') : t('settings.aclRoles.modal.save')}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

// moduleLabel resolves the permission-module group heading. It takes the
// translate function so the label follows the active language at render time.
function moduleLabel(t: (k: string) => string, m: string): string {
  switch (m) {
    case 'menu': return t('settings.aclRoles.module.menu');
    case 'setting': return t('settings.aclRoles.module.setting');
    case 'project': return t('settings.aclRoles.module.project');
    case 'action': return t('settings.aclRoles.module.action');
    default: return m;
  }
}
