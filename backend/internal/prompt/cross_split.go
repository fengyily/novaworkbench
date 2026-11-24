package prompt

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 「跨项目拆分」的提示词与 JSON 契约。
//
// 拆分是「把一份跨仓库的整体方案切成若干个**各归各的项目**的子需求」。
// 与同项目拆分（split.go）同构，但每条候选多一个 `project` 字段 —— 它是
// **建议值**，服务端在落库前会用参与项目集合校验（D7：AI 幻觉出的项目名必须
// 被服务端拒绝）。
//
// 本文件是纯函数：handler 拥有所有 I/O。

// CrossSplitJSONContract — 跨项目拆分产物的 JSON 契约。
const CrossSplitJSONContract = `你正在把一份**跨多个仓库**的整体技术方案拆分成若干个可独立交付的子需求。

## 输出格式

先写一段人类可读的拆分说明（每个子需求为什么归到这个项目、边界在哪），然后
在回复的**最后**放一个 json 围栏块，结构如下：

` + "```json" + `
{
  "tasks": [
    {
      "title": "子需求标题，≤ 40 字",
      "description": "自包含正文：本子需求要达成什么、怎么做、产出什么形态",
      "scope": ["backend/internal/handler/example.go"],
      "acceptance": ["可核对的验收点 1", "可核对的验收点 2"],
      "boundary": "与兄弟子需求的分工边界 + 双方约定的接口契约",
      "project": "承接这个子需求的项目名",
      "order": 1,
      "depends_on": []
    }
  ]
}
` + "```" + `

## 硬性要求

1. **数量**：tasks 数量在 2 到 12 之间。
2. **project 必须取自「涉及项目」清单里的项目名**（原样照抄，不要改写、不要
   简写、不要发明新名字）。服务器会校验，非法会整批拒绝。
3. **description 必须自包含**：子需求的执行 Agent 看不到整体方案全文，只能看到
   自己的 description 和系统注入的跨项目上下文块。跨仓库协作的接口契约**必须**
   写进 description 或 boundary。
4. **禁止指代**：不要写「见整体方案」「如上所述」。
5. **scope 尽量不重叠**：不同项目的改动范围天然不交叉；同一项目内的多个子需求
   也要尽量分开。
6. **acceptance 必须可核对**：写能对着代码 / 接口验证的验收点。
7. **depends_on 只能引用同一批次内 order 更小的任务序号**（1 起算），且只能向
   后指，不得成环。跨项目的依赖尤其要如实标出：「B 项目的对接依赖 A 项目的
   接口定义」——服务器会把它变成一条**前置闸门**：A 没做完，B 不许开工。
8. **JSON 块必须是整个回复的最后一段**。`

const crossSplitContractTail = "\n\n现在请输出拆分说明，并在最后给出符合上述结构的 json 块。"

// CrossSplitCandidate — 一条跨项目子需求候选（已归一化）。
type CrossSplitCandidate struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Scope       []string `json:"scope"`
	Acceptance  []string `json:"acceptance"`
	Boundary    string   `json:"boundary"`
	// Project — 模型写下的项目名 / id（原样保留，用于在 UI 上回显模型的建议）。
	Project string `json:"project"`
	// ProjectID — 解析到参与项目集合后的**项目 id**。空 = 模型写了个不认识
	// 的项目名 —— 服务端在落库前会拒绝这一批。
	ProjectID string `json:"project_id"`
	// Order — 展示顺序（1 起算，连续）。
	Order int `json:"order"`
	// DependsOn — 前置依赖（批次内序号，1 起算）。
	DependsOn []int `json:"depends_on"`
}

// crossSplitMaxTasks — 单次拆分最多保留多少条候选。
const crossSplitMaxTasks = 12

// crossSplitTitleMaxRunes — 子需求标题的 rune 上限。
const crossSplitTitleMaxRunes = 40

