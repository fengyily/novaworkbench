package prompt

import (
	"fmt"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
)

// ── 跨项目需求 · 方案调整对话 ──────────────────────────────────────────────
//
// 整体方案是跨仓库的一次性产物。落地之后，用户常常发现方案里留着「待决策
// 事项」——接口超时取多少、幂等键用哪种策略、两个仓库的字段谁先谁后。
//
// 这段对话就是为这些点存在的：用户就着方案提一句，AI 答一句，双方把结论
// 谈拢；然后重跑一次方案作业（BuildCrossDesignPrompt 的 discussion 段），
// 产出一份把结论吃进去的完整方案。
//
// 与单仓库的「阶段答疑」（qa_chat.go）形状相同、意图不同：
//
//	阶段答疑 = 「这里为什么这么设计」—— 只读，只答，不产出结论。
//	方案调整 = 「这个点该取 3s 还是 5s」—— 只答，但**要收敛**：
//	          AI 必须把还没定的点列出来、把已定的点复述成清单，
//	          用户才能在这一串问答里逐条拍板。
//
// 那个「已确认结论」清单不是可有可无的：最后重跑方案作业时，喂进提示词的
// 就是这些回答正文 —— 结论不落到文字里，重跑时模型就无从遵守。
//
// 纯函数：handler 拥有所有 I/O（读跨项目需求 / 读讨论线程 / 读仓库基线）。

// CrossAdjustTurn — 调整对话里的一轮问答。
//
// 同一个类型在两处复用：作为「历史轮次」喂进调整提示词，以及作为「调整
// 讨论记录」喂进整体方案提示词。
type CrossAdjustTurn struct {
	// Question is the user's message.
	Question string
	// Answer is the model's reply.
	Answer string
}

// CrossAdjustPromptInput is everything one adjustment turn needs.
type CrossAdjustPromptInput struct {
	// Resumed selects the short form (the thread's session already holds the
	// design context) or the self-contained one (everything pasted into the
	// prompt).
	Resumed bool
	// Title / Description — the cross requirement itself.
	Title       string
	Description string
	// DesignDocs — the current overall design, as Markdown. This is the
	// authoritative state: the adjustment talk happens *about* this text.
	DesignDocs string
	// Projects — the participating projects (role / path / baseline).
	Projects []CrossProjectInput
	// Conventions — each participating repository's project CLAUDE.md.
	Conventions []CrossProjectDoc
	// AttachManifest — the attachment manifest (empty when there are none).
	AttachManifest string
	// PendingDecisions — the open questions parsed out of the design.
	PendingDecisions []model.CrossPendingDecision
	// History — the thread's earlier answered turns, oldest first.
	History []CrossAdjustTurn
	// Question — the user's message. Always present in both forms.
	Question string
}

// Truncation caps for the self-contained form. Rune-based, not byte-based: a
// byte cut would split a multi-byte Chinese character and emit mojibake into
// the prompt. The design document is the biggest block (it IS the context the
// user is asking about), the description is secondary, and each replayed
// answer is capped so a long thread cannot crowd out the question.
const (
	crossAdjustDescMaxRunes   = 4 * 1024
	crossAdjustDesignMaxRunes = 24 * 1024
	crossAdjustAnswerMaxRunes = 4000
)

// crossAdjustHistoryMaxTurns caps how many earlier turns are replayed into a
// fresh (self-contained) prompt.
//
// The tail is kept, not the head: the newest turns are the ones the model must
// not contradict. A long thread loses its oldest exchanges — acceptable,
// because the design document itself is replayed above and carries the
// original reasoning.
const crossAdjustHistoryMaxTurns = 10

