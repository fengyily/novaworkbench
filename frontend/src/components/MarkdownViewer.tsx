import { useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import './MarkdownViewer.css';

interface Props {
  title?: string;
  content: string;
  onClose: () => void;
}

export default function MarkdownViewer({ title, content, onClose }: Props) {
  const { t } = useTranslation();
  // Close on Escape
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose]);

  return (
    <div className="md-viewer-overlay modal-fullscreen-overlay" onClick={onClose}>
      <div className="md-viewer-box modal-fullscreen" onClick={e => e.stopPropagation()}>
        <div className="md-viewer-header">
          <h3>{title || t('components.markdownViewer.preview')}</h3>
          <button className="btn btn-sm" onClick={onClose} title={t('components.markdownViewer.closeTitle')}>{t('components.markdownViewer.close')}</button>
        </div>
        <div className="md-viewer-body">
          <ReactMarkdown remarkPlugins={[remarkGfm]}>{content}</ReactMarkdown>
        </div>
      </div>
    </div>
  );
}
