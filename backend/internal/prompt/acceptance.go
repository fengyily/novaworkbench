package prompt

import (
	"fmt"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
)

// 「验收核对」的提示词。
//
// 主需求把开发**下放**给拆出来的子需求之后，它自己就不再写代码了。但主需求
// 的验收标准不会跟着消失 —— 「4 个子需求都标成 done」不等于「主需求的目的
// 达成了」。这个文件负责把两样东西摆在模型面前：
//
//	契约（判定依据）：主需求的标题 / 描述 / 验收标准 / 技术方案
//	实现（判定对象）：每个子需求**自己的** worktree 目录
//
// 提示词里只给**路径**，不给内容 —— 内容由模型自己去读（llm.StreamOpts 的
// AddDirs 把这些目录授权给会话）。把代码塞进提示词会让上下文爆掉，也让
// 模型更容易对着过时快照编造结论。
//
// 本文件是纯函数：handler 拥有所有 I/O。

// AcceptanceContract — 验收核对报告的结构契约。
//
// 报告的每一节都要能被人**复核**：结论必须落到具体文件、具体验收点，
// 不能只有一句「基本达成」。
const AcceptanceContract = `你在为一个**主需求**做验收核对。主需求的开发已经下放给若干**子需求**，
你的任务是站在验收者的角度，核对这些子需求的开发合起来**是否达成了主需求的目的**。

## 输出格式

直接输出 Markdown 报告，不要包在代码围栏里。必须包含以下章节（顺序固定）：

## 总体结论
一句话说明核对结果，并给出总体结论：**已达成 / 部分达成 / 未达成**。
再用一两句说明依据。
若仍有子需求没完成（见「本次核对的时点」），总体结论最高只能是「部分达成」，
并在结论里点明还有几个子需求在开发中。

## 逐条验收判定
把主需求的验收标准**逐条**列出，每条包含：
- 验收点（主需求的要求原文或它的忠实概括）
- 判定：**通过 / 部分通过 / 未通过 / 无法验证**
- 依据：具体到 ` + "`文件:行号`" + ` / 函数名 / 测试名，说明你**实际**读到了什么
- 归属：这条验收点由哪个子需求承担（写子需求标题）

## 未实现清单
列出主需求要求了、但子需求**没有实现**的部分。每条写明缺什么、期望是什么、
本该由哪个子需求承担。
「无法验证」（子需求没有实现目录、代码不在本地）**不算**未实现，请把它放在
判定表的「无法验证」里，不要混进本节。
仍在开发中的子需求**也不算**未实现 —— 它只是「还没写完」，请把它放在判定表的
「部分通过」或「无法验证」里，并在「遗留风险」里点名是哪个子需求还没写完。

## 建议再拆的子需求
针对上面确实缺失、且**值得独立成一个子需求**的部分，给出建议。每条包含：
- 建议标题
- 目标（这个子需求要达成什么）
- 验收标准（几条可核对的验收点）
- 建议前置（依赖哪些已有子需求）

确实不需要再拆时写「无需再拆」。

## 遗留风险
本次核对没有覆盖到的点，以及合入前需要注意的事项。

## 硬性要求

1. **不要编造**。你没有实际读到某个子需求的实现时，结论写「无法验证」并说明
   原因 —— 编造的「通过」比「无法验证」有害得多。
2. **必须真的去读实现目录**。每个子需求给了自己的实现目录，你要用 Read /
   Grep / Glob 去核实，而不是只看子需求的描述就下结论。
3. **依据必须可复核**。写 ` + "`文件:行号`" + ` 或函数名，不要写「已确认」「基本实现」
   这类空话。依据写不出来的条目，判定只能是「无法验证」。
4. **不要给修复代码，也不要改任何文件**。你的任务是核对与报告，返工由子需求
   自己的开发流程处理。
5. **区分「未实现」与「无法验证」**：前者是代码里确实没有，后者是你看不到。
   两者混为一谈会冤枉子需求。
6. **区分「未实现」与「还没写完」**：仍在开发中的子需求，缺的部分是「还没写完」，
   不是「未实现」 —— 判「部分通过」或「无法验证」，别写进「未实现清单」。
   「未实现清单」只放**已完成却没做**的部分。
7. 正文用中文。`

// acceptanceContractTail is the sentence that closes the prompt, after the
// contract. It comes last so the model reads the output format right before
// it writes.
const acceptanceContractTail = "\n\n现在请输出验收核对报告。"

