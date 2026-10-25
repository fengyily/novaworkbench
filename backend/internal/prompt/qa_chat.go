package prompt

import (
	"fmt"
	"strings"
)

// ── 阶段答疑对话 (Stage Q&A Chat) ──────────────────────────────────────────
//
// 答疑是对「方案设计会话」/「开发会话」的**只读追问**:用户在需求详情页问
// 「这里为什么这么设计」「这段实现是什么意思」。答疑跑在从原会话 fork 出来的
// 独立会话里,任何一轮都不写回原会话,也不碰文件与方案文档。
//
// 两种形态,和 Review 追问 (BuildFollowupPrompt) 同构:
//
//	Resumed = true  —— QA 会话健在,模型手里还有 fork 继承来的完整上下文
//	                  (原方案 / 原代码 + 之前的问答)。此时 prompt 只要问题
//	                  本身,重贴方案文档纯属浪费上下文。
//	Resumed = false —— 源会话已失效(用户重跑了阶段、CLI 清了 jsonl、远端
//	                  副本没同步下来),降级为全新会话。此时 prompt 必须自己
//	                  扛起上下文:标题 + 描述 + 方案文档 + 本线程此前的问答。
//
// 纯函数:handler 负责所有 I/O(读 requirement / 读历史轮次),所以整个
// prompt 可以在没有 DB、没有 claude 的情况下被单测钉住。

// StageQAPromptInput is everything one Q&A turn needs.
type StageQAPromptInput struct {
	// Resumed selects the short form (the QA session still holds the forked
	// context) or the self-contained one (context pasted into the prompt).
	Resumed bool
	// Stage is "design" or "coding" — which production session is being asked
	// about. It selects the wording and which context fields are meaningful.
	Stage string
	// Question is the user's message. Always present in both forms.
	Question string
	// Title / Description / DesignDocs / CodingSummary are the fresh-form
	// context. DesignDocs carries the plan for the design stage;
	// CodingSummary carries the compressed coding context for the coding
	// stage (there is no "plan document" to paste for a coding run — the
	// design doc IS the plan, and the coding session's own transcript is what
	// got lost).
	Title         string
	Description   string
	DesignDocs    string
	CodingSummary string
	// History is the thread's earlier answered turns, oldest first. Replayed
	// so a degraded (fresh) session still reads as a conversation.
	History []QATurn
}

// QATurn is one earlier question/answer pair of the same thread.
type QATurn struct {
	Question string
	Answer   string
}

// Truncation caps for the fresh form. Rune-based, not byte-based: a byte cut
// would split a multi-byte Chinese character and emit mojibake into the
// prompt. The design doc is the biggest block (it IS the context the user is
// asking about), the description is secondary, and each replayed answer is
// capped so a long thread can't crowd out the question.
const (
	qaDescMaxRunes    = 4 * 1024
	qaDesignMaxRunes  = 24 * 1024
	qaAnswerMaxRunes  = 4000
	qaSummaryMaxRunes = 8 * 1024
)

// BuildStageQAPrompt assembles the Q&A task prompt (Chinese, per the repo
// convention that UI text and AI prompts are Chinese).
func BuildStageQAPrompt(in StageQAPromptInput) string {
	var b strings.Builder

	if in.Resumed {
		b.WriteString(qaResumedLead(in.Stage))
		b.WriteString("\n\n")
	} else {
		b.WriteString(qaFreshLead(in.Stage))
		b.WriteString("\n\n")
		b.WriteString("## 需求上下文\n\n")
		if t := strings.TrimSpace(in.Title); t != "" {
			fmt.Fprintf(&b, "- 标题：%s\n", t)
		}
		if d := strings.TrimSpace(in.Description); d != "" {
			fmt.Fprintf(&b, "\n### 需求描述\n\n%s\n\n", truncateRunes(d, qaDescMaxRunes))
		}
		if doc := strings.TrimSpace(in.DesignDocs); doc != "" {
			fmt.Fprintf(&b, "### 技术方案（原文）\n\n%s\n\n", truncateRunes(doc, qaDesignMaxRunes))
		}
		if sum := strings.TrimSpace(in.CodingSummary); sum != "" {
			fmt.Fprintf(&b, "### 开发阶段上下文摘要\n\n%s\n\n", truncateRunes(sum, qaSummaryMaxRunes))
		}
	}

	if len(in.History) > 0 {
		b.WriteString("## 本轮答疑之前的问答\n\n")
		for i, turn := range in.History {
			fmt.Fprintf(&b, "**问题 %d**：%s\n\n", i+1, strings.TrimSpace(turn.Question))
			if a := strings.TrimSpace(turn.Answer); a != "" {
				fmt.Fprintf(&b, "**回答 %d**：%s\n\n", i+1, truncateRunes(a, qaAnswerMaxRunes))
			}
		}
	}

	fmt.Fprintf(&b, "## 本次问题\n\n%s\n\n", strings.TrimSpace(in.Question))

	b.WriteString(qaRules)
	return b.String()
}

