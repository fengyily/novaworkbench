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

// reviewMermaidDirective is the diagram half of the review output contract:
// a review report should show the shape of a change (call chain, flow,
// module layout, state transitions), not just describe it in prose — the
// same bar the architect-stage design doc is held to (see
// kind_blocks.go's mermaidDirective / wikiMermaidDirective, whose wording
// and Mermaid-11 compat rules this deliberately mirrors so a reader moving
// between 方案设计 and Review sees the same kind of picture).
//
// Two things are tuned differently from the design-doc twin:
//
//  1. Scope is the diff, not the system. A review that redraws the whole
//     architecture buries the three nodes that actually changed, so the
//     directive caps the count, asks for 【新增】/【改】 markers, and gives
//     an explicit out ("写「无」") for one-line changes — without that out
//     the model dutifully draws a two-node flowchart for a typo fix.
//  2. Ordering is load-bearing. handler.extractTrailingJSON finds the LAST
//     ```json fence and, failing that, brace-matches backwards from the end
//     of the text; a Mermaid block emitted AFTER the contract block would
//     leave prose trailing the JSON and defeat both paths. Hence the final
//     bullet pinning diagrams to the body.
//
// Blocks that still come back malformed are repaired by
// service.SanitizeMermaidBlocks before the report is persisted.
const reviewMermaidDirective = "## 输出图示（Mermaid）\n\n" +
	"凡是能用图说清楚的改动，都要在「## 改动概览图」里用 ```mermaid ... ``` 代码块画出来，**不要只写文字描述**。" +
	"按改动的实际形态挑图，不要为了凑数把每种图都画一遍：\n" +
	"- 跨模块 / 跨层的调用链、请求在前后端之间的往返 → ` ```mermaid ...sequenceDiagram... ``` `（先声明 participant）。\n" +
	"- 新增或改动的处理流程、分支判断、数据流转 → ` ```mermaid ...flowchart TD/LR... ``` `。\n" +
	"- 新增模块 / 组件依赖、分层结构的变化 → ` ```mermaid ...flowchart LR... ``` ` 画架构图（用 subgraph 分层）。\n" +
	"- 状态机 / 生命周期 / 状态字段取值流转 → ` ```mermaid ...stateDiagram-v2... ``` `（[*] 表示起止）。\n" +
	"- 表结构 / 实体关系的变化 → ` ```mermaid ...erDiagram... ``` `。\n\n" +
	"画图的尺度：\n" +
	"- 一般 1-2 张，最多 3 张；每张图前用一句话说明这张图在讲什么。\n" +
	"- **只画本次 diff 涉及的部分**，不要把整个系统重画一遍；可以保留少量未改动的节点作为上下文。\n" +
	"- 新增 / 改动的节点用 `【新增】` / `【改】` 前缀标出，让人一眼看出增量在哪。\n" +
	"- 改动确实只是单文件小修小补（改文案、改常量、补注释）时，「## 改动概览图」直接写「无」，不要硬凑。\n\n" +
	"**Mermaid 11.x 兼容约束**（违反会被前端标红 \"Syntax error in text\"）：\n" +
	"- 节点 label 含 `(`、`)`、`{`、`}` 或半角 `;` 时，整段 label 用双引号包起来：`[\"foo(n)<br/>bar\"]`。\n" +
	"- 平行四边形 `[/text/]` 必须 `[/` 开头、`/]` 结尾，两段斜杠都不能少。\n" +
	"- sequenceDiagram 的消息文本、stateDiagram-v2 的转移说明里都不要用半角 `;` —— mermaid 11 会把它当成新语句开始并报错。\n" +
	"- Mermaid 块里**只放图本身**（节点 + 连线），文件路径 / 函数签名等细节写到正文里，不要塞进节点文字。\n" +
	"- 所有 Mermaid 块都必须出现在报告正文中，位置在文末 json 块**之前**。\n"