// AcceptanceJSONSchema is the ONE description of the machine-readable shape an
// acceptance report must carry.
//
// It is exported for the same reason prompt.ReviewJSONSchema is: the contract
// (what the model is asked to emit) and the parser (what the backend reads)
// must not drift — a drift would mean the parser paying for a contract the
// prompt never described.
const AcceptanceJSONSchema = `{"verdict":"yes|partial|no","summary":"一句话结论","checks":[{"criterion":"主需求的一条验收点","status":"yes|partial|no|unknown","evidence":"判断依据：文件:行号 / 函数名 / 测试名","child_id":"归属子需求的 id（不知道就留空）","child_title":"归属子需求的标题"}],"gaps":[{"title":"缺失项标题","detail":"缺什么、期望是什么","child_id":"归属子需求的 id","child_title":"归属子需求的标题"}],"proposals":[{"title":"建议的子需求标题","goal":"目标","acceptance":["一条验收标准"],"depends_on":["建议前置的子需求标题"]}]}`

// acceptanceJSONContract is the machine-readable tail of the report.
//
// The shape mirrors the Review contract's discipline (one fenced json block at
// the very end, fixed field names, an explicit value vocabulary) but the
// content is the acceptance verdict, not findings.
const acceptanceJSONContract = `
**报告正文结束后，必须在最末尾追加一个 json 代码围栏块**，供系统解析核对结论：

**这是硬性要求**：缺了这个块，逐条判定 / 未实现清单 / 建议再拆的子需求都拿不到，
系统只能把整篇报告当纯 Markdown 展示。不可省略、不可放在正文中间、不可改写字段名。

` + "```json" + `
` + AcceptanceJSONSchema + `
` + "```" + `

JSON 块的约束：
- ` + "`verdict`" + ` 取值只能是 ` + "`yes`" + ` / ` + "`partial`" + ` / ` + "`no`" + `，分别表示
  「已达成」/「部分达成」/「未达成」。
- ` + "`summary`" + ` 是结论的一句话概括。
- ` + "`checks`" + ` 与正文的「逐条验收判定」一一对应，顺序一致。
  - ` + "`status`" + ` 只能是 ` + "`yes`" + ` / ` + "`partial`" + ` / ` + "`no`" + ` / ` + "`unknown`" + `；
    ` + "`unknown`" + ` 表示该验收点无法验证（子需求没有实现目录、代码不在本地，
    或该子需求仍在开发中、这一条还没写到），
    不要用它来回避判断。
  - ` + "`evidence`" + ` 必须是可核对的出处： ` + "`文件:行号`" + ` / 函数名 / 测试名，
    不要写「已确认」这类空话。
  - ` + "`child_id`" + ` / ` + "`child_title`" + ` 写这条验收点归属的子需求；
    提示词里给了子需求的 id 与标题，照抄即可，不知道就留空字符串。
  - 实在拆不出验收点时写 ` + "\"checks\":[]" + `，不要省略这个字段。
- ` + "`gaps`" + ` 是正文的「未实现清单」，逐条对应；没有缺口时写 ` + "\"gaps\":[]" + `。
- ` + "`proposals`" + ` 是正文的「建议再拆的子需求」；不需要再拆时写 ` + "\"proposals\":[]" + `。
- JSON 必须是单个合法对象。
- json 块之后**不要再输出任何字符**（不要写总结、致谢、说明之类的话）。
`

// AcceptanceMember is one sub-requirement as it appears in the prompt.
//
// The implementation directory is the point of this struct: a split
// sub-requirement develops in ITS OWN worktree, so that is the only place
// its code exists. An empty WorktreePath means the sub-requirement never
// entered development — the prompt says so explicitly, so the model reports
// "无法验证" instead of silently grading the base branch.
type AcceptanceMember struct {
	// RequirementID is the sub-requirement id.
	RequirementID string
	// Title is the sub-requirement title.
	Title string
	// Status is the sub-requirement's current status.
	Status string
	// Description is the sub-requirement body (goal / scope / acceptance /
	// boundary, assembled by service.buildSplitDescription).
	Description string
	// AcceptanceCriteria is the sub-requirement's own acceptance points
	// (a JSON array string, one per line in the prompt).
	AcceptanceCriteria []string
	// WorktreePath is the sub-requirement's implementation directory.
	WorktreePath string
	// Stage is the child's development state at check time
	// (model.AcceptanceStage*): done / in_progress / not_started /
	// unavailable.
	//
	// It is what keeps the three states apart in the report:
	//
	//	done        — the delivery is final; what is missing is really missing;
	//	in_progress — the directory holds a PARTIAL snapshot, so "not written
	//	              yet" is NOT "not implemented";
	//	not_started — nothing was written at all;
	//	unavailable — it is being developed but its code is not on this
	//	              machine.
	Stage string
	// DependsOn are the titles of the sub-requirements this one depends on.
	DependsOn []string
	// DependsOnUnmet reports that at least one of those predecessors is not
	// finished yet — so this sub-requirement's output cannot yet be relied
	// on to satisfy a shared acceptance point.
	DependsOnUnmet bool
}

