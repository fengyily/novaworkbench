// rewriteNovaLinks — convert the editor's `nova://attachment/<id>` custom
// protocol to a URL MarkdownRender's existing <img> handler can fetch
// (the same content endpoint AttachmentPanel's download button uses).
//
// MarkdownRender already gives us click-to-zoom + PNG/JPG download on every
// <img>, so we just need to get the bytes under an <img> tag. A pure
// string-replace keeps this decoupled from remark/rehype plumbing and means
// there's exactly one source of truth for the URL shape (the AttachmentHandler
// route in the backend).
//
// We only rewrite inside the (alt](...) portion of a Markdown image/link
// pair — anywhere else (code fences, plain text) the literal "nova://..."
// is left alone, which is what the user wants for the textarea source.

const NOVA_LINK_RE =
  /(!\[[^\]]*\]\(|\]\()nova:\/\/attachment\/([a-zA-Z0-9_-]+)\)/g;

export function rewriteNovaLinks(markdown: string, requirementId: string): string {
  if (!markdown || !requirementId) return markdown;
  return markdown.replace(
    NOVA_LINK_RE,
    (_match, prefix, attId) =>
      `${prefix}/api/requirements/${requirementId}/attachments/${attId}/content)`,
  );
}