// reviewAttachmentDirective tells the reviewer how to USE the uploaded files,
// not merely that they exist.
//
// Attachments are the product's answer to "did this branch actually achieve
// what it was supposed to": a run log from the branch, the design doc it was
// built against, a zip of outputs. The earlier one-liner ("按需读取") left the
// model free to skim them, and in practice a 2 MB log got a glance and a
// hand-wave. Naming the three common shapes and the first move for each
// (grep the log for failures; diff the doc's requirements against the code;
// list the zip before extracting) is what turns an attachment into evidence
// that can be quoted in goal_checks.
const reviewAttachmentDirective = "### 附件（功能验证材料）\n\n" +
	"用户上传了附件（清单见系统提示中的「📎 已附加文件」块），它们是判断「目标是否达成」的**一手证据**，必须用 Read / Grep / Bash 真正看过，不能只凭 diff 下结论。按类型处理：\n" +
	"- **运行日志 / 输出文本**（`.log` / `.txt` / 控制台粘贴）：先 `grep -nEi 'error|panic|fail|exception|traceback'` 找异常，再回到改动目标，确认日志里是否出现了本次改动应当产生的行为（新接口被调用、新分支被走到、新字段被打印）。\n" +
	"- **方案 / 需求文档**（`.md` / `.pdf` / 截图）：把文档里的验收点逐条拆出来，对照 diff 检查每一条是否落地，缺失或走样的直接作为「目标未达成」的依据。\n" +
	"- **zip / 压缩包**：先看清单（系统提示里已列出包内文件），再按需 `unzip -p` 读取其中与改动相关的文件，不要整包解压。\n" +
	"- 附件与代码矛盾时，**以代码为准**，并把矛盾本身作为一条问题点出来（文档过期、日志来自旧版本都是常见情况）。\n\n" +
	"在「## 目标达成情况」里引用附件时，要给出可核对的出处：日志写到行号或原文片段，文档写到小节标题，zip 写到包内文件名。\n\n"

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
		b.WriteString(reviewAttachmentDirective)
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

## 改动概览图
（用 Mermaid 画出本次改动，规则见下面的「输出图示（Mermaid）」。改动确实简单到不值得画图时，只写「无」）

## 目标达成情况
（把用户描述的目标拆成逐条验收点，每条给出「达成 / 部分达成 / 未达成 / 无法判断」+ 判断依据；
依据要能核对：` + "`文件:行号`" + ` 、附件日志里的原文片段、附件文档的小节标题。这一小节与下面 json 块里的
` + "`goal_checks`" + ` 必须一一对应）

## P0 — 必须修复
## P1 — 建议修复
## P3 — 优化项建议
（每项格式：**标题** — ` + "`文件路径:行号`" + `，问题描述，然后给出具体修改建议；
问题本身涉及多步调用 / 并发时序 / 状态流转时，在该条目下补一个 Mermaid 图说明出错路径）

## 优点
（值得保留的好设计）

`)

	b.WriteString(reviewMermaidDirective)

	b.WriteString(reviewJSONContract)

	return b.String()
}

// reviewJSONContract is the machine-readable tail of the report, shared by
// the first review and by a regeneration so the two can never drift (a
// regeneration whose JSON shape differed would silently zero the counts of
// the report it replaced).
//
// handler.extractTrailingJSON parses it; handler.parseFindings decides what
// survives. goal_checks is the one OPTIONAL member: a model that answers only
// `goal_met` still produces a fully usable report, which is why the parser
// treats a missing or malformed goal_checks as "no evidence table" rather
// than as a contract violation.
const reviewJSONContract = `
**报告正文结束后，必须在最末尾追加一个 json 代码围栏块**，供系统解析分级统计（这是硬性要求，不可省略、不可放在正文中间）：

` + "```json" + `
{"goal_met":"yes|partial|no","goal_checks":[{"criterion":"目标中的一条验收点","status":"yes|partial|no|unknown","evidence":"判断依据：diff 位置 / 日志行 / 文档小节"}],"findings":[{"severity":"P0","file":"path/to/file.go","line":"12-20","title":"一句话标题","detail":"问题说明","suggestion":"修改建议"}]}
` + "```" + `