// BuildCrossSplitPrompt renders the cross-project split prompt.
//
// Inputs: the cross requirement, the overall design document (the material
// being split), the participating projects (the closed set the model must
// pick from), the participating projects' CLAUDE.md conventions and the
// attachment manifest (empty when the cross requirement has no attachments).
//
// The conventions go BEFORE the output contract: the model reads the
// contract last, and it must read the conventions before it starts
// splitting — a sub-requirement scoped by the wrong repository's rules is
// worse than no sub-requirement at all.
func BuildCrossSplitPrompt(title string, designDocs string, projects []CrossProjectInput, conventions []CrossProjectDoc, attachManifest string) string {
	var b strings.Builder

	b.WriteString("请把下面这份**跨项目整体方案**拆分成若干个可独立交付、各归各的项目、可独立提 PR 的子需求。\n\n")

	b.WriteString("## 跨项目需求\n")
	if t := strings.TrimSpace(title); t != "" {
		b.WriteString("- 标题：" + t + "\n")
	}
	b.WriteString("\n")

	b.WriteString("## 涉及项目（project 字段只能从这里选）\n")
	for _, p := range projects {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = strings.TrimSpace(p.ID)
		}
		b.WriteString("- " + name)
		if role := strings.TrimSpace(p.Role); role != "" {
			b.WriteString("（角色：" + role + "）")
		}
		if base := strings.TrimSpace(p.BaseBranch); base != "" {
			b.WriteString("（基础分支：" + base + "）")
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")

	b.WriteString("## 整体方案\n\n")
	if docs := strings.TrimSpace(designDocs); docs != "" {
		b.WriteString(truncateRunes(docs, 24*1024))
		b.WriteString("\n\n")
	} else {
		b.WriteString("（暂无整体方案正文 —— 请根据需求描述拆分。）\n\n")
	}

	// 各参与仓库的项目级 CLAUDE.md —— 子需求必须按目标仓库的约定划分。
	if section := BuildCrossProjectConventionSection(conventions); section != "" {
		b.WriteString(section)
		b.WriteString("\n\n")
	}

	// 用户上传的附件 —— 拆分时要按附件里给出的信息划分边界。
	appendCrossAttachment(&b, attachManifest)

	b.WriteString(CrossSplitJSONContract)
	b.WriteString(crossSplitContractTail)
	return b.String()
}

// NormalizeCrossSplitCandidates parses and normalizes the model's raw split
// output into the candidate list.
//
// Rules (mirrors NormalizeSplitTasks):
//   - a task without a title is dropped
//   - the title is truncated
//   - a missing / non-positive order is re-derived from the array position
//   - depends_on keeps only ordinals >= 1 and strictly smaller than the task's
//     own order (a forward or self reference is a cycle)
//   - an empty acceptance list gets a placeholder
//   - the ordinals are renumbered to a contiguous 1..N space
//   - the project reference is resolved against the participating set
//     (by id first, then by case-insensitive name); an unknown reference
//     leaves ProjectID empty — the service rejects the whole batch at confirm
//     time instead of guessing.
func NormalizeCrossSplitCandidates(raw string, projects []CrossProjectInput) []CrossSplitCandidate {
	out := []CrossSplitCandidate{}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return out
	}

	payload := trimmed
	if m := splitFenceRe.FindStringSubmatch(trimmed); m != nil {
		payload = strings.TrimSpace(m[1])
	}

	var envelope struct {
		Tasks []struct {
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Scope       []string `json:"scope"`
			Acceptance  []string `json:"acceptance"`
			Boundary    string   `json:"boundary"`
			Project     string   `json:"project"`
			Order       int      `json:"order"`
			DependsOn   []int    `json:"depends_on"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return out
	}
	if len(envelope.Tasks) == 0 {
		return out
	}

	for i := range envelope.Tasks {
		t := envelope.Tasks[i]
		title := truncateRunes(strings.TrimSpace(t.Title), crossSplitTitleMaxRunes)
		if title == "" {
			continue
		}
		if t.Order <= 0 {
			t.Order = i + 1
		}
		acceptance := cleanStringList(t.Acceptance)
		if len(acceptance) == 0 {
			acceptance = []string{"（待补充验收标准）"}
		}
		cand := CrossSplitCandidate{
			Title:       title,
			Description: strings.TrimSpace(t.Description),
			Scope:       cleanStringList(t.Scope),
			Acceptance:  acceptance,
			Boundary:    strings.TrimSpace(t.Boundary),
			Project:     strings.TrimSpace(t.Project),
			Order:       t.Order,
			DependsOn:   normalizeDependsOn(t.DependsOn, t.Order),
		}
		cand.ProjectID = ResolveCrossProjectID(cand.Project, projects)
		out = append(out, cand)
	}

	// Stable sort by order.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Order < out[j-1].Order; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if len(out) > crossSplitMaxTasks {
		out = out[:crossSplitMaxTasks]
	}

	// Renumber to a contiguous 1..N and translate depends_on along.
	//
	// The ordinals must live in ONE coordinate space on the whole chain:
	// model output → candidate table → dependency edges → persisted rows.
	// A non-contiguous `order` (1, 3, 7) would make "依赖 3" point at a
	// different row depending on who reads it.
	remap := renumberSplitTasks(crossCandidatesAsSplitTasks(out))
	for i := range out {
		out[i].Order = i + 1
		out[i].DependsOn = remapOrdinals(out[i].DependsOn, remap)
	}
	return out
}

// crossCandidatesAsSplitTasks adapts the candidates onto the shared
// SplitTask shape so the renumbering helper can be reused verbatim.
func crossCandidatesAsSplitTasks(cands []CrossSplitCandidate) []SplitTask {
	out := make([]SplitTask, len(cands))
	for i, c := range cands {
		out[i] = SplitTask{Title: c.Title, Order: c.Order}
	}
	return out
}

// ResolveCrossProjectID resolves a model-written project reference against the
// participating project set.
//
// By id first (exact), then by name (case-insensitive, trimmed). An empty
// result means the model invented a project — the caller rejects the batch.
func ResolveCrossProjectID(ref string, projects []CrossProjectInput) string {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return ""
	}
	for _, p := range projects {
		if p.ID != "" && p.ID == trimmed {
			return p.ID
		}
	}
	lower := strings.ToLower(trimmed)
	for _, p := range projects {
		if p.Name != "" && strings.ToLower(strings.TrimSpace(p.Name)) == lower {
			return p.ID
		}
	}
	for _, p := range projects {
		if p.Name != "" && strings.Contains(strings.ToLower(p.Name), lower) {
			return p.ID
		}
	}
	return ""
}

// CrossSplitSummary renders the one-line job summary of a split run.
func CrossSplitSummary(cands []CrossSplitCandidate) string {
	return fmt.Sprintf("已生成 %d 个候选子需求", len(cands))
}