// qaResumedLead is the one-line lead-in of the resumed form.
func qaResumedLead(stage string) string {
	if stage == "coding" {
		return "我针对你刚才完成的开发实现有一个追问，请基于本次会话已有的代码、你的实现决策与之前的问答直接回答。"
	}
	return "我针对你刚才给出的技术方案有一个追问，请基于本次会话已有的方案内容、你的设计决策与之前的问答直接回答。"
}

// qaFreshLead is the lead-in of the self-contained form.
func qaFreshLead(stage string) string {
	if stage == "coding" {
		return "你是这个需求的开发者。用户针对已完成的开发实现提出追问，但原来的会话上下文已经不可用。" +
			"请结合下面的需求上下文、方案原文与仓库代码回答。"
	}
	return "你是这个需求的技术方案作者。用户针对已产出的技术方案提出追问，但原来的会话上下文已经不可用。" +
		"请结合下面的需求上下文、方案原文与仓库代码回答。"
}

// qaRules is the answering contract shared by both forms. It is what makes a
// Q&A turn a Q&A turn and not a second implementation run:
//
//   - 只答疑:不改文件、不改方案文档 —— 协议层还禁掉了 Write / Edit，但 prompt
//     也要说清楚,因为模型可能仍然"顺手"给出一份改好的方案。
//   - 禁止输出实施计划:一旦模型开始列"第一步改哪里",用户拿到的是一份没人
//     批准的计划,而不是答案。
//   - 引用要可核对:文件路径:行号,不确定就先 Read / Grep 核实。
const qaRules = `## 回答要求

1. **只答疑**。直接回答问题本身，不要输出实施计划、任务清单或"接下来怎么做"的步骤。
2. **不要修改任何文件**，也不要改写方案文档 —— 这是一次纯问答。（协议层已禁用 Write / Edit，Read / Glob / Grep / Bash 仍可用，供你核实代码。）
3. 需要引用代码时，用 ` + "`文件路径:行号`" + ` 指明位置；不确定的地方先用 Read / Grep 工具确认，不要凭印象作答。
4. 如果经核对发现原方案 / 原实现里的结论有误或不准确，直接说明并更正，不要为了自洽而辩护。
5. 用 Markdown 作答，控制在必要篇幅内；解释清楚「为什么这么定」比罗列事实更有价值。
6. 问题涉及调用链 / 并发时序 / 状态流转 / 模块依赖时，用 ` + "```mermaid" + ` 代码块作图说明（` + "`sequenceDiagram`" + ` / ` + "`flowchart TD`" + ` / ` + "`stateDiagram-v2`" + `），一张讲清楚即可，不要只写文字。
   Mermaid 11.x 兼容：节点 label 含 ` + "`(`" + `、` + "`)`" + `、` + "`{`" + `、` + "`}`" + ` 或半角 ` + "`;`" + ` 时整段用双引号包起来；sequenceDiagram 的消息文本里不要用半角 ` + "`;`" + `。
7. **不要输出 json 代码围栏块**。
`
