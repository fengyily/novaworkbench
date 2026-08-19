import { useState, useEffect } from 'react';
import { aclApi, projectsApi, type User, type ACLRole, type Project } from '../api/client';

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
      setError('用户名和密码不能为空（编辑时密码留空表示不修改）');
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
    if (!confirm('确认删除该用户？此操作不可撤销。')) return;
    setBusyId(id);
    try {
      await aclApi.deleteUser(id);
      await load();
    } catch (err) {
      alert(err instanceof Error ? err.message : String(err));
    } finally {
      setBusyId('');
    }
  };

  return (
    <div className="settings-section">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">用户管理</h3>
          <p className="settings-section-desc">
            管理登录账号、为用户分配角色（决定权限范围）与项目（决定可见项目）。多角色权限取并集。
          </p>
        </div>
        <button className="btn btn-primary" onClick={openCreate}>+ 添加用户</button>
      </div>

      {loading && <div className="settings-empty">加载中...</div>}

      {!loading && users.length === 0 && (
        <div className="settings-empty"><p>暂无用户。</p></div>
      )}

      {users.length > 0 && (
        <table className="project-table">
          <thead>
            <tr>
              <th>用户名</th>
              <th>显示名</th>
              <th>角色</th>
              <th>项目数</th>
              <th>状态</th>
              <th>最近登录</th>
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
                    {u.is_admin && <span className="admin-badge">管理员</span>}
                  </td>
                  <td>{u.display_name || '—'}</td>
                  <td className="path-cell">{roleNames.length ? roleNames.join('、') : '—'}</td>
                  <td>{u.project_ids?.length ?? 0}</td>
                  <td>
                    <span className={`status-badge ${u.status === 'active' ? 'status-active' : 'status-disabled'}`}>
                      {u.status === 'active' ? '启用' : '禁用'}
                    </span>
                  </td>
                  <td>{u.last_login_at ? new Date(u.last_login_at).toLocaleString('zh-CN') : '—'}</td>
                  <td>
                    <button className="btn-link" onClick={() => openEdit(u)}>编辑</button>
                    <button
                      className="btn-link btn-danger-link"
                      onClick={() => handleDelete(u.id)}
                      disabled={busyId === u.id}
                    >
                      {busyId === u.id ? '删除中...' : '删除'}
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
            <h3 className="modal-title">{editingId ? '编辑用户' : '添加用户'}</h3>
            {error && <div className="form-error">{error}</div>}

            <div className="form-group">
              <label>用户名</label>
              <input
                className="form-input"
                value={form.username}
                onChange={(e) => setForm((f) => ({ ...f, username: e.target.value }))}
                disabled={!!editingId}
                placeholder="登录用户名"
              />
            </div>

            <div className="form-group">
              <label>显示名</label>
              <input
                className="form-input"
                value={form.display_name}
                onChange={(e) => setForm((f) => ({ ...f, display_name: e.target.value }))}
                placeholder="可选"
              />
            </div>

            <div className="form-group">
              <label>密码 {editingId && <span className="form-hint">（留空表示不修改）</span>}</label>
              <input
                className="form-input"
                type="password"
                value={form.password}
                onChange={(e) => setForm((f) => ({ ...f, password: e.target.value }))}
                placeholder={editingId ? '不修改则留空' : '设置初始密码'}
              />
            </div>

            <div className="form-group">
              <label className="checkbox-row">
                <input
                  type="checkbox"
                  checked={form.is_admin}
                  onChange={(e) => setForm((f) => ({ ...f, is_admin: e.target.checked }))}
                />
                <span>管理员（绕过所有权限检查，可访问全部项目）</span>
              </label>
            </div>

            {!form.is_admin && (
              <>
                <div className="form-group">
                  <label>分配角色（多选，权限取并集）</label>
                  <div className="checkbox-grid">
                    {roles.map((r) => (
                      <label key={r.id} className="checkbox-row">
                        <input
                          type="checkbox"
                          checked={form.role_ids.includes(r.id)}
                          onChange={() => toggle('role_ids', r.id)}
                        />
                        <span>{r.name}{r.is_builtin ? '（内置）' : ''}</span>
                      </label>
                    ))}
                    {roles.length === 0 && <span className="form-hint">无可用角色</span>}
                  </div>
                </div>

                <div className="form-group">
                  <label>分配项目（多选，用户仅可见被分配项目）</label>
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
                    {projects.length === 0 && <span className="form-hint">暂无项目</span>}
                  </div>
                </div>
              </>
            )}

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
