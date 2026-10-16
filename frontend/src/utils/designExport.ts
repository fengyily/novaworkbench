// Design-doc export utilities — shared between the PDF and Markdown paths.
//
// Why one builder for both: the previous layout had `designToMarkdown`,
// `parseDesign`, and `stripOuterFence` as locals inside RequirementDetail.tsx
// and only the PDF path consumed them. That made the Markdown export fork
// copy-paste the same string-assembly logic — and any future change to how
// plan-mode / legacy-JSON designs get reassembled would have to be applied
// in two places. Lifting them here gives both the PDF and the new .md
// download the same source-of-truth string, so they can never disagree.
//
// `parseDesign` and `stripOuterFence` are also imported by the wiki-doc
// helper inside RequirementDetail (parseWikiDoc), so the move widens the
// sharing without changing observable behavior.
//
// No import of `mermaid` / `marked` / `html2pdf.js` here on purpose — the
// Markdown download path is sync, and pulling the ~300KB PDF render stack
// into a chunk that doesn't need it would bloat every export click.
import i18next from 'i18next';

export interface DesignData {
  overview?: string;
  files?: string[];
  steps?: string[];
  model_changes?: string;
  risks?: string[];
  plan_markdown?: string; // plan-mode output (raw markdown, not the legacy JSON schema)
}

// Strip the outer ```lang ... ``` (or bare ``` ... ```) fence that Claude
// sometimes wraps its apply-doc output in. Only triggers when the entire
// trimmed string is exactly one fence pair, so a stray triple-backtick pair
// inside the body (a sample code block, say) is never touched.
const FENCE_RE = /^```[^\n]*\n([\s\S]*?)\n```\s*$/;

export function stripOuterFence(s: string): string {
  if (!s) return s;
  const m = FENCE_RE.exec(s.trim());
  return m ? m[1] : s;
}

// 设计文档存在多种存储形态:
//   - 计划模式 (新): 原始 Markdown 字符串
//   - legacy JSON 对象: {overview, files, steps, model_changes, risks}
//   - legacy JSON 数组: ["# 方案..."]
//   - 边缘情形 1: finalResult fallback 可能产出以 { 开头的非设计 JSON
//   - 边缘情形 2: Claude 在 apply-doc 输出被 ```markdown ... ``` 围栏包裹,
//     ReactMarkdown 会把整段当代码块渲染,需要 strip 外层围栏
//   - 边缘情形 3: schema 默认值 '[]' (backend/internal/db/schema.go:55)
//     在 designing 阶段仍携带字面量 '[]';任何解析结果若不含有效设计
//     内容,一律视为无设计,返回 {} —— 避免 hasDesign 误为 true 后
//     ReactMarkdown 把字面量渲染出来,也避免 doStartCoding 注入
//     `## 技术方案\n[]` 到开发 prompt。
//
// 仅当 JSON 解析结果显式携带 plan_markdown 或任意 legacy 字段,
// 才视为 DesignData;若为合法 JSON 但不带设计内容,统一返回 {}。
// 当作 Markdown 之前先剥掉外层代码围栏(只发生在 JSON parse 失败分支)。
export function parseDesign(raw: string): DesignData {
  if (!raw) return {};
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    // 不是合法 JSON:整段视为 Markdown(去除可能的代码围栏)。
    return { plan_markdown: stripOuterFence(raw) };
  }
  // legacy JSON 数组形态:["# 方案\n## 详情", ...]
  // 空数组 / 全空白元素 / 全非字符串元素 → 一律视为无设计。
  if (Array.isArray(parsed)) {
    const first = parsed.find((v) => typeof v === 'string' && v.trim() !== '');
    if (typeof first !== 'string') return {};
    return { plan_markdown: stripOuterFence(first) };
  }
  if (parsed !== null && typeof parsed === 'object') {
    const obj = parsed as Partial<DesignData>;
    const objHasShape = typeof obj.plan_markdown === 'string'
        || obj.overview !== undefined
        || obj.files !== undefined
        || obj.steps !== undefined
        || obj.model_changes !== undefined
        || obj.risks !== undefined;
    if (objHasShape) {
      if (typeof obj.plan_markdown === 'string') {
        // 空 / 全空白 plan_markdown → undefined,避免下游三元
        // 同时被误判 truthy,与 hasDesign 语义保持一致。
        const trimmed = obj.plan_markdown.trim();
        obj.plan_markdown = trimmed === '' ? undefined : stripOuterFence(trimmed);
      }
      return obj;
    }
  }
  // JSON 解析成功但不带设计内容:'{}' / '42' / 'null' / 'true' 等
  // 任何不携带 DesignData 字段的形态。返回 {} 而非把 raw 当 Markdown
  // 渲染,以防 hasDesign 误开启。
  return {};
}

