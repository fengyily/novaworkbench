package service

import (
	"regexp"
	"strings"
)

// SanitizeMermaidBlocks fixes common Mermaid 11.x syntax incompatibilities
// in fenced ```mermaid ... ``` blocks inside Markdown content. Driven by
// empirical failures observed on req_b37e6f385d371b7e and re-verified with
// mermaid 11.13.0 (mermaid.parse() round-trip via Node script).
//
// Why this exists: the upstream Claude model can produce visually plausible
// Mermaid that the 11.x lexer rejects. Three concrete failure modes have
// been observed and validated end-to-end:
//
//  1. ASCII ';' inside a sequenceDiagram message text. Mermaid 11 splits
//     the message at the first ';' and tries to parse the remainder as a
//     new statement, then bails with "Expecting ... got 'NEWLINE'".
//     Fix: replace ASCII ';' inside message text with the full-width '；'
//     (visually identical to a human reader, parser treats it as text).
//
//  2. Parallelogram labels `[/...content]` that are MISSING the closing
//     `/`. Mermaid 11 requires both `[/` (opener) AND `/]` (closer).
//     When the model writes `[/mcp<br/>mcp.Server]` (closing `/`
//     forgotten) the lexer can't find `/]` and bails with "Lexical
//     error ... Unrecognized text" pointing at the label. Fix: insert
//     the missing `/` immediately before the closing `]`. We only fix
//     labels that we know to be broken (no closing `/`); labels that
//     already have a closing `/` (e.g. `[/foo<br/>bar/]`,
//     `[/api/foo/]`) parse cleanly and are left alone so the
//     parallelogram intent is preserved.
//
//  3. Unquoted square labels `[content]` that contain ANY of these
//     parser-confusing chars: `"`, `(`, `)`, `{`, `}`. Mermaid 11
//     treats `(` `)` as the cylinder shape opener / closer and `{` `}`
//     as the rhombus opener/closer. When they appear INSIDE a plain
//     square label (e.g. `X[a(b)c]` or `X[a{b}c]`) the lexer mistakes
//     them for nested shape delimiters and bails with "Parse error
//     ... got 'NEWLINE'". Same goes for an unmatched `"` mid-label.
//     Fix: walk the label content respecting quoted strings to find
//     the matching ']', then wrap the whole label in `["..."]`. The
//     inside chars are preserved verbatim because quoted-square labels
//     accept them as literal text. This is a small state machine —
//     regex alone can't track quote state in stdlib regexp.
//
// The sanitizer is intentionally conservative — only the patterns above
// are touched. Anything more aggressive risks breaking diagrams that
// currently render correctly.
func SanitizeMermaidBlocks(content string) string {
	var out strings.Builder
	out.Grow(len(content))
	i := 0
	for i < len(content) {
		// Find the next opening ```mermaid fence.
		fenceIdx := strings.Index(content[i:], "```mermaid")
		if fenceIdx < 0 {
			out.WriteString(content[i:])
			break
		}
		// Copy everything up to and including the opening fence.
		out.WriteString(content[i : i+fenceIdx])
		out.WriteString("```mermaid")
		i += fenceIdx + len("```mermaid")
		// Skip the newline immediately after the fence.
		if i < len(content) && content[i] == '\n' {
			out.WriteByte('\n')
			i++
		}
		// Find the matching closing ``` on its own line. We scan forward
		// looking for "\n```" so we don't accidentally treat a backtick
		// inside a Mermaid block as the closer.
		closeStart, closeEnd := findMermaidClose(content, i)
		if closeStart < 0 {
			// No closing fence — append the rest verbatim and stop.
			out.WriteString(content[i:])
			i = len(content)
			break
		}
		block := content[i:closeStart]
		cleaned := sanitizeMermaidBlock(block)
		out.WriteString(cleaned)
		// Append the closing fence + trailing newline (if any).
		out.WriteString(content[closeStart:closeEnd])
		i = closeEnd
	}
	return out.String()
}

