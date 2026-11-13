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
9. **JSON 块必须是整个回复的最后一段**。正文里不要出现其它 json 围栏块，否则
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

// 拆分产物的归一化上限。
const (
	// splitMaxTasks — 单次拆分最多保留多少条候选。契约里写死 2–12，超出截断
	// （防模型一次吐 30 条把详情页刷屏）。
	splitMaxTasks = 12
	// splitTitleMaxRunes — 子需求标题的 rune 上限。标题进列表行 / 树形节点，
	// 太长会把行撑开。
	splitTitleMaxRunes = 40
)

// splitFenceRe — 剥掉模型可能带上的 ```json / ``` 围栏。
var splitFenceRe = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)\\s*```")

// BuildSplitPrompt renders the split prompt.
//
// Inputs: the master requirement (title / kind), its design docs (the
// material being split), the children it already has (so the model does not
// re-split work that was already broken out) and the requirement-context
// block (attachments / granted directories / referenced requirements — see
// split_context.go).
//
// The context block is inserted between 「主需求」 and 「技术方案」 so the
// model reads the material BEFORE it reads the plan it has to cut up. An
// empty block changes nothing: the output is byte-identical to the prompt
// without the feature, which is what keeps a requirement without any
// material on exactly the old behaviour.
//
// Pure function — the handler owns the DB reads and the CLI invocation.
func BuildSplitPrompt(req *model.Requirement, designDocs string, existingChildren []*model.Requirement, contextBlock string) string {
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

	// The requirement's own material: attachments, granted directories and
	// referenced requirements. Present only when the requirement carries
	// any — see BuildSplitContextBlock.
	if ctxBlock := strings.TrimSpace(contextBlock); ctxBlock != "" {
		b.WriteString(ctxBlock)
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
	out := []SplitTask{}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return out
	}

	payload := trimmed
	if m := splitFenceRe.FindStringSubmatch(trimmed); m != nil {
		payload = strings.TrimSpace(m[1])
	}

	var envelope struct {
		Tasks []SplitTask `json:"tasks"`
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
		out = append(out, task)
	}

	// Stable sort by order.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Order < out[j-1].Order; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if len(out) > splitMaxTasks {
		out = out[:splitMaxTasks]
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
