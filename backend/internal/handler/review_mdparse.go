package handler

import (
	"regexp"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
)

// ── 第 1 层结构化来源：报告正文里的分级小节 ─────────────────────────────
//
// WHY THIS EXISTS: the structured half of a Review report (the graded
// findings that become fix sub-tasks) used to have exactly ONE source — the
// trailing ```json contract block. When the model didn't emit it (a long
// report on a custom model gets truncated, or the model simply forgot), the
// report degraded to "readable prose, no grading": the P0/P1/P3 sections
// were right there in the body, but nothing could read them, so the
// 「生成修复子任务」 button had nothing to work with.
//
// The report BODY is therefore the second source. This file parses it
// deterministically — no model, no tokens, milliseconds — and produces the
// exact same []model.ReviewFinding the contract parser produces, so every
// downstream reader (the ledger, the findings list, CreateFixes, the
// verdict) is unchanged.
//
// The rules are deliberately rigid. A loose parser would invent findings the
// model never wrote (worse than no findings: the user would file fix
// sub-tasks for problems that do not exist), so:
//   - only "## P0/P1/P3" sections count (P2 does not exist in this product);
//   - only list items (or, in a section without lists, blank-line
//     paragraphs) are findings;
//   - "无 / 没有 / N/A / 暂无" sections produce nothing;
//   - the severity always comes from the SECTION, never from the item text —
//     the model's own declared bucket is the authoritative one.

// Where the structured half of a review report came from. Persisted in
// code_reviews.findings_source so the UI can say so out loud — a user whose
// grading was recovered from the report body should know that, because it
// is a different (slightly less precise) extraction than the contract.
const (
	// reviewFindingsSourceContract = the report's trailing ```json block.
	reviewFindingsSourceContract = "contract"
	// reviewFindingsSourceMarkdown = the report's own P0/P1/P3 sections.
	reviewFindingsSourceMarkdown = "markdown"
	// reviewFindingsSourceLLM = the LLM extractor (the last resort).
	reviewFindingsSourceLLM = "llm"
)

// maxMarkdownDetailRunes caps the detail text of a finding parsed out of the
// report body. The contract path has no such cap (the model writes what it
// writes), but a body-parsed item can carry a whole paragraph of prose plus
// an embedded Mermaid block, and the detail is rendered in a card.
const maxMarkdownDetailRunes = 1200

// maxMarkdownTitleRunes caps a title we could not find a bold marker for.
const maxMarkdownTitleRunes = 60

var (
	// mdHeadingRe matches a level-2 Markdown heading. "###" and deeper do NOT
	// match (the third character must be a space), so a sub-heading stays
	// inside its parent section instead of becoming one.
	mdHeadingRe = regexp.MustCompile(`^##\s+(.+)$`)

	// mdSeverityRe recognises a severity bucket at the START of a heading.
	// Only P0/P1/P3 — P2 is not a bucket in this product.
	mdSeverityRe = regexp.MustCompile(`(?i)^P([013])\b`)

	// mdAnySeverityRe finds severity tokens anywhere in a heading, so a
	// summary heading like "P0/P1 汇总" can be rejected instead of being
	// read as a P0 section.
	mdAnySeverityRe = regexp.MustCompile(`(?i)\bP[013]\b`)

	// mdListItemRe matches a TOP-LEVEL list marker ("- ", "* ", "+ ", "1. ",
	// "1) "). A marker indented by 4+ spaces is a nested bullet and stays
	// with its parent item.
	mdListItemRe = regexp.MustCompile(`^( {0,3})(?:[-*+]|\d+[.)])\s+`)

	// mdBoldRe finds the "**title**" form.
	mdBoldRe = regexp.MustCompile(`\*\*([^*]+)\*\*`)

	// mdCodeSpanRe finds a `code span` — the only place a file path can
	// reliably hide.
	mdCodeSpanRe = regexp.MustCompile("`([^`]+)`")

	// mdPathRe recognises something that looks like a file path.
	mdPathRe = regexp.MustCompile(`(?:^|/)[^\s/]+\.[A-Za-z0-9]{1,6}$`)

	// mdPathLineRe splits "path/to/file.go:42" (or ":12-20") into the two.
	mdPathLineRe = regexp.MustCompile(`^(.*):(\d+(?:-\d+)?)$`)

	// mdLineOnlyRe matches a bare line number.
	mdLineOnlyRe = regexp.MustCompile(`^\d+(?:-\d+)?$`)

	// mdSeverityPrefixRe strips a redundant "[P0]" prefix from a title
	// (the item format the prompt asks for redundantly carries its bucket).
	mdSeverityPrefixRe = regexp.MustCompile(`(?i)^\[P[013]\]\s*`)

	// mdSuggestionMarkerRe finds the "建议：" family of suggestion markers.
	mdSuggestionMarkerRe = regexp.MustCompile(`(?:修改建议|修复建议|改进建议|建议|推荐)\s*[：:]`)
)

