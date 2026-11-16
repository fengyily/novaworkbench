package handler

import "strings"

// claudeDocReuseWarnRatio is the share of the existing on-disk CLAUDE.md's
// prose that has to reappear verbatim in a generated document before the
// run is flagged as "this looks like a copy".
//
// The threshold is not zero because the generation prompt asks for a
// specific section layout, so some lines legitimately coincide. It is not
// low either: a warning the user sees on every run is a warning they stop
// reading.
const claudeDocReuseWarnRatio = 0.6

// claudeDocReuseMinSubstantialLines is the minimum number of comparable
// lines the existing document must have before a reuse ratio means
// anything. A two-sentence CLAUDE.md has nothing to copy.
const claudeDocReuseMinSubstantialLines = 3

// claudeDocSubstantialLineMinLen is the shortest prose line that carries
// enough content to be evidence of copying.
const claudeDocSubstantialLineMinLen = 16

// claudeDocReuseRatio reports which share of the existing on-disk
// CLAUDE.md reappears verbatim in the freshly generated document.
//
// It is the observable counterpart of the generation prompt's "do not copy
// the existing document" rule: the prompt is a request, and a request can
// be ignored. This measures what actually came out.
//
// Only PROSE is compared. The other parts of a document coincide by
// construction and would drown the signal:
//   - the section headings are prescribed by the generation prompt;
//   - the command lines, file paths and build targets MUST be identical,
//     because they are read off the repository's own build configuration —
//     a correct document reproduces them, so comparing them would flag
//     every correct run.
//
// What a copied document gives away is its PROSE: the sentences describing
// the architecture, the conventions and the workflow. Those are where the
// stale claims live, and where a derived document necessarily differs.
//
// The ratio is 0 when the existing document has too few comparable lines to
// judge — there is nothing to copy, so there is nothing to report.
func claudeDocReuseRatio(existing, generated string) float64 {
	substantial := claudeDocSubstantialLines(existing)
	if len(substantial) < claudeDocReuseMinSubstantialLines {
		return 0
	}
	matched := 0
	for _, line := range substantial {
		if strings.Contains(generated, line) {
			matched++
		}
	}
	return float64(matched) / float64(len(substantial))
}

// claudeDocSubstantialLines extracts the comparable prose lines of a
// document: the free-standing sentences, and nothing else.
//
// Excluded: headings (prescribed by the prompt), list items and table
// rows (they carry the commands and paths, which a correct document
// reproduces), block quotes and fenced code (not prose at all).
func claudeDocSubstantialLines(doc string) []string {
	out := make([]string, 0, 16)
	inFence := false
	for _, raw := range strings.Split(doc, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if inFence || line == "" {
			continue
		}
		if claudeDocIsStructuralLine(line) {
			continue
		}
		if len([]rune(line)) < claudeDocSubstantialLineMinLen {
			continue
		}
		out = append(out, line)
	}
	return out
}

// claudeDocIsStructuralLine reports whether a line is layout rather than
// prose: a heading, a list item, a table row, a block quote or a
// numbered item.
func claudeDocIsStructuralLine(line string) bool {
	if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ">") ||
		strings.HasPrefix(line, "|") || strings.HasPrefix(line, "-") ||
		strings.HasPrefix(line, "*") || strings.HasPrefix(line, "+") {
		return true
	}
	// Numbered list item ("1. ", "2) ", "3、").
	if len(line) >= 2 && line[0] >= '0' && line[0] <= '9' {
		rest := line[1:]
		if strings.HasPrefix(rest, ". ") || strings.HasPrefix(rest, ") ") ||
			strings.HasPrefix(rest, ". ") || strings.HasPrefix(rest, "、") {
			return true
		}
	}
	return false
}
