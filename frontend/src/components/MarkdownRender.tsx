// MarkdownRender — NovaWorkbench 共享 Markdown 渲染组件。
//
// 单一来源包揽全应用 markdown 渲染（knowledge / wiki / design-doc / 子任务回复/
// weekly report / PDF 导出）。在原 react-markdown + remark-gfm 的基础上，再
// 增加一个轻量级 Mermaid 处理：监听 `code` 组件的 className，遇
// `language-mermaid` 就走 mermaid.render() 出内联 SVG，零临时文件。
//
// 不走 rehype-mermaid 管线，而是直接 override `code`：依赖更轻、SSR/CSR 行为
// 更可控、错误处理更直接（可以在客户端 fallback 到 <pre> 文本而不是把整页卡
// 死）。Mermaid 懒初始化一次 + 主题跟随系统色。
//
// 同步在 `img` 组件上加 click-to-zoom + 下载 PNG/JPG：知识库 / 设计文档里
// 的截图默认压缩到容器宽度，无法看清细节；点击进入全屏查看，悬浮工具栏
// 提供 PNG / JPG 下载。
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import mermaid from 'mermaid';
import './MarkdownRender.css';

let mermaidReady = false;
function ensureMermaid() {
  if (mermaidReady) return;
  mermaid.initialize({
    startOnLoad: false,
    theme: 'neutral',
    securityLevel: 'loose',
    fontFamily: 'inherit',
  });
  mermaidReady = true;
}

let mermaidSerial = 0;
function nextMermaidId(): string {
  mermaidSerial += 1;
  return `mermaid-${Date.now().toString(36)}-${mermaidSerial}`;
}

// MermaidCode — react-markdown 的 code 组件 override。识别 ```mermaid 块，渲
// 染为内联 SVG；其他语言走默认 <code>；`inline`（行内 `code`）始终走默认 <code>。
function MermaidCode(props: {
  inline?: boolean;
  className?: string;
  children?: React.ReactNode;
}) {
  const { inline, className, children } = props;
  if (inline) {
    return <code className={className}>{children}</code>;
  }
  const match = /language-(\w+)/.exec(className || '');
  const lang = match ? match[1] : '';
  if (lang !== 'mermaid') {
    return <code className={className}>{children}</code>;
  }
  return <MermaidBlock source={String(children).replace(/\n$/, '')} />;
}

// MermaidBlock — 客户端异步 mermaid.render() 出 SVG，挂到 DOM。
// 错误的代码块不会让整页崩 — 走 <pre className="mermaid-error">。
// 整个 mermaid-svg 容器是可点击 + 可下载的：点击区域走全屏浮层，
// 工具栏提供 PNG / JPG / SVG 三种格式下载。
function MermaidBlock({ source }: { source: string }) {
  const [id] = useState(() => nextMermaidId());
  const [svg, setSvg] = useState<string>('');
  const [error, setError] = useState<string>('');

  useEffect(() => {
    let cancelled = false;
    ensureMermaid();
    mermaid
      .render(id, source)
      .then(({ svg: out }) => {
        if (!cancelled) setSvg(out);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(String((e as Error)?.message || e));
      });
    return () => {
      cancelled = true;
    };
  }, [id, source]);

  if (error) {
    return (
      <pre className="mermaid-error" title={error}>
        {source}
      </pre>
    );
  }
  if (!svg) {
    return <pre className="mermaid-loading">{source}</pre>;
  }
  return (
    <ImageFigure svgSrc={svg} alt="Mermaid diagram" filenameBase="mermaid-diagram">
      <div
        className="mermaid-svg"
        dangerouslySetInnerHTML={{ __html: svg }}
      />
    </ImageFigure>
  );
}

