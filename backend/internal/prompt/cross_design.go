package prompt

import (
	"fmt"
	"strings"
)

// 「跨项目整体方案」的提示词与 Markdown 章节契约。
//
// 整体方案是**一份**文档（不是文档数组），它回答的是「这几个仓库合起来要
// 做什么、各自负责什么、接口长什么样、怎么联调」—— 正是单仓库方案回答不了
// 的那一层。
//
// 章节契约是**硬性**的：`## 跨项目接口契约` 缺失时服务端拒绝保存（见
// HasCrossContractSection），因为缺了它，各仓库会各自设计、各自实现，联调时
// 才暴露不一致。
//
// 本文件是纯函数（对齐 split.go 的风格）：handler 拥有所有 I/O。

// CrossDesignContract — 整体方案的 Markdown 章节契约。
const CrossDesignContract = `你正在为一个**横跨多个仓库**的需求编写整体技术方案。

## 输出格式

直接输出 Markdown 正文，不要包在代码围栏里。必须包含以下章节（标题用
二级标题，顺序固定）：

## 需求目标
一句话说明这个需求最终要交付什么能力，以及它跨了哪几个仓库。

## 涉及项目
逐个列出参与项目，标明它在整体中的角色（前端 / 后端 / 移动端 / 数据层 …）
以及它承接的范围。

## 跨项目接口契约
**本节是硬性要求，不可省略。** 逐条写出跨仓库的接口契约：
- 接口的调用方与提供方（哪个项目提供、哪个项目调用）
- 路径 / 方法 / 请求与响应字段（能落到字段级别的程度）
- 错误码与边界情况
- 数据结构 / 事件格式（若涉及）

每一项都用表格或列表给出，**字段名必须写死**——各仓库的 Agent 只会看到这一份
契约，写不清楚就会各自发挥，联调时才发现对不上。

## 各项目落地清单
按项目分节，每个项目写：承接哪些子任务、改动范围（路径 / 模块）、对本项目
的验收标准。

## 集成与验证计划
写清楚联调 / 集成测试的**阶段**划分：每个阶段包含哪些交付物、怎么验证
（可核对的步骤）、验证失败时回退到哪个项目。

## 硬性要求

1. **契约必须是字段级**。写「订单服务提供查询接口」没有约束力；写
   「GET /api/v1/orders/{id} 返回 {id, status, amount, createdAt}，status
   枚举为 created/paid/shipped/completed」才有。
2. **不要假设不存在的接口**。整体方案里没提到的能力，要么写进契约，要么
   明确标注「依赖某项目提供」。
3. **落地清单里的范围要具体到路径**，避免各项目改动同一片代码导致合并冲突。
4. **验收标准必须可核对**：能对着代码或接口验证的句子，不要「功能正常」。
5. **不要写实现细节**（函数怎么写、类怎么设计）——那是各项目自己的方案阶段
   的事。这里只定边界与契约。
6. 正文用中文。`

// crossDesignTail — 收尾指令，重复「章节齐全」的要求（模型更看重最后一句）。
const crossDesignTail = "\n\n现在请输出整体方案，务必包含「## 跨项目接口契约」章节。"

// CrossProjectInput — 参与项目在提示词里的最小描述。
type CrossProjectInput struct {
	// ID — 项目 id。
	ID string
	// Name — 项目显示名。
	Name string
	// Role — 项目在整体中的角色。
	Role string
	// Path — 项目在磁盘上的路径（模型可以读它来落地契约）。
	Path string
	// Summary — 项目的简要说明（来自项目描述）。
	Summary string
	// BaseBranch — 该参与项目的基础分支（子需求从这条分支开发）。
	//
	// 空 = 该项目沿用自己的默认分支。写进提示词是为了让模型知道各仓库
	// 处在不同的发布线上 —— 方案里的版本 / 兼容性结论必须按各自的线来写。
	BaseBranch string
}

// BuildCrossDesignPrompt renders the overall-design prompt.
//
// Inputs: the cross requirement itself (title / description / acceptance),
// the participating projects, the overall design document produced by a
// previous run (empty on the first run — the "regenerate" path feeds it back
// so the model can refine instead of starting from scratch) and the
// participating projects' CLAUDE.md conventions.
//
// The conventions go BEFORE the output contract: the model reads the
// contract last, and it must read the conventions before it starts
// designing.
func BuildCrossDesignPrompt(title, description string, acceptance []string, projects []CrossProjectInput, previousDesign string, conventions []CrossProjectDoc) string {
	var b strings.Builder

	b.WriteString("请为下面这个**横跨多个仓库**的需求编写一份整体技术方案。\n\n")

	b.WriteString("## 需求\n")
	if t := strings.TrimSpace(title); t != "" {
		b.WriteString("- 标题：" + t + "\n")
	}
	if d := strings.TrimSpace(description); d != "" {
		b.WriteString("- 描述：" + truncateRunes(d, 6*1024) + "\n")
	}
	if len(acceptance) > 0 {
		b.WriteString("- 验收标准：\n")
		for _, a := range acceptance {
			if t := strings.TrimSpace(a); t != "" {
				b.WriteString("  - " + t + "\n")
			}
		}
	}
	b.WriteString("\n")

	b.WriteString("## 涉及项目\n")
	if len(projects) == 0 {
		b.WriteString("（未指定参与项目）\n")
	}
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
		if path := strings.TrimSpace(p.Path); path != "" {
			b.WriteString("（路径：" + path + "）")
		}
		b.WriteString("\n")
		if summary := strings.TrimSpace(p.Summary); summary != "" {
			b.WriteString("  - 说明：" + truncateRunes(summary, 1024) + "\n")
		}
	}
	b.WriteString("\n")

	if prev := strings.TrimSpace(previousDesign); prev != "" {
		b.WriteString("## 上一版整体方案\n\n")
		b.WriteString(truncateRunes(prev, 16*1024))
		b.WriteString("\n\n")
	}

	// 各参与仓库的项目级 CLAUDE.md —— 整体方案必须逐仓库对齐各自的约定。
	if section := BuildCrossProjectConventionSection(conventions); section != "" {
		b.WriteString(section)
		b.WriteString("\n\n")
	}

	b.WriteString(CrossDesignContract)
	b.WriteString(crossDesignTail)
	return b.String()
}

// CrossDesignSummary renders the one-line job summary of a design run.
func CrossDesignSummary(projectCount int) string {
	return fmt.Sprintf("已生成整体方案（涉及 %d 个项目）", projectCount)
}
