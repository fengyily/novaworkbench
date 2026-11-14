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

// flexString decodes a JSON value into a string, tolerating the type drift
// models routinely produce.
//
// WHY THIS EXISTS: the contract asks for `"line": "12-20"`, but a model
// writing `"line": 664` is the RULE, not the exception — and a strict
// decode turns that single type mismatch into an UnmarshalTypeError for the
// WHOLE blob. That error used to cost the report its entire grading (and,
// because goal_checks was decoded from the same blob, its evidence table
// too): one field's type and eleven findings' worth of judgement went down
// together.
//
// So the string-ish fields are decoded ONE AT A TIME, from raw JSON, and a
// value that isn't a string is rendered as its textual form:
//   - "abc"      → "abc"
//   - 664        → "664"
//   - true       → "true"
//   - null       → ""
//   - {} / []    → "" (no sensible textual form; the caller treats it as
//                  "this field said nothing")
//
// It changes TYPES, never MEANINGS: the severity whitelist, the goal_met
// whitelist and the goal_checks status normalisation all still apply.
func flexString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		if b {
			return "true"
		}
		return "false"
	}
	return ""
}

// contractSections is the tolerant decode of one trailing contract block.
//
// The three parts are decoded INDEPENDENTLY on purpose: `goal_met`,
// `findings` and `goal_checks` have different stakes (a one-word verdict /
// the report itself / supporting evidence) and none of them is allowed to
// take the others down.
type contractSections struct {
	// GoalMet is the one-word verdict, already normalised to the
	// prompt.GoalMet* vocabulary ("" when the model said something else).
	GoalMet string
	// Findings are the graded issues. FindingsOK reports whether the
	// `findings` section was USABLE — false means it was not an array at
	// all, which is the only failure that costs the whole report.
	Findings   []model.ReviewFinding
	FindingsOK bool
	// GoalChecksJSON is the re-marshalled evidence table, "" when there is
	// none worth showing.
	GoalChecksJSON string
}

// parseContractSections decodes a trailing contract block, section by
// section.
//
// The block itself must be a JSON OBJECT — that much is non-negotiable, or
// there is no contract at all. Inside it, every section is decoded on its
// own, so:
//   - `findings` not an array → FindingsOK=false (the report degrades) but
//     goal_checks still survives;
//   - `goal_checks` broken → the evidence table is empty but the findings
//     are graded;
//   - one finding row with a wrongly typed field → that field is normalised
//     (or that row is dropped), the other rows are unaffected.
func parseContractSections(jsonPart string) contractSections {
	var out contractSections
	jsonPart = strings.TrimSpace(jsonPart)
	if jsonPart == "" {
		return out
	}
	// The envelope is decoded as a raw map so a type error INSIDE a section
	// cannot abort the decode of the others.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonPart), &envelope); err != nil {
		return out
	}

	out.GoalMet = normalizeGoalMet(flexString(envelope["goal_met"]))

	// findings — the report itself.
	//
	// A MISSING key means "the model found nothing" (the contract's
	// `"findings":[]` written shorter), which is a valid, empty grading. A
	// key that is present but ISN'T an array means the model broke the
	// contract in a way that leaves no usable grading — the caller degrades
	// to plain Markdown rather than inventing one.
	if raw, present := envelope["findings"]; present {
		var rows []json.RawMessage
		if err := json.Unmarshal(raw, &rows); err != nil {
			// Not an array: no usable grading. goal_checks is still decoded
			// below — one broken section must not take the others down.
			out.FindingsOK = false
			out.GoalChecksJSON = parseGoalChecksFrom(envelope["goal_checks"])
			return out
		}
		findings := make([]model.ReviewFinding, 0, len(rows))
		for _, row := range rows {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(row, &fields); err != nil {
				// A row that isn't an object says nothing; drop it and keep
				// the rest of the report.
				continue
			}
			f := model.ReviewFinding{
				Severity:   flexString(fields["severity"]),
				File:       flexString(fields["file"]),
				Line:       flexString(fields["line"]),
				Title:      flexString(fields["title"]),
				Detail:     flexString(fields["detail"]),
				Suggestion: flexString(fields["suggestion"]),
			}
			sev := strings.ToUpper(strings.TrimSpace(f.Severity))
			if sev != prompt.SevP0 && sev != prompt.SevP1 && sev != prompt.SevP3 {
				// A stray "P2" would otherwise be invisible in the UI's
				// three-way grouping while still inflating nothing.
				continue
			}
			f.Severity = sev
			findings = append(findings, f)
		}
		out.Findings = findings
		out.FindingsOK = true
	} else {
		out.Findings = []model.ReviewFinding{}
		out.FindingsOK = true
	}

	// goal_checks — supporting evidence, decoded on its own so a broken
	// findings section (or vice versa) cannot take it down.
	out.GoalChecksJSON = parseGoalChecksFrom(envelope["goal_checks"])
	return out
}

