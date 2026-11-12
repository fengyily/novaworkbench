// Shared helpers for the sub-task artifact Markdown.
//
// The artifact the backend persists is a full document:
//
//   # 子任务: <title>
//   <blank>
//   **提示词**: <prompt>
//   <blank>
//   **完成时间**: YYYY-MM-DD HH:MM:SS  **模型**: <model>
//   <blank>
//   ---
//   <blank>
//   <body>
//
// The card renders only the BODY, so every consumer has to strip the header
// the same way. They live here (rather than inside SubTaskPanel.tsx) so the
// requirement-level Review card can reuse them without importing the panel
// — which would be a circular import.

// stripArtifactHeader removes the 4-line Markdown header that
// `buildSubTaskArtifact` (backend/internal/handler/wizard_subtask.go:1329-1351)
// prepends to every artifact. The structured ReportHeader renders those
// fields itself, so the body only carries Claude's raw output.
//
// Header shape (note \n\n between fields):
//   # 子任务: <title>
//   <blank>
//   **提示词**: <prompt>
//   <blank>
//   **完成时间**: YYYY-MM-DD HH:MM:SS  **模型**: <model>
//   <blank>
//   ---
//   <blank>
//   <body>
//
// If the regex does not match (older artifact without # 子任务: line),
// return md unchanged so the UI degrades to the legacy inline-bold view.
export function stripArtifactHeader(md: string): string {
  if (!md) return md;
  const header = /^# 子任务:[^\n]*\n\n(?:\*\*提示词\*\*:[^\n]*\n\n)?(?:\*\*完成时间\*\*:[^\n]*\*\*模型\*\*:[^\n]*\n\n)?---\n+/;
  return md.replace(header, '');
}

// stripArtifactCodeFence unwraps a single triple-backtick code fence when
// the artifact body is entirely wrapped in one. The LLM sometimes emits
// its full response inside ```...``` (a formatting slip that's especially
// common when the user asked for a Markdown table — the model wraps the
// table "as code" to preserve column alignment). Without this, ReactMarkdown
// treats the whole body as a single code block: tables, headings, lists all
// collapse into monospace text and the user's "表格的形式输出" never renders
// as a real <table>.
//
// Heuristic: the (trimmed) body must start with a single opening fence
// (``` with optional language identifier) and end with a single closing
// fence. We strip both, leaving the inner content — including any nested
// ```code blocks``` from the assistant's thinking — untouched. We do NOT
// fire when the body opens with a fence but lacks the matching closing
// fence (that's a real code block the user asked for).
export function stripArtifactCodeFence(body: string): string {
  if (!body) return body;
  const trimmed = body.replace(/^\s+|\s+$/g, '');
  // Anchored to ^ and $; non-greedy *? combined with the $ anchor still
  // finds the LAST closing ``` because the engine expands the inner match
  // until the suffix fits. This is exactly what we want for artifacts that
  // contain nested code blocks — we only strip the outermost wrapper.
  const m = trimmed.match(/^```[^\n]*\n([\s\S]*?)\n```$/);
  if (!m) return body;
  return m[1];
}
