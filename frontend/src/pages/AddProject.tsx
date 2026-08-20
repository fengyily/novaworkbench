import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { projectsApi } from '../api/client';
import FolderPicker from '../components/FolderPicker';
import './AddProject.css';

export default function AddProject() {
  const navigate = useNavigate();
  const [mode, setMode] = useState<'local' | 'remote'>('local');
  const [localPath, setLocalPath] = useState('');
  const [remoteUrl, setRemoteUrl] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleSubmit = async () => {
    setError(null);
    setLoading(true);
    try {
      if (mode === 'remote') {
        await projectsApi.add('', remoteUrl, false);
      } else {
        await projectsApi.add(localPath, '', true); // always allow git init for new dirs
      }
      navigate('/');
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Unknown error');
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="add-project-page">
      <h1 className="page-title">📁 添加项目</h1>

      <div className="add-project-card">
        <div className="mode-tabs">
          <button
            className={`mode-tab ${mode === 'local' ? 'active' : ''}`}
            onClick={() => setMode('local')}
          >
            📂 本地目录
          </button>
          <button
            className={`mode-tab ${mode === 'remote' ? 'active' : ''}`}
            onClick={() => setMode('remote')}
          >
            🌐 Git 远程仓库
          </button>
        </div>

        {mode === 'local' ? (
          <FolderPicker value={localPath} onChange={setLocalPath} />
        ) : (
          <div className="form-group">
            <label>Git 仓库地址:</label>
            <input
              type="text"
              value={remoteUrl}
              onChange={e => setRemoteUrl(e.target.value)}
              placeholder="git@github.com:user/repo.git"
              className="form-input"
            />
          </div>
        )}

        {error && <div className="form-error">❌ {error}</div>}

        <div className="form-actions">
          <button className="btn" onClick={() => navigate('/')}>取消</button>
          <button
            className="btn btn-primary"
            onClick={handleSubmit}
            disabled={loading || (mode === 'local' ? !localPath : !remoteUrl)}
          >
            {loading ? '⏳ 添加中...' : '开始添加'}
          </button>
        </div>
      </div>
    </div>
  );
}