// ImageFigure — 给任意"图块"（普通 <img> 或 mermaid SVG）统一的：点击放大 +
// 工具栏下载 PNG/JPG（+ SVG 给 mermaid）。children 渲染在 <figure> 内，
// 工具栏以 .image-toolbar 浮在右下角；整张 <figure> 可点击触发全屏浮层。
function ImageFigure({
  src,
  svgSrc,
  alt,
  filenameBase,
  children,
}: {
  src?: string;
  svgSrc?: string;
  alt: string;
  filenameBase: string;
  children: React.ReactNode;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false);
    };
    document.addEventListener('keydown', onKey);
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    return () => {
      document.removeEventListener('keydown', onKey);
      document.body.style.overflow = prevOverflow;
    };
  }, [open]);

  const downloadBlob = (blob: Blob, ext: string) => {
    const url = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = url;
    link.download = `${filenameBase}.${ext}`;
    document.body.appendChild(link);
    link.click();
    document.body.removeChild(link);
    // 给浏览器一点时间发起下载再回收；过早 revoke 会让 Safari 取消下载。
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  };

  const downloadPng = async () => {
    try {
      const blob = await renderToPngBlob(src, svgSrc);
      if (blob) downloadBlob(blob, 'png');
    } catch (e: unknown) {
      alert(t('components.markdownImage.downloadFailPrefix') + String((e as Error)?.message || e));
    }
  };

  const downloadJpg = async () => {
    try {
      const blob = await renderToJpgBlob(src, svgSrc);
      if (blob) downloadBlob(blob, 'jpg');
    } catch (e: unknown) {
      alert(t('components.markdownImage.downloadFailPrefix') + String((e as Error)?.message || e));
    }
  };

  const downloadSvg = () => {
    if (!svgSrc) return;
    const blob = new Blob([svgSrc], { type: 'image/svg+xml;charset=utf-8' });
    downloadBlob(blob, 'svg');
  };

  return (
    <>
      <figure className="image-figure" onClick={() => setOpen(true)}>
        {children}
        <div className="image-toolbar" onClick={(e) => e.stopPropagation()}>
          <button
            type="button"
            className="image-toolbar-btn"
            onClick={() => setOpen(true)}
            title={t('components.markdownImage.zoomTitle')}
          >
            ⛶
          </button>
          <span className="image-toolbar-sep" />
          <button
            type="button"
            className="image-toolbar-btn"
            onClick={downloadPng}
            title={t('components.markdownImage.downloadPngTitle')}
          >
            PNG
          </button>
          <button
            type="button"
            className="image-toolbar-btn"
            onClick={downloadJpg}
            title={t('components.markdownImage.downloadJpgTitle')}
          >
            JPG
          </button>
          {svgSrc && (
            <button
              type="button"
              className="image-toolbar-btn"
              onClick={downloadSvg}
              title={t('components.markdownImage.downloadSvgTitle')}
            >
              SVG
            </button>
          )}
        </div>
      </figure>

      {open && (
        <div
          className="img-zoom-overlay"
          role="dialog"
          aria-modal="true"
          onClick={() => setOpen(false)}
        >
          {/* 普通图片走 <img>；mermaid SVG 走 dangerouslySetInnerHTML。
              两者互斥（src 与 svgSrc 只会有一个），避免渲染空 src 的破图。 */}
          {src && (
            <img
              className="img-zoom-target"
              src={src}
              alt={alt}
              onClick={(e) => e.stopPropagation()}
            />
          )}
          {svgSrc && !src && (
            <div
              className="img-zoom-target img-zoom-svg"
              dangerouslySetInnerHTML={{ __html: svgSrc }}
              onClick={(e) => e.stopPropagation()}
            />
          )}
        </div>
      )}
    </>
  );
}

