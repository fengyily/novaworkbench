// Context-heading helpers shared by the parent-context and referenced-context
// contracts.
//
// Why this exists: both contracts ask the architect to emit a level-2 section
// with a fixed name (「主需求关联上下文」 / 「关联需求上下文」), and both
// handlers verify the produced plan against that contract before storing it.
// The verifier used to be a plain `strings.HasPrefix` on the heading text —
// which silently failed for the single most common way a model writes a
// heading: with an ordinal in front of it ("## 2. 主需求关联上下文").
//
// The failure mode was not cosmetic. The verifier answered "the model did not
// write the section", so the handler appended the machine-written fallback
// appendix — a raw, truncated copy of the master plan — on top of the
// distilled section the model had already written. The stored plan then
// carried the same information twice (once distilled, once verbatim), the
// appendix claimed "模型生成的技术方案未包含「主需求关联上下文」章节" which
// was false, and the verbatim copy was cut mid-sentence by a rune budget.
//
// This file therefore separates three questions that used to be conflated:
//
//  1. Does the document carry the section at all?  → HasContextSection
//     (accepts the model-authored form AND the machine appendix)
//  2. Did the MODEL write the section?          → HasAuthoredContextSection
//     (authored form only — this is what gates the fallback append)
//  3. Is there a stale machine appendix to remove? → StripContextAppendix
//
// (3) is not optional: without it, switching (2) to the authored-only
// predicate would make every re-run append a *second* appendix to a plan
// that already carried one — the exact accumulation the original code
// comment warned against.
package prompt

import (
	"regexp"
	"strings"
	"unicode"
)

// ContextHeadingKind classifies a heading line against a contracted section
// name.
type ContextHeadingKind int

const (
	// ContextHeadingNone — the line is not the contracted section.
	ContextHeadingNone ContextHeadingKind = iota
	// ContextHeadingAuthored — the model wrote the section (optionally with an
	// ordinal prefix, optionally with trailing punctuation).
	ContextHeadingAuthored
	// ContextHeadingAppendix — the machine-written fallback appendix
	// ("（自动附录）" suffix).
	ContextHeadingAppendix
)

// appendixMarkers are the decorations the fallback appendix appends to its
// heading. Kept in one place so the matcher, the stripper and the builder
// cannot drift apart.
var appendixMarkers = []string{"自动附录", "系统自动附加"}

// leadingOrdinalRe matches the numbering a model puts in front of a heading:
// "2. ", "2、", "2)", "（2）", "二、", "第二、".
//
// The delimiter is mandatory, so a heading that merely *starts* with a
// Chinese numeral ("十进制") is not affected.
var leadingOrdinalRe = regexp.MustCompile(`^(?:\d+\s*[.．、)）%]\s*|[(（]\s*\d+\s*[)）]\s*|[〇零一二三四五六七八九十百千]+\s*[、.．)）]\s*)`)

// NormalizeContextHeading strips the decorations a model may put around a
// contracted section name and returns the bare name.
//
// It removes a leading ordinal ("2. 主需求关联上下文" → "主需求关联上下文")
// and trailing punctuation ("主需求关联上下文：" → "主需求关联上下文"). It
// deliberately KEEPS the fallback appendix suffix, because the caller needs
// to tell an authored heading from an appendix one.
func NormalizeContextHeading(text string) string {
	t := strings.TrimSpace(text)
	t = leadingOrdinalRe.ReplaceAllString(t, "")
	t = strings.TrimSpace(t)
	// Trailing punctuation: a heading like "主需求关联上下文：" is the same
	// section as "主需求关联上下文".
	for len(t) > 0 {
		r := []rune(t)
		last := r[len(r)-1]
		if !isHeadingPunctuation(last) {
			break
		}
		t = strings.TrimSpace(string(r[:len(r)-1]))
	}
	return t
}

