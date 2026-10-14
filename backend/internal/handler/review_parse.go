package handler

import (
	"encoding/json"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
)

// extractTrailingJSON splits a review report into (jsonPart, markdownPart).
//
// The contract in prompt.BuildReviewPrompt asks the model to end its reply
// with a ```json fenced block. We cannot reuse wizard_stream.go's
// extractJSON here: that one scans from the FIRST '{' in the text, and a
// review report's prose is full of braces (code snippets, Go struct
// literals, template syntax), so it would reliably grab the wrong span.
//
// Resolution order:
//  1. the LAST ```json … ``` fence in the text
//  2. failing that, a brace-matched object anchored at the LAST top-level
//     '{' that balances through to the end of the text
//
// markdownPart is the report with that span (and its fence) removed and
// trailing whitespace trimmed. When nothing parses, jsonPart is "" and
// markdownPart is the input unchanged — the caller then stores the whole
// report and the UI falls back to plain-Markdown rendering.
func extractTrailingJSON(s string) (jsonPart, markdownPart string) {
	if fenceJSON, rest, ok := lastJSONFence(s); ok {
		return fenceJSON, strings.TrimRight(rest, " \t\r\n")
	}
	if braceJSON, rest, ok := lastBalancedObject(s); ok {
		return braceJSON, strings.TrimRight(rest, " \t\r\n")
	}
	return "", s
}

// lastJSONFence finds the last ```json (or ```JSON) fence and returns its
// body plus the text with the whole fence removed.
func lastJSONFence(s string) (body, rest string, ok bool) {
	lower := strings.ToLower(s)
	open := strings.LastIndex(lower, "```json")
	if open < 0 {
		return "", "", false
	}
	// Body starts after the fence marker's own line.
	nl := strings.Index(s[open:], "\n")
	if nl < 0 {
		return "", "", false
	}
	bodyStart := open + nl + 1
	closeIdx := strings.Index(s[bodyStart:], "```")
	if closeIdx < 0 {
		// Unterminated fence — take everything to the end; models truncate
		// the closing marker often enough that dropping the block entirely
		// would lose real data.
		return strings.TrimSpace(s[bodyStart:]), s[:open], true
	}
	end := bodyStart + closeIdx + len("```")
	return strings.TrimSpace(s[bodyStart : bodyStart+closeIdx]), s[:open] + s[end:], true
}

// lastBalancedObject walks backwards over every '{' and returns the first
// one whose braces balance at the end of the string (ignoring braces inside
// JSON string literals). That anchoring is what keeps prose braces out.
func lastBalancedObject(s string) (body, rest string, ok bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != '{' {
			continue
		}
		if end, good := matchObject(s, i); good {
			if strings.TrimSpace(s[end:]) != "" {
				// Trailing prose after the object — not the terminal block.
				continue
			}
			return strings.TrimSpace(s[i:end]), s[:i], true
		}
	}
	return "", "", false
}

// matchObject returns the index just past the '}' that closes the object
// opened at start, honouring string literals and backslash escapes.
func matchObject(s string, start int) (int, bool) {
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// reviewContract is the shape of the trailing JSON block.
type reviewContract struct {
	GoalMet  string                `json:"goal_met"`
	Findings []model.ReviewFinding `json:"findings"`
}

// parseFindings decodes the contract block. ok=false means "no usable
// structured result" — the caller then persists the full report text with
// empty findings and zero counts.
//
// Findings whose severity isn't one of the three known buckets are dropped
// (a stray "P2" would otherwise be invisible in the UI's three-way grouping
// while still inflating nothing). goal_met is normalised to lowercase and
// cleared when it isn't one of the three verdicts.
func parseFindings(jsonPart string) (findings []model.ReviewFinding, goalMet string, ok bool) {
	jsonPart = strings.TrimSpace(jsonPart)
	if jsonPart == "" {
		return nil, "", false
	}
	var c reviewContract
	if err := json.Unmarshal([]byte(jsonPart), &c); err != nil {
		return nil, "", false
	}
	switch strings.ToLower(strings.TrimSpace(c.GoalMet)) {
	case prompt.GoalMetYes:
		goalMet = prompt.GoalMetYes
	case prompt.GoalMetPartial:
		goalMet = prompt.GoalMetPartial
	case prompt.GoalMetNo:
		goalMet = prompt.GoalMetNo
	}
	findings = make([]model.ReviewFinding, 0, len(c.Findings))
	for _, f := range c.Findings {
		sev := strings.ToUpper(strings.TrimSpace(f.Severity))
		if sev != prompt.SevP0 && sev != prompt.SevP1 && sev != prompt.SevP3 {
			continue
		}
		f.Severity = sev
		findings = append(findings, f)
	}
	return findings, goalMet, true
}

// countBySeverity denormalises the three bucket sizes for code_reviews'
// p0/p1/p3_count columns, so the history table renders without parsing
// findings_json per row.
func countBySeverity(findings []model.ReviewFinding) (p0, p1, p3 int) {
	for _, f := range findings {
		switch f.Severity {
		case prompt.SevP0:
			p0++
		case prompt.SevP1:
			p1++
		case prompt.SevP3:
			p3++
		}
	}
	return p0, p1, p3
}