// normalizeGoalMet maps the contract's verdict onto the known vocabulary.
// Anything else (including a number) is cleared: an unknown verdict must
// not be shown as one of the three.
func normalizeGoalMet(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case prompt.GoalMetYes:
		return prompt.GoalMetYes
	case prompt.GoalMetPartial:
		return prompt.GoalMetPartial
	case prompt.GoalMetNo:
		return prompt.GoalMetNo
	}
	return ""
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
	sections := parseContractSections(jsonPart)
	if !sections.FindingsOK {
		return nil, "", false
	}
	return sections.Findings, sections.GoalMet, true
}

// parseFindingsArray decodes the PERSISTED findings array
// (code_reviews.findings_json).
//
// It is a DIFFERENT shape from the contract block parseFindings decodes:
// the contract is the whole `{"goal_met":…,"findings":[…]}` object, while
// the persisted column holds ONLY the `findings` array, re-marshalled by
// buildReviewOutcome. Feeding the array to parseFindings fails — an array
// cannot unmarshal into the contract struct — which is why this function
// exists.
//
// The severity normalisation is the same rule parseFindings applies: an
// unknown bucket ("P2"…) is dropped, so the three-way grouping stays total.
//
// ok=false means the column holds something that is not a findings array
// (the model broke the contract and the column is empty, or a legacy row
// stores the raw contract object). Callers then report "no structured
// findings" rather than pretending the report is empty.
func parseFindingsArray(findingsJSON string) (findings []model.ReviewFinding, ok bool) {
	findingsJSON = strings.TrimSpace(findingsJSON)
	if findingsJSON == "" {
		return nil, false
	}
	var rows []json.RawMessage
	if err := json.Unmarshal([]byte(findingsJSON), &rows); err != nil {
		return nil, false
	}
	out := make([]model.ReviewFinding, 0, len(rows))
	for _, row := range rows {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(row, &fields); err != nil {
			continue
		}
		f := model.ReviewFinding{
			Severity:   flexString(fields["severity"]),
			File:       flexString(fields["file"]),
			Line:       flexString(fields["line"]),
			Title:      flexString(fields["title"]),
			Detail:     flexString(fields["detail"]),
			Suggestion: flexString(fields["suggestion"]),
		}
		sev := strings.ToUpper(strings.TrimSpace(f.Severity))
		if sev != prompt.SevP0 && sev != prompt.SevP1 && sev != prompt.SevP3 {
			continue
		}
		f.Severity = sev
		out = append(out, f)
	}
	return out, true
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

// parseGoalChecks pulls the optional `goal_checks` array out of the contract
// block and re-marshals the rows worth keeping, ready for
// code_reviews.goal_checks_json.
//
// It is separate from parseFindings (and fails separately) because the two
// have different stakes. Findings are the report: losing them means losing
// the grading. goal_checks is supporting evidence for a verdict the report
// states anyway, so every failure mode here degrades to "" — no evidence
// table, verdict and findings untouched.
//
// Rows are dropped when they carry no criterion (an entry with only a status
// says nothing) and statuses outside the four known verdicts are normalised
// to "unknown" rather than discarded: the criterion is still worth showing,
// and silently dropping it would misrepresent how many points were checked.
func parseGoalChecks(jsonPart string) string {
	jsonPart = strings.TrimSpace(jsonPart)
	if jsonPart == "" {
		return ""
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonPart), &envelope); err != nil {
		return ""
	}
	return parseGoalChecksFrom(envelope["goal_checks"])
}

// parseGoalChecksFrom decodes the `goal_checks` section on its own.
//
// It takes the RAW section rather than the whole blob so that a broken
// `findings` section cannot take the evidence table down with it — the
// failure this function used to share with the grading.
//
// Field types are normalised with flexString for the same reason: a model
// that writes `"evidence": 42` (or a numeric criterion) means something, and
// losing the whole evidence table over one field's type would be the same
// mistake one level up.
func parseGoalChecksFrom(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return ""
	}
	checks := make([]model.GoalCheck, 0, len(rows))
	for _, row := range rows {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(row, &fields); err != nil {
			continue
		}
		c := model.GoalCheck{
			Criterion: strings.TrimSpace(flexString(fields["criterion"])),
			Status:    normalizeGoalCheckStatus(flexString(fields["status"])),
			Evidence:  strings.TrimSpace(flexString(fields["evidence"])),
		}
		if c.Criterion == "" {
			continue
		}
		checks = append(checks, c)
	}
	if len(checks) == 0 {
		return ""
	}
	out, err := json.Marshal(checks)
	if err != nil {
		return ""
	}
	return string(out)
}

// normalizeGoalCheckStatus maps a per-criterion verdict onto the four values
// the UI colours. Anything unrecognised (including empty) becomes "unknown",
// which is a real verdict here: "this criterion could not be checked".
func normalizeGoalCheckStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case prompt.GoalMetYes:
		return prompt.GoalMetYes
	case prompt.GoalMetPartial:
		return prompt.GoalMetPartial
	case prompt.GoalMetNo:
		return prompt.GoalMetNo
	default:
		return model.GoalCheckUnknown
	}
}
