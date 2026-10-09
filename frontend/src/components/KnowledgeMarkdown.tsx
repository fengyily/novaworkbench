import { Link } from 'react-router-dom';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import './KnowledgeMarkdown.css';

interface Props {
  content: string;
}

// KnowledgeMarkdown — 轻量 Markdown 渲染组件，独立于 MarkdownViewer（后者
// 绑死全屏模态浮层，无法复用为独立页面）。 渲染 GFM（表格 / 任务列表 / 删除线 /
// 自动链接），并把链接分两类：同源 SPA 路径走 react-router 的 Link（保持单页
// 导航体验），外部 URL 走 target=_blank + rel=noopener noreferrer。
export default function KnowledgeMarkdown({ content }: Props) {
  return (
    <div className="kmd">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{ a: SmartLink }}
      >
        {content}
      </ReactMarkdown>
    </div>
  );
}

// SmartLink — 区分内部 SPA 路径与外部 URL：
//   - `/foo/bar`   → react-router <Link>，客户端跳转
//   - 其它 (含 hash / http / mailto) → 原生 <a target="_blank">
// 判别规则：href 以 `/` 开头、且不包含 `//`（避免把 `//evil.com` 当成
// 内部路径），同时不含 query 之外的协议部分。
function SmartLink({ href, children, ...rest }: { href?: string; children?: React.ReactNode }) {
  const url = href || '';
  const isInternal = url.startsWith('/') && !url.startsWith('//');
  if (isInternal) {
    return (
      <Link to={url} {...rest}>
        {children}
      </Link>
    );
  }
  return (
    <a href={url} target="_blank" rel="noopener noreferrer" {...rest}>
      {children}
    </a>
  );
}
