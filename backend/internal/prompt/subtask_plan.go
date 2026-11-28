package prompt

import "strings"

// ── 子任务 Plan Mode（方案先行 → 自动开发，两段式）────────────────────────
//
// 一个「方案先行」的子任务分两段跑:
//
//	第 1 段(plan)  —— 只读探索代码库,产出一份 Markdown 实施方案,不写任何代码。
//	第 2 段(auto)  —— 把上面那份方案当规格注入,再落盘实现。
//
// 两段之间有一道人工闸门:用户先审阅方案(渲染 + 导出),再点「开始开发」。
// 所以本文件里的两段文本有两个完全不同的受众:
//
//	SubTaskPlanInstructionsBlock —— plan 段的用户指令。它必须读起来像一份
//	    「任务交办单」,而不是「实现命令」:模型要做的是探索 + 写方案,
//	    不是改代码。CLI 的 plan 模式会拦掉 Write/Edit(除了写方案文件),
//	    但提示词也要把话说明白,否则模型会反复尝试然后放弃。
//
//	SubTaskPlanDevBlock —— 第 2 段的方案注入块。方案在这里是**规格**,
//	    实现者要照着做,所以措辞是命令式的。
//
// 纯函数:不碰 DB、不碰 claude,可以在 prompt 包里直接单测。

// SubTaskPlanInstructionsBlock is the user-prompt block for the plan phase of
// a sub-task.
//
// It is deliberately scoped to ONE sub-task. The requirement-level architect
// persona asks for a whole design (UI element lists, ASCII layouts, data
// model changes); a single sub-task only needs "what to change, where, and
// how we will know it works".
const SubTaskPlanInstructionsBlock = `
## 任务：只产出实施方案，不要写代码

这是一个**方案先行**的子任务。本阶段你的唯一产出是一份 Markdown 实施方案。

### 硬性约束

1. **只读探索**：用 Read / Grep / Glob 读代码，**不要修改任何项目文件**，
   不要新建文件，不要执行会改动工作区的命令。
2. **不要提交 / 推送**：本阶段不产生代码改动，因此没有任何东西需要 commit 或 push。
3. **不要拆子任务**：本子任务已经是一个被拆好的最小单元，直接给方案。

### 实施方案必须包含且只包含这几节

用二级标题（##）分节，顺序固定，每节都要填实：

- 「## 目标」—— 本子任务要达成什么。1~3 句，说清楚「做完之后世界有什么不一样」。
- 「## 现状」—— 相关代码现在的结构：关键文件、关键函数/类型、它们的调用关系。
  每一项都写成「文件路径:符号名」（例：backend/internal/service/sub_task.go:SubTaskService.Create）。
- 「## 改动点」—— 要改哪些文件、每个文件改什么。逐条写「文件路径:行号」或
  「文件路径:符号名」+ 这处要做的具体事情。涉及新增文件时写明新文件路径与职责。
- 「## 验收点」—— 怎么判断这一步做完了。必须可执行、可观察（例：某个测试通过、
  某个接口返回什么、某个命令的输出是什么）。不要写「功能正常」这种没法验证的话。
- 「## 风险」—— 已知风险与应对。没有风险就写「（无）」。

### 落盘方式

完成调研后，用 Write 工具把方案 Markdown **一次性**写入
` + "`~/.claude/plans/<slug>.md`" + `（slug 用子任务标题的英文短串）。
后端会从这次 Write 捕获方案正文并持久化。若改用 plan 审批工具（ExitPlanMode）
提交方案，后端也会从其 plan 参数兜底捕获，但优先走 Write 路径。

正文里不要加任何额外的前言或结语——落盘的就是方案本身。
`

// SubTaskPlanDevBlock wraps the plan produced by the plan phase so the
// implementation phase can follow it as a specification.
//
// An empty plan yields an empty string: the caller must not inject an empty
// heading (the implementation phase of an ordinary sub-task has no plan).
func SubTaskPlanDevBlock(planMarkdown string) string {
	plan := strings.TrimSpace(planMarkdown)
	if plan == "" {
		return ""
	}
	return "## 实施方案（上一阶段 plan 模式产出，请严格按此实现）\n\n" + plan + "\n"
}
