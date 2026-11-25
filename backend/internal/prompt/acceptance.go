package prompt

import (
	"fmt"
	"strings"
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
6. 正文用中文。`

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
    ` + "`unknown`" + ` 表示该验收点无法验证（子需求没有实现目录、或代码不在本地），
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
		b.WriteString(fmt.Sprintf("### %d. %s\n", i+1, title))
		b.WriteString("- 子需求 id：" + m.RequirementID + "\n")
		b.WriteString("- 状态：" + strings.TrimSpace(m.Status) + "\n")
		if wt := strings.TrimSpace(m.WorktreePath); wt != "" {
			// The implementation directory — the ONLY place this child's code
			// exists. The model must verify against this tree.
			b.WriteString("- 实现目录：" + wt + "（**核对以这个目录里的代码为准**）\n")
		} else {
			// No implementation yet. Say so loudly: a silent omission would
			// let the model grade the base branch and report it as verified.
			b.WriteString("- 实现目录：（无 —— 该子需求尚未进入开发，没有可核对的实现；" +
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
func AcceptanceSummaryText(memberCount int) string {
	return fmt.Sprintf("已生成验收核对报告（覆盖 %d 个子需求）", memberCount)
}