// AcceptanceContractInput is the master requirement's side of the prompt.
//
// It is the YARDSTICK: the acceptance check grades the children against the
// master's contract, never against the children's own (much weaker)
// acceptance points.
type AcceptanceContractInput struct {
	// Title / Description — the master's own statement of the goal.
	Title       string
	Description string
	// AcceptanceCriteria — the master's acceptance points, one per item.
	AcceptanceCriteria []string
	// DesignDocs — the master's technical plan (the "方案"). It is the
	// detailed contract the children were split out of.
	DesignDocs string
	// WorktreePath — the master's own worktree (the stack base). It is the
	// BASELINE: it shows the integration surface the children land on, and
	// it is where the children are expected to be merged eventually.
	WorktreePath string
}

// BuildAcceptancePrompt renders the acceptance-check prompt.
//
// The order matters and is deliberate:
//
//  1. the task and the contract (what is being judged against),
//  2. the implementation directories (what to read),
//  3. the project conventions (how the code is supposed to look),
//  4. the output contract (how to answer).
//
// The conventions come before the output contract because the model reads
// the contract last and must have the conventions in mind before it starts
// judging.
func BuildAcceptancePrompt(master AcceptanceContractInput, members []AcceptanceMember, conventions string) string {
	var b strings.Builder

	b.WriteString("请对下面这个主需求做一次验收核对。\n\n")

	// ── 1. 契约 ──────────────────────────────────────────────────────────
	b.WriteString("## 主需求（核对依据）\n")
	if t := strings.TrimSpace(master.Title); t != "" {
		b.WriteString("- 标题：" + t + "\n")
	}
	if d := strings.TrimSpace(master.Description); d != "" {
		b.WriteString("- 描述：\n\n" + truncateRunes(d, 4*1024) + "\n")
	}
	b.WriteString("\n")
	if len(master.AcceptanceCriteria) > 0 {
		b.WriteString("### 验收标准（逐条核对的对象）\n")
		for _, c := range master.AcceptanceCriteria {
			c = strings.TrimSpace(c)
			if c == "" {
				continue
			}
			b.WriteString("- " + c + "\n")
		}
		b.WriteString("\n")
	}
	if dd := strings.TrimSpace(master.DesignDocs); dd != "" {
		b.WriteString("### 技术方案（拆分子需求的依据）\n\n")
		b.WriteString(truncateRunes(dd, 16*1024))
		b.WriteString("\n\n")
	}

	// ── 2. 子需求与实现目录 ──────────────────────────────────────────────
	b.WriteString("## 子需求与实现目录（核对对象）\n\n")
	if len(members) == 0 {
		b.WriteString("（本需求没有拆分子需求）\n\n")
	}
	for i, m := range members {
		title := strings.TrimSpace(m.Title)
		if title == "" {
			title = "(无标题)"
		}
		// The caller may leave the stage empty; deriving it here keeps the
		// prompt honest no matter who builds the member list.
		stage := m.Stage
		if stage == "" {
			stage = AcceptanceStage(m.Status, m.WorktreePath)
		}
		b.WriteString(fmt.Sprintf("### %d. %s\n", i+1, title))
		b.WriteString("- 子需求 id：" + m.RequirementID + "\n")
		b.WriteString("- 状态：" + strings.TrimSpace(m.Status) + acceptanceStageNote(stage) + "\n")
		if wt := strings.TrimSpace(m.WorktreePath); wt != "" {
			// The implementation directory — the ONLY place this child's code
			// exists. The model must verify against this tree.
			b.WriteString("- 实现目录：" + wt + "（**核对以这个目录里的代码为准**" +
				acceptanceImplDirNote(stage) + "）\n")
		} else {
			// No implementation to read. Say so loudly: a silent omission
			// would let the model grade the base branch and report it as
			// verified.
			b.WriteString("- 实现目录：（无 —— " + acceptanceNoImplReason(stage) + "；" +
				"与它相关的验收点请判定为「无法验证」，**不要**写进「未实现清单」）\n")
		}
		if len(m.AcceptanceCriteria) > 0 {
			b.WriteString("- 该子需求自己的验收标准：\n")
			for _, c := range m.AcceptanceCriteria {
				c = strings.TrimSpace(c)
				if c == "" {
					continue
				}
				b.WriteString("  - " + c + "\n")
			}
		}
		if len(m.DependsOn) > 0 {
			b.WriteString("- 前置子需求：" + strings.Join(m.DependsOn, "、"))
			if m.DependsOnUnmet {
				b.WriteString("（**其中前置尚未完成** —— 后置子需求的产出不可用于支撑验收，" +
					"相关验收点请判定为「无法验证」）")
			}
			b.WriteString("\n")
		}
		if desc := strings.TrimSpace(m.Description); desc != "" {
			b.WriteString("\n" + truncateRunes(desc, 2*1024) + "\n")
		}
		b.WriteString("\n")
	}

	if wt := strings.TrimSpace(master.WorktreePath); wt != "" {
		b.WriteString("## 基线目录（集成面）\n\n")
		b.WriteString("主需求自己的分支检出在：" + wt + "\n")
		b.WriteString("子需求最终会合入到这条线。请顺带确认基线与各子需求实现之间的接口是否对得上" +
			"（例如子需求新增的接口在基线上是否已被调用）。\n\n")
	}

	// ── 2b. 核对的时点 ───────────────────────────────────────────────────
	// The snapshot section. It is only rendered when something is still in
	// flight — with every child delivered, "the verdict is a snapshot" is
	// noise.
	if s := acceptanceSnapshotSection(members); s != "" {
		b.WriteString(s)
	}

	// 可用工具 —— 模型得知道它能真的去读实现目录。
	b.WriteString(AcceptanceToolNote)
	b.WriteString("\n\n")

	// ── 3. 项目约定 ──────────────────────────────────────────────────────
	if c := strings.TrimSpace(conventions); c != "" {
		b.WriteString(c)
		b.WriteString("\n\n")
	}

	// ── 4. 输出契约 ──────────────────────────────────────────────────────
	b.WriteString(AcceptanceContract)
	b.WriteString(acceptanceJSONContract)
	b.WriteString(acceptanceContractTail)
	return b.String()
}