// BuildCrossAdjustPrompt assembles the adjustment turn's task prompt
// (Chinese, per the repo convention that UI text and AI prompts are Chinese).
func BuildCrossAdjustPrompt(in CrossAdjustPromptInput) string {
	var b strings.Builder

	if in.Resumed {
		b.WriteString(crossAdjustResumedLead)
		b.WriteString("\n\n")
	} else {
		b.WriteString(crossAdjustFreshLead)
		b.WriteString("\n\n")
		b.WriteString("## 需求上下文\n\n")
		if t := strings.TrimSpace(in.Title); t != "" {
			fmt.Fprintf(&b, "- 标题：%s\n", t)
		}
		if d := strings.TrimSpace(in.Description); d != "" {
			fmt.Fprintf(&b, "\n### 需求描述\n\n%s\n\n", truncateRunes(d, crossAdjustDescMaxRunes))
		}
		if doc := strings.TrimSpace(in.DesignDocs); doc != "" {
			fmt.Fprintf(&b, "### 当前整体方案（原文）\n\n%s\n\n", truncateRunes(doc, crossAdjustDesignMaxRunes))
		}
		if section := BuildCrossProjectConventionSection(in.Conventions); section != "" {
			b.WriteString(section)
			b.WriteString("\n\n")
		}
		appendCrossAttachment(&b, in.AttachManifest)
		if len(in.PendingDecisions) > 0 {
			b.WriteString("### 当前方案里的待决策事项\n\n")
			for _, d := range in.PendingDecisions {
				detail := strings.TrimSpace(d.Detail)
				if detail == "" {
					detail = strings.TrimSpace(d.Title)
				}
				fmt.Fprintf(&b, "- %s\n", detail)
			}
			b.WriteString("\n")
		}
	}

	// The history replay belongs to the SELF-CONTAINED form only.
	//
	// In the resumed form the thread's session **is** the history — it was
	// forked from the design session and every turn since then was appended
	// to it. Replaying it would make the model read the same exchanges twice
	// and, worse, look like they are being re-stated.
	if !in.Resumed && len(in.History) > 0 {
		b.WriteString("## 本轮调整之前的讨论\n\n")
		for i, turn := range crossAdjustHistoryTail(in.History) {
			fmt.Fprintf(&b, "**问题 %d**：%s\n\n", i+1, strings.TrimSpace(turn.Question))
			if a := strings.TrimSpace(turn.Answer); a != "" {
				fmt.Fprintf(&b, "**回答 %d**：%s\n\n", i+1, truncateRunes(a, crossAdjustAnswerMaxRunes))
			}
		}
	}

	fmt.Fprintf(&b, "## 本次问题\n\n%s\n\n", strings.TrimSpace(in.Question))

	b.WriteString(crossAdjustRules)
	return b.String()
}

// crossAdjustHistoryTail returns the most recent turns of a thread, oldest
// first.
func crossAdjustHistoryTail(history []CrossAdjustTurn) []CrossAdjustTurn {
	if len(history) <= crossAdjustHistoryMaxTurns {
		return history
	}
	return history[len(history)-crossAdjustHistoryMaxTurns:]
}

// crossAdjustResumedLead is the one-line lead-in of the resumed form.
const crossAdjustResumedLead = "我针对你刚才给出的跨项目整体方案有一个调整请求。" +
	"请基于本次会话已有的方案内容、你的设计决策与本线程此前的讨论直接回答。"

// crossAdjustFreshLead is the lead-in of the self-contained form.
const crossAdjustFreshLead = "你是这个跨项目需求的整体方案作者。用户针对已产出的整体方案提出调整请求，" +
	"但原来的会话上下文已经不可用。请结合下面的需求上下文、当前方案原文、各仓库约定与仓库代码回答。"

// crossAdjustRules is the answering contract of an adjustment turn.
//
// 它与阶段答疑的规则同构，但目标不同：答疑是「解释一下」，调整对话是
// **收敛** —— 用户要把方案里剩下的取舍拍板，然后重跑方案作业。
//
// 所以回答必须显式分三段，标题固定。这三段是「按讨论结论重新补全方案」
// 能吃下结论的**唯一**依据：
//
//	## 已确认结论 —— 用户明确拍板的决定（服务端会单独抽出来）
//	## 待决策事项 —— 仍然需要人拍板的点（用户照着这条回复）
//	## 调整建议 —— 模型认为该改、但用户还没拍板的地方
//
// 「已确认结论」与「待决策事项」的**区分**是这里最要紧的一条：用户只是
// 提问、还在权衡、或者模型说「建议选 A」的，一律不算「已确认」——把没拍板
// 的东西写成结论，重跑方案作业时就会被当成用户的决定写进方案。
//
// 另外两条硬约束：
//
//   - 只讨论，不改文件、不改方案文档（协议层已禁 Write / Edit / Bash）；
//   - 不要输出整份改好的方案 —— 那是最后「重新补全方案」那一步的事，在这
//     里输出等于把没拍板的点也一起定了。
const crossAdjustRules = `## 回答要求

1. **回答问题，不要顺手重写整份方案。** 这一轮只回答用户问的事、只说明该改哪里。整份方案的重新补全是最后一步「按讨论结论重新补全方案」的动作（由用户单独触发），不是这一轮的事；用户明确说「重写」时除外。
2. **只讨论，不落盘。** 不要修改任何文件，也不要改写方案文档。（协议层已禁用 Write / Edit / Bash / NotebookEdit，Read / Glob / Grep 仍可用，供你核实代码。）
3. **回答必须显式分三段，标题固定** —— 这是「按讨论结论重新补全方案」能吃下结论的唯一依据：

   ## 已确认结论
   把用户在本次会话中**明确拍板**的决定逐条列出来（每条一行：定了什么、为什么这么定）。
   用户只是提问、还在权衡、或者你说「建议选 A」的，一律**不要**写进这一节。

   ## 待决策事项
   仍然需要人来拍板的点，逐条写：决策点 / 可选项（每个选项一句话说明代价或影响）/ 你的建议与理由 / 不决策的后果。没有就给「（无）」。

   ## 调整建议
   你认为方案里应该改、但用户还没拍板的地方。没有就给「（无）」。

4. 引用代码时用 ` + "`文件路径:行号`" + ` 指明位置；不确定的地方先用 Read / Grep 工具确认，不要凭印象作答。
5. 如果经核对发现当前方案里的结论有误或不准确，直接说明并更正，不要为了自洽而辩护。
6. 用 Markdown 作答，控制在必要篇幅内；解释清楚「为什么这么定」比罗列事实更有价值。
7. 问题涉及调用链 / 并发时序 / 状态流转 / 模块依赖时，用 ` + "```mermaid" + ` 代码块作图说明（` + "`sequenceDiagram`" + ` / ` + "`flowchart TD`" + ` / ` + "`stateDiagram-v2`" + `），一张讲清楚即可，不要只写文字。
   Mermaid 11.x 兼容：节点 label 含 ` + "`(`" + `、` + "`)`" + `、` + "`{" + `、` + "`}`" + ` 或半角 ` + "`;`" + ` 时整段用双引号包起来；sequenceDiagram 的消息文本里不要用半角 ` + "`;`" + `。
8. **不要输出 json 代码围栏块**。
`

