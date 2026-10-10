// One-click export of a technical design doc (Markdown) to PDF.
//
// The design is stored as plan-mode Markdown in requirements.design_docs (and
// knowledge entries as Markdown in `knowledge.content`). We pre-bake any
// ` ```mermaid ... ``` ` blocks into inline SVG via mermaid.render() and then
// use `marked.parse()` — sync, no React, no rehype pipeline — to turn the
// resulting markdown + inline SVG into an HTML string. That string is dropped
// into a print-styled container with concrete (non-CSS-var) colors so
// html2canvas can snapshot it without surprises, and html2pdf.js produces an
// A4 PDF.
//
// Why marked (not react-markdown + renderToStaticMarkup)?
//   - markdown content can contain raw mermaid SVG blocks (and design docs
//     often embed other HTML like <kbd>/<details>) — react-markdown 9 escapes
//     inline HTML by default and would render the SVG as escaped source.
//   - renderToStaticMarkup + react-markdown in React 19 SSR mode had
//     compatibility gaps (hooks-heavy components could return empty markup),
//     which surfaced as blank PDFs until we switched to marked.
//   - marked gives us sync output, inline HTML passes through unchanged, and
//     the output feeds straight into a hidden container that html2pdf
//     snapshots. No SSR, no React DOM tree for a single one-shot render.
//
// Mermaid is async + browser-only, so we still pre-render all mermaid blocks
// to <svg> strings up front and substitute them back into the markdown before
// handing it to marked. Failed renders fall back to a styled <pre> so the PDF
// still includes the source for debugging.
import i18next from 'i18next';
import { marked } from 'marked';
import mermaid from 'mermaid';

export interface DesignExportInput {
  title: string;
  meta?: string; // e.g. "project-name · req_xxx"
  markdown: string;
  filename?: string;
  // Optional filename component inserted between the (sanitized) filename and
  // `.pdf` — the design-doc exporter defaults to "技术方案" via
  // requirements.exportDesign.pdfSuffix; the knowledge-base exporter overrides
  // with "知识条目" so the download looks like "<title>-知识条目.pdf".
  // Same string is used as the empty-filename fallback.
  pdfLabel?: string;
}

// Matches a fenced ```mermaid ... ``` block (non-greedy, dotall). The first
// capture group is the mermaid source body. Used by prebakeMermaid below.
const MERMAID_FENCE = /```mermaid\s*\n([\s\S]*?)```/g;

