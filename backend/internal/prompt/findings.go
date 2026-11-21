// Findings: the structured channel that carries what a child requirement
// discovered back to the master requirement.
//
// The problem it solves: a master requirement is split into children, and
// each child discovers things the master's plan did not anticipate. Today
// that knowledge dies with the child's own plan — nothing carries it back,
// so the master plan keeps describing the world as it looked BEFORE the
// split, and the later children keep designing against a stale picture.
//
// The producer is the plan contract itself: the architect is asked to write a
// fixed 「### 与主需求的偏差」 subsection, and the handler parses it out of the
// stored plan at persist time. That keeps the cost at zero extra LLM calls —
// the model was going to write that section anyway.
//
// The consumer is the master plan's refine prompt: accepted findings become
// a mandatory checklist the model has to reconcile into the master plan.

package prompt

import (
	"strings"

	"github.com/novaworkbench/backend/internal/model"
)

// FindingsSourceSection is the plan subsection the architect is asked to fill
// in. Parsed at persist time to seed the master requirement's findings queue.
const FindingsSourceSection = "与主需求的偏差"

// FindingsAbsorbHeading is the section the master plan must carry once
// findings have been absorbed into it.
const FindingsAbsorbHeading = "实施发现与决策变更"

// ExtractSectionBody returns the body under the heading whose text matches
// `heading`, regardless of the heading's level.
//
// The heading is matched after normalization (an ordinal prefix and trailing
// punctuation are ignored), so 「### 2. 与主需求的偏差」 is found the same way
// as 「### 与主需求的偏差」. Fenced code blocks are skipped — a heading quoted
// inside a code block is documentation, not structure.
//
// Returns "" when the heading is absent.
func ExtractSectionBody(md, heading string) string {
	if strings.TrimSpace(md) == "" || strings.TrimSpace(heading) == "" {
		return ""
	}
	lines := strings.Split(md, "\n")
	inFence := false
	start := -1
	level := 0
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		lvl, text, ok := parseHeading(line)
		if !ok {
			continue
		}
		if NormalizeContextHeading(text) == heading {
			start, level = i, lvl
			break
		}
	}
	if start < 0 {
		return ""
	}
	// Collect until the next heading of the same or a higher level. Fenced
	// code blocks are skipped entirely: a bullet inside a ```markdown block
	// is the model documenting the contract, not reporting a finding.
	var body []string
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
		if lvl, _, ok := parseHeading(line); ok && lvl <= level {
			break
		}
		body = append(body, lines[i])
	}
	return strings.TrimSpace(strings.Join(body, "\n"))
}

// ExtractSectionBullets returns the list items under the given heading.
//
// Accepts "- ", "* " and "1. " item markers. A line that is not an item is
// ignored, so a paragraph explaining the section does not become a finding.
func ExtractSectionBullets(md, heading string) []string {
	body := ExtractSectionBody(md, heading)
	if body == "" {
		return nil
	}
	var out []string
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if item, ok := stripBulletMarker(line); ok {
			item = strings.TrimSpace(item)
			if item != "" {
				out = append(out, item)
			}
		}
	}
	return out
}

// stripBulletMarker removes a leading list marker from a line and reports
// whether the line was a list item at all.
func stripBulletMarker(line string) (string, bool) {
	for _, marker := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(line, marker) {
			return strings.TrimPrefix(line, marker), true
		}
	}
	// Ordered item: "1. " / "12) " / "1、".
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(line) {
		rest := line[i:]
		if strings.HasPrefix(rest, ". ") || strings.HasPrefix(rest, ") ") || strings.HasPrefix(rest, "、") {
			return strings.TrimSpace(rest[min(2, len(rest)):]), true
		}
	}
	return line, false
}

// parseHeading splits a markdown heading line into its level and text.
// Returns ok=false for anything that is not a heading.
func parseHeading(line string) (int, string, bool) {
	if !strings.HasPrefix(line, "#") {
		return 0, "", false
	}
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	rest := line[level:]
	if rest != "" && !strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "\t") {
		// "#hashtag" is not a heading.
		return 0, "", false
	}
	return level, strings.TrimSpace(rest), true
}

// BuildFindingsAbsorbBlock renders the accepted-but-unabsorbed findings as a
// checklist the model must reconcile into the master plan.
//
// The block is only rendered when there is something outstanding, so the
// common case (no findings) leaves the refine prompt byte-identical to
// before.
//
// The contract it carries is deliberately strict: the model has to address
// EVERY item — either by changing the affected section of the plan, or by
// recording an explicit rejection with a reason. Silently ignoring an item
// would defeat the point of collecting it.
func BuildFindingsAbsorbBlock(findings []model.RequirementFinding) string {
	if len(findings) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## 必须吸收的变更（来自子需求的实施发现，已确认）\n\n")
	b.WriteString("以下条目由子需求在设计 / 开发期间发现，并已被确认。主需求方案必须**逐条处理**：\n\n")
	for _, f := range findings {
		b.WriteString("- 来源子需求 `" + f.RequirementID + "`")
		if f.Stage != "" {
			b.WriteString("（" + f.Stage + " 阶段）")
		}
		b.WriteString("：" + strings.TrimSpace(f.Title))
		if d := strings.TrimSpace(f.Detail); d != "" {
			b.WriteString(" —— " + d)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n处理方式（二选一，逐条给出）：\n")
	b.WriteString("1. 改写主需求方案中受该发现影响的章节；\n")
	b.WriteString("2. 在主需求方案新增/更新「" + FindingsAbsorbHeading + "」一节，记录「**暂不采纳** + 理由」。\n")
	b.WriteString("\n该章节固定按下面的格式逐条记录：\n\n")
	b.WriteString("## " + FindingsAbsorbHeading + "\n\n")
	b.WriteString("- 来源子需求 / 发现 / 对主需求方案的影响 / 处理决定（已采纳 / 暂不采纳 + 理由）\n")
	return b.String()
}