// Compose the design as a Markdown string. plan_markdown is already raw
// Markdown; legacy JSON designs are reassembled into Markdown sections
// mirroring how doStartCoding builds the dev-instruction payload.
//
// Translated to use i18next directly (was t() in the component) so the
// function is callable from this utility without a React render context.
export function buildLegacyDesignMarkdown(d: DesignData): string {
  if (d.plan_markdown) return d.plan_markdown;
  const parts: string[] = [];
  if (d.overview) parts.push(`## ${i18next.t('requirements.detail2.designMdOverview')}\n${d.overview}`);
  if (d.steps?.length) parts.push(`## ${i18next.t('requirements.detail2.designMdSteps')}\n${d.steps.map((s, i) => `${i + 1}. ${s}`).join('\n')}`);
  if (d.files?.length) parts.push(`## ${i18next.t('requirements.detail2.designMdFiles')}\n${d.files.map(f => `- ${f}`).join('\n')}`);
  if (d.model_changes && d.model_changes !== i18next.t('requirements.detail2.designModelChangesNone')) parts.push(`## ${i18next.t('requirements.detail2.designModelChangesHeader')}\n${d.model_changes}`);
  if (d.risks?.length) parts.push(`## ${i18next.t('requirements.detail2.designMdRisks')}\n${d.risks.map(r => `- ${r}`).join('\n')}`);
  return parts.join('\n\n');
}

export interface DesignExportMarkdownInput {
  title: string;
  meta?: string; // e.g. "project-name · req_xxx"
  markdown: string;
  filename?: string; // unsanitized user-supplied title (also used for the download stem)
  // Filename component inserted between the (sanitized) filename and `.md` —
  // defaults to requirements.exportDesign.markdownSuffix ("技术方案" / "design").
  markdownSuffix?: string;
}

function sanitizeFilename(name: string, fallback: string): string {
  return name.replace(/[\\/:*?"<>|]/g, '_').slice(0, 80).trim() || fallback;
}

// Build the full Markdown document the user receives. Kept deliberately
// plain — no HTML, no CSS — so the same string also feeds the PDF path.
export function buildDesignMarkdown(input: DesignExportMarkdownInput): string {
  const { title, meta, markdown } = input;
  const lines: string[] = [`# ${title}`, ''];
  const metaLines: string[] = [];
  if (meta) metaLines.push(`- ${i18next.t('requirements.exportDesign.projectLabel')}: ${meta}`);
  metaLines.push(`- ${i18next.t('requirements.exportDesign.generatedAt')}: ${new Date().toISOString()}`);
  lines.push(metaLines.join('\n'));
  lines.push('');
  lines.push(markdown);
  return lines.join('\n') + '\n';
}

// Filename for the .md download. Mirrors exportDesignPdf's own sanitiser
// (`_` for bad chars, not `-`) so the .md and .pdf of one design land next
// to each other alphabetically in a downloads folder.
export function designExportFilename(input: DesignExportMarkdownInput, ext: 'md'): string {
  const suffix = input.markdownSuffix ?? i18next.t('requirements.exportDesign.markdownSuffix');
  const fallback = i18next.t('requirements.exportDesign.designFallback');
  const stem = sanitizeFilename(input.filename || input.title, fallback);
  return `${stem}-${suffix}.${ext}`;
}

// Download a built Markdown document as a `.md` file. Clones
// downloadReviewMarkdown's lifecycle (Blob + dynamic <a> + delayed
// revokeObjectURL) so they behave identically.
export function downloadDesignMarkdown(input: DesignExportMarkdownInput): void {
  const markdown = buildDesignMarkdown(input);
  const blob = new Blob([markdown], { type: 'text/markdown;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = designExportFilename(input, 'md');
  document.body.appendChild(a);
  a.click();
  a.remove();
  // Revoking immediately can race the download in Safari; one tick is
  // enough and the object is small either way.
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