// AcceptanceStage maps a child's status + implementation directory onto the
// development stage the acceptance check sees.
//
// The stage is about what can be READ, not about what the workflow believes:
// a child that is still being developed holds a partial implementation, and
// a child whose implementation is not on this machine holds none at all.
//
// It lives here (next to the prompt that renders it) because the prompt is
// what turns the stage into an instruction; the handler reuses it to decide
// whether a check has anything to read at all.
func AcceptanceStage(status, worktree string) string {
	st := strings.TrimSpace(status)
	hasDir := strings.TrimSpace(worktree) != ""
	switch st {
	case "done", "archived":
		// Archived is a finished requirement that was moved to the knowledge
		// base: its delivery is final either way.
		return model.AcceptanceStageDone
	case "developing":
		if hasDir {
			return model.AcceptanceStageInProgress
		}
		return model.AcceptanceStageUnavailable
	}
	if hasDir {
		return model.AcceptanceStageInProgress
	}
	return model.AcceptanceStageNotStarted
}

// acceptanceStageNote renders the development state of one member next to its
// raw status.
//
// It is the sentence that keeps "not written yet" and "not implemented"
// apart: a child that is still being developed has NOT failed to deliver
// anything, it simply has not finished.
func acceptanceStageNote(stage string) string {
	switch stage {
	case model.AcceptanceStageDone:
		return "（已完成 —— 目录里的代码就是它的最终交付）"
	case model.AcceptanceStageInProgress:
		return "（**开发中** —— 还没交付，本次核对只是一次阶段性快照）"
	case model.AcceptanceStageUnavailable:
		return "（开发中，但本地看不到它的实现）"
	case model.AcceptanceStageNotStarted:
		return "（尚未进入开发）"
	}
	return ""
}

