package handler

import (
	"strings"
	"unicode/utf8"

	"github.com/novaworkbench/backend/internal/model"
)

// simpleTaskMaxRunes is the soft cap for the auto-heuristic: prompts whose
// stripped rune count is at or below this are considered "small" by the
// length gate. Multi-step prompts (lists, headers, code fences) blow past
// this easily; the markdown-signals branch below catches the remaining
// long-but-single-line cases (a single long Markdown line that just happens
// to be ≤80 runes is still treated as simple).
const simpleTaskMaxRunes = 80

// simpleTaskMaxLines caps the number of non-blank lines for the auto path:
// a multi-line prompt is almost always describing more than one change, so
// the "执行 + 1~3 句话" brief framing would under-fit. 2 matches what a
// user can type on two short paragraphs without setting off the gate.
const simpleTaskMaxLines = 2

// simpleTaskMarkdownSignals lists Markdown markers that, if present in the
// stripped prompt, indicate a structured / multi-step task. Any one of these
// being present flips the heuristic to "complex" regardless of length, so a
// short prompt that already lists steps stays on the full report.
//
// The list is double-checked via prefix-match in looksSimpleTask so a prompt
// that BEGINS with a heading (`## 标题`) is also caught even though the
// substring-based check below requires a leading newline.
var simpleTaskMarkdownSignals = []string{
	"\n- ",   // bullet
	"\n* ",   // bullet (asterisk)
	"\n1. ",  // ordered list
	"\n2. ",
	"\n3. ",
	"\n# ",   // heading
	"\n## ",
	"\n### ",
	"```",    // code fence
	"\n- [",  // task list
	"\n* [",
}

// simpleTaskPrefixSignals mirrors simpleTaskMarkdownSignals for the leading
// edge of the prompt: when the user types `## 标题` at column 0 there is no
// leading newline, so the substring scan above misses it. The check is
// deliberately the same shape as the newline-prefixed list — adding a new
// marker only requires updating both lists.
var simpleTaskPrefixSignals = []string{
	"- ",  // leading bullet
	"* ",
	"1. ",
	"2. ",
	"3. ",
	"# ",
	"## ",
	"### ",
	"```",
	"- [",
	"* [",
}

// looksSimpleTask is the auto-mode heuristic. It returns true when the
// prompt looks like a single, self-contained instruction — e.g. "好像没
// 推送，请推送吧" — so the runner can skip the three-section prompt and
// just ask Claude to "动手做 + 1~3 句话". A false return keeps the existing
// three-section behavior so complex tasks don't lose the report shape.
//
// The gate is intentionally permissive about "true" (a one-step Chinese
// instruction) and conservative about "false" (anything structured or long
// stays full). The rationale: a brief that turns out to be slightly under-
// specified is easy to recover from (the user can re-read the prompt in the
// card and the small "✅ 结果" block); a full that turned out to be
// ceremonial on a trivial task is exactly the bug we're fixing.
//
// Implementation notes:
//   - rune count (not byte count) — a 12-rune Chinese prompt is
//     36 UTF-8 bytes and would falsely blow past a byte-based cap.
//   - lines are counted after strip, so a leading blank line (e.g. a
//     pasted " \n好像没推送...") doesn't count.
//   - signals are matched AFTER strip, but on the original-text byte
//     indices — close enough for a heuristic and saves an allocation.
func looksSimpleTask(body string) bool {
	stripped := strings.TrimSpace(body)
	if stripped == "" {
		// Empty prompt is impossible (StartSubTask 400s) but be defensive:
		// treat empty as simple so the runner doesn't inject a phase prompt
		// for a row that has nothing to report.
		return true
	}
	if utf8.RuneCountInString(stripped) > simpleTaskMaxRunes {
		return false
	}
	// Normalize CRLF to LF for the line + signal scans so a Windows-pasted
	// prompt doesn't smuggle in an extra line.
	normalized := strings.ReplaceAll(stripped, "\r\n", "\n")
	nonBlank := 0
	for _, line := range strings.Split(normalized, "\n") {
		if strings.TrimSpace(line) != "" {
			nonBlank++
			if nonBlank > simpleTaskMaxLines {
				return false
			}
		}
	}
	for _, sig := range simpleTaskMarkdownSignals {
		if strings.Contains(normalized, sig) {
			return false
		}
	}
	for _, sig := range simpleTaskPrefixSignals {
		if strings.HasPrefix(normalized, sig) {
			return false
		}
	}
	return true
}

// resolveReportMode normalizes the (user-picked, prompt) pair into a
// definitive brief|full value. "auto" runs the heuristic; "brief" / "full"
// pass through verbatim (so the user can force one direction on a tricky
// prompt). Unknown values fall back to "auto" so a future client version
// that adds a new mode can't accidentally break a row.
func resolveReportMode(mode, body string) string {
	switch mode {
	case model.SubTaskReportModeBrief:
		return model.SubTaskReportModeBrief
	case model.SubTaskReportModeFull:
		return model.SubTaskReportModeFull
	default:
		if looksSimpleTask(body) {
			return model.SubTaskReportModeBrief
		}
		return model.SubTaskReportModeFull
	}
}

// briefInstructionsBlock is the prompt fragment injected in place of the
// three-section `phaseInstructionsBlock` when the resolved report mode is
// "brief". It tells the child agent: don't emit the three sentinels, don't
// pad the reply with section headers, just answer in 1–3 sentences. The
// runner is the only writer so the wording lives in exactly one place.
//
// Kept in lockstep with the Chinese in `phaseInstructionsBlock` so the two
// paths read as the same prompt shape to a Claude that has seen either one
// across runs.
const briefInstructionsBlock = `

## 输出要求（简洁模式）

这是一个小改动，请直接动手完成，**不要**输出「任务理解 / 实施 / 小结」分节标题，也不要输出 <<<...>>> 围栏块。
完成后只用 1~3 句话说明：做了什么、动了哪些文件、是否还有遗留项。
`
