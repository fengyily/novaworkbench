package prompt

import (
	"regexp"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
)

// 「跨项目上下文块」—— 注入到跨项目需求的**子需求**里，让各仓库的执行 Agent
// 看到自己那一份之外的整体契约。
//
// 为什么必须持久化到子需求的 description 里（D10）：子需求的执行 Agent 看不到
// 父实体的整体方案全文，它只能看到自己的描述 + 本上下文块。所以这段正文必须
// **自包含** —— 落库时写进 description（见 service.buildCrossDescription），
// prompt 注入只是刷新，不依赖。
//
// 本文件是纯函数（对齐 parent_context.go 的风格）：handler 拥有所有 I/O。

// CrossProjectRef — 参与项目在上下文块里的最小描述。
type CrossProjectRef struct {
	// ID — 项目 id（req_<hex> / proj_<hex>）。
	ID string
	// Name — 项目显示名。
	Name string
	// Role — 角色（前端 / 后端 / 移动端 …）。
	Role string
	// BaseBranch — 该项目的基础分支（空 = 沿用项目默认分支）。
	//
	// 写进上下文块是为了让执行 Agent 知道自己那条开发线的基线 —— 跨仓库
	// 协作里「A 在 develop、B 在 release/2026q1」是常见情形，Agent 不知道
	// 就可能按错误的版本假设写代码。
	BaseBranch string
}

// crossContractHeading — 整体方案里「跨项目接口契约」章节的标题。
//
// 服务端用 HasCrossContractSection 校验整体方案是否包含这个章节 —— 缺了它，
// 各仓库就会各自设计、各自实现，联调时才暴露不一致（风险表里的中风险项）。
const crossContractHeading = "## 跨项目接口契约"

// crossContractMaxRunes — 契约章节注入到子需求正文时的 rune 上限。
//
// 子需求正文要进 prompt，全量注入整体方案会挤爆上下文预算。契约章节是
// 跨仓库协作的**唯一**硬约定，所以它优先保留；其余部分截断。
const crossContractMaxRunes = 6 * 1024

// crossOverviewMaxRunes — 整体方案摘要（除契约章节外）的 rune 上限。
const crossOverviewMaxRunes = 4 * 1024

// HasCrossContractSection reports whether the overall design document
// contains the mandatory 「跨项目接口契约」 section.
//
// The check is intentionally forgiving about the heading level and the
// whitespace around it (the model writes "## 跨项目接口契约" but may add a
// colon or trailing spaces) — what matters is that a section with that
// heading exists, not that it is byte-identical.
func HasCrossContractSection(markdown string) bool {
	trimmed := strings.TrimSpace(markdown)
	if trimmed == "" {
		return false
	}
	for _, line := range strings.Split(trimmed, "\n") {
		head := strings.TrimSpace(line)
		if !strings.HasPrefix(head, "#") {
			continue
		}
		// Strip the leading '#'s and any trailing punctuation.
		head = strings.TrimLeft(head, "#")
		head = strings.TrimSpace(head)
		head = strings.TrimSuffix(head, "：")
		head = strings.TrimSuffix(head, ":")
		head = strings.TrimSpace(head)
		if head == strings.TrimSpace(strings.TrimPrefix(crossContractHeading, "## ")) {
			return true
		}
	}
	return false
}

// ExtractCrossContractSection returns the body of the 「跨项目接口契约」
// section (the heading line and everything until the next heading of the same
// or higher level). Empty when the section is absent.
func ExtractCrossContractSection(markdown string) string {
	trimmed := strings.TrimSpace(markdown)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	start := -1
	end := len(lines)
	for i, line := range lines {
		head := strings.TrimSpace(line)
		if !strings.HasPrefix(head, "#") {
			continue
		}
		level := len(head) - len(strings.TrimLeft(head, "#"))
		name := strings.TrimSpace(strings.TrimLeft(head, "#"))
		name = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(name), "："), ":")
		if name == "跨项目接口契约" {
			start = i + 1
			continue
		}
		if start >= 0 && level <= 2 {
			end = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	if end <= start {
		return ""
	}
	return strings.TrimSpace(strings.Join(lines[start:end], "\n"))
}

