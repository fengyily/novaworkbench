package prompt

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
)

// 「方案拆分」的提示词与 JSON 契约。
//
// 拆分是「让架构师把一份大方案切成若干可独立交付的子需求」。产出走两段式：
// AI 先给候选（可编辑 / 删除 / 排序 / 指定前置），**人工确认后**才落库 ——
// 与项目里 analyst / architect / developer 三个阶段的「人工动作完成闸门」同构。
//
// 本文件是纯函数（对齐 parent_context.go / reference_context.go 的风格）：
// handler 拥有所有 I/O，所以整个契约可以脱离 DB / CLI 单测。

// SplitJSONContract — 拆分产物的 JSON 契约文本。
//
// CLI 通道把它作为 system prompt 的一部分下发，HTTP 降级通道把它内嵌进
// system message —— 两边必须看到**同一份**契约，所以它是导出的常量而不是
// 两处各写一遍。
const SplitJSONContract = `你正在把一份技术方案拆分成若干个可独立交付的子需求。

## 输出格式

先写一段人类可读的拆分说明（每个子需求为什么这么切、边界在哪），然后在回复的
**最后**放一个 json 围栏块，结构如下：

` + "```json" + `
{
  "tasks": [
    {
      "title": "子需求标题，≤ 40 字",
      "description": "自包含正文：本子需求要达成什么、怎么做、产出什么形态",
      "scope": ["backend/internal/handler/example.go"],
      "acceptance": ["可核对的验收点 1", "可核对的验收点 2"],
      "boundary": "与兄弟子需求的分工边界 + 双方约定的接口契约",
      "order": 1,
      "depends_on": []
    }
  ],
  "references": [
    { "from": 1, "to": 2, "reason": "2 的上报格式由 1 定义，两者必须保持一致" }
  ]
}
` + "```" + `

## 硬性要求

1. **数量**：tasks 数量在 2 到 12 之间。方案不值得拆时宁少勿多 —— 只有一个子
   需求的「拆分」没有意义。
2. **description 必须自包含**：子需求的执行 Agent 看不到主需求的方案全文，
   它只能看到自己的 description 以及系统自动注入的主需求上下文块。所以
   description 里必须把「本子需求要做什么」讲清楚。
3. **禁止指代**：不要写「见上一步」「如主方案所述」「同前面的分析」这类指向
   别处的说法。读者只有你这一段文字。
4. **禁止假设不存在的接口**：不要把主需求方案里没有出现的接口 / 表 / 字段
   当作「已经存在」。要依赖某个能力，就在 description 里说明它需要被谁提供。
5. **scope 尽量不重叠**：不同子需求的改动范围尽量不交叉。所有子需求都基于
   同一条基线分支并行开发，重叠的改动会在 review 阶段变成合并冲突。
6. **acceptance 必须可核对**：写能对着代码验证的验收点（例如「GET /api/x 在
   缺少参数时返回 400」），不要写「功能正常」这类无法验证的目标。
7. **boundary 写接口契约，不写实现耦合**：写清楚「谁提供什么、谁调用什么」的
   契约级别边界。**禁止**写成「必须等 X 的代码合并后我才能编译」—— 所有子需求
   共享同一条基线，合并顺序由依赖关系决定，不由编译依赖决定。
8. **depends_on 只能引用同一批次内 order 更小的任务序号**（1 起算）。序号
   表示「本子需求依赖谁先完成」，只能向后指，不得成环。
9. **references 由你以架构师视角判断**（这是本次拆分的核心判断项，务必认真对待）：
   - **互相参考 ≠ 前置依赖**。depends_on 是执行顺序闸门（后者没做完，前者不能
     开始）；references 是**上下文共享**（两个子需求在方案阶段互相引用对方的
     产出 / 接口约定 / 设计约束），它不阻塞任何流程。
   - **只在确实共享上下文时才连**。典型场景：一方定义了另一方必须遵守的接口 /
     数据结构 / 协议；两者必须对同一份设计保持一致；一方要消费另一方的产出。
     如果两个子需求只是「都属于同一个大需求」，那是**不连**的理由，不是连的理由。
   - **宁可少连，不要两两相连**。漏连的代价只是少一点上下文；连多了会让每个
     子需求的上下文被无关内容淹没，反而稀释了关键信息。
   - from / to 都是同一批次内的 order 序号（1 起算），表示「这两个子需求需要互相
     引用」；from 可以大于 to，含义相同（互相引用是无向的）。
   - **reason 必填**，用一句话说明为什么这两个子需求需要互相引用。用户会在确认
     界面上看到它，并据此判断你的判断是否正确 —— 写「无理由」「相关」这种废话
     等于没判断。
   - 如果确实没有任何一对需要互相引用，references 给空数组 []。
10. **JSON 块必须是整个回复的最后一段**。正文里不要出现其它 json 围栏块，否则
    解析会取到错误的那一段。`

