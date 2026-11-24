package prompt

import (
	"strings"
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