// BuildCrossContextBlock renders the cross-project context block that is
// injected into a child requirement's prompt.
//
// The block has three parts:
//  1. 整体方案摘要（除契约章节外，截断）
//  2. 跨项目接口契约（完整保留，这是跨仓库协作的硬约定）
//  3. 参与项目清单（每个项目的角色）
//
// An empty design document yields an empty block — the caller then behaves
// exactly as it did before the feature existed.
func BuildCrossContextBlock(designMarkdown string, projects []CrossProjectRef, childTitle string) string {
	design := strings.TrimSpace(designMarkdown)
	if design == "" && len(projects) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n## 跨项目上下文\n")
	if title := strings.TrimSpace(childTitle); title != "" {
		b.WriteString("\n本需求属于一个跨项目需求（横跨多个仓库），子需求「" + title + "」是其中之一。\n")
	}
	if len(projects) > 0 {
		b.WriteString("\n### 参与项目\n")
		for _, p := range projects {
			name := strings.TrimSpace(p.Name)
			if name == "" {
				name = strings.TrimSpace(p.ID)
			}
			role := strings.TrimSpace(p.Role)
			if role != "" {
				b.WriteString("- " + name + "（" + role + "）")
			} else {
				b.WriteString("- " + name)
			}
			if base := strings.TrimSpace(p.BaseBranch); base != "" {
				b.WriteString("（基础分支：" + base + "）")
			}
			b.WriteString("\n")
		}
	}
	contract := ExtractCrossContractSection(design)
	if contract != "" {
		b.WriteString("\n### 跨项目接口契约（必须遵守，不得自行改动接口形状）\n\n")
		b.WriteString(truncateRunes(contract, crossContractMaxRunes))
		b.WriteString("\n")
	}
	if overview := strings.TrimSpace(removeSection(design, "跨项目接口契约")); overview != "" {
		b.WriteString("\n### 整体方案摘要\n\n")
		b.WriteString(truncateRunes(overview, crossOverviewMaxRunes))
		b.WriteString("\n")
	}
	return b.String()
}

