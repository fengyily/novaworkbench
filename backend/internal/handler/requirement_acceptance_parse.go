package handler

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
)

// Acceptance-check report parsing.
//
// The model emits "a Markdown report + a trailing ```json contract block".
// This file splits the two halves: the Markdown part is for humans (it
// lands in code_reviews.result_md), the JSON part is for the panel (it
// lands in code_reviews.acceptance_json).
//
// The key trade-off: **the model's output is not trustworthy**. So the
// parser has three degradation levels, and NONE of them panics — a model
// that writes one field wrong should cost that ONE verdict (it becomes
// "unverifiable"), not the whole report and every other verdict with it.
//
//	① the trailing json block decodes → the structured result comes from it
//	② the block is missing or broken → the report's own sections are parsed
//	   deterministically as a fallback
//	③ nothing usable → only the Markdown survives, and the panel degrades
//	   to plain-Markdown rendering
//
// It reuses two techniques from review_parse.go (same package):
//   - extractTrailingJSON: takes the LAST ```json fence (the prose is full
//     of braces, so scanning from the first '{' would grab the wrong span);
//   - flexString: tolerates type drift (a model writing a line number as a
//     number is routine, not exceptional).

// parseAcceptanceReport splits one acceptance-check output into the
// human-readable report and the structured summary.
//
// The three return values mean:
//
//   - resultMD — the report with the trailing JSON block removed. Always
//     usable as long as the input was not empty.
//   - summary — the structured result. May be EMPTY (level ③): the caller
//     then persists an empty acceptance_json and the UI renders the report
//     as plain Markdown.
//   - err — non-nil when the trailing JSON block was PRESENT but could not
//     be decoded. The caller keeps resultMD (and whatever the Markdown
//     fallback recovered) and reports the degradation; it must NOT fail the
//     whole run, because a usable report exists.
//
// A MISSING JSON block is not an error: the contract is best-effort and the
// Markdown fallback covers it.
func parseAcceptanceReport(raw string) (string, model.AcceptanceSummary, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", model.AcceptanceSummary{}, fmt.Errorf("验收报告为空")
	}

	jsonPart, markdownPart := extractTrailingJSON(raw)
	markdownPart = strings.TrimSpace(markdownPart)

	var summary model.AcceptanceSummary
	var err error
	if strings.TrimSpace(jsonPart) != "" {
		summary, err = parseAcceptanceJSON(jsonPart)
		if err != nil {
			// The block is broken: fall back to the Markdown sections so the
			// user still gets a structured verdict, and report the breakage.
			fallback := parseAcceptanceFromMarkdown(markdownPart)
			if fallbackHasContent(fallback) {
				summary = fallback
			}
			return markdownPart, summary, err
		}
		return markdownPart, summary, nil
	}

	// No JSON block: rebuild from the report's own sections.
	summary = parseAcceptanceFromMarkdown(markdownPart)
	return markdownPart, summary, nil
}

// fallbackHasContent reports whether a Markdown-derived summary carries
// anything worth persisting.
func fallbackHasContent(s model.AcceptanceSummary) bool {
	return s.Verdict != "" || s.Summary != "" ||
		len(s.Checks) > 0 || len(s.Gaps) > 0 || len(s.Proposals) > 0
}

// parseAcceptanceJSON decodes the trailing contract block.
//
// Every section is decoded INDEPENDENTLY: a broken `checks` array must not
// take the gaps and the proposals down with it, and one row with a wrongly
// typed field must not take the other rows down. That is the same
// discipline parseContractSections applies to a Review contract.
//
// The returned error is only about the ENVELOPE: "this is not a JSON object
// at all". A missing section is a valid, empty section.
func parseAcceptanceJSON(jsonPart string) (model.AcceptanceSummary, error) {
	var out model.AcceptanceSummary
	jsonPart = strings.TrimSpace(jsonPart)
	if jsonPart == "" {
		return out, fmt.Errorf("验收 JSON 块为空")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonPart), &envelope); err != nil {
		return out, fmt.Errorf("验收 JSON 块解析失败: %w", err)
	}

	out.Verdict = normalizeAcceptanceVerdict(flexString(envelope["verdict"]))
	out.Summary = strings.TrimSpace(flexString(envelope["summary"]))
	out.Checks = parseAcceptanceChecks(envelope["checks"])
	out.Gaps = parseAcceptanceGaps(envelope["gaps"])
	out.Proposals = parseAcceptanceProposals(envelope["proposals"])
	return out, nil
}

