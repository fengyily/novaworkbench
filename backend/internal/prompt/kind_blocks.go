// Package prompt hosts the small, role-agnostic text fragments the wizard
// handler appends to its task prompts to tailor them to a requirement's kind
// (issue / requirement / idea). It deliberately does NOT define AI personas —
// those live in the roles table and are injected via --system-prompt by the
// gateway. The blocks here only steer the task shape: an Issue prompt asks
// for "现象 → 复现路径 → 根因" framing, a Requirement prompt asks for the
// legacy four-section layout (and adds nothing here — the role system prompt
// already covers it), an Idea prompt asks for "可行性 + 多个方向 + 风险"
// without producing any concrete code.
//
// Kind values are matched as plain strings rather than constants so this
// package has no import on internal/service (which would form a cycle once the
// service package starts importing prompt for unit tests). The wizard handler
// is the sole caller; it forwards req.Kind verbatim.
package prompt

import (
	"strings"

	"github.com/novaworkbench/backend/internal/model"
)

// AnalystBlock returns the kind-specific tail appended to the analyst-chat
// first-turn prompt (see buildAnalystFirstPrompt in handler/wizard.go).
// "requirement" returns the empty string — the role persona already covers it.
// "issue" and "idea" each prepend a "## 本次任务类型" header so the model can
// see the framing at a glance.
func AnalystBlock(kind string, _ *model.Requirement) string {
	switch kind {
	case "issue":
		return "## 本次任务类型：问题排查（Issue）\n" +
			"请把上面的描述当成一份 Bug 报告：聚焦「现象 → 复现路径 → 根因 → 修复方向」四步。\n" +
			"- 不要追问需求背景或产品决策，只澄清**从代码中无法确定的**：触发条件、报错信息、相关日志。\n" +
			"- 输出请尽量结构化：先列现象/复现步骤，再给可能的根因（按可能性排序），最后列出 1-2 个关键澄清问题。"
	case "idea":
		return "## 本次任务类型：想法探讨（Idea）\n" +
			"把上面内容当成**灵感或探索方向**，不要硬套需求模板。\n" +
			"- 不要追问「验收标准 / 边界条件」，那是确定要做才需要的。\n" +
			"- 重点回应：可行性（依赖现有代码能做到吗）/ 大致思路（2-3 个方向）/ 关键风险点（性能、复杂度、依赖）。\n" +
			"- 鼓励给出多个方案对比，让用户选择方向；如果信息不足以判断方向，直接反问 1-2 个核心问题。\n" +
			"- **不要生成任何代码或具体函数签名**，保持概念层面讨论。"
	default:
		return ""
	}
}

