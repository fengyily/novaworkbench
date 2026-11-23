package prompt

import (
	"fmt"
	"strings"
)

// 「阶段验证」的提示词。
//
// 阶段 = 一次跨项目的联调 / 集成测试单元。验证作业**不跑测试**（MVP 不引入各
// 项目自定义命令编排），而是让 AI 读各仓库的当前实现，产出一份**集成验证
// 报告**，由人工判定通过 / 不通过。
//
// 所以报告的结构固定为「验证项 / 方法 / 结论 / 遗留风险」四段 —— 人工判定的
// 依据必须落在可核对的条目上。
//
// 本文件是纯函数：handler 拥有所有 I/O。

// CrossPhaseContract — 阶段验证报告的结构契约。
const CrossPhaseContract = `你在为一个**跨项目**需求做阶段验证（联调 / 集成测试）。

## 输出格式

直接输出 Markdown 报告，不要包在代码围栏里。必须包含以下章节（顺序固定）：

## 验证项
逐条列出本次要验证的内容，每条包含：
- 验证项名称
- 涉及的项目
- 判定标准（可核对的：某个接口返回什么、某个字段是什么、某个状态怎么流转）

## 验证方法
每个验证项怎么验证：读哪个仓库的哪个文件 / 调哪个接口 / 走哪条链路。写清楚
你**实际**依据了什么，不要写「已确认」这种无法复核的结论。

## 结论
逐条给出每个验证项的结论：**通过 / 不通过 / 无法验证**。不通过必须写明
**具体现象**（期望什么、实际什么、在哪个项目）。

## 遗留风险
本次验证没有覆盖到的点，以及后续阶段需要注意的事项。

## 硬性要求

1. **不要编造**。你没有实际运行测试时，结论写「无法验证」并说明原因 ——
   编造的「通过」比「无法验证」有害得多。
2. **结论必须落到具体项目与具体现象**。「接口基本正常」不合格；
   「B 项目的 GET /api/v1/orders/{id} 缺少 amount 字段，返回体只有
   {id, status}，与 A 项目的契约不符」才合格。
3. **不要给修复代码**。你的任务是验证与报告，返工由各项目自己的开发流程处理。
4. 正文用中文。`

const crossPhaseContractTail = "\n\n现在请输出阶段验证报告。"

// CrossPhaseMember — 阶段成员在提示词里的最小描述。
type CrossPhaseMember struct {
	// RequirementID — 子需求 id。
	RequirementID string
	// Title — 子需求标题。
	Title string
	// ProjectName — 所属项目名。
	ProjectName string
	// ProjectPath — 所属项目在磁盘上的路径。
	ProjectPath string
	// Status — 子需求当前状态。
	Status string
	// Description — 子需求描述（自包含，含跨项目契约）。
	Description string
}

// BuildCrossPhasePrompt renders the phase-verification prompt.
//
// Inputs: the phase (name / description), its member sub-requirements and
// the overall design (so the contract the report must check against is in
// front of the model).
func BuildCrossPhasePrompt(phaseName, phaseDescription string, members []CrossPhaseMember, designMarkdown string) string {
	var b strings.Builder

	b.WriteString("请对下面这个阶段做跨项目验证。\n\n")

	b.WriteString("## 阶段\n")
	if t := strings.TrimSpace(phaseName); t != "" {
		b.WriteString("- 名称：" + t + "\n")
	}
	if d := strings.TrimSpace(phaseDescription); d != "" {
		b.WriteString("- 说明：" + truncateRunes(d, 2*1024) + "\n")
	}
	b.WriteString("\n")

	b.WriteString("## 本阶段的交付物\n")
	if len(members) == 0 {
		b.WriteString("（本阶段没有登记成员）\n")
	}
	for _, m := range members {
		title := strings.TrimSpace(m.Title)
		if title == "" {
			title = "(无标题)"
		}
		b.WriteString("- " + title)
		if p := strings.TrimSpace(m.ProjectName); p != "" {
			b.WriteString("（项目：" + p + "）")
		}
		b.WriteString("（状态：" + strings.TrimSpace(m.Status) + "）\n")
		if desc := strings.TrimSpace(m.Description); desc != "" {
			b.WriteString("  - " + truncateRunes(desc, 2*1024) + "\n")
		}
	}
	b.WriteString("\n")

	if design := strings.TrimSpace(designMarkdown); design != "" {
		b.WriteString("## 整体方案（含跨项目接口契约 —— 验证的依据）\n\n")
		b.WriteString(truncateRunes(design, 16*1024))
		b.WriteString("\n\n")
	}

	b.WriteString(CrossPhaseContract)
	b.WriteString(crossPhaseContractTail)
	return b.String()
}

// CrossPhaseSummary renders the one-line job summary of a verification run.
func CrossPhaseSummary(memberCount int) string {
	return fmt.Sprintf("已生成阶段验证报告（覆盖 %d 个交付物）", memberCount)
}
