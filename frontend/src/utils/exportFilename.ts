// One filename-sanitising rule shared by every document exporter.
//
// Lives in its own module (rather than in exportDesignPdf) so callers that
// only need the string rule — e.g. the stage Q&A thread building a `.md`
// filename — do not statically pull mermaid / marked / html2pdf into their
// bundle just to download a text file.
export function sanitizeFilename(name: string, fallback: string): string {
  return name.replace(/[\\/:*?"<>|]/g, '_').slice(0, 80).trim() || fallback;
}
