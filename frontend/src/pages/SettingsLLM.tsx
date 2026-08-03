import { useState, useEffect } from 'react';
import { llmApi } from '../api/client';

export default function SettingsLLM() {
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [savedAt, setSavedAt] = useState('');

  const [keySet, setKeySet] = useState(false);
  const [keyPreview, setKeyPreview] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [baseURL, setBaseURL] = useState('');
  const [model, setModel] = useState('');

  useEffect(() => {
    llmApi.get()
      .then(cfg => {
        setKeySet(cfg.api_key_set);
        setKeyPreview(cfg.api_key_preview);
        setBaseURL(cfg.base_url || '');
        setModel(cfg.model || '');
      })
      .catch(err => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setLoading(false));
  }, []);

  const handleSave = async () => {
    setSaving(true);
    setError('');
    try {
      // Only send the api key if the user typed a new one; leaving it blank
      // keeps the existing secret server-side.
      const cfg = await llmApi.update({
        base_url: baseURL.trim(),
        api_key: apiKey || undefined,
        model: model.trim(),
      });
      setKeySet(cfg.api_key_set);
      setKeyPreview(cfg.api_key_preview);
      setApiKey('');
      setSavedAt(new Date().toLocaleTimeString('zh-CN'));
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const handleClearKey = async () => {
    if (!confirm('确定清除已保存的 API Key 吗？清除后 LLM 通道将不可用，需求标题将回退为内容首行截断。')) return;
    setSaving(true);
    setError('');
    try {
      const cfg = await llmApi.update({
        base_url: baseURL.trim(),
        model: model.trim(),
        clear_api_key: true,
      });
      setKeySet(cfg.api_key_set);
      setKeyPreview(cfg.api_key_preview);
      setApiKey('');
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
          <h3 className="settings-section-title">直连 LLM 配置</h3>
          <p className="settings-section-desc">
            配置 OpenAI 兼容的直连 LLM 通道（如 DeepSeek）。仅用于需求标题提炼，不经 Claude CLI，响应更快。Base URL 与 API Key 均填写后通道才激活；调用失败时回退为需求内容首行截断，不影响需求创建。
          </p>
        </div>
      </div>

      {error && <div className="form-error">{error}</div>}

      <div className="form-group">
        <label>API Key</label>
        <input
          className="form-input"
          type="password"
          placeholder={keySet ? `已设置（${keyPreview}）— 留空保持不变` : '输入 API Key（留空则不修改）'}
          value={apiKey}
          onChange={e => setApiKey(e.target.value)}
          autoComplete="off"
        />
        <small className="form-hint">
          {keySet
            ? `当前已保存 Key：${keyPreview}。留空保存只会更新 Base URL 与模型。`
            : '尚未设置 Key。留空保存不会写入 Key。'}
        </small>
      </div>

      <div className="form-group">
        <label>Base URL</label>
        <input
          className="form-input"
          type="text"
          placeholder='如 https://api.deepseek.com/v1'
          value={baseURL}
          onChange={e => setBaseURL(e.target.value)}
          autoComplete="off"
        />
        <small className="form-hint">
          OpenAI 兼容端点前缀。兼容 DeepSeek 官方与自建网关，代码会自动补全 /chat/completions 路径。
        </small>
      </div>

      <div className="form-group">
        <label>模型名</label>
        <input
          className="form-input"
          type="text"
          placeholder='如 deepseek-chat'
          value={model}
          onChange={e => setModel(e.target.value)}
          autoComplete="off"
        />
        <small className="form-hint">
          支持 deepseek-chat、gpt-4o-mini 等 OpenAI 兼容模型名。留空时由服务端决定（DeepSeek 官方要求必填）。
        </small>
      </div>

      <div className="form-actions">
        <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
          {saving ? '保存中...' : '💾 保存'}
        </button>
        {savedAt && !saving && <span className="form-hint">已保存 · {savedAt}</span>}
        {keySet && (
          <button className="btn btn-secondary" onClick={handleClearKey} disabled={saving}>
            清除 Key
          </button>
        )}
      </div>
    </div>
  );
}
