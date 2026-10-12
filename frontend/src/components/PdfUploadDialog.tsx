// PdfUploadDialog — 从本地选择一个 PDF 文件 → 上传到后端 → 后端解析 +
// 走 LLM 整理为 Markdown → 写入 knowledge 表 → 列表自动刷新。
//
// 后端契约:
//   POST /api/knowledge/upload-pdf
//   multipart/form-data; 字段 file (PDF) + project_id
//   20 MB 单文件上限;非 PDF / 超大 → 400 / 413
//   成功 → 201 + KnowledgeItem JSON
//
// 关键校验前置到前端:
//   1. 后缀 + MIME 必须是 application/pdf (双保险: 部分 OS 报空 MIME);
//   2. file.size > 20 * 1024 * 1024 → 立即 toast,不发请求。
//
// i18n: 复用 knowledge.* 模块,不要新增模块名(详见
// frontend/src/i18n/locales/{zh-CN,en-US}.ts)。
import { useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { knowledgeApi, type KnowledgeItem } from '../api/client';
import { errorMessage } from '../utils/errMsg';
import './PdfUploadDialog.css';

const MAX_BYTES = 20 * 1024 * 1024; // 20 MB — 与后端 pdfUploadMaxMemory 对齐

interface Props {
  projectId: string;       // 必填,父组件从 selectedProject 传
  onClose: () => void;
  onSuccess: (item: KnowledgeItem) => void; // 上传成功后父组件刷新列表
}

export default function PdfUploadDialog({ projectId, onClose, onSuccess }: Props) {
  const { t } = useTranslation();
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const [file, setFile] = useState<File | null>(null);
  const [uploading, setUploading] = useState(false);
  const [errorText, setErrorText] = useState('');

  // 把字节数格式化为可读字符串 (1024 进制),用于 selected 提示。
  function fmtSize(bytes: number): string {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  }

  // 双保险:同时校验后缀与 MIME。OS 报上来的 MIME 偶有偏差,所以用 OR。
  function isPdf(f: File): boolean {
    if (f.type === 'application/pdf') return true;
    return f.name.toLowerCase().endsWith('.pdf');
  }

  function handleFileChange(e: React.ChangeEvent<HTMLInputElement>) {
    setErrorText('');
    const f = e.target.files?.[0] || null;
    if (!f) {
      setFile(null);
      return;
    }
    if (!isPdf(f)) {
      setErrorText(t('knowledge.uploadPdfInvalidType'));
      setFile(null);
      // 清空 input,允许用户重选同一文件
      if (fileInputRef.current) fileInputRef.current.value = '';
      return;
    }
    if (f.size > MAX_BYTES) {
      setErrorText(t('knowledge.uploadPdfTooLarge'));
      setFile(null);
      if (fileInputRef.current) fileInputRef.current.value = '';
      return;
    }
    setFile(f);
  }

  function handleSelectClick() {
    fileInputRef.current?.click();
  }

  async function handleSubmit() {
    if (!file) {
      setErrorText(t('knowledge.uploadPdfEmpty'));
      return;
    }
    setUploading(true);
    setErrorText('');
    try {
      const item = await knowledgeApi.uploadPdf(projectId, file);
      onSuccess(item);
      onClose();
    } catch (err) {
      // 失败时 dialog 保持打开,让用户重试或换文件
      setErrorText(t('knowledge.uploadPdfFailed', { msg: errorMessage(err) }));
    } finally {
      setUploading(false);
    }
  }

  return (
    <div className="pdf-upload-overlay modal-overlay" onClick={onClose}>
      <div
        className="pdf-upload-box modal-box modal-fullscreen"
        onClick={(e) => e.stopPropagation()}
        style={{ maxWidth: 480 }}
      >
        <h3>📄 {t('knowledge.uploadPdfTitle')}</h3>
        <div className="form-group">
          <p className="pdf-upload-hint">{t('knowledge.uploadPdfSelectHint')}</p>
        </div>
        <div className="form-group">
          <input
            ref={fileInputRef}
            type="file"
            accept="application/pdf,.pdf"
            onChange={handleFileChange}
            className="pdf-upload-file-input"
            aria-label={t('knowledge.uploadPdfSelectFile')}
          />
          <button
            type="button"
            className="btn"
            onClick={handleSelectClick}
            disabled={uploading}
          >
            {t('knowledge.uploadPdfSelectFile')}
          </button>
          {file && (
            <div className="pdf-upload-selected">
              {t('knowledge.uploadPdfSelected', { name: file.name, size: fmtSize(file.size) })}
            </div>
          )}
        </div>
        {errorText && <div className="pdf-upload-error">{errorText}</div>}
        <div className="form-actions btn-row-2col">
          <button className="btn" onClick={onClose} disabled={uploading}>
            {t('knowledge.dialog.cancel')}
          </button>
          <button
            className="btn btn-primary"
            onClick={handleSubmit}
            disabled={!file || uploading}
          >
            {uploading ? (
              <>
                <span className="pdf-upload-spinner" aria-hidden="true" />
                {t('knowledge.uploadPdfUploading')}
              </>
            ) : (
              t('knowledge.uploadPdf')
            )}
          </button>
        </div>
      </div>
    </div>
  );
}