// mdEmptyItems are the "nothing to report here" bodies a model writes in an
// empty severity section. They must NOT become findings.
var mdEmptyItems = map[string]bool{
	"无":       true,
	"没有":     true,
	"无。":      true,
	"没有。":     true,
	"无问题":    true,
	"暂无":     true,
	"暂无。":    true,
	"n/a":    true,
	"na":     true,
	"none":   true,
	"-":      true,
	"—":      true,
	"－":      true,
	"空":      true,
	"(无)":   true,
	"（无）":   true,
	"（无）无":  true,
	"no issue": true,
}

// mdSection is one "## …" section of the report body.
type mdSection struct {
	heading string
	body    string
}

// splitMarkdownSections splits the report into its level-2 sections.
//
// Only "## " starts a section: "###" (and deeper) is a sub-heading of the
// section it sits in, and a sub-heading must not be able to steal the
// severity from its parent.
func splitMarkdownSections(md string) []mdSection {
	var out []mdSection
	var cur mdSection
	started := false
	for _, line := range strings.Split(md, "\n") {
		if m := mdHeadingRe.FindStringSubmatch(line); m != nil {
			if started || strings.TrimSpace(cur.body) != "" {
				out = append(out, cur)
			}
			cur = mdSection{heading: strings.TrimSpace(m[1])}
			started = true
			continue
		}
		if !started {
			// Text before the first heading belongs to no section (the
			// preamble) — it is never a finding.
			continue
		}
		cur.body += line + "\n"
	}
	if started {
		out = append(out, cur)
	}
	return out
}

// severityFromHeading returns the bucket a section declares, or "" when it
// declares none.
//
// A heading that names MORE than one bucket ("P0/P1 汇总") is a summary, not
// a severity section: reading it as P0 would attribute every line under it
// to the wrong bucket.
func severityFromHeading(heading string) string {
	m := mdSeverityRe.FindStringSubmatch(strings.TrimSpace(heading))
	if m == nil {
		return ""
	}
	if len(mdAnySeverityRe.FindAllString(heading, -1)) > 1 {
		return ""
	}
	switch strings.ToUpper(m[1]) {
	case "0":
		return prompt.SevP0
	case "1":
		return prompt.SevP1
	case "3":
		return prompt.SevP3
	}
	return ""
}

// splitSectionItems splits a section body into candidate findings.
//
// Preferred shape: top-level list items, with their continuation lines
// (indented lines, nested bullets, plain follow-up prose) folded into the
// item they belong to.
//
// Fallback shape: a section with no list markers at all is split on blank
// lines, one paragraph per item. That is how a model writes a section it
// did not format as a list.
func splitSectionItems(body string) []string {
	lines := strings.Split(body, "\n")
	var items []string
	var cur []string
	sawMarker := false

	flush := func() {
		if len(cur) == 0 {
			return
		}
		items = append(items, strings.TrimSpace(strings.Join(cur, "\n")))
		cur = nil
	}

	for _, line := range lines {
		// A sub-heading (### and deeper) is part of the section, not an
		// item of it — it must not become a finding on its own.
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if m := mdListItemRe.FindStringSubmatch(line); m != nil {
			sawMarker = true
			flush()
			cur = append(cur, strings.TrimSpace(line[len(m[0]):]))
			continue
		}
		if strings.TrimSpace(line) == "" {
			// A blank line does not end an item — but a blank line followed
			// by a new marker does. Both are handled by flush() on the next
			// marker; here we only need to not lose the current item.
			continue
		}
		if len(cur) == 0 {
			// Prose before any marker: keep it, it may be the item itself
			// in a section the model wrote as paragraphs.
			cur = append(cur, strings.TrimSpace(line))
			continue
		}
		cur = append(cur, strings.TrimSpace(line))
	}
	flush()

	if sawMarker {
		return items
	}
	return splitByBlankLines(body)
}

// splitByBlankLines splits a body into blank-line separated paragraphs.
func splitByBlankLines(body string) []string {
	var out []string
	var cur []string
	for _, line := range strings.Split(body, "\n") {
		// Sub-headings belong to the section, not to an item of it.
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.TrimSpace(line) == "" {
			if len(cur) > 0 {
				out = append(out, strings.TrimSpace(strings.Join(cur, "\n")))
				cur = nil
			}
			continue
		}
		cur = append(cur, strings.TrimSpace(line))
	}
	if len(cur) > 0 {
		out = append(out, strings.TrimSpace(strings.Join(cur, "\n")))
	}
	return out
}

