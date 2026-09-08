import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { platformApi, projectsApi, type PlatformToken } from '../api/client';
import { errorMessage } from '../utils/errMsg';
import './AddProject.css';

// Best-effort host → platform map. Mirrors backend internal/service/project.go
// hostPlatform(). Unknown hosts (self-hosted GitLab/Gitea) need a manually
// picked token.
function detectPlatform(url: string): string {
  const m = url.match(/^(?:https?|git|ssh):\/\/(?:[^@/]+@)?([^/:]+)/)
    ?? url.match(/^git@([^:]+):/);
  if (!m) return '';
  const host = m[1].toLowerCase();
  if (host === 'github.com') return 'github';
  if (host === 'gitlab.com') return 'gitlab';
  // Self-hosted GitLab/Gitea can only be inferred from the chosen token's
  // base_url — leave blank so the user picks manually.
  return '';
}

export default function AddProject() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [remoteUrl, setRemoteUrl] = useState('');
  const [branch, setBranch] = useState('');
  const [tokens, setTokens] = useState<PlatformToken[]>([]);
  const [platformTokenId, setPlatformTokenId] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Pull the platform-token list once. The list endpoint omits the raw
  // secret, so it's safe to surface here.
  useEffect(() => {
    platformApi.list()
      .then(data => setTokens(data ?? []))
      .catch(() => setTokens([]));
  }, []);

  const inferredPlatform = detectPlatform(remoteUrl);
  // Filter the token dropdown to the inferred platform when the URL host
  // is recognizable (github.com / gitlab.com). For self-hosted URLs we
  // show everything and let the user pick.
  const visibleTokens = inferredPlatform
    ? tokens.filter(t => t.platform === inferredPlatform)
    : tokens;

  const handleSubmit = async () => {
    setError(null);
    setLoading(true);
    try {
      const chosen = tokens.find(t => t.id === platformTokenId);
      const platformType = chosen?.platform ?? inferredPlatform;
      await projectsApi.add({
        remote_url: remoteUrl,
        branch: branch || undefined,
        platform_type: platformType || undefined,
        platform_token_id: platformTokenId || undefined,
      });
      navigate('/');
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="add-project-page">
      <h1 className="page-title">{t('projects.add.title')}</h1>

      <div className="add-project-card">
        <div className="form-group">
          <label>{t('projects.add.remoteLabel')}</label>
          <input
            type="text"
            value={remoteUrl}
            onChange={e => setRemoteUrl(e.target.value)}
            placeholder={t('projects.add.remotePlaceholder')}
            className="form-input"
          />
          {inferredPlatform && (
            <span className="input-hint">
              {t('projects.add.detected', { platform: inferredPlatform === 'github' ? 'GitHub' : inferredPlatform === 'gitlab' ? 'GitLab' : inferredPlatform })}
            </span>
          )}
        </div>
        <div className="form-group">
          <label>{t('projects.add.branchLabel')}</label>
          <input
            type="text"
            value={branch}
            onChange={e => setBranch(e.target.value)}
            placeholder={t('projects.add.branchPlaceholder')}
            className="form-input"
          />
        </div>
        <div className="form-group">
          <label>{t('projects.add.tokenLabel')}</label>
          <select
            value={platformTokenId}
            onChange={e => setPlatformTokenId(e.target.value)}
            className="form-input"
          >
            <option value="">{t('projects.add.tokenNone')}</option>
            {visibleTokens.map(t => (
              <option key={t.id} value={t.id}>
                {t.name} ({t.platform})
              </option>
            ))}
          </select>
          {visibleTokens.length === 0 && tokens.length > 0 && (
            <span className="input-hint">
              {t('projects.add.noMatchingToken')}
            </span>
          )}
          {tokens.length === 0 && (
            <span className="input-hint">
              {t('projects.add.noTokens')}
            </span>
          )}
        </div>

        {error && <div className="form-error">❌ {error}</div>}

        <div className="form-actions stack-mobile">
          <button className="btn" onClick={() => navigate('/')}>{t('projects.add.cancel')}</button>
          <button
            className="btn btn-primary"
            onClick={handleSubmit}
            disabled={loading || !remoteUrl}
          >
            {loading ? t('projects.add.adding') : t('projects.add.submit')}
          </button>
        </div>
      </div>
    </div>
  );
}