// removeSection strips one named section (heading + body) from a Markdown
// document. Used to build the "everything but the contract" overview.
func removeSection(markdown string, name string) string {
	trimmed := strings.TrimSpace(markdown)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	out := make([]string, 0, len(lines))
	skipping := false
	skipLevel := 0
	for _, line := range lines {
		head := strings.TrimSpace(line)
		if strings.HasPrefix(head, "#") {
			level := len(head) - len(strings.TrimLeft(head, "#"))
			title := strings.TrimSpace(strings.TrimLeft(head, "#"))
			title = strings.TrimSuffix(strings.TrimSuffix(title, "："), ":")
			if skipping {
				if level <= skipLevel {
					skipping = false
				} else {
					continue
				}
			}
			if title == name {
				skipping = true
				skipLevel = level
				continue
			}
		}
		if !skipping {
			out = append(out, line)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// ── 待决策事项 ──────────────────────────────────────────────────────────────
//
// 整体方案常常留着「还没定、需要人拍板」的点（接口超时取多少、幂等键用哪种
// 策略……）。这些点是「方案追加调整」这条路径的入口：用户点一下就能就着它
// 提问，跟 AI 谈拢之后重跑一次方案作业。
//
// 解析出来的结果**不落库** —— 它就是方案正文的一部分，方案一改它就跟着变，
// 详情接口每次现算（见 service.CrossRequirementService.Detail）。

// crossPendingHeadingKeywords — 「待决策事项」章节的标题关键词。
//
// 刻意不只认「待决策事项」这一种写法：章节契约只是**建议**模型写这一节，
// 而契约出现之前写出来的存量方案，待决策点多半在模型自选的标题下（「## 待定」
// 「## 开放问题」……）。按**包含**匹配一组关键词，命中即解析。
//
// 命中不到不是错误 —— 返回空列表，chip 不出现，调整对话照常可用。
var crossPendingHeadingKeywords = []string{
	"待决策",
	"待定",
	"待确认",
	"开放问题",
	"遗留问题",
}

// crossBoldRe matches the first **bold** span of a line — the convention the
// design contract uses for a decision's headline ("**接口超时时间**：…").
var crossBoldRe = regexp.MustCompile(`\*\*([^*]+)\*\*`)

// crossDecisionTitleMaxRunes caps the chip label. A chip longer than this is
// unreadable in the toolbar; the full line stays in Detail.
const crossDecisionTitleMaxRunes = 40

// crossDecisionMaxCount caps how many decisions one design may contribute.
//
// A design with fifty open questions is not a design with fifty open
// questions — it is a design that did not decide. Capping keeps the chip row
// readable and keeps the prompt's pending-decision list bounded.
const crossDecisionMaxCount = 30

// crossPendingPlaceholders — the "nothing pending" words.
//
// The contract tells the model to write 「（无）」 when there is nothing to
// decide. A model that does that must not produce a chip that says 「无」.
var crossPendingPlaceholders = []string{
	"（无）", "(无)", "无。", "无", "暂无", "没有", "N/A", "NA", "n/a",
	"none", "None", "无待决策事项", "无待决策", "无待确认事项",
}

// isCrossPlaceholder reports whether a line is one of the "nothing pending"
// words rather than an actual decision.
func isCrossPlaceholder(text string) bool {
	t := strings.TrimSpace(strings.Trim(strings.TrimSpace(text), "：:"))
	if t == "" {
		return true
	}
	for _, p := range crossPendingPlaceholders {
		if t == p {
			return true
		}
	}
	return false
}

// ExtractCrossPendingDecisions parses the open questions out of an overall
// design document.
//
// One entry per list item under a 「待决策事项」-style heading. A section
// written as prose (no bullets) still yields entries — one per blank-line
// separated paragraph — because a model that ignored the list convention
// should not cost the user the feature.
func ExtractCrossPendingDecisions(markdown string) []model.CrossPendingDecision {
	out := []model.CrossPendingDecision{}
	trimmed := strings.TrimSpace(markdown)
	if trimmed == "" {
		return out
	}
	lines := strings.Split(trimmed, "\n")
	for i := 0; i < len(lines); i++ {
		level, title, ok := crossHeadingOf(lines[i])
		if !ok || !isCrossPendingHeading(title) {
			continue
		}
		// The body runs until the next heading of the same or higher level.
		body := []string{}
		for j := i + 1; j < len(lines); j++ {
			lvl, _, isHeading := crossHeadingOf(lines[j])
			if isHeading && lvl <= level {
				break
			}
			body = append(body, lines[j])
		}
		out = append(out, parseCrossDecisionItems(body)...)
		if len(out) >= crossDecisionMaxCount {
			return out[:crossDecisionMaxCount]
		}
	}
	return out
}

// extractCrossSectionBody returns the body of the section titled `name`
// (the heading line is excluded).
//
// It is the generic sibling of ExtractCrossContractSection, which stays
// untouched: that one breaks at the next heading of level ≤ 2 regardless of
// the level of its own heading, and a test pins that behaviour.
//
// name is matched **exactly** after stripping the '#'s, trailing spaces and a
// trailing colon. The caller is responsible for choosing a name the model
// actually writes (see crossAdjustConfirmedHeading / crossAdjustOpenHeading).
func extractCrossSectionBody(markdown string, name string) string {
	target := strings.TrimSpace(name)
	if target == "" {
		return ""
	}
	trimmed := strings.TrimSpace(markdown)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	start := -1
	end := len(lines)
	for i, line := range lines {
		level, title, ok := crossHeadingOf(line)
		if !ok {
			continue
		}
		if title == target {
			start = i + 1
			continue
		}
		if start >= 0 && level <= 2 {
			end = i
			break
		}
	}
	if start < 0 || end <= start {
		return ""
	}
	return strings.TrimSpace(strings.Join(lines[start:end], "\n"))
}

// crossAdjustConfirmedHeading — 调整对话回答里「已确认结论」小节的标题。
const crossAdjustConfirmedHeading = "已确认结论"

// crossAdjustOpenHeading — 调整对话回答里「待决策事项」小节的标题。
//
// 刻意与整体方案契约里的章节同名：模型在两处用的是同一个词，抽取与渲染
// 才对得上。
const crossAdjustOpenHeading = "待决策事项"

// ExtractCrossConfirmedConclusions returns the body of the 「已确认结论」
// section inside an adjustment answer.
//
// Empty when the answer has no such section — the caller then falls back to
// the raw transcript, which always carries the full exchange.
func ExtractCrossConfirmedConclusions(answer string) string {
	return extractCrossSectionBody(answer, crossAdjustConfirmedHeading)
}

// ExtractCrossOpenDecisions returns the body of the 「待决策事项」 section
// inside an adjustment answer.
func ExtractCrossOpenDecisions(answer string) string {
	return extractCrossSectionBody(answer, crossAdjustOpenHeading)
}

// isCrossPendingHeading reports whether a section title is one of the
// "pending decision" headings.
func isCrossPendingHeading(title string) bool {
	t := strings.TrimSpace(title)
	if t == "" {
		return false
	}
	for _, kw := range crossPendingHeadingKeywords {
		if strings.Contains(t, kw) {
			return true
		}
	}
	return false
}

// crossHeadingOf parses one line as a Markdown heading.
//
// It returns ok=false for anything that is not a heading, and level 0 for a
// line that starts with '#' but is not a heading (e.g. "#######").
func crossHeadingOf(line string) (level int, title string, ok bool) {
	head := strings.TrimSpace(line)
	if !strings.HasPrefix(head, "#") {
		return 0, "", false
	}
	level = len(head) - len(strings.TrimLeft(head, "#"))
	if level < 1 || level > 6 {
		return 0, "", false
	}
	title = strings.TrimSpace(strings.TrimLeft(head, "#"))
	// A trailing colon ("## 待决策事项：") is punctuation, not part of the name.
	title = strings.TrimSuffix(strings.TrimSuffix(title, "："), ":")
	return level, strings.TrimSpace(title), true
}

// parseCrossDecisionItems turns a section body into decision entries.
//
// Bullets are the primary shape. A body with no bullets at all falls back to
// one entry per paragraph.
func parseCrossDecisionItems(lines []string) []model.CrossPendingDecision {
	out := []model.CrossPendingDecision{}
	var current []string

	flush := func() {
		if len(current) == 0 {
			return
		}
		text := strings.TrimSpace(strings.Join(current, " "))
		current = nil
		if text == "" {
			return
		}
		if d, ok := crossDecisionFromText(text); ok {
			out = append(out, d)
		}
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if isCrossListItem(trimmed) {
			flush()
			current = []string{trimmed}
			continue
		}
		if cell, ok := crossTableCell(trimmed); ok {
			flush()
			current = []string{cell}
			flush()
			continue
		}
		if len(current) > 0 {
			// A continuation line of the item above.
			current = append(current, trimmed)
			continue
		}
		// Prose before any bullet — kept aside as the paragraph fallback.
		if len(out) == 0 && len(current) == 0 {
			current = []string{trimmed}
			flush()
		}
	}
	flush()
	return out
}

// crossTableCell extracts the first cell of a Markdown table row.
//
// A model asked for "one line per decision" sometimes answers with a table
// anyway (「决策点 | 可选项 | 建议」). The first cell is the decision.
func crossTableCell(line string) (string, bool) {
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") || len(line) < 3 {
		return "", false
	}
	cells := strings.Split(strings.Trim(line, "|"), "|")
	if len(cells) == 0 {
		return "", false
	}
	first := strings.TrimSpace(cells[0])
	if first == "" {
		return "", false
	}
	return first, true
}

// isCrossListItem reports whether a line starts a list item ("- ", "* ",
// "+ ", "1. ", "12) ").
func isCrossListItem(s string) bool {
	if strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "* ") || strings.HasPrefix(s, "+ ") {
		return true
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || i+1 >= len(s) {
		return false
	}
	rest := s[i:]
	return strings.HasPrefix(rest, ". ") || strings.HasPrefix(rest, ") ")
}

// stripCrossBullet removes a leading list marker from a line.
//
// Checkbox items ("- [ ] …", "- [x] …") lose the checkbox too — the state is
// not part of the decision.
func stripCrossBullet(s string) string {
	t := strings.TrimSpace(s)
	for _, p := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(t, p) {
			t = strings.TrimSpace(t[len(p):])
			return stripCrossCheckbox(t)
		}
	}
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(t) {
		rest := t[i:]
		if strings.HasPrefix(rest, ". ") || strings.HasPrefix(rest, ") ") {
			return stripCrossCheckbox(strings.TrimSpace(rest[2:]))
		}
	}
	return t
}

// stripCrossCheckbox removes a leading "[ ]" / "[x]" / "[X]" marker.
func stripCrossCheckbox(s string) string {
	t := strings.TrimSpace(s)
	if len(t) >= 3 && t[0] == '[' && t[2] == ']' {
		return strings.TrimSpace(t[3:])
	}
	return t
}

// stripCrossEmphasis removes Markdown emphasis markers from a label.
func stripCrossEmphasis(s string) string {
	out := strings.ReplaceAll(s, "**", "")
	out = strings.ReplaceAll(out, "__", "")
	out = strings.Trim(out, "*_` ")
	return strings.TrimSpace(out)
}

// crossDecisionFromText builds one decision entry out of a single line.
//
// The title is the bolded span when there is one, else the text before the
// first colon; the detail is the whole line (minus the bullet marker), with
// the emphasis markers stripped so it reads as plain text in a tooltip.
//
// A "nothing pending" placeholder produces no entry at all.
func crossDecisionFromText(text string) (model.CrossPendingDecision, bool) {
	clean := stripCrossBullet(text)
	if clean == "" || isCrossPlaceholder(clean) {
		return model.CrossPendingDecision{}, false
	}

	title := ""
	if m := crossBoldRe.FindStringSubmatch(clean); m != nil {
		title = strings.TrimSpace(m[1])
	}
	if title == "" {
		if idx := strings.Index(clean, "："); idx > 0 {
			title = strings.TrimSpace(clean[:idx])
		} else if idx := strings.Index(clean, ":"); idx > 0 {
			title = strings.TrimSpace(clean[:idx])
		}
	}
	if title == "" {
		title = clean
	}
	title = stripCrossEmphasis(title)
	if title == "" {
		title = stripCrossEmphasis(clean)
	}
	if runes := []rune(title); len(runes) > crossDecisionTitleMaxRunes {
		title = string(runes[:crossDecisionTitleMaxRunes]) + "…"
	}

	detail := strings.ReplaceAll(clean, "**", "")
	return model.CrossPendingDecision{Title: title, Detail: detail}, true
}