// acceptanceImplDirNote renders the caveat that belongs to an implementation
// directory.
//
// Only the in-progress stage needs one: the directory holds a PARTIAL
// snapshot, and without the caveat the model reads "the code is not there"
// as "the child never implemented it".
func acceptanceImplDirNote(stage string) string {
	if stage == model.AcceptanceStageInProgress {
		return "；该子需求仍在开发中，这里是**阶段性**快照 —— 还没写的部分请判「部分通过」或「无法验证」，**不要**写进「未实现清单」"
	}
	return ""
}

// acceptanceNoImplReason explains why a member has no implementation to read.
//
// The wording follows the stage: a child that never started and a child that
// is being developed on another machine are different situations, and the
// model should not tell the user they are the same.
func acceptanceNoImplReason(stage string) string {
	switch stage {
	case model.AcceptanceStageUnavailable:
		return "该子需求仍在开发中，但本地没有它的实现目录，没有可核对的实现"
	case model.AcceptanceStageDone:
		return "该子需求已完成，但本地没有它的实现目录，没有可核对的实现"
	case model.AcceptanceStageNotStarted:
		return "该子需求尚未进入开发，没有可核对的实现"
	}
	return "本地没有它的实现目录，没有可核对的实现"
}

// acceptanceSnapshotSection renders the "when" of the check.
//
// It exists because an acceptance check is runnable at ANY moment — the
// user asks for one while the children are still being developed, because
// they want to know where the work stands. The verdict is then a snapshot
// of the current progress, not a sign-off, and the report has to say so.
func acceptanceSnapshotSection(members []AcceptanceMember) string {
	var done, inProgress, pending int
	for _, m := range members {
		stage := m.Stage
		if stage == "" {
			stage = AcceptanceStage(m.Status, m.WorktreePath)
		}
		switch stage {
		case model.AcceptanceStageDone:
			done++
		case model.AcceptanceStageInProgress:
			inProgress++
			pending++
		default:
			pending++
		}
	}
	if pending == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 本次核对的时点\n\n")
	b.WriteString("本次核对反映的是**当前进度**的一次快照。子需求的开发还在推进时也可以发起核对，"+
		"但结论只对当下成立：\n\n")
	b.WriteString("- 已完成：" + fmt.Sprintf("%d", done) + " 个（它们的交付是最终交付，缺什么就是真缺）\n")
	b.WriteString("- 开发中：" + fmt.Sprintf("%d", inProgress) + " 个（目录里是阶段性快照，「还没写」不等于「未实现」）\n")
	b.WriteString("- 未完成或实现不在本地：" + fmt.Sprintf("%d", pending-inProgress) + " 个（没有可核对的实现，相关验收点判「无法验证」）\n\n")
	b.WriteString("只要还有子需求没完成，总体结论最高只能是「部分达成」，并且要在结论里点明"+
		"仍有子需求在开发中。\n\n")
	return b.String()
}

// AcceptanceToolNote tells the model which tools it may use.
//
// Read / Grep / Glob / Bash stay available — an acceptance check that
// cannot open the implementation directories would be a guess, not a check.
// Write / Edit are denied at the CLI level (see the handler); the note only
// has to say so once.
const AcceptanceToolNote = `## 可用工具

你可以用 Read / Grep / Glob 读实现目录，用 Bash 跑只读命令（` + "`ls`" + ` / ` + "`cat`" + ` /
` + "`git log`" + ` / ` + "`git diff`" + ` / ` + "`git show`" + `）来核实。
Write / Edit 已被禁用 —— 本次核对**只读**，不要尝试改动任何文件。`

// AcceptanceSummaryText renders the one-line job summary of an acceptance run.
//
// pendingCount is how many children had not delivered when the check ran. A
// non-zero count makes the summary say so: the verdict is then a snapshot
// of the current progress, not a sign-off, and the live stream is where the
// user reads that first.
func AcceptanceSummaryText(memberCount, pendingCount int) string {
	if pendingCount > 0 {
		return fmt.Sprintf("已生成验收核对报告（覆盖 %d 个子需求，其中 %d 个尚未完成 —— 阶段性结论）",
			memberCount, pendingCount)
	}
	return fmt.Sprintf("已生成验收核对报告（覆盖 %d 个子需求）", memberCount)
}
