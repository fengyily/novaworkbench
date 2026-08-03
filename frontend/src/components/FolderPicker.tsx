import { useState, useEffect, useCallback } from 'react';
import { API_BASE } from '../api/client';
import './FolderPicker.css';

interface FileItem {
  name: string;
  path: string;
  is_dir: boolean;
  is_git: boolean;
  size: number;
}

interface Props {
  value: string;
  onChange: (path: string) => void;
}

export default function FolderPicker({ value, onChange }: Props) {
  const [currentPath, setCurrentPath] = useState<string>('');
  const [items, setItems] = useState<FileItem[]>([]);
  const [breadcrumb, setBreadcrumb] = useState<FileItem[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    // Try home dir first
    listDir('~');
  }, []);

  const listDir = useCallback(async (path: string) => {
    setLoading(true);
    setError(null);
    try {
      const res = await fetch(`${API_BASE}/api/fs/ls?path=${encodeURIComponent(path)}`);
      const json = await res.json();
      if (!json.success) throw new Error(json.error?.message || 'Failed');
      setCurrentPath(json.data.current);
      setItems(json.data.items || []);
      setBreadcrumb(json.data.breadcrumb || []);
    } catch (err: any) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  }, []);

  const handleSelect = (item: FileItem) => {
    if (item.is_dir) {
      listDir(item.path);
    }
  };

  const handlePick = (item: FileItem) => {
    if (item.is_git) {
      onChange(item.path);
    }
  };

  // If user has typed a path, validate it
  const handleInputChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const p = e.target.value;
    onChange(p);
    // Try to navigate to parent
    const parent = p.lastIndexOf('/') > 0 ? p.substring(0, p.lastIndexOf('/')) : '/';
    if (parent !== currentPath) {
      listDir(parent);
    }
  };

  return (
    <div className="folder-picker">
      {/* Breadcrumb */}
      <div className="fp-breadcrumb">
        {breadcrumb.map((b, i) => (
          <span key={b.path}>
            {i > 0 && <span className="fp-sep">›</span>}
            <button className="fp-crumb" onClick={() => listDir(b.path)}>
              {b.name}
            </button>
          </span>
        ))}
      </div>

      {/* Path input */}
      <div className="fp-input-row">
        <input
          type="text"
          className="form-input"
          value={value}
          onChange={handleInputChange}
          placeholder="路径: ~/workspace/my-project"
        />
      </div>

      {/* File list */}
      <div className="fp-list">
        {loading && <div className="fp-loading">⏳ 加载中...</div>}
        {error && <div className="fp-error">❌ {error}</div>}

        {!loading && !error && items.length === 0 && (
          <div className="fp-empty">目录为空</div>
        )}

        {items.map(item => (
          <div
            key={item.path}
            className={`fp-item ${item.is_git ? 'is-git' : ''} ${item.is_dir ? 'is-dir' : ''} ${item.name === '..' ? 'is-parent' : ''}`}
            onClick={() => handleSelect(item)}
            onDoubleClick={() => item.is_git && handlePick(item)}
          >
            <span className="fp-icon">
              {!item.is_dir ? '📄' : item.is_git ? '🔷' : item.name === '..' ? '↩' : '📁'}
            </span>
            <span className="fp-name">{item.name}</span>
            {item.is_git && <span className="fp-git-badge">Git</span>}
            {item.is_git && (
              <button
                className="fp-pick-btn btn btn-primary btn-sm"
                onClick={(e) => { e.stopPropagation(); handlePick(item); }}
              >
                选择
              </button>
            )}
          </div>
        ))}
      </div>

      {value && (
        <div className="fp-selected">
          已选择: <code>{value}</code>
        </div>
      )}
    </div>
  );
}