// Inline, self-contained print styles. Concrete values (not var(--…)) because
// html2canvas snapshots computed styles and some CSS-variable color spaces
// (oklch etc.) can break it. Mirrors .analysis-summary on screen.
//
// Important for mermaid: the <svg> string returned by mermaid.render uses its
// own internal class names (e.g. `.label`, `.nodeLabel`) and comes with a
// <style> block embedded inside the svg. We force text color via inline
// `style="color: #1e293b"` on the svg root so html2canvas doesn't drop the
// text on light backgrounds.
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
  .pdf-doc img { max-width: 100%; height: auto; display: block; margin: 8px auto; }
  .pdf-doc .pdf-mermaid { background: #f7f8fb; border: 1px solid #e5e7eb; border-radius: 6px; padding: 12px; margin: 10px 0; text-align: center; overflow-x: auto; }
  .pdf-doc .pdf-mermaid svg { max-width: 100%; height: auto; color: #1e293b; }
  .pdf-doc .pdf-mermaid-error { background: #fef2f2; border: 1px solid #fecaca; color: #991b1b; text-align: left; }
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

function sanitizeFilename(name: string, fallback: string): string {
  return name.replace(/[\\/:*?"<>|]/g, '_').slice(0, 80).trim() || fallback;
}

// mermaidReady — single-init flag mirroring MarkdownRender.tsx. The PDF path
// can be entered before any on-screen MarkdownRender has mounted, so this
// helper makes sure mermaid.initialize runs at least once.
let mermaidReady = false;
function ensureMermaid() {
  if (mermaidReady) return;
  mermaid.initialize({ startOnLoad: false, theme: 'neutral', securityLevel: 'loose', fontFamily: 'inherit' });
  mermaidReady = true;
}

let mermaidSerial = 0;
function nextMermaidId(): string {
  mermaidSerial += 1;
  return `pdf-mermaid-${Date.now().toString(36)}-${mermaidSerial}`;
}

// prebakeMermaid scans markdown for ```mermaid ... ``` blocks, renders each
// one to inline SVG via mermaid.render(), and rewrites the markdown so the
// marked pass sees a stream of inline HTML (no async fetches at export time).
// A failed render is gracefully degraded to a styled <pre> code block — the
// PDF still works, just without a diagram.
async function prebakeMermaid(markdown: string): Promise<string> {
  if (!/```mermaid/i.test(markdown)) return markdown;
  ensureMermaid();
  const matches: { match: string; svg: string }[] = [];
  const re = new RegExp(MERMAID_FENCE.source, 'g');
  let m: RegExpExecArray | null;
  while ((m = re.exec(markdown)) !== null) {
    const src = m[1];
    const id = nextMermaidId();
    try {
      const { svg } = await mermaid.render(id, src);
      // mermaid.render's output ends with or without a trailing newline; trim
      // so our wrapper div sits flush.
      matches.push({ match: m[0], svg: svg.trim() });
    } catch (e: unknown) {
      const msg = String((e as Error)?.message || e);
      const errHtml = `<div class="pdf-mermaid pdf-mermaid-error"><pre>${escapeHtml(src)}\n\n[render error: ${escapeHtml(msg)}]</pre></div>`;
      matches.push({ match: m[0], svg: errHtml });
    }
  }
  let out = markdown;
  for (const { match, svg } of matches) {
    out = out.replace(match, `<div class="pdf-mermaid">${svg}</div>`);
  }
  return out;
}

export async function exportDesignPdf(input: DesignExportInput): Promise<void> {
  const { title, meta, markdown, pdfLabel } = input;
  // Caller-provided label wins so different surfaces (design doc / knowledge
  // entry / future report) get a recognizable filename suffix without forking
  // the i18n key namespace.
  const label = pdfLabel ?? i18next.t('requirements.exportDesign.pdfSuffix');
  const fallback = pdfLabel ?? i18next.t('requirements.exportDesign.designFallback');
  const filename = `${sanitizeFilename(input.filename || title, fallback)}-${label}.pdf`;

  // Step 1: pre-bake any mermaid blocks into inline SVG so the markdown pass
  // sees only static HTML for diagrams.
  const prebaked = await prebakeMermaid(markdown);

  // Step 2: marked.parse is sync and lets inline HTML (our pre-baked SVG
  // containers) pass through verbatim. gfm: true enables tables / task lists /
  // strikethrough / autolinks, matching what on-screen MarkdownRender shows
  // via remark-gfm. breaks:true so long single lines don't overflow the page
  // width (kept from the legacy SSR implementation).
  const bodyHtml = marked.parse(prebaked, {
    gfm: true,
    breaks: false,
    async: false,
  }) as string;

  const stamp = new Date().toLocaleString(i18next.language || 'zh-CN', { hour12: false });

  // The container MUST be inside the rendered viewport (top: 0 + left: 0) —
  // html2canvas snapshots painted geometry, and a container parked at
  // left: -99999px (the previous SSR-era approach) was correctly invisible to
  // the canvas pass on some browsers, producing blank PDFs. Keeping it
  // off-screen via opacity:0 + pointer-events:none still lets html2canvas read
  // computed styles + layout.
  const container = document.createElement('div');
  container.className = 'pdf-export-stage';
  container.style.position = 'fixed';
  container.style.left = '0';
  container.style.top = '0';
  container.style.width = '780px';
  container.style.zIndex = '-1';
  container.style.opacity = '0';
  container.style.pointerEvents = 'none';
  container.style.background = '#ffffff';
  container.innerHTML = `
    <style>${STYLES}</style>
    <div class="pdf-doc">
      <div class="pdf-header">
        <h1>${escapeHtml(title)}</h1>
        <div class="pdf-meta">${meta ? escapeHtml(meta) + ' · ' : ''}${escapeHtml(i18next.t('requirements.exportDesign.generatedAt'))} ${escapeHtml(stamp)}</div>
      </div>
      <div class="pdf-body">${bodyHtml}</div>
    </div>
  `;
  document.body.appendChild(container);

  try {
    // Lazy-load html2pdf (it bundles html2canvas + jsPDF, ~600 KB) so it only
    // enters a separate chunk when the user actually clicks export.
    const { default: html2pdf } = await import('html2pdf.js');
    const worker = html2pdf();
    // html2pdf.js v0.14's bundled types omit `pagebreak` (supported at
    // runtime), so the options object is cast past excess-property checks.
    type Options = Parameters<typeof worker.set>[0] & { pagebreak?: { mode?: string[] } };
    await worker
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
      } as Options)
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