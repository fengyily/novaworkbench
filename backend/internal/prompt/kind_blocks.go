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
func ArchitectBlock(kind string, _ *model.Requirement) string {
	switch kind {
	case "issue":
		return "## 本次任务类型：Issue 修复\n" +
			"- 方案目标是**最小改动定位并修复根因**，不要扩大重构。\n" +
			"- 必须包含：触发条件分析、根因假设（带证据）、涉及文件/函数、最小修复 patch 草案、回滚方案。\n" +
			"- 若无法仅靠预读信息定位根因，把「需要进一步排查的具体路径」列为开放问题，不要硬猜。"
	case "idea":
		return "## 本次任务类型：Idea 讨论（非实现）\n" +
			"- 目标是**多种可行方向的对比分析**，每个方向给出：核心思路、依赖模块、改动量估算、风险。\n" +
			"- 不要给出完整 plan / 文件路径 / 函数签名——那属于需求确认后的阶段。\n" +
			"- 文末请明确建议「是否值得进一步推进为需求」，并列出还需要的输入信息。"
	default:
		return ""
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