JSON 块的约束：
- ` + "`goal_met`" + ` 取值只能是 ` + "`yes`" + ` / ` + "`partial`" + ` / ` + "`no`" + `，表示用户描述的改动目标的达成程度。
- ` + "`goal_checks`" + ` 把「目标达成情况」拆成可逐条核对的清单，与正文该小节一一对应：
  - ` + "`criterion`" + ` 是从用户描述的改动目标里拆出的**一条**验收点（用户没写描述时，填你从 diff 推断出的意图）。
  - ` + "`status`" + ` 只能是 ` + "`yes`" + ` / ` + "`partial`" + ` / ` + "`no`" + ` / ` + "`unknown`" + `；` + "`unknown`" + ` 表示现有材料不足以判断（例如需要运行日志但用户未提供），不要用它来回避判断。
  - ` + "`evidence`" + ` 必须是可核对的出处：` + "`文件:行号`" + ` 、日志中的原文片段、附件文档的小节标题之一，不要写「已确认」这类空话。
  - 实在无法拆出验收点时写 ` + "`\"goal_checks\":[]`" + `，不要省略这个字段。
- ` + "`findings`" + ` 必须包含正文里列出的**全部**问题，顺序与正文一致；没有任何问题时写 ` + "`\"findings\":[]`" + `。
- ` + "`severity`" + ` 只能是 ` + "`P0`" + ` / ` + "`P1`" + ` / ` + "`P3`" + `。
- ` + "`line`" + ` 是字符串（可写 ` + "`\"42\"`" + ` 或 ` + "`\"12-20\"`" + `，不确定时写 ` + "`\"\"`" + `）。
- JSON 必须是单个合法对象，且是整个回复的最后一段内容。
`

// ── 追问 (follow-up questions on a finished review) ──────────────────────────

// FollowupPromptInput is everything a follow-up turn needs. Two shapes come
// out of it, selected by Resumed:
//
//	Resumed = true  — the claude session that produced the report is still
//	                  available and the run uses `--resume`, so the model
//	                  already holds the diff, the files it read and its own
//	                  report. The prompt is then just the question plus the
//	                  answering rules; re-pasting the report would only burn
//	                  context.
//	Resumed = false — a fresh session (old review, session expired, or a
//	                  remote run whose host no longer has the jsonl). The
//	                  prompt has to carry the review context itself: branch,
//	                  diff range, the user's 改动目标, the report, and the
//	                  earlier Q&A of this same thread.
//
// Either way a follow-up is read-only: it explains, it does not re-grade, and
// it must NOT emit the trailing JSON contract (that block is parsed into
// findings_json and a second one would corrupt the stored report).
type FollowupPromptInput struct {
	Resumed     bool
	Branch      string
	BaseBranch  string
	HeadRef     string
	BaseRef     string
	PRNumber    int
	PRTitle     string
	Description string // the user's 改动目标, as given to the original review
	ReportMD    string // the stored result_md (already trimmed by the caller)
	History     []FollowupTurn
	Question    string
}

// FollowupTurn is one earlier question/answer pair of the same thread,
// replayed into a fresh session so the follow-ups read as a conversation even
// when the original claude session is gone.
type FollowupTurn struct {
	Question string
	Answer   string
}

