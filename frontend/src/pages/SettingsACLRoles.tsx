import { useState, useEffect } from 'react';
import { aclApi, type ACLRole, type Permission } from '../api/client';

interface RoleForm {
  key: string;
  name: string;
  description: string;
  enabled: boolean;
  permission_ids: string[];
}

export default function SettingsACLRoles() {
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
      setError('角色名称不能为空');
      return;
    }
    if (!editingId && !form.key) {
      setError('角色 key 不能为空（如 dev_lead）');
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
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const handleDelete = async (r: ACLRole) => {
    if (r.is_builtin) return;
    if (!confirm(`确认删除角色「${r.name}」？`)) return;
    setBusyId(r.id);
    try {
      await aclApi.deleteRole(r.id);
      await load();
    } catch (err) {
      alert(err instanceof Error ? err.message : String(err));
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
          <h3 className="settings-section-title">角色权限</h3>
          <p className="settings-section-desc">
            管理用户角色及其权限范围。此处的「角色」是用户权限角色（区别于 AI 角色配置）。多角色用户的权限取并集。
          </p>
        </div>
        <button className="btn btn-primary" onClick={openCreate}>+ 添加角色</button>
      </div>

      {loading && <div className="settings-empty">加载中...</div>}

      {!loading && roles.length === 0 && (
        <div className="settings-empty"><p>暂无角色。</p></div>
      )}

      {roles.length > 0 && (
        <div className="role-grid">
          {roles.map((r) => (
            <div key={r.id} className="role-card">
              <div className="role-card-header">
                <div>
                  <span className="role-name">{r.name}</span>
                  {r.is_builtin && <span className="builtin-badge">内置</span>}
                  {!r.enabled && <span className="disabled-badge">已禁用</span>}
                  <span className="role-key">{r.key}</span>
                </div>
                <div className="role-card-actions">
                  <button className="btn-link" onClick={() => openEdit(r)}>编辑</button>
                  {!r.is_builtin && (
                    <button
                      className="btn-link btn-danger-link"
                      onClick={() => handleDelete(r)}
                      disabled={busyId === r.id}
                    >
                      {busyId === r.id ? '删除中...' : '删除'}
                    </button>
                  )}
                </div>
              </div>
              <p className="role-desc">{r.description || '—'}</p>
              <div className="role-perms">
                {(r.permission_keys ?? []).length === 0 ? (
                  <span className="form-hint">无权限（管理员除外）</span>
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
              <div className="role-meta">使用此角色的用户：{r.user_count ?? 0}</div>
            </div>
          ))}
        </div>
      )}

      {showModal && (
        <div className="modal-overlay" onClick={() => setShowModal(false)}>
          <div className="modal-box" style={{ maxWidth: 640 }} onClick={(e) => e.stopPropagation()}>
            <h3 className="modal-title">{editingId ? '编辑角色' : '添加角色'}</h3>
            {error && <div className="form-error">{error}</div>}

            <div className="form-group">
              <label>角色 Key {editingId && <span className="form-hint">（不可修改）</span>}</label>
              <input
                className="form-input"
                value={form.key}
                onChange={(e) => setForm((f) => ({ ...f, key: e.target.value }))}
                disabled={!!editingId}
                placeholder="如 dev_lead（小写+下划线）"
              />
            </div>

            <div className="form-group">
              <label>角色名称</label>
              <input
                className="form-input"
                value={form.name}
                onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
                placeholder="如 开发组长"
              />
            </div>

            <div className="form-group">
              <label>描述</label>
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
                <span>启用</span>
              </label>
            </div>

            <div className="form-group">
              <label>权限（多选，用户多角色取并集）</label>
              {modules.map((m) => (
                <div key={m} className="perm-module">
                  <div className="perm-module-title">{moduleLabel(m)}</div>
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
              <button className="btn btn-secondary" onClick={() => setShowModal(false)}>取消</button>
              <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
                {saving ? '保存中...' : '保存'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

function moduleLabel(m: string): string {
  switch (m) {
    case 'menu': return '菜单';
    case 'setting': return '设置';
    case 'project': return '项目';
    case 'action': return '操作';
    default: return m;
  }
}
