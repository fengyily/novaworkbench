// Package prompt — review.go carries the code-review task contract: the
// three-dimension checklist (代码问题 / 修改目标是否达成 / 安全性), the
// P0/P1/P3 severity ladder, and the machine-readable output contract the
// handler parses back into code_reviews.findings_json.
//
// This lives in the TASK prompt (`-p`), not in the configurable "reviewer"
// role persona, on purpose: RoleService.SeedDefaults() backfills per key, so
// an existing install's reviewer row is never overwritten — and users are
// free to rewrite their persona. Putting the contract here means every run
// gets it regardless of what the persona says. role_defaults.go's reviewer
// text is kept in the same voice for fresh installs, but nothing depends
// on it.
package prompt

import (
	"fmt"
	"strings"
)

// Severity ladder. P2 is deliberately absent — the product spec names three
// buckets, and the prompt tells the model so explicitly, otherwise it
// invents a P2 to fill the gap. Every literal lives here so adding P2 later
// is a one-file change (plus the frontend's grouping order).
const (
	SevP0 = "P0" // 必须修复才能合入
	SevP1 = "P1" // 建议修复
	SevP3 = "P3" // 优化项建议
)

// Goal-attainment verdicts for the "修改目标是否达成" dimension. Empty means
// the model didn't answer (or there was no user description to check).
const (
	GoalMetYes     = "yes"
	GoalMetPartial = "partial"
	GoalMetNo      = "no"
)

// ReviewPromptInput is everything the review task prompt needs. HeadRef /
// BaseRef are the already-resolved git refs (handler.resolveDiffRefs picks
// origin/<x> when it exists and falls back to the local branch), so the
// model never has to guess whether a branch was pushed.
type ReviewPromptInput struct {
	ReviewType        string // "branch" | "pr"
	Branch            string
	BaseBranch        string
	HeadRef           string // resolved, e.g. "origin/feat/x" or "feat/x"
	BaseRef           string // resolved, e.g. "origin/main" or "main"
	PRNumber          int
	PRTitle           string
	Description       string // user-written 改动目标 / Review 上下文
	ExtraRequirements string // legacy single-line field from the PR entry point
	HasAttachments    bool
}

