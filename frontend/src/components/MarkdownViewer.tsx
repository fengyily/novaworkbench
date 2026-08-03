import { useEffect } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import './MarkdownViewer.css';

interface Props {
  title?: string;
  content: string;
  onClose: () => void;
}

export default function MarkdownViewer({ title, content, onClose }: Props) {
  // Close on Escape
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose]);

  return (
    <div className="md-viewer-overlay" onClick={onClose}>
      <div className="md-viewer-box" onClick={e => e.stopPropagation()}>
        <div className="md-viewer-header">
          <h3>{title || '文档预览'}</h3>
          <button className="btn btn-sm" onClick={onClose} title="关闭 (Esc)">✕ 关闭</button>
        </div>
        <div className="md-viewer-body">
          <ReactMarkdown remarkPlugins={[remarkGfm]}>{content}</ReactMarkdown>
        </div>
      </div>
    </div>
  );
}
