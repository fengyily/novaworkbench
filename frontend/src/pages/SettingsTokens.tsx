import { useState, useEffect } from 'react';
import { platformApi, type PlatformToken } from '../api/client';

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

export default function SettingsTokens() {
  const [tokens, setTokens] = useState<PlatformToken[]>([]);
  const [loading, setLoading] = useState(true);
  const [showModal, setShowModal] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [deleteId, setDeleteId] = useState('');

  const [form, setForm] = useState({
    name: '',
    platform: 'github',
    base_url: '',
    token: '',
  });

  useEffect(() => {
    platformApi.list()
      .then(data => setTokens(data ?? []))
      .catch(() => setTokens([]))
      .finally(() => setLoading(false));
  }, []);

  const openModal = () => {
    setForm({ name: '', platform: 'github', base_url: '', token: '' });
    setError('');
    setShowModal(true);
  };

  const handleCreate = async () => {
    if (!form.name || !form.token) {
      setError('名称和 Token 不能为空');
      return;
    }
    if ((form.platform === 'gitea') && !form.base_url) {
      setError('Gitea 需要填写 Base URL');
      return;
    }
    setSaving(true);
    setError('');
    try {
      const tok = await platformApi.create(form);
      setTokens(prev => [tok, ...prev]);
      setShowModal(false);
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

  const needsBaseUrl = form.platform === 'gitea' || form.platform === 'gitlab';

  return (
    <div className="settings-section">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">平台 Token</h3>
          <p className="settings-section-desc">配置 GitHub / GitLab / Gitea 的访问 Token，用于拉取 PR 列表和提交 Review 评论。</p>
        </div>
        <button className="btn btn-primary" onClick={openModal}>+ 添加 Token</button>
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
                <td>{new Date(tok.created_at).toLocaleDateString('zh-CN')}</td>
                <td>
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
        <div className="modal-overlay" onClick={() => setShowModal(false)}>
          <div className="modal-box" onClick={e => e.stopPropagation()}>
            <h3 className="modal-title">添加平台 Token</h3>

            {error && <div className="form-error">{error}</div>}

            <div className="form-group">
              <label>名称</label>
              <input
                className="form-input"
                placeholder="如：GitHub Personal Token"
                value={form.name}
                onChange={e => setForm(f => ({ ...f, name: e.target.value }))}
              />
            </div>

            <div className="form-group">
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
              <div className="form-group">
                <label>Base URL</label>
                <input
                  className="form-input"
                  placeholder={form.platform === 'gitlab' ? 'https://gitlab.com（自建填实际地址）' : 'https://gitea.example.com'}
                  value={form.base_url}
                  onChange={e => setForm(f => ({ ...f, base_url: e.target.value }))}
                />
              </div>
            )}

            <div className="form-group">
              <label>Token</label>
              <input
                className="form-input"
                type="password"
                placeholder="粘贴 Personal Access Token"
                value={form.token}
                onChange={e => setForm(f => ({ ...f, token: e.target.value }))}
              />
            </div>

            <div className="form-actions">
              <button className="btn btn-secondary" onClick={() => setShowModal(false)}>取消</button>
              <button className="btn btn-primary" onClick={handleCreate} disabled={saving}>
                {saving ? '保存中...' : '保存'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
