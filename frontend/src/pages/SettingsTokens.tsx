import { useState, useEffect } from 'react';
import { platformApi, type PlatformToken } from '../api/client';
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

// One form is reused for both the "+ 添加 Token" and "编辑" modals. The
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
      setError('名称不能为空');
      return;
    }
    if (!editingId && !form.token) {
      // New token rows must carry a PAT; edits can leave it blank to keep
      // the existing secret.
      setError('Token 不能为空');
      return;
    }
    if (!editingId && (form.platform === 'gitea') && !form.base_url) {
      setError('Gitea 需要填写 Base URL');
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
      setError(err instanceof Error ? err.message : String(err));
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
          <h3 className="settings-section-title">平台 Token</h3>
          <p className="settings-section-desc">
            配置 GitHub / GitLab / Gitea 的访问 Token，用于拉取 PR 列表和提交 Review 评论。
            可同时填写 Git 提交身份，Docker 环境下没有挂载 ~/.gitconfig 时会自动使用。
          </p>
        </div>
        <button className="btn btn-primary" onClick={openCreateModal}>+ 添加 Token</button>
      </div>

      {loading && <div className="settings-empty">加载中...</div>}

      {!loading && tokens.length === 0 && (
        <div className="settings-empty">
          <p>暂无配置。点击「添加 Token」开始配置。</p>
        </div>
      )}

      {tokens.length > 0 && (
        <table className="project-table">
          <thead>
            <tr>
              <th>名称</th>
              <th>平台</th>
              <th>Base URL</th>
              <th>Git 身份</th>
              <th>创建时间</th>
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
                    编辑
                  </button>
                  <button
                    className="btn-link btn-danger-link"
                    onClick={() => handleDelete(tok.id)}
                    disabled={deleteId === tok.id}
                  >
                    {deleteId === tok.id ? '删除中...' : '删除'}
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
            <h3>{isEdit ? '编辑平台 Token' : '添加平台 Token'}</h3>

            {error && <div className="form-error">{error}</div>}

            <div className="modal-field">
              <label>名称</label>
              <input
                className="form-input"
                placeholder="如：GitHub Personal Token"
                value={form.name}
                onChange={e => setForm(f => ({ ...f, name: e.target.value }))}
              />
            </div>

            {!isEdit && (
              <>
                <div className="modal-field">
                  <label>平台</label>
                  <select
                    className="form-input"
                    value={form.platform}
                    onChange={e => setForm(f => ({ ...f, platform: e.target.value }))}
                  >
                    <option value="github">GitHub</option>
                    <option value="gitlab">GitLab</option>
                    <option value="gitea">Gitea（自建）</option>
                  </select>
                </div>

                {needsBaseUrl && (
                  <div className="modal-field">
                    <label>Base URL</label>
                    <input
                      className="form-input"
                      placeholder={form.platform === 'gitlab' ? 'https://gitlab.com（自建填实际地址）' : 'https://gitea.example.com'}
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
              <label>{isEdit ? '新 Token（留空保持原值）' : 'Token'}</label>
              <input
                className="form-input"
                type="password"
                placeholder={isEdit ? '仅在轮换 Token 时填写' : '粘贴 Personal Access Token'}
                value={form.token}
                onChange={e => setForm(f => ({ ...f, token: e.target.value }))}
              />
            </div>

            <div className="modal-field">
              <label>Git 用户名（提交者姓名）</label>
              <input
                className="form-input"
                placeholder="如：Zhang San"
                value={form.git_user_name}
                onChange={e => setForm(f => ({ ...f, git_user_name: e.target.value }))}
              />
            </div>

            <div className="modal-field">
              <label>Git 邮箱（提交者邮箱）</label>
              <input
                className="form-input"
                placeholder="如：zhangsan@example.com"
                value={form.git_user_email}
                onChange={e => setForm(f => ({ ...f, git_user_email: e.target.value }))}
              />
              <div className="form-hint">
                Docker 环境下没有挂载宿主机的 ~/.gitconfig 时，提交会使用这里填写的身份。可与平台账号的姓名/邮箱保持一致。
              </div>
            </div>

            <div className="modal-actions btn-row-2col">
              <button className="btn" onClick={closeModal}>取消</button>
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