// BuildFollowupPrompt assembles the follow-up task prompt (Chinese, per the
// repo convention that UI text and AI prompts are Chinese).
func BuildFollowupPrompt(in FollowupPromptInput) string {
	var b strings.Builder

	if in.Resumed {
		b.WriteString("我针对你刚才输出的 Review 报告有一个追问，请基于本次会话已有的 diff、源码与报告内容回答。\n\n")
	} else {
		b.WriteString("你是代码 Review 的作者。用户针对一份已完成的 Review 报告提出追问，请结合下面的报告原文与仓库代码回答。\n\n")
		b.WriteString("## Review 上下文\n\n")
		if in.PRTitle != "" {
			fmt.Fprintf(&b, "- PR 标题：%s\n", in.PRTitle)
		}
		if in.PRNumber > 0 {
			fmt.Fprintf(&b, "- PR 编号：#%d\n", in.PRNumber)
		}
		if in.Branch != "" {
			fmt.Fprintf(&b, "- 目标分支：`%s`\n", in.Branch)
		}
		if in.BaseBranch != "" {
			fmt.Fprintf(&b, "- 基础分支：`%s`\n", in.BaseBranch)
		}
		if in.BaseRef != "" && in.HeadRef != "" {
			fmt.Fprintf(&b, "- 对比区间：`git diff %s...%s`（需要时可自行运行查看改动）\n", in.BaseRef, in.HeadRef)
		}
		b.WriteString("\n")
		if strings.TrimSpace(in.Description) != "" {
			fmt.Fprintf(&b, "### 用户描述的改动目标\n\n%s\n\n", strings.TrimSpace(in.Description))
		}
		if strings.TrimSpace(in.ReportMD) != "" {
			fmt.Fprintf(&b, "### 已输出的 Review 报告原文\n\n%s\n\n", strings.TrimSpace(in.ReportMD))
		}
	}

	if len(in.History) > 0 {
		b.WriteString("## 本轮追问之前的问答\n\n")
		for i, turn := range in.History {
			fmt.Fprintf(&b, "**追问 %d**：%s\n\n", i+1, strings.TrimSpace(turn.Question))
			if strings.TrimSpace(turn.Answer) != "" {
				fmt.Fprintf(&b, "**回答 %d**：%s\n\n", i+1, strings.TrimSpace(turn.Answer))
			}
		}
	}

	fmt.Fprintf(&b, "## 本次追问\n\n%s\n\n", strings.TrimSpace(in.Question))

	b.WriteString(`## 回答要求

1. 直接回答问题本身，不要重复输出整份 Review 报告，也不要重新给所有问题分级。
2. 需要引用代码时，用 ` + "`文件路径:行号`" + ` 指明位置；不确定的地方要先用 Read / Grep 工具确认，不要凭印象作答。
3. 如果追问涉及某条具体问题，请说明：它为什么是问题、会在什么场景下出错、应该怎么改（必要时给出修改后的代码片段）。
4. 如果经核对发现原报告中的结论有误或不准确，直接说明并更正，不要为了自洽而辩护。
5. 用 Markdown 作答，控制在必要篇幅内。
6. 问题涉及调用链 / 并发时序 / 状态流转 / 模块依赖时，用 ` + "```mermaid" + ` 代码块作图说明（` + "`sequenceDiagram`" + ` / ` + "`flowchart TD`" + ` / ` + "`stateDiagram-v2`" + `），一张讲清楚即可，不要只写文字。
   Mermaid 11.x 兼容：节点 label 含 ` + "`(`" + `、` + "`)`" + `、` + "`{`" + `、` + "`}`" + ` 或半角 ` + "`;`" + ` 时整段用双引号包起来；sequenceDiagram 的消息文本里不要用半角 ` + "`;`" + `。
7. **不要输出 json 代码围栏块**（分级统计只由 Review 报告本身产出，追问不参与）。
`)

	return b.String()
}

// ── 基于追问对话重新生成报告 ────────────────────────────────────────────────

// RegeneratePromptInput is everything a regeneration needs.
//
// A regeneration is NOT a re-review: the diff has not moved, and re-reading
// it from scratch would throw away exactly the thing that makes the second
// report better — the 追问 thread, where the user corrected the reviewer,
// pushed back on false positives and asked for the parts that were unclear.
// So the prompt is "here is your report, here is what we then discussed,
// now write the report you would have written knowing all of it".
//
// Resumed selects the same two shapes as FollowupPromptInput: with the
// original claude session alive the model still holds the diff and the files
// it read, and re-pasting the report would only burn context; without it the
// prompt has to carry the review context itself.
type RegeneratePromptInput struct {
	Resumed     bool
	ReviewType  string // "branch" | "pr"
	Branch      string
	BaseBranch  string
	HeadRef     string
	BaseRef     string
	PRNumber    int
	PRTitle     string
	Description string // the user's 改动目标 (branch reviews)
	// ExtraRequirements is the PR entry point's single-line 额外审查要求.
	// One stored column, two meanings — see handler.buildReviewTaskPrompt;
	// labelling a PR's extra requirements as the 改动目标 would make the
	// reviewer grade 目标达成 against the wrong text.
	ExtraRequirements string
	ReportMD          string // the current result_md (already trimmed by the caller)
	History           []FollowupTurn
	// HasAttachments keeps the attachment directive in play: a regeneration
	// that silently stopped consulting the uploaded log would quietly
	// downgrade every goal_check that cited it.
	HasAttachments bool
}