// BuildReviewPrompt assembles the review task prompt (Chinese, per the
// repo convention that UI text and AI prompts are Chinese).
func BuildReviewPrompt(in ReviewPromptInput) string {
	var b strings.Builder

	if in.ReviewType == "pr" {
		b.WriteString("请对以下 PR 的改动进行代码 Review。\n\n")
	} else {
		b.WriteString("请对以下分支的改动进行代码 Review。\n\n")
	}

	b.WriteString("## 本次 Review 的上下文\n\n")
	if in.PRTitle != "" {
		fmt.Fprintf(&b, "- PR 标题：%s\n", in.PRTitle)
	}
	if in.PRNumber > 0 {
		fmt.Fprintf(&b, "- PR 编号：#%d\n", in.PRNumber)
	}
	fmt.Fprintf(&b, "- 目标分支：`%s`\n", in.Branch)
	if in.BaseBranch != "" {
		fmt.Fprintf(&b, "- 基础分支：`%s`\n", in.BaseBranch)
	}
	fmt.Fprintf(&b, "- 对比区间：`git diff %s...%s`\n", in.BaseRef, in.HeadRef)
	b.WriteString("\n")

	if strings.TrimSpace(in.Description) != "" {
		b.WriteString("### 用户描述的改动目标（判断「目标是否达成」的唯一依据）\n\n")
		b.WriteString(strings.TrimSpace(in.Description))
		b.WriteString("\n\n")
	} else {
		b.WriteString("### 用户描述的改动目标\n\n（用户未填写描述。请从 diff 与提交信息中自行推断本次改动意图，并在「目标达成情况」中说明你的推断依据。）\n\n")
	}
	if strings.TrimSpace(in.ExtraRequirements) != "" {
		fmt.Fprintf(&b, "### 额外审查要求\n\n%s\n\n", strings.TrimSpace(in.ExtraRequirements))
	}
	if in.HasAttachments {
		b.WriteString("### 附件\n\n用户上传了附件（清单见系统提示中的「📎 已附加文件」块）。请用 Read 工具按需读取与本次改动相关的附件，它们是判断「目标是否达成」的重要依据。\n\n")
	}

	fmt.Fprintf(&b, `## 操作步骤

1. 运行 `+"`git diff %s...%s`"+` 查看本次全部改动（该区间已由后端解析好，直接使用，不要自行改写成别的 ref）。
2. 运行 `+"`git log --oneline %s..%s`"+` 了解提交脉络。
3. 用 Read 工具阅读改动涉及的关键源文件，必要时查看其调用方 / 被调用方，避免只看 diff 片段下结论。
4. 若有附件，按需 Read 相关附件。
5. 按下面的检查清单与输出契约产出报告。

`, in.BaseRef, in.HeadRef, in.BaseRef, in.HeadRef)

	b.WriteString(`## 检查清单（三个维度，缺一不可）

### 1. 代码问题
- 逻辑错误、边界条件、空值 / 越界 / 并发安全
- 错误处理是否完整，失败路径是否被吞掉
- 可读性、命名、重复代码、与项目既有约定是否一致
- 性能：多余查询、N+1、无界循环、内存泄漏
- 测试覆盖：关键路径是否有测试

### 2. 修改目标是否达成
- 逐条对照上面「用户描述的改动目标」（以及附件内容）核对，指出哪些目标已达成、哪些未达成或只做了一半
- 注意「做多了」也是问题：超出目标范围的改动要单独点出
- 若目标本身在 diff 中找不到任何对应实现，必须报 P0

### 3. 安全性问题
- 注入（SQL / 命令 / 路径穿越）、越权、认证绕过
- 敏感信息泄露（凭据、Token、日志打印）
- 不安全的默认值、缺失的输入校验、依赖风险

## 问题分级（只用这三档）

- **P0 — 必须修复才能合入**：会导致功能不可用、数据损坏、安全漏洞，或目标明确未达成。
- **P1 — 建议修复**：存在实际缺陷或明显隐患，但不阻塞合入。
- **P3 — 优化项建议**：可读性 / 结构 / 性能上的改进建议，不修也能合。

注意：本项目**不使用 P2**。请勿输出 P2 或其他等级，每个问题必须落在 P0 / P1 / P3 之一。

## 输出契约

先输出 Markdown 报告，小节固定如下（没有内容的小节写「无」，不要删掉小节）：

## 总体评价
（一到三句话，包含「是否建议合入」的明确结论）

## 目标达成情况
（逐条对照用户描述的目标说明达成情况）

## P0 — 必须修复
## P1 — 建议修复
## P3 — 优化项建议
（每项格式：**标题** — ` + "`文件路径:行号`" + `，问题描述，然后给出具体修改建议）

## 优点
（值得保留的好设计）

**报告正文结束后，必须在最末尾追加一个 json 代码围栏块**，供系统解析分级统计（这是硬性要求，不可省略、不可放在正文中间）：

` + "```json" + `
{"goal_met":"yes|partial|no","findings":[{"severity":"P0","file":"path/to/file.go","line":"12-20","title":"一句话标题","detail":"问题说明","suggestion":"修改建议"}]}
` + "```" + `

JSON 块的约束：
- ` + "`goal_met`" + ` 取值只能是 ` + "`yes`" + ` / ` + "`partial`" + ` / ` + "`no`" + `，表示用户描述的改动目标的达成程度。
- ` + "`findings`" + ` 必须包含正文里列出的**全部**问题，顺序与正文一致；没有任何问题时写 ` + "`\"findings\":[]`" + `。
- ` + "`severity`" + ` 只能是 ` + "`P0`" + ` / ` + "`P1`" + ` / ` + "`P3`" + `。
- ` + "`line`" + ` 是字符串（可写 ` + "`\"42\"`" + ` 或 ` + "`\"12-20\"`" + `，不确定时写 ` + "`\"\"`" + `）。
- JSON 必须是单个合法对象，且是整个回复的最后一段内容。
`)

	return b.String()
}
