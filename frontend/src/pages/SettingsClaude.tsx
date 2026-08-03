import { useState, useEffect } from 'react';
import { claudeApi } from '../api/client';

export default function SettingsClaude() {
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [savedAt, setSavedAt] = useState('');

  const [tokenSet, setTokenSet] = useState(false);
  const [tokenPreview, setTokenPreview] = useState('');
  const [token, setToken] = useState('');
  const [baseURL, setBaseURL] = useState('');

  useEffect(() => {
    claudeApi.get()
      .then(cfg => {
        setTokenSet(cfg.anthropic_auth_token_set);
        setTokenPreview(cfg.anthropic_auth_token_preview);
        setBaseURL(cfg.anthropic_base_url || '');
      })
      .catch(err => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setLoading(false));
  }, []);

  const handleSave = async () => {
    setSaving(true);
    setError('');
    try {
      // Only send the token if the user typed a new one; leaving it blank keeps
      // the existing secret server-side.
      const cfg = await claudeApi.update({
        anthropic_auth_token: token || undefined,
        anthropic_base_url: baseURL.trim(),
      });
      setTokenSet(cfg.anthropic_auth_token_set);
      setTokenPreview(cfg.anthropic_auth_token_preview);
      setToken('');
      setSavedAt(new Date().toLocaleTimeString('zh-CN'));
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const handleClearToken = async () => {
    if (!confirm('确定清除已保存的 Auth Token 吗？清除后 Claude CLI 将回退到默认认证。')) return;
    setSaving(true);
    setError('');
    try {
      const cfg = await claudeApi.update({
        anthropic_base_url: baseURL.trim(),
        clear_token: true,
      });
      setTokenSet(cfg.anthropic_auth_token_set);
      setTokenPreview(cfg.anthropic_auth_token_preview);
      setToken('');
      setSavedAt(new Date().toLocaleTimeString('zh-CN'));
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  if (loading) return <div className="settings-empty">加载中...</div>;

  return (
    <div className="settings-section">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">Claude CLI 配置</h3>
          <p className="settings-section-desc">
            配置 Claude CLI 运行时使用的环境变量。所有 AI 功能（需求分析、方案设计、代码生成、PR Review）启动 <code>claude</code> 子进程时都会注入以下变量。
          </p>
        </div>
      </div>

      {error && <div className="form-error">{error}</div>}

      <div className="form-group">
        <label>ANTHROPIC_AUTH_TOKEN</label>
        <input
          className="form-input"
          type="password"
          placeholder={tokenSet ? `已设置（${tokenPreview}）— 留空保持不变` : '输入 Auth Token（留空则不修改）'}
          value={token}
          onChange={e => setToken(e.target.value)}
          autoComplete="off"
        />
        <small className="form-hint">
          {tokenSet
            ? `当前已保存 Token：${tokenPreview}。留空保存只会更新 Base URL。`
            : '尚未设置 Token。留空保存不会写入 Token。'}
        </small>
      </div>

      <div className="form-group">
        <label>ANTHROPIC_BASE_URL</label>
        <input
          className="form-input"
          type="text"
          placeholder='如 https://api.anthropic.com（留空使用默认）'
          value={baseURL}
          onChange={e => setBaseURL(e.target.value)}
          autoComplete="off"
        />
        <small className="form-hint">
          指向自建/第三方 Anthropic 兼容网关时填写。留空则使用 CLI 默认地址。
        </small>
      </div>

      <div className="form-actions">
        <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
          {saving ? '保存中...' : '💾 保存'}
        </button>
        {savedAt && !saving && <span className="form-hint">已保存 · {savedAt}</span>}
        {tokenSet && (
          <button className="btn btn-secondary" onClick={handleClearToken} disabled={saving}>
            清除 Token
          </button>
        )}
      </div>
    </div>
  );
}
