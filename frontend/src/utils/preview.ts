// Turn a Markdown string into a short, readable plain-text preview for list
// cards. We strip structural markdown noise (headings, code fences, emphasis,
// links, list markers, blockquotes) so the preview reads as prose instead of
// raw syntax, then collapse runs of whitespace. Length is bounded by the
// caller via CSS line-clamp, not by a hard character cut here.
export function stripMarkdownPreview(input: string): string {
  if (!input) return '';
  let text = input;

  // Fenced code blocks: drop the fences, keep the inner lines (often a
  // project-tree or code excerpt that's useful as context).
  text = text.replace(/```[^\n]*\n?/g, '');
  // Inline code backticks.
  text = text.replace(/`([^`]+)`/g, '$1');
  // Images -> alt text.
  text = text.replace(/!\[([^\]]*)\][^)]*/g, '$1');
  // Links -> label.
  text = text.replace(/\[([^\]]+)\]\([^)]*\)/g, '$1');
  // Headings: strip leading #'s.
  text = text.replace(/^#{1,6}\s*/gm, '');
  // Emphasis / bold.
  text = text.replace(/\*\*([^*]+)\*\*/g, '$1');
  text = text.replace(/__([^_]+)__/g, '$1');
  text = text.replace(/(^|[^*])\*([^*\n]+)\*/g, '$1$2');
  text = text.replace(/(^|[^_])_([^_\n]+)_/g, '$1$2');
  // Blockquote markers.
  text = text.replace(/^>\s?/gm, '');
  // Unordered list markers.
  text = text.replace(/^[\t ]*[-*+]\s+/gm, '');
  // Ordered list markers.
  text = text.replace(/^[\t ]*\d+\.\s+/gm, '');
  // Horizontal rules.
  text = text.replace(/^[-*_]{3,}\s*$/gm, '');
  // Trailing whitespace per line.
  text = text.replace(/[ \t]+$/gm, '');
  // Collapse 3+ newlines into one blank line.
  text = text.replace(/\n{3,}/g, '\n\n');
  return text.trim();
}