// normalizeAcceptanceVerdict maps the contract's overall verdict onto the
// known vocabulary. Anything else is cleared — an unknown verdict must not
// be shown as one of the three.
func normalizeAcceptanceVerdict(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case model.AcceptanceVerdictYes:
		return model.AcceptanceVerdictYes
	case model.AcceptanceVerdictPartial:
		return model.AcceptanceVerdictPartial
	case model.AcceptanceVerdictNo:
		return model.AcceptanceVerdictNo
	}
	return ""
}

// normalizeAcceptanceStatus maps a per-criterion verdict onto the four values
// the UI colours. "unknown" is a real outcome here ("could not be checked"),
// so an unrecognised status becomes "unknown" rather than being dropped.
func normalizeAcceptanceStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case model.AcceptanceVerdictYes:
		return model.AcceptanceVerdictYes
	case model.AcceptanceVerdictPartial:
		return model.AcceptanceVerdictPartial
	case model.AcceptanceVerdictNo:
		return model.AcceptanceVerdictNo
	default:
		return model.AcceptanceCheckUnknown
	}
}

// hollowEvidencePhrases are the verdicts-as-evidence the contract forbids:
// "已确认" tells the reader nothing they could check.
//
// They are matched as SUBSTRINGS and stripped, so an evidence that carries a
// real reference alongside one of them ("已确认，见 backend/x.go:12") still
// counts as checkable.
var hollowEvidencePhrases = []string{
	"已确认", "已验证", "已实现", "已达成", "已满足", "已检查", "已核对",
	"符合预期", "符合要求", "满足要求", "实现无误", "确定无误", "无误",
	"无问题", "正常", "通过", "满足", "达成",
}

// hollowEvidence reports whether an evidence string says nothing checkable.
//
// WHY THIS EXISTS: the contract asks for `文件:行号`-shaped evidence, but a
// model that did not really open the file can still emit a confident verdict
// with a hollow justification. A "通过" backed by "已确认" is indistinguishable
// from a real check — and a WRONG acceptance verdict is worse than no verdict
// at all, because the user closes the master requirement on it.
//
// So an evidence that carries no checkable reference forces the criterion's
// status down to "unknown": the model said it passed, we recorded that we
// could not verify it.
func hollowEvidence(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return true
	}
	for _, p := range hollowEvidencePhrases {
		t = strings.ReplaceAll(t, p, "")
	}
	t = strings.TrimSpace(strings.Trim(t, "：:，,。.、；;（）()[]【】\"'`-—~ \t\r\n"))
	return t == ""
}

