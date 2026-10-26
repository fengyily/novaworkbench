// The headline of a stage Q&A question — its first non-empty line.
//
// A question can be several lines long (the composer allows them), and the
// places that need to NAME a question — the panel's question index, the
// exported document's table of contents and its section headings — all want
// a label rather than the whole paragraph.
//
// One helper, so the index and the exported document never disagree about
// what a question is called.

export function questionHeadline(question: string): string {
  for (const line of question.split('\n')) {
    const trimmed = line.trim();
    if (trimmed) return trimmed;
  }
  return question.trim();
}

/**
 * The whole question on ONE line: every line trimmed and joined with a
 * space. A heading has to stay a heading — a question with line breaks
 * would split into a heading plus a stray paragraph — so the exported
 * document's section headings use this instead of the raw text.
 */
export function questionOneline(question: string): string {
  return question
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)
    .join(' ');
}