// ArchitectBlock returns the kind-specific tail appended to the
// architect-design prompt. Idea rows never reach this stage (the frontend
// hides the "生成技术方案" CTA when kind=idea), but we still emit a guard
// block for completeness — defense in depth in case a future caller bypasses
// the UI gate.
//
// All three cases share a final "输出图示（Mermaid）" paragraph so the
// architect-stage plan naturally embeds diagrams the knowledge page can
// render directly via MarkdownRender's ```mermaid``` handling. Mirrors
// WikiBlock's existing directive (kind_blocks.go:124). The requirement to
// embed diagrams is intentional: the wizard's "提取架构图为知识条目" UI
// button (frontend/RequirementDetail.tsx) walks the plan for ```mermaid blocks
// after the architect stage so visual designs become reusable knowledge rows
// without an extra LLM round-trip.
func ArchitectBlock(kind string, _ *model.Requirement) string {
	const mermaidDirective = "\n\n## 输出图示（Mermaid）\n" +
		"凡是能用图表达清楚的协作/调用/状态/数据关系，**必须**在对应章节里用 ```mermaid ... ``` 代码块表达，**不要只写文字**：\n" +
		"- 复杂调用链 / 模块协作 → ` ```mermaid ...sequenceDiagram... ``` `（先声明 participant）。\n" +
		"- 状态机 / 生命周期 → ` ```mermaid ...stateDiagram-v2... ``` `（[*] 表示起止）。\n" +
		"- 模块分层 / 组件依赖 / 业务流转 → ` ```mermaid ...flowchart TD/LR... ``` `。\n" +
		"- 数据模型 → ` ```mermaid ...erDiagram... ``` `。\n" +
		"Mermaid 块**只放图本身**（节点 + 连线），文件路径 / 函数签名等具体实现细节放到正文「涉及文件」列表里，不要塞进节点文字。"

	switch kind {
	case "issue":
		return "## 本次任务类型：Issue 修复\n" +
			"- 方案目标是**最小改动定位并修复根因**，不要扩大重构。\n" +
			"- 必须包含：触发条件分析、根因假设（带证据）、涉及文件/函数、最小修复 patch 草案、回滚方案。\n" +
			"- 若无法仅靠预读信息定位根因，把「需要进一步排查的具体路径」列为开放问题，不要硬猜。" +
			mermaidDirective
	case "idea":
		return "## 本次任务类型：Idea 讨论（非实现）\n" +
			"- 目标是**多种可行方向的对比分析**，每个方向给出：核心思路、依赖模块、改动量估算、风险。\n" +
			"- 不要给出完整 plan / 文件路径 / 函数签名——那属于需求确认后的阶段。\n" +
			"- 文末请明确建议「是否值得进一步推进为需求」，并列出还需要的输入信息。" +
			mermaidDirective
	default:
		return mermaidDirective
	}
}

// DeveloperBlock returns the kind-specific tail appended to the developer
// (start-coding / adjust-coding / continue-coding / developer-chat) prompts.
// "idea" intentionally returns "" — the frontend never lets a user reach the
// developer stage for an Idea; the wizard handler also rejects it as a
// defensive guard (see StartCoding).
func DeveloperBlock(kind string, _ *model.Requirement) string {
	switch kind {
	case "issue":
		return "## 本次任务类型：Issue 修复\n" +
			"- 仅实现**方案中明确批准的修复**，不要顺手优化无关代码。\n" +
			"- 若修复涉及错误处理或边界条件，补一个最小回归测试；否则不强求新增测试。\n" +
			"- 在 PR/commit 描述中明确「修复了哪个 issue 的哪个现象」。"
	default:
		return ""
	}
}

// AllBlocks concatenates the kind-specific tails for the analyst + architect
// stages in one shot. Useful when the wizard handler wants to embed both in
// the same prompt (currently unused — kept for completeness / future flows).
func AllBlocks(kind string, req *model.Requirement) string {
	var parts []string
	if a := AnalystBlock(kind, req); a != "" {
		parts = append(parts, a)
	}
	if b := ArchitectBlock(kind, req); b != "" {
		parts = append(parts, b)
	}
	if d := DeveloperBlock(kind, req); d != "" {
		parts = append(parts, d)
	}
	return strings.Join(parts, "\n\n")
}