// MatchContextHeading classifies one heading line against the contracted
// section name.
//
// The level gate is strict: only a level-2 heading counts. "### 主需求关联上下文"
// is a different section and "####" is a sub-subsection. The name comparison
// is "starts with the name AND the remainder is decoration only", so
// "主需求关联上下文XYZ" does not match while "主需求关联上下文（自动附录）"
// and "主需求关联上下文：" do.
func MatchContextHeading(line, section string) ContextHeadingKind {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "##") || strings.HasPrefix(trimmed, "###") {
		return ContextHeadingNone
	}
	text := NormalizeContextHeading(trimmed[2:])
	if text == "" || section == "" {
		return ContextHeadingNone
	}
	if !strings.HasPrefix(text, section) {
		return ContextHeadingNone
	}
	rest := strings.TrimSpace(text[len(section):])
	if rest == "" {
		return ContextHeadingAuthored
	}
	if hasAppendixMarker(rest) {
		return ContextHeadingAppendix
	}
	if isPunctuationOnly(rest) {
		return ContextHeadingAuthored
	}
	return ContextHeadingNone
}

// HasContextSection reports whether the document carries the contracted
// section in ANY form — model-authored or machine appendix.
//
// This is the predicate the refine/apply keep-rules use: a document that
// carries the machine appendix must also survive a rewrite, otherwise the
// next round would lose the parent context entirely.
func HasContextSection(md, section string) bool {
	return scanContextHeadings(md, func(line string) bool {
		return MatchContextHeading(line, section) != ContextHeadingNone
	})
}

// HasAuthoredContextSection reports whether the MODEL wrote the contracted
// section — the machine fallback appendix does not count.
//
// This is the predicate that gates the fallback append: append the appendix
// only when the model wrote nothing.
func HasAuthoredContextSection(md, section string) bool {
	return scanContextHeadings(md, func(line string) bool {
		return MatchContextHeading(line, section) == ContextHeadingAuthored
	})
}

// StripContextAppendix removes the machine-written fallback appendix for the
// given section from md.
//
// The whole level-2 block is removed (heading line through the next level-1
// or level-2 heading, or EOF) — removing only the heading line would leave
// the appendix's subsections orphaned under whatever heading follows.
//
// Returns md unchanged when there is no appendix to remove.
func StripContextAppendix(md, section string) string {
	if md == "" || section == "" {
		return md
	}
	lines := strings.Split(md, "\n")
	inFence := false
	start := -1
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if MatchContextHeading(line, section) == ContextHeadingAppendix {
			start = i
			break
		}
	}
	if start < 0 {
		return md
	}
	// Find the end of the section: the next level-1 or level-2 heading.
	end := len(lines)
	inFence = false
	for i := start + 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if isTopLevelHeading(line) {
			end = i
			break
		}
	}
	out := make([]string, 0, len(lines)-(end-start))
	out = append(out, lines[:start]...)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n")
}

// scanContextHeadings walks the document's heading lines outside fenced code
// blocks and reports whether any of them satisfies pred.
//
// Fence awareness matters: a plan that *quotes* the contracted heading inside
// a ```json or ```markdown block is documenting the contract, not satisfying
// it, and must not suppress the fallback appendix.
func scanContextHeadings(md string, pred func(string) bool) bool {
	found := false
	inFence := false
	for _, raw := range strings.Split(md, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if pred(line) {
			found = true
			break
		}
	}
	return found
}

// isTopLevelHeading reports whether the line is a level-1 or level-2 heading
// (the levels that terminate a level-2 section).
func isTopLevelHeading(line string) bool {
	if !strings.HasPrefix(line, "#") {
		return false
	}
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	return level == 1 || level == 2
}

// hasAppendixMarker reports whether the text carries one of the fallback
// appendix decorations.
func hasAppendixMarker(s string) bool {
	for _, m := range appendixMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// isPunctuationOnly reports whether every rune is punctuation or whitespace.
func isPunctuationOnly(s string) bool {
	if strings.TrimSpace(s) == "" {
		return true
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// isHeadingPunctuation reports whether the rune is a punctuation that a model
// may append to a heading.
func isHeadingPunctuation(r rune) bool {
	switch r {
	case ':', '：', '。', '，', '、', '；', ';', '.', ',', '!', '！', '?', '？':
		return true
	}
	return unicode.IsSpace(r)
}
