// One-click export of a technical design doc (Markdown) to PDF.
//
// The design is stored as plan-mode Markdown in requirements.design_docs.
// We render it to static HTML via react-markdown (same renderer the page uses),
// wrap it in a print-styled container with concrete colors (no CSS vars, so
// html2canvas — which html2pdf uses under the hood — renders reliably), and
// hand it to html2pdf.js to produce an A4 PDF download.
import { renderToStaticMarkup } from 'react-dom/server';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';

export interface DesignExportInput {
  title: string;
  meta?: string; // e.g. "项目名 · req_xxx"
  markdown: string;
  filename?: string;
}

// Inline, self-contained print styles. Concrete values (not var(--…)) because
// html2canvas snapshots computed styles and some CSS-variable color spaces
// (oklch etc.) can break it. Mirrors .analysis-summary on screen.
const STYLES = `
  .pdf-doc { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', 'PingFang SC', 'Microsoft YaHei', sans-serif; color: #1e293b; line-height: 1.7; font-size: 14px; background: #fff; }
  .pdf-doc .pdf-header { border-bottom: 2px solid #4F46E5; padding-bottom: 12px; margin-bottom: 18px; }
  .pdf-doc .pdf-header h1 { font-size: 20px; font-weight: 700; margin: 0 0 6px; color: #0f172a; }
  .pdf-doc .pdf-header .pdf-meta { font-size: 12px; color: #64748b; }
  .pdf-doc .pdf-body > :first-child { margin-top: 0; }
  .pdf-doc .pdf-body > :last-child { margin-bottom: 0; }
  .pdf-doc p { margin: 8px 0; }
  .pdf-doc ul, .pdf-doc ol { padding-left: 22px; margin: 8px 0; }
  .pdf-doc li { margin: 3px 0; }
  .pdf-doc strong { font-weight: 600; color: #0f172a; }
  .pdf-doc h1 { font-size: 20px; font-weight: 700; margin: 20px 0 10px; color: #0f172a; }
  .pdf-doc h2 { font-size: 17px; font-weight: 600; margin: 18px 0 8px; border-bottom: 1px solid #e2e8f0; padding-bottom: 4px; color: #0f172a; }
  .pdf-doc h3 { font-size: 15px; font-weight: 600; margin: 14px 0 6px; color: #0f172a; }
  .pdf-doc h4 { font-size: 14px; font-weight: 600; margin: 12px 0 6px; color: #0f172a; }
  .pdf-doc code { background: #f1f5f9; padding: 1px 5px; border-radius: 4px; font-family: 'SF Mono', Menlo, Consolas, monospace; font-size: 12.5px; color: #0f172a; }
  .pdf-doc pre { background: #f8fafc; border: 1px solid #e2e8f0; border-radius: 6px; padding: 12px; overflow-x: auto; margin: 10px 0; }
  .pdf-doc pre code { background: none; padding: 0; font-size: 12.5px; color: #1e293b; }
  .pdf-doc blockquote { border-left: 3px solid #c7d2fe; color: #475569; margin: 10px 0; padding: 2px 14px; }
  .pdf-doc table { border-collapse: collapse; margin: 10px 0; width: 100%; }
  .pdf-doc th, .pdf-doc td { border: 1px solid #e2e8f0; padding: 6px 10px; text-align: left; }
  .pdf-doc th { background: #f1f5f9; font-weight: 600; }
  .pdf-doc a { color: #4F46E5; text-decoration: underline; }
  .pdf-doc hr { border: none; border-top: 1px solid #e2e8f0; margin: 16px 0; }
  /* Keep block-level content from being split across a page boundary, so a
   * line of text never lands half on one page and half on the next. html2pdf
   * honors these (in 'css'/'avoid-all' pagebreak mode) by re-rendering each
   * page rather than slicing one tall canvas image. */
  .pdf-doc p, .pdf-doc li, .pdf-doc pre, .pdf-doc blockquote,
  .pdf-doc h1, .pdf-doc h2, .pdf-doc h3, .pdf-doc h4,
  .pdf-doc tr, .pdf-doc img, .pdf-doc ul, .pdf-doc ol {
    break-inside: avoid;
    page-break-inside: avoid;
  }
`;

function sanitizeFilename(name: string): string {
  return name.replace(/[\\/:*?"<>|]/g, '_').slice(0, 80).trim() || '技术方案';
}

export async function exportDesignPdf(input: DesignExportInput): Promise<void> {
  const { title, meta, markdown } = input;
  const filename = `${sanitizeFilename(input.filename || title)}-技术方案.pdf`;

  const body = renderToStaticMarkup(
    <ReactMarkdown remarkPlugins={[remarkGfm]}>{markdown}</ReactMarkdown>,
  );

  const stamp = new Date().toLocaleString('zh-CN', { hour12: false });

  const container = document.createElement('div');
  container.style.position = 'fixed';
  container.style.left = '-99999px';
  container.style.top = '0';
  container.style.width = '780px';
  container.style.background = '#ffffff';
  container.innerHTML = `
    <style>${STYLES}</style>
    <div class="pdf-doc">
      <div class="pdf-header">
        <h1>${escapeHtml(title)}</h1>
        <div class="pdf-meta">${meta ? escapeHtml(meta) + ' · ' : ''}生成于 ${escapeHtml(stamp)}</div>
      </div>
      <div class="pdf-body">${body}</div>
    </div>
  `;
  document.body.appendChild(container);

  try {
    // Lazy-load html2pdf (it bundles html2canvas + jsPDF, ~600 KB) so it only
    // enters a separate chunk when the user actually clicks export.
    const { default: html2pdf } = await import('html2pdf.js');
    await html2pdf()
      .set({
        margin: [12, 12, 14, 12],
        filename,
        image: { type: 'jpeg', quality: 0.98 },
        html2canvas: { scale: 2, useCORS: true, backgroundColor: '#ffffff' },
        jsPDF: { unit: 'mm', format: 'a4', orientation: 'portrait' },
        enableLinks: true,
        // Drive pagination by element boundaries instead of slicing one tall
        // canvas at fixed pixel offsets — the cause of lines cut in half
        // across pages. 'avoid-all' keeps every element intact, 'css' honors
        // the break-inside: avoid rules in STYLES, 'legacy' is a fallback that
        // still splits at element edges when a block is taller than a page.
        pagebreak: { mode: ['avoid-all', 'css', 'legacy'] },
      })
      .from(container.querySelector('.pdf-doc') as HTMLElement)
      .save();
  } finally {
    container.remove();
  }
}

function escapeHtml(s: string): string {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}