// parseMarkdownFindings parses the graded findings out of a report body.
//
// ok=false means "the body carries no parseable grading" — the caller then
// falls through to the next source (the LLM extractor). A body that simply
// has no problems in it also returns ok=false, which is the same thing for
// every caller: there is nothing to structure.
func parseMarkdownFindings(md string) ([]model.ReviewFinding, bool) {
	if strings.TrimSpace(md) == "" {
		return nil, false
	}
	var out []model.ReviewFinding
	for _, sec := range splitMarkdownSections(md) {
		sev := severityFromHeading(sec.heading)
		if sev == "" {
			continue
		}
		for _, item := range splitSectionItems(sec.body) {
			f, ok := parseFindingItem(sev, item)
			if !ok {
				continue
			}
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// parseFindingItem turns ONE item of a severity section into a finding.
//
// The extraction is layered so a richer format keeps its information and a
// poorer one still yields something:
//   - title: the first "**bold**", else the text before the first
//     separator, else the head of the first line;
//   - file / line: the first code span that looks like a path, split on a
//     trailing ":42" / ":12-20"; a bare number in the next code span is
//     accepted as the line too;
//   - suggestion: the "建议：…" tail of the item;
//   - detail: everything else.
func parseFindingItem(severity, item string) (model.ReviewFinding, bool) {
	text := strings.TrimSpace(item)
	if text == "" || mdEmptyItems[strings.ToLower(text)] {
		return model.ReviewFinding{}, false
	}

	f := model.ReviewFinding{Severity: severity}

	// Title — the bold form first.
	title := ""
	rest := text
	if m := mdBoldRe.FindStringSubmatch(text); m != nil {
		title = strings.TrimSpace(mdSeverityPrefixRe.ReplaceAllString(m[1], ""))
		rest = strings.Replace(text, m[0], " ", 1)
	}

	// File / line — out of the code spans.
	file, line, rest := extractLocation(rest)

	if title == "" {
		title = deriveTitle(rest)
	}
	rest = stripLeadingSeparators(rest)

	// Suggestion — the "建议：…" tail.
	detail := rest
	suggestion := ""
	if m := mdSuggestionMarkerRe.FindStringIndex(detail); m != nil {
		suggestion = strings.TrimSpace(detail[m[1]:])
		detail = strings.TrimSpace(detail[:m[0]])
		detail = strings.TrimRight(detail, " —–-,，、:：")
	}
	if len([]rune(detail)) > maxMarkdownDetailRunes {
		detail = string([]rune(detail)[:maxMarkdownDetailRunes])
	}

	f.Title = title
	f.File = file
	f.Line = line
	f.Detail = detail
	f.Suggestion = suggestion

	// An item with neither a title nor a location says nothing actionable.
	if f.Title == "" && f.File == "" {
		return model.ReviewFinding{}, false
	}
	return f, true
}

// extractLocation pulls the file (and line) out of the item's code spans.
//
// The first span that looks like a path wins; a ":42" / ":12-20" suffix on
// it becomes the line. A path span followed by a bare-number span is
// accepted as well — models write both shapes.
func extractLocation(text string) (file, line, rest string) {
	spans := mdCodeSpanRe.FindAllStringSubmatchIndex(text, -1)
	if len(spans) == 0 {
		return "", "", text
	}
	for i, sp := range spans {
		content := strings.TrimSpace(text[sp[2]:sp[3]])
		if content == "" {
			continue
		}
		if m := mdPathLineRe.FindStringSubmatch(content); m != nil && mdPathRe.MatchString(m[1]) {
			return m[1], m[2], removeSpan(text, sp)
		}
		if mdPathRe.MatchString(content) {
			file = content
			rest = removeSpan(text, sp)
			// A following span holding only a number is the line.
			if i+1 < len(spans) {
				next := strings.TrimSpace(text[spans[i+1][2]:spans[i+1][3]])
				if mdLineOnlyRe.MatchString(next) {
					line = next
					rest = removeSpan(rest, findSpan(rest, next))
				}
			}
			return file, line, rest
		}
	}
	return "", "", text
}

// findSpan locates a code span holding exactly the given content.
func findSpan(text, content string) []int {
	for _, sp := range mdCodeSpanRe.FindAllStringSubmatchIndex(text, -1) {
		if strings.TrimSpace(text[sp[2]:sp[3]]) == content {
			return sp
		}
	}
	return nil
}

// removeSpan deletes the given span from the text.
func removeSpan(text string, sp []int) string {
	if sp == nil {
		return text
	}
	return text[:sp[0]] + " " + text[sp[1]:]
}

// deriveTitle produces a title for an item that has no bold marker: the
// text before the first separator, else the head of the first line.
func deriveTitle(text string) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return ""
	}
	// Only the FIRST line is considered — a title is a headline, not a
	// paragraph.
	first := t
	if idx := strings.Index(t, "\n"); idx >= 0 {
		first = t[:idx]
	}
	cut := len(first)
	for _, sep := range []string{"——", "—", "：", ":"} {
		if idx := strings.Index(first, sep); idx >= 0 && idx < cut {
			cut = idx
		}
	}
	if cut > 0 {
		cand := strings.TrimSpace(first[:cut])
		if cand != "" {
			return truncateRunes(cand, maxMarkdownTitleRunes)
		}
	}
	return truncateRunes(strings.TrimSpace(first), maxMarkdownTitleRunes)
}

// stripLeadingSeparators removes the punctuation that separated the title
// from the body, so the detail does not start with "— ".
func stripLeadingSeparators(text string) string {
	s := strings.TrimSpace(text)
	for {
		trimmed := strings.TrimLeft(s, " \t—–-,，、:：;；")
		if trimmed == s {
			return s
		}
		s = trimmed
	}
}

// truncateRunes shortens a string to at most n runes (rune-safe, so a
// Chinese character is never cut in half).
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