// WikiBlock returns the kind-specific preface injected into the
// "生成知识库文档" prompt (handler/wizard_wiki.go GenerateWikiDoc). Enforces
// read-only behavior: Claude must NOT modify any source file, NOT create
// docs in the project directory, NOT launch sub-agents, and only output
// Markdown content via its final assistant turn. Diagrams (flow / sequence
// / state / data) MUST be expressed as fenced ```mermaid ... ``` code
// blocks — not described in prose — so the KnowledgeMarkdown renderer can
// draw them as inline SVG. Each section must be filled with concrete
// material (file paths, function signatures, Mermaid blocks, code
// snippets); one-line "label-only" sections like "1. **背景**：MCP 在..."
// are a hard failure mode and the backend rejects them via
// looksLikeWikiOutline.
//
// The wizard handler additionally passes
// DisallowedTools=[Write,Edit,NotebookEdit,Task] to claude so plan-mode +
// tool restrictions backstop the textual instruction: a misbehaving model
// cannot physically touch the project tree or launch sub-agents even if
// it ignores this preface.
func WikiBlock(kind string, _ *model.Requirement) string {
	switch kind {
	case "wiki":
		return "\n\n[知识库类型约束]\n" +
			"1. 你正在为「知识库」类需求生成文档，仅用于沉淀为可复用的知识条目。\n" +
			"2. 严禁修改任何源代码、严禁新建项目内文档、严禁执行破坏性命令。\n" +
			"3. 严禁启动 Explore / Plan 等子代理（Task 工具已在结构层禁用）。自己直接用 Read / Glob / Grep 阅读相关源文件，一次性输出完整文档。\n" +
			"4. 仅通过最终消息输出完整 Markdown 文档正文，不要发\"我已启动\"、\"等待回收中\"之类的进度消息，也不要先\"复述需求\"、\"列出大纲\"再写正文——直接给出最终 Markdown 全文。如果信息不足，直接在文档里写\"信息不足\"章节，不要拖延。\n" +
			"5. " + wikiMermaidDirective + "\n" +
			"6. 每个章节标题下面必须有真正的内容：具体文件路径（如 `backend/internal/foo/bar.go`）、关键函数签名、Mermaid 块或代码片段。**禁止**以一句话标签收尾的空壳章节（例如「1. **背景**：MCP 在 Controller 内的定位」这种纯标签）。整篇文档会被后端的 `looksLikeWikiOutline` 启发式扫描，命中即拒收。\n"
	default:
		return ""
	}
}

// wikiMermaidDirective is the wiki-kind twin of ArchitectBlock's
// mermaidDirective: a hard mandate (rather than a suggestion) to express
// any collaborative / call / state / data relationship as a fenced
// ```mermaid ... ``` block. Extracted so the WikiBlock string stays
// readable and so the wording stays aligned with ArchitectBlock (same
// triggers per diagram kind) — the wiki kind had been suffering from
// "describes what the diagram would contain" instead of producing the
// diagram itself, see req_b37e6f385d371b7e for the canonical failure.
const wikiMermaidDirective = "凡是能用图表达清楚的协作 / 调用 / 状态 / 数据关系，**必须**在对应章节里用 ```mermaid ... ``` 代码块表达，**不要只写文字描述**：\n" +
	"- 复杂调用链 / 模块协作 → ` ```mermaid ...sequenceDiagram... ``` `（先声明 participant）。\n" +
	"- 状态机 / 生命周期 → ` ```mermaid ...stateDiagram-v2... ``` `（[*] 表示起止）。\n" +
	"- 模块分层 / 组件依赖 / 业务流转 → ` ```mermaid ...flowchart TD/LR... ``` `。\n" +
	"- 数据模型 → ` ```mermaid ...erDiagram... ``` `。\n" +
	"Mermaid 块**只放图本身**（节点 + 连线），文件路径 / 函数签名等具体实现细节放到正文「涉及文件」列表里，不要塞进节点文字。\n" +
	"**Mermaid 11.x 兼容约束**（违反会被前端标红 \"Syntax error in text\"）：\n" +
	"- 节点 label 含 `(`, `)`, `{`, `}` 或半角分号 `;` 时，整段 label 用双引号包起来：`[\"foo(n)<br/>bar\"]`。\n" +
	"- 平行四边形 `[/text/]` 必须有开头 `[/` 和结尾 `/]` 两段斜杠。\n" +
	"- sequenceDiagram 消息文本里**不要**用半角 `;` —— mermaid 11 会把它当成新语句开始并报错。\n" +
	"- stateDiagram-v2 转移说明里同样避免半角 `;`。\n" +
	"（即使不满足这些约束，后端 `SanitizeMermaidBlocks` 会做兜底修复，但首轮尽量一次写对可以避免无意义重试。）"
