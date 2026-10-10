// DiagramPreview — 受控的 Mermaid 实时预览组件。
//
// 与 MarkdownRender 里自动渲染 ```mermaid 块不同，本组件走人工调用：
// 接收 source 字符串 → 客户端 mermaid.render() → 内联 SVG。供
// DiagramGenerator 在 Dialog 内右侧预览面板使用。无需后端、无临时文件。
//
// 错误路径走 <pre className="mermaid-error">（与 MarkdownRender 视觉一
// 致），源语法问题不会让 Dialog 崩。
import { useEffect, useState } from 'react';
import mermaid from 'mermaid';

let ready = false;
function ensureMermaid() {
  if (ready) return;
  mermaid.initialize({ startOnLoad: false, theme: 'neutral', securityLevel: 'loose', fontFamily: 'inherit' });
  ready = true;
}

let previewSerial = 0;
function nextId(): string {
  previewSerial += 1;
  return `diagram-preview-${Date.now().toString(36)}-${previewSerial}`;
}

interface Props {
  source: string;
  className?: string;
}

export default function DiagramPreview({ source, className }: Props) {
  const [id] = useState(nextId);
  const [svg, setSvg] = useState<string>('');
  const [error, setError] = useState<string>('');

  useEffect(() => {
    let cancelled = false;
    const trimmed = source.trim();
    if (!trimmed) {
      setSvg('');
      setError('');
      return () => {
        cancelled = true;
      };
    }
    ensureMermaid();
    mermaid
      .render(id, trimmed)
      .then(({ svg: out }) => {
        if (!cancelled) {
          setSvg(out);
          setError('');
        }
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
      <pre className={`mermaid-error ${className || ''}`} title={error}>
        {source}
      </pre>
    );
  }
  if (!source.trim()) {
    return <div className={`mermaid-loading ${className || ''}`}>等待文本预览…</div>;
  }
  if (!svg) {
    return <pre className={`mermaid-loading ${className || ''}`}>{source}</pre>;
  }
  return (
    <div
      className={`mermaid-svg ${className || ''}`}
      dangerouslySetInnerHTML={{ __html: svg }}
    />
  );
}
