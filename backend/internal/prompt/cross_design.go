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

## 待决策事项
**本节是软性要求：确实没有需要人来拍板的事时写「（无）」即可；但只要方案里有
需要人决定的取舍，就必须逐条列在这里 —— 这几项会由用户逐条拍板，拍板结果会
回灌进下一版方案。**
- 决策点：一句话说清要定什么（如「跨仓库接口的默认超时时间」）。
- 可选项：每个选项一句话说明它的代价 / 影响，不要只写选项名。
- 建议：你的推荐 + 一句理由。
- 不决策的后果：这条不定会卡住谁、会导致什么。

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
6. 正文用中文。
7. **需要人拍板的取舍写进「## 待决策事项」**，不要在正文里替用户做决定；该节
   没有内容时写「（无）」，不要省略这一节。`

// crossDesignTail — 收尾指令，重复「章节齐全」的要求（模型更看重最后一句）。
const crossDesignTail = "\n\n现在请输出整体方案，务必包含「## 跨项目接口契约」章节；" +
	"若有需要人来拍板的事项，写进「## 待决策事项」章节（没有就写「（无）」）。"

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
	// BaseSHA — 本次方案锁定的基础提交（40 hex）。
	//
	// 写进提示词是为了让模型知道它读的到底是哪个提交：Path 指向的是一份
	// detached worktree（见 handler/cross_requirement_workspace.go），它停在
	// 这个提交上。方案里的「现状是这样的」因此有一个可复核的锚点，而不是
	// 「大概是现在仓库的样子」。
	BaseSHA string
}

// BuildCrossDesignPrompt renders the overall-design prompt.
//
// Inputs: the cross requirement itself (title / description / acceptance),
// the participating projects, the overall design document produced by a
// previous run (empty on the first run — the "regenerate" path feeds it back
// so the model can refine instead of starting from scratch), the
// participating projects' CLAUDE.md conventions and the attachment manifest
// (empty when the cross requirement has no attachments), and the adjustment
// discussion (empty when the user never talked the design through).
//
// The conventions go BEFORE the output contract: the model reads the
// contract last, and it must read the conventions before it starts
// designing.
//
// discussion is the 「方案调整对话」 thread: the questions the user asked
// about the pending decisions and the answers that settled them. It rides
// along with previousDesign — the previous design says "what it said last
// time", the discussion says "what we agreed since". An empty discussion
// leaves the prompt byte-identical to the one from before the feature
// existed.
func BuildCrossDesignPrompt(title, description string, acceptance []string, projects []CrossProjectInput, previousDesign string, conventions []CrossProjectDoc, attachManifest string, discussion []CrossAdjustTurn) string {
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
		if sha := strings.TrimSpace(p.BaseSHA); sha != "" {
			if len(sha) > 7 {
				sha = sha[:7]
			}
			b.WriteString("（基线提交：" + sha + "）")
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

	// 调整讨论记录 —— 用户与 AI 谈拢的结论。空线程不写一个字节。
	if block := BuildCrossDiscussionBlock(discussion); block != "" {
		b.WriteString(block)
		b.WriteString("\n")
	}

	// 各参与仓库的项目级 CLAUDE.md —— 整体方案必须逐仓库对齐各自的约定。
	if section := BuildCrossProjectConventionSection(conventions); section != "" {
		b.WriteString(section)
		b.WriteString("\n\n")
	}

	// 用户上传的附件 —— 整体方案必须看得见它们（截图 / 日志 / 契约文档）。
	appendCrossAttachment(&b, attachManifest)

	b.WriteString(CrossDesignContract)
	b.WriteString(crossDesignTail)
	return b.String()
}

// CrossDesignSummary renders the one-line job summary of a design run.
func CrossDesignSummary(projectCount int) string {
	return fmt.Sprintf("已生成整体方案（涉及 %d 个项目）", projectCount)
}