// findMermaidClose returns the [start, end) byte indices of the closing
// "```" fence of a mermaid block whose body starts at bodyStart. The
// returned range covers the "\n```" or leading "```" through to just
// past the trailing "\n" (or end of string). Returns -1, -1 when no
// closing fence is found.
func findMermaidClose(content string, bodyStart int) (int, int) {
	j := bodyStart
	for j < len(content) {
		nl := strings.IndexByte(content[j:], '\n')
		if nl < 0 {
			return -1, -1
		}
		lineStart := j
		lineEnd := j + nl
		j = lineEnd + 1
		line := strings.TrimRight(content[lineStart:lineEnd], "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "```" {
			return lineStart, j
		}
	}
	return -1, -1
}

// sanitizeMermaidBlock applies the per-block fixes for one mermaid body.
// The body is the raw text between the fences (no surrounding ```mermaid
// or ``` markers). The returned text has the same shape — same number of
// lines, same indentation — so it drops back into the document verbatim.
func sanitizeMermaidBlock(body string) string {
	lines := strings.Split(body, "\n")
	if len(lines) == 0 {
		return body
	}
	first := firstMermaidDiagramLine(lines)
	diag := strings.ToLower(strings.TrimSpace(first))
	var fixer func(string) string
	switch {
	case strings.HasPrefix(diag, "sequencediagram"):
		fixer = sanitizeSequenceDiagramLine
	case strings.HasPrefix(diag, "flowchart") || strings.HasPrefix(diag, "graph"):
		fixer = sanitizeFlowchartLine
	case strings.HasPrefix(diag, "statediagram"):
		fixer = sanitizeStateDiagramLine
	default:
		// classDiagram / erDiagram / gantt / pie / journey — leave alone.
		return body
	}
	for i, l := range lines {
		lines[i] = fixer(l)
	}
	return strings.Join(lines, "\n")
}

// firstMermaidDiagramLine returns the first non-empty, non-comment line
// of a mermaid block (used to identify the diagram type). Comments use
// "%%" in mermaid; we treat any line beginning with "%%" as a comment.
func firstMermaidDiagramLine(lines []string) string {
	for _, l := range lines {
		s := strings.TrimSpace(l)
		if s == "" || strings.HasPrefix(s, "%%") {
			continue
		}
		return l
	}
	return ""
}

// ───────────────────────────── sequenceDiagram ─────────────────────────────

// sdMessageLineRe matches a participant-arrow message line.
// The arrow grammar is intentionally lenient so it covers the shapes
// mermaid 11 actually emits: `->`, `-->`, `->>`, `-->>`, `-x`, `--x`,
// `->>+`, `-->)-`, etc. All share the pattern "one-or-more dashes
// followed by one-or-more arrowhead chars" (`[>x.)` plus optional
// activate/deactivate markers). We don't need to recognise every
// variant perfectly — just enough to skip past the head and isolate
// the message text after the first `:`.
// Examples that match:
//   A->>B: hello world
//   A-->>B: x
//   A->>+B: ...
//   A-xB: ...
//   A->>B: foo; bar
// Patterns we deliberately don't touch:
//   "Note over X: ..."
//   "loop ...\n  ...\nend"
//   "alt ...\n  ...\nelse ...\n  ...\nend"
//   "participant A as ..."
// The regex requires an arrow token, so Note/loop/alt/participant lines
// (which start with a keyword, not a participant+arrow) won't match.
var sdMessageLineRe = regexp.MustCompile(`^(\s*\S+\s*-+[>x.)]+\s*[+\-!]?\s*\S+\s*:\s*)(.*)$`)

// sanitizeSequenceDiagramLine replaces ASCII ';' inside message text
// with the full-width '；'. The rest of the line is left untouched so
// arrow shapes, participant names, etc. are preserved verbatim.
func sanitizeSequenceDiagramLine(line string) string {
	m := sdMessageLineRe.FindStringSubmatchIndex(line)
	if m == nil {
		return line
	}
	// m indexes: 0=full, 2,3=group1 (head), 4,5=group2 (message body).
	msgStart := m[4]
	msgEnd := m[5]
	msg := line[msgStart:msgEnd]
	if !strings.Contains(msg, ";") {
		return line
	}
	return line[:msgStart] + strings.ReplaceAll(msg, ";", "；") + line[msgEnd:]
}

// ───────────────────────────── flowchart / graph ────────────────────────────

// nodeIdAndOpenRe matches a flowchart node label's opening token: the
// identifier (with possible whitespace before) followed by the opening
// bracket character. Used to anchor the parallelogram + quote-aware
// label fixers.
var nodeIdAndOpenRe = regexp.MustCompile(`(?:^|[\s>])([A-Za-z_][\w-]*)\[`)

// sanitizeFlowchartLine applies the known-broken flowchart label
// patterns:
//   - parallelogram [/...content] missing `/` closer → insert missing `/`
//   - unquoted [...] whose content has any of `"`, `(`, `)`, `{`, `}` →
//     wrap in ["..."] (quoted square accepts these as literal text)
//   - unquoted [...] whose content has '"' that opens an unterminated
//     quoted string → drop the inner '"' so the new outer "..." is
//     well-formed (a special case of the previous bullet)
func sanitizeFlowchartLine(line string) string {
	return sanitizeUnquotedLabelWithDelims(sanitizeParallelogramMissingSlash(line))
}

// sanitizeParallelogramMissingSlash rewrites `ID[/...content]` (missing
// the closing `/`) into `ID[/...content/]`. The closing `/` is
// mandatory in mermaid 11 — omitting it makes the lexer bail with a
// "Lexical error" pointing at the label.
//
// Detection: `\[/...content?\]` where the optional `/` is NOT present
// before the closing `]`. We do the detection by capturing an optional
// `/` immediately before `]` and only rewriting when the capture is
// empty. Go's stdlib regexp doesn't support negative lookahead so we
// use the capture-group approach (parallel to the rest of this file).
func sanitizeParallelogramMissingSlash(line string) string {
	re := regexp.MustCompile(`((?:^|[\s>])([A-Za-z_][\w-]*)\[/[^\n]+?)(/?)(\])`)
	return re.ReplaceAllStringFunc(line, func(match string) string {
		sm := re.FindStringSubmatch(match)
		if sm == nil {
			return match
		}
		slash := sm[3]
		if slash == "/" {
			return match // already a valid parallelogram, leave alone
		}
		// sm[1] is the prefix + opening `[/` + content; sm[4] is `]`.
		return sm[1] + "/" + sm[4]
	})
}

// sanitizeUnquotedLabelWithDelims rewrites `[content]` where `content`
// contains any of `"`, `(`, `)`, `{`, `}` (chars that mermaid 11
// misinterprets as nested-shape delimiters or unterminated strings) to
// `["content"]`. Quoted-square labels accept these as literal text.
//
// Uses a small character-level walker because regex can't track quote
// state: from a candidate `[` we step forward one char at a time,
// tracking whether we're inside a `"..."` quoted run. The first `]`
// outside of a quote is the label's closer.
//
// Safe controls (verified empirically):
//   - `[Walk → redacted]` — no confusing chars, skipped entirely
//   - `["already quoted"]` — first `[` is followed by `"` which makes
//     the walker bail (inQuote=true at offset 1, so it can never find
//     a matching `]` outside of a quote) — the label is preserved
//     verbatim. This is the desired no-op for already-quoted labels.
//   - `[normal text]` — no confusing chars, skipped
//   - `[a(b)c]` — contains `(`, wrapped to `["a(b)c"]`
//   - `[a{b}c]` — contains `{`, wrapped to `["a{b}c"]`
//   - `[walk → "[redacted]"]` — contains `"`, wrapped to `["walk → [redacted]"]`
func sanitizeUnquotedLabelWithDelims(line string) string {
	var out strings.Builder
	out.Grow(len(line))
	i := 0
	for i < len(line) {
		openIdx := -1
		for j := i; j < len(line); j++ {
			if line[j] == '[' {
				k := j - 1
				for k >= i && isIdentChar(line[k]) {
					k--
				}
				k++
				if k < j && (k == i || line[k-1] == ' ' || line[k-1] == '\t' || line[k-1] == '>') {
					openIdx = j
					break
				}
			}
		}
		if openIdx < 0 {
			out.WriteString(line[i:])
			break
		}
		// Append everything up to and including the `[`.
		out.WriteString(line[i : openIdx+1])
		// Walk forward to find the matching `]` while tracking quote
		// state. A `"` toggles inQuote; `]` closes the label only when
		// we're outside a quoted run.
		j := openIdx + 1
		inQuote := false
		closeIdx := -1
		for j < len(line) {
			c := line[j]
			if c == '"' {
				inQuote = !inQuote
			} else if c == ']' && !inQuote {
				closeIdx = j
				break
			}
			j++
		}
		if closeIdx < 0 {
			// No matching ']' — leave the rest of the line alone.
			out.WriteString(line[openIdx+1:])
			i = len(line)
			break
		}
		content := line[openIdx+1 : closeIdx]
		if !labelNeedsQuoting(content) {
			// No confusing chars — leave the label alone.
			out.WriteString(content)
			out.WriteByte(']')
		} else {
			// Wrap the whole label in `["..."]`. If `content` had any
			// inner `"`, drop them so the new outer quoted string stays
			// well-formed; everything else is preserved verbatim since
			// quoted-square labels accept it as literal text.
			safe := strings.ReplaceAll(content, `"`, "")
			out.WriteString(`"`)
			out.WriteString(safe)
			out.WriteString(`"]`)
		}
		i = closeIdx + 1
	}
	return out.String()
}

// labelNeedsQuoting reports whether a flowchart label's content
// contains a char that mermaid 11 misinterprets inside a plain square
// label. The check is intentionally conservative — we only trigger on
// the chars that empirically break the lexer. Plain text labels
// (including those with `<br/>`, `/`, `→`, etc.) don't trigger.
func labelNeedsQuoting(content string) bool {
	return strings.ContainsAny(content, `"(){}`)
}

// isIdentChar reports whether b can appear in a flowchart node id.
// Mirrors the lenient char set in mermaid's lexer: letters, digits,
// underscores, hyphens, periods.
func isIdentChar(b byte) bool {
	return (b >= 'A' && b <= 'Z') ||
		(b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9') ||
		b == '_' || b == '-' || b == '.'
}

// ───────────────────────────── stateDiagram-v2 ─────────────────────────────

// stateTransitionRe matches a stateDiagram-v2 transition line:
//   "State --> NextState" or
//   "State --> NextState: transition text".
// We capture the head (everything up to ':' transition text) and the
// transition text. ASCII ';' inside the transition text triggers the
// same parser bug as in sequenceDiagram messages, so we apply the same
// full-width replacement.
var stateTransitionRe = regexp.MustCompile(`^(\s*\S+\s*--+>\s*\S+\s*:\s*)(.*)$`)

// sanitizeStateDiagramLine replaces ASCII ';' inside transition text
// with full-width '；'. Lines that don't match the transition shape
// (state declarations, classDefs, etc.) are left alone.
func sanitizeStateDiagramLine(line string) string {
	m := stateTransitionRe.FindStringSubmatchIndex(line)
	if m == nil {
		return line
	}
	// m indexes: 0=full, 2,3=group1 (head), 4,5=group2 (transition text).
	msgStart := m[4]
	msgEnd := m[5]
	msg := line[msgStart:msgEnd]
	if !strings.Contains(msg, ";") {
		return line
	}
	return line[:msgStart] + strings.ReplaceAll(msg, ";", "；") + line[msgEnd:]
}