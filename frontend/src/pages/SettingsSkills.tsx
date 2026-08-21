import { useState, useEffect } from 'react';
import { skillsApi, type Skill, type MarketSkill, type SkillMarket } from '../api/client';

interface SkillForm {
  name: string;
  slug: string;
  description: string;
  content: string;
  enabled: boolean;
  source_url: string;
}

const emptyForm = (): SkillForm => ({
  name: '',
  slug: '',
  description: '',
  content: '',
  enabled: true,
  source_url: '',
});

export default function SettingsSkills() {
  const [activeTab, setActiveTab] = useState<'installed' | 'market'>('installed');
  const [skills, setSkills] = useState<Skill[]>([]);
  const [loading, setLoading] = useState(true);
  const [showModal, setShowModal] = useState(false);
  const [editingId, setEditingId] = useState('');
  const [saving, setSaving] = useState(false);
  const [busyId, setBusyId] = useState('');
  const [error, setError] = useState('');
  const [form, setForm] = useState<SkillForm>(emptyForm());

  // Market state
  const [markets, setMarkets] = useState<SkillMarket[]>([]);
  const [selectedMarket, setSelectedMarket] = useState<string>('');
  const [customRegistry, setCustomRegistry] = useState('');
  const [marketSkills, setMarketSkills] = useState<MarketSkill[]>([]);
  const [marketLoading, setMarketLoading] = useState(false);
  const [marketError, setMarketError] = useState('');
  const [installingSlug, setInstallingSlug] = useState('');
  const [searchQuery, setSearchQuery] = useState('');

  const load = async () => {
    setLoading(true);
    try {
      const data = await skillsApi.list();
      setSkills(data ?? []);
    } catch {
      setSkills([]);
    } finally {
      setLoading(false);
    }
  };

  const loadMarkets = async () => {
    try {
      const data = await skillsApi.markets();
      setMarkets(data ?? []);
      if (data && data.length > 0 && !selectedMarket) {
        setSelectedMarket(data[0].id);
      }
    } catch {
      setMarkets([]);
    }
  };

  useEffect(() => {
    load();
  }, []);

  useEffect(() => {
    if (activeTab === 'market' && markets.length === 0) {
      loadMarkets();
    }
  }, [activeTab]);

  // Auto-fetch when market selection changes
  useEffect(() => {
    if (activeTab === 'market' && selectedMarket) {
      fetchMarketSkills(selectedMarket, '');
    }
  }, [selectedMarket]);

  const fetchMarketSkills = async (marketId: string, registry: string) => {
    setMarketLoading(true);
    setMarketError('');
    setMarketSkills([]);
    setSearchQuery('');
    try {
      const data = await skillsApi.market(
        marketId ? { market: marketId } : { registry }
      );
      setMarketSkills(data ?? []);
    } catch (e: unknown) {
      setMarketError(e instanceof Error ? e.message : '加载失败');
    } finally {
      setMarketLoading(false);
    }
  };

  const openCreate = () => {
    setEditingId('');
    setForm(emptyForm());
    setError('');
    setShowModal(true);
  };

  const openEdit = (sk: Skill) => {
    setEditingId(sk.id);
    setForm({
      name: sk.name,
      slug: sk.slug,
      description: sk.description,
      content: sk.content,
      enabled: sk.enabled,
      source_url: sk.source_url,
    });
    setError('');
    setShowModal(true);
  };

  const handleSave = async () => {
    if (!form.name.trim() || !form.slug.trim() || !form.content.trim()) {
      setError('名称、Slug、内容不能为空');
      return;
    }
    setSaving(true);
    setError('');
    try {
      if (editingId) {
        await skillsApi.update(editingId, {
          name: form.name,
          slug: form.slug,
          content: form.content,
          description: form.description,
          enabled: form.enabled,
        });
      } else {
        await skillsApi.create({
          name: form.name,
          slug: form.slug,
          content: form.content,
          description: form.description,
          source_url: form.source_url,
        });
      }
      setShowModal(false);
      await load();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : '保存失败');
    } finally {
      setSaving(false);
    }
  };

  const handleDelete = async (sk: Skill) => {
    if (!confirm(`确认删除 Skill「${sk.name}」？`)) return;
    setBusyId(sk.id);
    try {
      await skillsApi.delete(sk.id);
      await load();
    } finally {
      setBusyId('');
    }
  };

  const handleToggle = async (sk: Skill) => {
    setBusyId(sk.id);
    try {
      await skillsApi.update(sk.id, {
        name: sk.name,
        slug: sk.slug,
        content: sk.content,
        description: sk.description,
        enabled: !sk.enabled,
      });
      await load();
    } finally {
      setBusyId('');
    }
  };

  const handleInstall = async (mk: MarketSkill) => {
    setInstallingSlug(mk.slug);
    try {
      await skillsApi.create({
        name: mk.name,
        slug: mk.slug,
        content: mk.content,
        description: mk.description,
        source_url: mk.source_url,
      });
      await load();
      setActiveTab('installed');
    } finally {
      setInstallingSlug('');
    }
  };

  const installedSlugs = new Set(skills.map((s) => s.slug));

  const q = searchQuery.trim().toLowerCase();
  const filteredMarketSkills = q
    ? marketSkills.filter(
        (mk) =>
          mk.name.toLowerCase().includes(q) ||
          mk.slug.toLowerCase().includes(q) ||
          mk.description.toLowerCase().includes(q)
      )
    : marketSkills;

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
        <h3 style={{ margin: 0 }}>Skills 管理</h3>
        <button className="btn btn-primary" onClick={openCreate}>
          + 新建 Skill
        </button>
      </div>

      {/* Usage hint */}
      <div style={{
        background: '#EEF2FF',
        border: '1px solid #C7D2FE',
        borderRadius: 8,
        padding: '10px 14px',
        marginBottom: 16,
        fontSize: 13,
        color: '#3730A3',
        lineHeight: 1.6,
      }}>
        <strong>如何使用：</strong>已启用的 Skill 会在调用 AI（分析 / 架构 / 开发阶段）前自动写入项目的
        {' '}<code style={{ background: '#C7D2FE', padding: '1px 4px', borderRadius: 3 }}>.claude/agents/&lt;slug&gt;.md</code>，
        Claude 即可通过 <code style={{ background: '#C7D2FE', padding: '1px 4px', borderRadius: 3 }}>/slug</code> 调用该 Skill。调用结束后文件自动清理。
      </div>

      {/* Tabs */}
      <div style={{ display: 'flex', gap: 8, marginBottom: 20, borderBottom: '1px solid #e2e8f0' }}>
        {(['installed', 'market'] as const).map((tab) => (
          <button
            key={tab}
            onClick={() => setActiveTab(tab)}
            style={{
              padding: '6px 16px',
              border: 'none',
              background: 'none',
              cursor: 'pointer',
              borderBottom: activeTab === tab ? '2px solid #4F46E5' : '2px solid transparent',
              color: activeTab === tab ? '#4F46E5' : '#64748B',
              fontWeight: activeTab === tab ? 600 : 400,
            }}
          >
            {tab === 'installed' ? `已安装 (${skills.length})` : '市场'}
          </button>
        ))}
      </div>

      {/* Installed Tab */}
      {activeTab === 'installed' && (
        <>
          {loading ? (
            <div style={{ color: '#64748B' }}>加载中...</div>
          ) : skills.length === 0 ? (
            <div style={{ color: '#64748B', padding: 20, textAlign: 'center' }}>
              暂无 Skill，点击「新建」或从市场安装
            </div>
          ) : (
            <table style={{ width: '100%', borderCollapse: 'collapse' }}>
              <thead>
                <tr style={{ background: '#F8FAFC', textAlign: 'left' }}>
                  <th style={{ padding: '8px 12px', color: '#64748B', fontWeight: 500 }}>名称</th>
                  <th style={{ padding: '8px 12px', color: '#64748B', fontWeight: 500 }}>Slug</th>
                  <th style={{ padding: '8px 12px', color: '#64748B', fontWeight: 500 }}>描述</th>
                  <th style={{ padding: '8px 12px', color: '#64748B', fontWeight: 500, width: 90 }}>启用</th>
                  <th style={{ padding: '8px 12px', color: '#64748B', fontWeight: 500 }}>操作</th>
                </tr>
              </thead>
              <tbody>
                {skills.map((sk) => (
                  <tr key={sk.id} style={{ borderTop: '1px solid #F1F5F9' }}>
                    <td style={{ padding: '10px 12px', fontWeight: 500 }}>{sk.name}</td>
                    <td style={{ padding: '10px 12px', fontFamily: 'monospace', color: '#64748B' }}>{sk.slug}</td>
                    <td style={{ padding: '10px 12px', color: '#64748B', maxWidth: 240, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                      {sk.description || '—'}
                    </td>
                    <td style={{ padding: '10px 12px' }}>
                      <label style={{ display: 'flex', alignItems: 'center', gap: 7, cursor: busyId === sk.id ? 'not-allowed' : 'pointer', userSelect: 'none' }}>
                        <div
                          onClick={() => busyId !== sk.id && handleToggle(sk)}
                          style={{
                            width: 36,
                            height: 20,
                            borderRadius: 10,
                            background: sk.enabled ? '#10B981' : '#CBD5E1',
                            position: 'relative',
                            transition: 'background 0.2s',
                            flexShrink: 0,
                          }}
                        >
                          <div style={{
                            position: 'absolute',
                            top: 2,
                            left: sk.enabled ? 18 : 2,
                            width: 16,
                            height: 16,
                            borderRadius: '50%',
                            background: '#fff',
                            boxShadow: '0 1px 3px rgba(0,0,0,0.2)',
                            transition: 'left 0.2s',
                          }} />
                        </div>
                        <span style={{ fontSize: 13, color: sk.enabled ? '#10B981' : '#94A3B8' }}>
                          {sk.enabled ? '启用' : '禁用'}
                        </span>
                      </label>
                    </td>
                    <td style={{ padding: '10px 12px', display: 'flex', gap: 8 }}>
                      <button className="btn btn-sm btn-secondary" onClick={() => openEdit(sk)}>
                        编辑
                      </button>
                      <button
                        className="btn btn-sm btn-danger"
                        disabled={busyId === sk.id}
                        onClick={() => handleDelete(sk)}
                      >
                        删除
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </>
      )}

      {/* Market Tab */}
      {activeTab === 'market' && (
        <div>
          {/* Market selector */}
          <div style={{ display: 'flex', gap: 8, marginBottom: 16, flexWrap: 'wrap' }}>
            {markets.map((m) => (
              <button
                key={m.id}
                onClick={() => { setSelectedMarket(m.id); setCustomRegistry(''); }}
                title={m.description}
                style={{
                  padding: '6px 14px',
                  borderRadius: 20,
                  border: selectedMarket === m.id && !customRegistry
                    ? '2px solid #4F46E5'
                    : '2px solid #E2E8F0',
                  background: selectedMarket === m.id && !customRegistry ? '#EEF2FF' : '#fff',
                  color: selectedMarket === m.id && !customRegistry ? '#4F46E5' : '#475569',
                  cursor: 'pointer',
                  fontWeight: selectedMarket === m.id && !customRegistry ? 600 : 400,
                  fontSize: 13,
                }}
              >
                {m.name}
              </button>
            ))}

            {/* Custom registry */}
            <div style={{ display: 'flex', gap: 6, flex: 1, minWidth: 240 }}>
              <input
                className="form-input"
                style={{ flex: 1, fontSize: 13 }}
                placeholder="自定义 GitHub 仓库 URL 或 manifest URL"
                value={customRegistry}
                onChange={(e) => { setCustomRegistry(e.target.value); setSelectedMarket(''); }}
              />
              <button
                className="btn btn-secondary"
                style={{ flexShrink: 0 }}
                disabled={!customRegistry.trim() || marketLoading}
                onClick={() => fetchMarketSkills('', customRegistry.trim())}
              >
                加载
              </button>
            </div>
          </div>

          {marketError && <div style={{ color: '#EF4444', marginBottom: 12 }}>{marketError}</div>}

          {/* Search */}
          {!marketLoading && marketSkills.length > 0 && (
            <input
              className="form-input"
              style={{ marginBottom: 12 }}
              placeholder="搜索 Skill 名称、描述..."
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.target.value)}
            />
          )}

          {marketLoading ? (
            <div style={{ color: '#64748B', padding: 20, textAlign: 'center' }}>加载市场中...</div>
          ) : filteredMarketSkills.length === 0 ? (
            <div style={{ color: '#64748B', padding: 20, textAlign: 'center' }}>
              {q ? `未找到与「${searchQuery}」相关的 Skill` : '暂无可用 Skill'}
            </div>
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
              {filteredMarketSkills.map((mk) => {
                const installed = installedSlugs.has(mk.slug);
                return (
                  <div
                    key={mk.slug}
                    style={{
                      border: '1px solid #E2E8F0',
                      borderRadius: 8,
                      padding: '12px 16px',
                      display: 'flex',
                      justifyContent: 'space-between',
                      alignItems: 'flex-start',
                      gap: 16,
                    }}
                  >
                    <div style={{ flex: 1, minWidth: 0 }}>
                      <div style={{ fontWeight: 600, marginBottom: 4 }}>{mk.name}</div>
                      {mk.description && (
                        <div style={{ color: '#64748B', fontSize: 13, marginBottom: 4 }}>{mk.description}</div>
                      )}
                      <code style={{ fontSize: 12, color: '#94A3B8' }}>{mk.slug}</code>
                    </div>
                    <button
                      className={`btn btn-sm ${installed ? 'btn-secondary' : 'btn-primary'}`}
                      disabled={installed || installingSlug === mk.slug}
                      onClick={() => handleInstall(mk)}
                      style={{ flexShrink: 0 }}
                    >
                      {installingSlug === mk.slug ? '安装中...' : installed ? '已安装' : '安装'}
                    </button>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      )}

      {/* Edit/Create Modal */}
      {showModal && (
        <div className="modal-overlay" onClick={() => setShowModal(false)}>
          <div className="modal-box" style={{ maxWidth: 640, width: '100%' }} onClick={(e) => e.stopPropagation()}>
            <h3 style={{ marginTop: 0 }}>{editingId ? '编辑 Skill' : '新建 Skill'}</h3>

            <div style={{ display: 'flex', gap: 12, marginBottom: 12 }}>
              <div style={{ flex: 1 }}>
                <label className="form-label">名称</label>
                <input
                  className="form-input"
                  value={form.name}
                  onChange={(e) => setForm({ ...form, name: e.target.value })}
                  placeholder="Frontend Expert"
                />
              </div>
              <div style={{ flex: 1 }}>
                <label className="form-label">Slug（文件名）</label>
                <input
                  className="form-input"
                  value={form.slug}
                  onChange={(e) => setForm({ ...form, slug: e.target.value })}
                  placeholder="frontend"
                  style={{ fontFamily: 'monospace' }}
                />
              </div>
            </div>

            <div style={{ marginBottom: 12 }}>
              <label className="form-label">描述</label>
              <input
                className="form-input"
                value={form.description}
                onChange={(e) => setForm({ ...form, description: e.target.value })}
                placeholder="可选简介"
              />
            </div>

            <div style={{ marginBottom: 12 }}>
              <label className="form-label">内容（Markdown）</label>
              <textarea
                className="form-input"
                rows={12}
                value={form.content}
                onChange={(e) => setForm({ ...form, content: e.target.value })}
                placeholder="# Frontend Expert&#10;&#10;你是一名专业的 React / TypeScript 前端工程师..."
                style={{ fontFamily: 'monospace', fontSize: 13 }}
              />
            </div>

            <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 16 }}>
              <input
                type="checkbox"
                id="skill-enabled"
                checked={form.enabled}
                onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
              />
              <label htmlFor="skill-enabled">启用</label>
            </div>

            {error && <div style={{ color: '#EF4444', marginBottom: 12 }}>{error}</div>}

            <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8 }}>
              <button className="btn btn-secondary" onClick={() => setShowModal(false)}>
                取消
              </button>
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