// splitContractTail is the closing instruction that repeats the "JSON last"
// rule right before the model starts writing — models weight the last
// instruction more heavily than the first.
const splitContractTail = "\n\n现在请输出拆分说明，并在最后给出符合上述结构的 json 块。"

// SplitTask — 模型产出的一条候选子需求（已归一化）。
//
// 注意 DependsOn 存的是**批次内序号**（1-based），不是数据库 id —— 落库时才
// 知道真实 id。
type SplitTask struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Scope       []string `json:"scope"`
	Acceptance  []string `json:"acceptance"`
	Boundary    string   `json:"boundary"`
	Order       int      `json:"order"`
	DependsOn   []int    `json:"depends_on"`
}

// SplitReference — 一对「互相参考」的子需求。
//
// From / To 是**批次内序号**（1-based，与 SplitTask.Order 同一坐标系），不是
// 数据库 id。互相引用是**无向**的：归一化后 From < To。
//
// Reason 是模型给出的判断依据（一句话）。用户在人眼难以判断这一层，所以由
// 架构师视角的模型给出结论 + 理由，用户只需要确认或微调。
type SplitReference struct {
	From   int    `json:"from"`
	To     int    `json:"to"`
	Reason string `json:"reason"`
}

// SplitOutput — 一次拆分的完整产出：候选列表 + 互相参考建议。
type SplitOutput struct {
	// Tasks — 候选子需求（已归一化）。
	Tasks []SplitTask
	// References — 互相参考建议（已归一化，无向去重）。
	References []SplitReference
}

// 拆分产物的归一化上限。
const (
	// splitMaxTasks — 单次拆分最多保留多少条候选。契约里写死 2–12，超出截断
	// （防模型一次吐 30 条把详情页刷屏）。
	splitMaxTasks = 12
	// splitTitleMaxRunes — 子需求标题的 rune 上限。标题进列表行 / 树形节点，
	// 太长会把行撑开。
	splitTitleMaxRunes = 40
	// splitMaxReferences — 单次拆分最多保留多少对「互相参考」。
	//
	// 12 个候选共有 66 对组合，全连等于没有引用（每个子需求的上下文都会被
	// 无关内容淹没）。契约要求模型「宁可少连」，这里再兜一道上限。
	splitMaxReferences = 20
)