// BuildCrossDiscussionBlock renders the 「## 调整讨论记录」 section that the
// overall-design prompt carries.
//
// 它是「方案调整对话」与「重新补全方案」之间的桥：讨论记录里写着用户与 AI
// 谈拢的结论，重跑方案作业时这段结论必须原样进入提示词，模型才知道该按什么
// 修订、哪些部分别动。
//
// 结构：先给原始问答（永远给 —— 结论抽取失败时它就是唯一的依据），再给两
// 份高信噪比的汇总（「已确认结论」与「仍未决」），最后写清楚修订规则。
//
// 空线程 → 空串（调用方据此让提示词与「这个特性存在之前」逐字相同）。
func BuildCrossDiscussionBlock(turns []CrossAdjustTurn) string {
	usable := make([]CrossAdjustTurn, 0, len(turns))
	for _, t := range turns {
		if strings.TrimSpace(t.Question) == "" || strings.TrimSpace(t.Answer) == "" {
			// A turn with no answer carries no conclusion — there is nothing
			// for the model to follow.
			continue
		}
		usable = append(usable, t)
	}
	if len(usable) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## 调整讨论记录\n\n")
	b.WriteString("用户在方案定稿前，就下面这些待决策事项与你进行过讨论。以下是讨论记录（按时间正序，每轮是「问 / 答」）：\n\n")

	for i, turn := range usable {
		fmt.Fprintf(&b, "**第 %d 轮**\n\n", i+1)
		fmt.Fprintf(&b, "- 问：%s\n\n", strings.TrimSpace(turn.Question))
		fmt.Fprintf(&b, "- 答：%s\n\n", truncateRunes(strings.TrimSpace(turn.Answer), crossAdjustAnswerMaxRunes))
	}

	// 两份汇总：逐轮抽取后合并（不能从合并串里抽 —— 标题会撞）。
	confirmed := make([]string, 0, len(usable))
	open := make([]string, 0, len(usable))
	for _, turn := range usable {
		if c := strings.TrimSpace(ExtractCrossConfirmedConclusions(turn.Answer)); c != "" {
			confirmed = append(confirmed, c)
		}
		if o := strings.TrimSpace(ExtractCrossOpenDecisions(turn.Answer)); o != "" {
			open = append(open, o)
		}
	}
	if len(confirmed) > 0 {
		b.WriteString("### 已确认结论（讨论中用户明确拍板的）\n\n")
		for _, c := range confirmed {
			b.WriteString(c)
			b.WriteString("\n\n")
		}
	}
	if len(open) > 0 {
		b.WriteString("### 讨论中仍未决的\n\n")
		for _, o := range open {
			b.WriteString(o)
			b.WriteString("\n\n")
		}
	}

	b.WriteString("按这份讨论修订「## 上一版整体方案」时：\n\n")
	b.WriteString("1. **只按「已确认结论」修订。** 讨论里只是提问、用户尚未拍板的，一律视为**仍未决**，继续列进「## 待决策事项」章节，不得替你拍板。\n")
	b.WriteString("2. **讨论没有覆盖的章节保持上一版原样** —— 不要顺手改写、不要重新设计、不要调整章节顺序。\n")
	b.WriteString("3. 已确认结论与上一版方案冲突时，以讨论结论为准，就地修正对应章节。\n")
	b.WriteString("4. 修订后仍需拍板的事项，继续逐条写进「## 待决策事项」（决策点 / 选项及代价 / 建议 / 不决策的后果）；确实没有了就写「（无）」。\n")
	return b.String()
}