// BuildRegeneratePrompt assembles the task prompt for 「基于追问对话重新生成」.
//
// It reuses the first review's contract wholesale — the same three-dimension
// checklist is implied by the report being revised, and the same
// reviewMermaidDirective + reviewJSONContract are appended verbatim. That
// reuse is load-bearing in two directions:
//
//  1. The output must parse through the same handler.extractTrailingJSON →
//     parseFindings path, including the rule that Mermaid blocks come BEFORE
//     the trailing json block.
//  2. The counts the new report produces replace the old ones, so a contract
//     that drifted would show up to the user as findings vanishing rather
//     than as a parse failure.
func BuildRegeneratePrompt(in RegeneratePromptInput) string {
	var b strings.Builder

	b.WriteString("请基于下面的追问对话，重新输出一份完整的代码 Review 报告。\n\n")

	if in.Resumed {
		b.WriteString("这是你此前对同一份改动产出的 Review，本次会话里已有完整的 diff、源码与报告上下文，直接在此基础上修订即可。\n\n")
	} else {
		b.WriteString("## Review 上下文\n\n")
		if in.PRTitle != "" {
			fmt.Fprintf(&b, "- PR 标题：%s\n", in.PRTitle)
		}
		if in.PRNumber > 0 {
			fmt.Fprintf(&b, "- PR 编号：#%d\n", in.PRNumber)
		}
		if in.Branch != "" {
			fmt.Fprintf(&b, "- 目标分支：`%s`\n", in.Branch)
		}
		if in.BaseBranch != "" {
			fmt.Fprintf(&b, "- 基础分支：`%s`\n", in.BaseBranch)
		}
		if in.BaseRef != "" && in.HeadRef != "" {
			fmt.Fprintf(&b, "- 对比区间：`git diff %s...%s`（需要核对时自行运行）\n", in.BaseRef, in.HeadRef)
		}
		b.WriteString("\n")
		if strings.TrimSpace(in.Description) != "" {
			fmt.Fprintf(&b, "### 用户描述的改动目标（判断「目标是否达成」的唯一依据）\n\n%s\n\n", strings.TrimSpace(in.Description))
		}
		if strings.TrimSpace(in.ExtraRequirements) != "" {
			fmt.Fprintf(&b, "### 额外审查要求\n\n%s\n\n", strings.TrimSpace(in.ExtraRequirements))
		}
		if strings.TrimSpace(in.ReportMD) != "" {
			fmt.Fprintf(&b, "### 上一版 Review 报告原文\n\n%s\n\n", strings.TrimSpace(in.ReportMD))
		}
		if in.HasAttachments {
			b.WriteString(reviewAttachmentDirective)
		}
	}

	if len(in.History) > 0 {
		b.WriteString("## 报告发出后的追问对话\n\n")
		for i, turn := range in.History {
			fmt.Fprintf(&b, "**追问 %d**：%s\n\n", i+1, strings.TrimSpace(turn.Question))
			if strings.TrimSpace(turn.Answer) != "" {
				fmt.Fprintf(&b, "**回答 %d**：%s\n\n", i+1, strings.TrimSpace(turn.Answer))
			}
		}
	}

	b.WriteString(`## 重新生成的要求

1. **把追问中确认的结论写进报告**：追问里补充的细节、修正的判断、给出的具体改法，都要落到对应条目里，而不是另起一段「补充说明」。
2. **纠正被推翻的结论**：追问中经核对发现原报告判断有误的，直接改正；被用户以事实否定的问题（例如那段代码本就不会被执行、该校验在上游已经做过）要降级或删除，不要为了自洽保留。
3. **不要凭追问臆造新问题**：新增条目必须同样有代码依据，必要时重新 Read / Grep 核对，不确定的宁可不写。
4. **输出的是一份完整报告，不是增量**：小节结构、分级口径、Mermaid 图、文末 json 块与首次 Review 完全一致，读者只看这一份就够，不需要再回头看上一版。
5. 分级仍然只用 P0 / P1 / P3 三档，**不使用 P2**。
6. ` + "`goal_met`" + ` / ` + "`goal_checks`" + ` 要反映追问后的最新判断——追问里被澄清的验收点，status 与 evidence 都要更新。

## 输出契约

先输出 Markdown 报告，小节固定如下（没有内容的小节写「无」，不要删掉小节）：

## 总体评价
（一到三句话，包含「是否建议合入」的明确结论）

## 改动概览图
（用 Mermaid 画出本次改动，规则见下面的「输出图示（Mermaid）」。改动确实简单到不值得画图时，只写「无」）

## 目标达成情况
（逐条验收点 + 达成情况 + 可核对的依据，与 json 块的 ` + "`goal_checks`" + ` 一一对应）

## P0 — 必须修复
## P1 — 建议修复
## P3 — 优化项建议
（每项格式：**标题** — ` + "`文件路径:行号`" + `，问题描述，然后给出具体修改建议）

## 优点
（值得保留的好设计）

`)

	b.WriteString(reviewMermaidDirective)
	b.WriteString(reviewJSONContract)

	return b.String()
}