// splitFenceRe — 剥掉模型可能带上的 ```json / ``` 围栏。
var splitFenceRe = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)\\s*```")

// BuildSplitPrompt renders the split prompt.
//
// Inputs: the master requirement (title / kind), its design docs (the
// material being split) and the children it already has (so the model does
// not re-split work that was already broken out).
//
// Pure function — the handler owns the DB reads and the CLI invocation.
func BuildSplitPrompt(req *model.Requirement, designDocs string, existingChildren []*model.Requirement) string {
	if req == nil {
		return ""
	}
	var b strings.Builder

	b.WriteString("请把下面这份技术方案拆分成若干个**可独立交付、可独立提 PR**的子需求。\n\n")

	b.WriteString("## 主需求\n")
	if title := strings.TrimSpace(req.Title); title != "" {
		b.WriteString("- 标题：" + title + "\n")
	}
	if kind := strings.TrimSpace(req.Kind); kind != "" {
		b.WriteString("- 类型：" + kind + "\n")
	}
	if desc := strings.TrimSpace(req.Description); desc != "" {
		b.WriteString("- 描述：" + truncateRunes(desc, 4*1024) + "\n")
	}
	b.WriteString("\n")

	if len(existingChildren) > 0 {
		b.WriteString("## 已有子需求（不要重复拆这些）\n")
		for _, c := range existingChildren {
			if c == nil {
				continue
			}
			title := strings.TrimSpace(c.Title)
			if title == "" {
				title = "(无标题)"
			}
			b.WriteString("- " + title + "（状态：" + strings.TrimSpace(c.Status) + "）\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("## 技术方案\n\n")
	if docs := strings.TrimSpace(designDocs); docs != "" {
		b.WriteString(truncateRunes(docs, 24*1024))
		b.WriteString("\n\n")
	} else {
		b.WriteString("（暂无技术方案正文 —— 请根据需求描述拆分。）\n\n")
	}

	b.WriteString(SplitJSONContract)
	b.WriteString(splitContractTail)
	return b.String()
}

// NormalizeSplitTasks parses and normalizes the model's raw output.
//
// Never returns nil: an unparseable payload yields an empty slice so the
// caller can decide how to report the failure without a nil check.
//
// Normalization rules:
//   - a task without a title is dropped (an untitled requirement is useless)
//   - the title is truncated to splitTitleMaxRunes runes
//   - a missing / non-positive order is re-derived from the array position
//   - depends_on keeps only ordinals that are >= 1 and strictly smaller than
//     the task's own order (a forward or self reference is a cycle)
//   - an empty acceptance list gets a placeholder so the requirement always
//     carries something the user can edit
//   - the result is sorted by order and capped at splitMaxTasks
func NormalizeSplitTasks(raw string) []SplitTask {
	return NormalizeSplitOutput(raw).Tasks
}

// NormalizeSplitOutput parses the model's raw output into the full split
// product: the candidate list AND the "互相参考" suggestions.
//
// It is the entry point the handlers use — the references are part of the
// model's architectural judgement, so they must travel with the candidates
// instead of being re-derived (or, worse, asked of the user).
//
// Never returns nil slices: an unparseable payload yields empty slices.
func NormalizeSplitOutput(raw string) SplitOutput {
	out := SplitOutput{Tasks: []SplitTask{}, References: []SplitReference{}}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return out
	}

	payload := trimmed
	if m := splitFenceRe.FindStringSubmatch(trimmed); m != nil {
		payload = strings.TrimSpace(m[1])
	}

	var envelope struct {
		Tasks      []SplitTask      `json:"tasks"`
		References []SplitReference `json:"references"`
	}
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return out
	}
	if len(envelope.Tasks) == 0 {
		return out
	}

	for i := range envelope.Tasks {
		task := envelope.Tasks[i]
		task.Title = truncateRunes(strings.TrimSpace(task.Title), splitTitleMaxRunes)
		if task.Title == "" {
			continue
		}
		if task.Order <= 0 {
			task.Order = i + 1
		}
		task.Description = strings.TrimSpace(task.Description)
		task.Boundary = strings.TrimSpace(task.Boundary)
		task.Scope = cleanStringList(task.Scope)
		task.Acceptance = cleanStringList(task.Acceptance)
		if len(task.Acceptance) == 0 {
			task.Acceptance = []string{"（待补充验收标准）"}
		}
		task.DependsOn = normalizeDependsOn(task.DependsOn, task.Order)
		out.Tasks = append(out.Tasks, task)
	}

	// Stable sort by order.
	for i := 1; i < len(out.Tasks); i++ {
		for j := i; j > 0 && out.Tasks[j].Order < out.Tasks[j-1].Order; j-- {
			out.Tasks[j], out.Tasks[j-1] = out.Tasks[j-1], out.Tasks[j]
		}
	}
	if len(out.Tasks) > splitMaxTasks {
		out.Tasks = out.Tasks[:splitMaxTasks]
	}

	out.References = normalizeSplitReferences(envelope.References, len(out.Tasks))
	return out
}

// normalizeSplitReferences sanitizes the model's reference suggestions.
//
// Rules:
//   - both ends must be valid ordinals into the (already normalized) task
//     list; anything else is dropped
//   - a self-reference is dropped
//   - references are UNDIRECTED: (2,1) and (1,2) are the same pair, stored as
//     (from < to) and de-duplicated (the first reason wins)
//   - the count is capped at splitMaxReferences
//
// The ordinals are positions in the NORMALIZED task list (which is sorted by
// order), so they line up with the candidate table the user sees.
func normalizeSplitReferences(in []SplitReference, taskCount int) []SplitReference {
	out := []SplitReference{}
	seen := map[[2]int]struct{}{}
	for _, ref := range in {
		from, to := ref.From, ref.To
		if from < 1 || to < 1 || from > taskCount || to > taskCount {
			continue
		}
		if from == to {
			continue
		}
		if from > to {
			from, to = to, from
		}
		key := [2]int{from, to}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, SplitReference{
			From:   from,
			To:     to,
			Reason: truncateRunes(strings.TrimSpace(ref.Reason), 200),
		})
		if len(out) >= splitMaxReferences {
			break
		}
	}
	return out
}

// normalizeDependsOn keeps only the ordinals that can legally be a
// prerequisite: >= 1 and strictly smaller than the task's own order.
// Anything else is a forward reference (which would be a cycle) or a
// self-reference.
func normalizeDependsOn(in []int, order int) []int {
	seen := map[int]struct{}{}
	out := []int{}
	for _, d := range in {
		if d < 1 || d >= order {
			continue
		}
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// cleanStringList trims each entry and drops the empty ones.
func cleanStringList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// SplitTaskDescription renders a candidate's description deterministically.
//
// The description is NOT the model's free text — it is assembled from the
// candidate's own fields so every child body has the same shape. The
// model's prose only fills the 「目标」 paragraph.
func SplitTaskDescription(task SplitTask) string {
	var b strings.Builder
	b.WriteString("## 目标\n")
	if desc := strings.TrimSpace(task.Description); desc != "" {
		b.WriteString(desc)
		b.WriteString("\n")
	}
	if scope := cleanStringList(task.Scope); len(scope) > 0 {
		b.WriteString("\n## 涉及范围\n")
		for _, s := range scope {
			b.WriteString("- " + s + "\n")
		}
	}
	if acc := cleanStringList(task.Acceptance); len(acc) > 0 {
		b.WriteString("\n## 验收标准\n")
		for _, a := range acc {
			b.WriteString("- " + a + "\n")
		}
	}
	if boundary := strings.TrimSpace(task.Boundary); boundary != "" {
		b.WriteString("\n## 与其它子需求的边界\n")
		b.WriteString(boundary)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// SplitTaskToInput converts a normalized candidate into the service-layer
// input shape. The description is assembled here (not in the service) so the
// rendering rules live next to the prompt that produced the fields.
func SplitTaskToInput(task SplitTask) model.SplitTaskInput {
	return model.SplitTaskInput{
		Title:       task.Title,
		Description: SplitTaskDescription(task),
		Scope:       cleanStringList(task.Scope),
		Acceptance:  cleanStringList(task.Acceptance),
		Boundary:    strings.TrimSpace(task.Boundary),
		Order:       task.Order,
		DependsOn:   task.DependsOn,
	}
}

// SplitSummary renders a one-line human summary of a split batch — used for
// the job log line and for the confirm response.
func SplitSummary(tasks []SplitTask) string {
	return fmt.Sprintf("已生成 %d 个候选子需求", len(tasks))
}