// parseAcceptanceChecks decodes the `checks` section.
//
// A row without a criterion says nothing and is dropped. A row whose
// evidence is hollow keeps its criterion (the point IS worth showing) but
// its status is downgraded to "unknown".
func parseAcceptanceChecks(raw json.RawMessage) []model.AcceptanceCheck {
	if len(raw) == 0 {
		return nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil
	}
	out := make([]model.AcceptanceCheck, 0, len(rows))
	for _, row := range rows {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(row, &fields); err != nil {
			continue
		}
		c := model.AcceptanceCheck{
			Criterion:  strings.TrimSpace(flexString(fields["criterion"])),
			Status:     normalizeAcceptanceStatus(flexString(fields["status"])),
			Evidence:   strings.TrimSpace(flexString(fields["evidence"])),
			ChildID:    strings.TrimSpace(flexString(fields["child_id"])),
			ChildTitle: strings.TrimSpace(flexString(fields["child_title"])),
		}
		if c.Criterion == "" {
			continue
		}
		if hollowEvidence(c.Evidence) {
			c.Status = model.AcceptanceCheckUnknown
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseAcceptanceGaps decodes the `gaps` section.
func parseAcceptanceGaps(raw json.RawMessage) []model.AcceptanceGap {
	if len(raw) == 0 {
		return nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil
	}
	out := make([]model.AcceptanceGap, 0, len(rows))
	for _, row := range rows {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(row, &fields); err != nil {
			continue
		}
		g := model.AcceptanceGap{
			Title:      strings.TrimSpace(flexString(fields["title"])),
			Detail:     strings.TrimSpace(flexString(fields["detail"])),
			ChildID:    strings.TrimSpace(flexString(fields["child_id"])),
			ChildTitle: strings.TrimSpace(flexString(fields["child_title"])),
		}
		if g.Title == "" {
			continue
		}
		out = append(out, g)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseAcceptanceProposals decodes the `proposals` section.
func parseAcceptanceProposals(raw json.RawMessage) []model.AcceptanceProposal {
	if len(raw) == 0 {
		return nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil
	}
	out := make([]model.AcceptanceProposal, 0, len(rows))
	for _, row := range rows {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(row, &fields); err != nil {
			continue
		}
		p := model.AcceptanceProposal{
			Title:      strings.TrimSpace(flexString(fields["title"])),
			Goal:       strings.TrimSpace(flexString(fields["goal"])),
			Acceptance: flexStringSlice(fields["acceptance"]),
			DependsOn:  flexStringSlice(fields["depends_on"]),
		}
		if p.Title == "" {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// flexStringSlice decodes a JSON array of strings, tolerating the type drift
// models produce (a number in a list of strings is a routine slip, not a
// reason to lose the whole list).
//
// A value that is neither a string nor a number is dropped rather than
// rendered as "{}" — an object in a list of acceptance points says nothing.
func flexStringSlice(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		s := strings.TrimSpace(flexString(row))
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ── Markdown fallback parsing ─────────────────────────────────────────────
//
// When the trailing json block is missing or broken, the verdict is
// recovered from the report's own sections.
//
// This is a best-effort DETERMINISTIC parse: it does not guess, it only
// recognises the most stable shape — "a ## heading + a bullet list". What
// it recovers is thinner than the json block (a multi-line structure with
// no field label loses its evidence and its owner), but it is still far
// better than a wall of plain text: the panel can at least show the
// overall verdict and the "not implemented" list.

// acceptanceSection is one "## " section of the report.
type acceptanceSection struct {
	// heading is the section title with the "## " prefix stripped.
	heading string
	// body is everything under the heading.
	body string
}

// splitAcceptanceSections splits a report into its "## " sections.
//
// A "## " heading that appears BEFORE any other heading produces an unnamed
// leading section — the contract asks for "## 总体结论" first, but a model
// may prepend a "# " title, and the leading prose then belongs to nobody.
func splitAcceptanceSections(md string) []acceptanceSection {
	lines := strings.Split(md, "\n")
	out := []acceptanceSection{}
	heading := ""
	var body []string
	flush := func() {
		out = append(out, acceptanceSection{heading: heading, body: strings.Join(body, "\n")})
		heading = ""
		body = nil
	}
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "## ") {
			flush()
			heading = strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			continue
		}
		body = append(body, ln)
	}
	flush()
	return out
}

// sectionMatches reports whether a section heading names one of the wanted
// sections. The comparison is a CONTAINS match on the heading text, so
// "逐条验收判定（主需求）" still lands on "逐条验收判定".
func sectionMatches(heading string, wanted ...string) bool {
	h := strings.TrimSpace(heading)
	for _, w := range wanted {
		if strings.Contains(h, w) {
			return true
		}
	}
	return false
}

// parseAcceptanceFromMarkdown rebuilds a summary from the report's own
// sections.
func parseAcceptanceFromMarkdown(md string) model.AcceptanceSummary {
	var out model.AcceptanceSummary
	md = strings.TrimSpace(md)
	if md == "" {
		return out
	}
	for _, sec := range splitAcceptanceSections(md) {
		switch {
		case sectionMatches(sec.heading, "总体结论", "总体判定", "结论"):
			if out.Summary == "" {
				out.Summary = firstMeaningfulLine(sec.body)
			}
			if out.Verdict == "" {
				out.Verdict = verdictFromText(sec.body)
			}
		case sectionMatches(sec.heading, "逐条验收判定", "逐条判定", "验收点"):
			out.Checks = append(out.Checks, parseAcceptanceCheckItems(sec.body)...)
		case sectionMatches(sec.heading, "未实现清单", "未实现", "缺失清单"):
			out.Gaps = append(out.Gaps, parseAcceptanceGapItems(sec.body)...)
		case sectionMatches(sec.heading, "建议再拆", "建议新增", "再拆"):
			out.Proposals = append(out.Proposals, parseAcceptanceProposalItems(sec.body)...)
		}
	}
	if out.Verdict == "" {
		out.Verdict = verdictFromChecks(out.Checks)
	}
	return out
}

// firstMeaningfulLine returns the first non-empty, non-bullet line of a
// section body — the headline sentence of a conclusion section.
func firstMeaningfulLine(body string) string {
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "-") || strings.HasPrefix(t, "*") {
			continue
		}
		return t
	}
	return ""
}

// verdictFromText reads the overall verdict out of a conclusion section.
//
// It looks for the three verdict words in Chinese (the contract asks for
// them) and falls back to the English ones.
func verdictFromText(body string) string {
	switch {
	case strings.Contains(body, "未达成"), strings.Contains(body, "未通过"):
		return model.AcceptanceVerdictNo
	case strings.Contains(body, "部分达成"), strings.Contains(body, "部分通过"):
		return model.AcceptanceVerdictPartial
	case strings.Contains(body, "已达成"), strings.Contains(body, "全部通过"):
		return model.AcceptanceVerdictYes
	}
	lower := strings.ToLower(body)
	switch {
	case strings.Contains(lower, "partial"):
		return model.AcceptanceVerdictPartial
	case strings.Contains(lower, "\"no\""), strings.Contains(lower, "'no'"):
		return model.AcceptanceVerdictNo
	case strings.Contains(lower, "\"yes\""), strings.Contains(lower, "'yes'"):
		return model.AcceptanceVerdictYes
	}
	return ""
}

// verdictFromChecks derives the overall verdict from the per-point verdicts.
//
// The rule is the conservative one: a single "no" makes the whole thing
// "no" (something required is missing), a "partial" makes it "partial",
// and "unknown" alone cannot claim a clean "yes" — the check did not see
// enough to certify the master requirement.
func verdictFromChecks(checks []model.AcceptanceCheck) string {
	if len(checks) == 0 {
		return ""
	}
	hasNo, hasPartial, hasUnknown, hasYes := false, false, false, false
	for _, c := range checks {
		switch c.Status {
		case model.AcceptanceVerdictNo:
			hasNo = true
		case model.AcceptanceVerdictPartial:
			hasPartial = true
		case model.AcceptanceCheckUnknown:
			hasUnknown = true
		case model.AcceptanceVerdictYes:
			hasYes = true
		}
	}
	switch {
	case hasNo:
		return model.AcceptanceVerdictNo
	case hasPartial:
		return model.AcceptanceVerdictPartial
	case hasUnknown:
		return ""
	case hasYes:
		return model.AcceptanceVerdictYes
	}
	return ""
}

// parseAcceptanceCheckItems parses the bullet list of the 「逐条验收判定」
// section.
//
// The shape the contract asks for is:
//
//	- 验收点 A
//	  - 判定：通过
//	  - 依据：backend/x.go:120
//	  - 归属：子需求①
//
// The inline form ("- 验收点 A · 判定：通过 · 依据：…") is accepted too,
// because that is how a model naturally writes a single line.
func parseAcceptanceCheckItems(body string) []model.AcceptanceCheck {
	out := []model.AcceptanceCheck{}
	var cur *model.AcceptanceCheck
	for _, ln := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" {
			continue
		}
		isBullet := strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ")
		if isBullet {
			text := strings.TrimSpace(strings.TrimLeft(trimmed, "-* "))
			if text == "" {
				continue
			}
			// A bullet that only carries a field ("- 判定：通过") belongs to
			// the current item, it does not start a new one.
			if cur != nil && isAcceptanceFieldLine(text) {
				applyAcceptanceField(cur, text)
				continue
			}
			out = append(out, model.AcceptanceCheck{})
			cur = &out[len(out)-1]
			cur.Criterion = text
			cur.Status = model.AcceptanceCheckUnknown
			applyAcceptanceInlineFields(cur, text)
			continue
		}
		if cur == nil {
			continue
		}
		applyAcceptanceField(cur, trimmed)
	}
	return out
}

// acceptanceFieldLabels are the field prefixes of a check row.
var acceptanceFieldLabels = []string{"判定", "结论", "依据", "归属", "说明", "验证"}

// isAcceptanceFieldLine reports whether a line is a FIELD of the current
// item rather than a new item.
func isAcceptanceFieldLine(text string) bool {
	for _, label := range acceptanceFieldLabels {
		if strings.HasPrefix(text, label) {
			return true
		}
	}
	return false
}

// applyAcceptanceField reads one "label: value" line into the check.
func applyAcceptanceField(c *model.AcceptanceCheck, text string) {
	label, value, ok := splitLabelValue(text)
	if !ok {
		return
	}
	switch label {
	case "判定", "结论":
		c.Status = normalizeAcceptanceStatus(statusFromText(value))
	case "依据", "验证":
		c.Evidence = value
	case "归属":
		c.ChildTitle = value
	}
}

// applyAcceptanceInlineFields reads the "· 判定：通过 · 依据：…" inline form
// out of an item's first line.
func applyAcceptanceInlineFields(c *model.AcceptanceCheck, text string) {
	parts := strings.Split(text, "·")
	if len(parts) < 2 {
		return
	}
	for _, p := range parts[1:] {
		applyAcceptanceField(c, strings.TrimSpace(p))
	}
}

// statusFromText maps a Chinese / English status word onto the vocabulary.
func statusFromText(v string) string {
	switch {
	case strings.Contains(v, "无法验证"), strings.Contains(v, "未验证"):
		return model.AcceptanceCheckUnknown
	case strings.Contains(v, "未通过"), strings.Contains(v, "未达成"), strings.Contains(v, "不通过"):
		return model.AcceptanceVerdictNo
	case strings.Contains(v, "部分通过"), strings.Contains(v, "部分达成"):
		return model.AcceptanceVerdictPartial
	case strings.Contains(v, "通过"), strings.Contains(v, "达成"):
		return model.AcceptanceVerdictYes
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// splitLabelValue splits a "label: value" line. It accepts the Chinese
// colon as well as the ASCII one.
//
// The value is cut by the colon's BYTE LENGTH, not by one byte: the Chinese
// colon is three bytes wide, and skipping a single byte would leave the last
// two bytes of it glued to the front of the value (a mojibake the model would
// have to read as part of the evidence).
func splitLabelValue(text string) (label, value string, ok bool) {
	for i, r := range text {
		if r != ':' && r != '：' {
			continue
		}
		label = strings.TrimSpace(text[:i])
		value = strings.TrimSpace(text[i+len(string(r)):])
		if label == "" {
			return "", "", false
		}
		return label, value, true
	}
	return "", "", false
}

// parseAcceptanceGapItems parses the bullet list of the 「未实现清单」 section.
func parseAcceptanceGapItems(body string) []model.AcceptanceGap {
	out := []model.AcceptanceGap{}
	for _, ln := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(ln)
		if !strings.HasPrefix(trimmed, "- ") && !strings.HasPrefix(trimmed, "* ") {
			continue
		}
		text := strings.TrimSpace(strings.TrimLeft(trimmed, "-* "))
		if text == "" {
			continue
		}
		g := model.AcceptanceGap{}
		// "缺少 X 校验 —— 归属子需求②" — the title/detail split.
		if title, detail, ok := splitGapText(text); ok {
			g.Title, g.Detail = title, detail
		} else {
			g.Title = text
		}
		if child, rest, ok := splitLabelValue(text); ok && (child == "归属" || child == "负责") {
			g.ChildTitle = rest
		}
		out = append(out, g)
	}
	return out
}

// splitGapText splits a gap line into its title and its detail.
//
// The separator is the em-dash pair the contract's own example uses
// ("缺少 X 校验 —— 归属子需求②"). A colon is accepted as a fallback.
func splitGapText(text string) (title, detail string, ok bool) {
	for _, sep := range []string{"——", "—", "：", ":"} {
		idx := strings.Index(text, sep)
		if idx <= 0 {
			continue
		}
		return strings.TrimSpace(text[:idx]), strings.TrimSpace(text[idx+len(sep):]), true
	}
	return "", "", false
}

// proposalFieldLabels are the field prefixes of a proposal row.
var proposalFieldLabels = []string{"目标", "验收标准", "验收点", "建议前置", "前置", "依赖"}

// isProposalFieldLine reports whether a bullet is a FIELD of the current
// proposal rather than a new proposal.
//
// The distinction is what makes the difference between ONE proposal with
// three fields and THREE proposals with one word each — and the contract's
// own example nests the fields under the proposal as bullets.
func isProposalFieldLine(text string) bool {
	for _, label := range proposalFieldLabels {
		if strings.HasPrefix(text, label) {
			return true
		}
	}
	return false
}

// parseAcceptanceProposalItems parses the bullet list of the
// 「建议再拆的子需求」 section.
func parseAcceptanceProposalItems(body string) []model.AcceptanceProposal {
	out := []model.AcceptanceProposal{}
	var cur *model.AcceptanceProposal
	for _, ln := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			text := strings.TrimSpace(strings.TrimLeft(trimmed, "-* "))
			if text == "" {
				continue
			}
			if cur != nil && isProposalFieldLine(text) {
				applyProposalField(cur, text)
				continue
			}
			out = append(out, model.AcceptanceProposal{})
			cur = &out[len(out)-1]
			cur.Title = text
			continue
		}
		if cur == nil {
			continue
		}
		applyProposalField(cur, trimmed)
	}
	return out
}

// applyProposalField reads one "label: value" line into the proposal.
func applyProposalField(p *model.AcceptanceProposal, text string) {
	label, value, ok := splitLabelValue(text)
	if !ok {
		return
	}
	switch label {
	case "目标":
		p.Goal = value
	case "验收标准", "验收点":
		p.Acceptance = append(p.Acceptance, splitInlineList(value)...)
	case "建议前置", "前置", "依赖":
		p.DependsOn = append(p.DependsOn, splitInlineList(value)...)
	}
}

// splitInlineList splits an inline "a、b；c" list into its items.
func splitInlineList(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '、' || r == ';' || r == '；' || r == ',' || r == '，'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