// ZoomableImage — 普通 markdown 图片：onClick 走 ImageFigure 全屏浮层 + 工具栏。
function ZoomableImage(props: {
  src?: string;
  alt?: string;
  title?: string;
}) {
  const { src, alt, title } = props;
  if (!src) return null;
  // 用 alt / title 推断文件名 base，去后缀 + sanitize；空时用图片序号时间戳。
  const guess = (title || alt || '').replace(/\.[a-z0-9]+$/i, '');
  const safeBase = guess.replace(/[\\/:*?"<>|]/g, '_').trim() || `image-${Date.now()}`;
  return (
    <ImageFigure
      src={src}
      alt={alt || ''}
      filenameBase={safeBase}
    >
      <img
        className="zoomable-image"
        src={src}
        alt={alt || ''}
        title={title || alt || ''}
        loading="lazy"
      />
    </ImageFigure>
  );
}

// ─── PNG / JPG 渲染工具 ───────────────────────────────────────
//
// 普通图片（PNG/JPG/GIF/WebP）：<img>.src → Image → canvas → toBlob。
// Mermaid SVG：先烘成 blob URL 让浏览器当图片解码，再画到 canvas。
//
// JPG 必须先铺白底 —— canvas 默认透明，直接编码成 JPEG 会把透明区变成黑块；
// PNG 保留透明。
const SVG_RASTER_SCALE = 2; // 2x 超采样，让导出的位图在 retina 上不糊

async function renderToBlob(
  src: string | undefined,
  svgSrc: string | undefined,
  format: 'png' | 'jpeg',
): Promise<Blob | null> {
  if (!src && !svgSrc) return null;
  const canvas = document.createElement('canvas');
  const ctx = canvas.getContext('2d');
  if (!ctx) return null;

  if (src) {
    const img = await loadImage(src);
    canvas.width = img.naturalWidth || img.width;
    canvas.height = img.naturalHeight || img.height;
    if (format === 'jpeg') {
      ctx.fillStyle = '#ffffff';
      ctx.fillRect(0, 0, canvas.width, canvas.height);
    }
    ctx.drawImage(img, 0, 0);
  } else if (svgSrc) {
    const { doc, width, height } = await decodeSvg(svgSrc);
    canvas.width = Math.round(width * SVG_RASTER_SCALE);
    canvas.height = Math.round(height * SVG_RASTER_SCALE);
    if (format === 'jpeg') {
      ctx.fillStyle = '#ffffff';
      ctx.fillRect(0, 0, canvas.width, canvas.height);
    }
    ctx.drawImage(doc, 0, 0, canvas.width, canvas.height);
  }

  return await new Promise<Blob | null>((resolve) =>
    canvas.toBlob(resolve, format === 'png' ? 'image/png' : 'image/jpeg', 0.92),
  );
}

function renderToPngBlob(src?: string, svgSrc?: string) {
  return renderToBlob(src, svgSrc, 'png');
}

function renderToJpgBlob(src?: string, svgSrc?: string) {
  return renderToBlob(src, svgSrc, 'jpeg');
}

function loadImage(src: string): Promise<HTMLImageElement> {
  return new Promise((resolve, reject) => {
    const img = new Image();
    img.crossOrigin = 'anonymous';
    img.onload = () => resolve(img);
    img.onerror = (e) =>
      reject(new Error('图片加载失败（可能是跨域）：' + (e?.toString?.() || '')));
    img.src = src;
  });
}

// decodeSvg — 把 mermaid 的 <svg> 字符串 decode 成可 drawImage 的图片。
//
// 尺寸来源必须是 viewBox，不能用 width 属性：mermaid v11 在 <svg> 根上写
// `width="100%"` + 内联 `style="max-width: {natural}px"`（见 mermaid 的
// chunk-7R4GIKGN.mjs），按 width 属性解析会把 "100%" 里的 100 当像素宽度，
// 导出的位图尺寸完全错。viewBox 的第三/四位才是真实逻辑尺寸。
async function decodeSvg(
  svgStr: string,
): Promise<{ doc: HTMLImageElement; width: number; height: number }> {
  const blob = new Blob([svgStr], { type: 'image/svg+xml;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  try {
    const doc = await new Promise<HTMLImageElement>((resolve, reject) => {
      const img = new Image();
      img.onload = () => resolve(img);
      img.onerror = () => reject(new Error('SVG 渲染失败'));
      img.src = url;
    });
    const { width, height } = svgIntrinsicSize(svgStr, doc);
    return { doc, width, height };
  } finally {
    URL.revokeObjectURL(url);
  }
}

// svgIntrinsicSize — viewBox 优先，其次 naturalWidth/Height，最后兜底
// 800×600。viewBox 也可能缺失（手写 SVG），所以两级 fallback 都留着。
function svgIntrinsicSize(svgStr: string, doc: HTMLImageElement): { width: number; height: number } {
  const vb = svgStr.match(/\bviewBox\s*=\s*["']([^"']+)["']/i)?.[1];
  if (vb) {
    const parts = vb.trim().split(/[\s,]+/).map(Number);
    if (parts.length === 4 && parts[2] > 0 && parts[3] > 0) {
      return { width: parts[2], height: parts[3] };
    }
  }
  if (doc.naturalWidth > 0 && doc.naturalHeight > 0) {
    return { width: doc.naturalWidth, height: doc.naturalHeight };
  }
  return { width: 800, height: 600 };
}

interface Props {
  content: string;
  components?: Record<string, React.ComponentType<any>>;
}

export default function MarkdownRender({ content, components }: Props) {
  // 顺序很重要：调用方传入的 components 必须能 override 默认的 MermaidCode
  // / ZoomableImage，这里把默认值放前面。
  const merged = { code: MermaidCode, img: ZoomableImage, ...components };
  return (
    <ReactMarkdown remarkPlugins={[remarkGfm]} components={merged}>
      {content}
    </ReactMarkdown>
  );
}