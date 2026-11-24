package prompt

import "strings"

// 「跨项目需求」的项目级 CLAUDE.md 约定块。
//
// 跨项目作业横跨 N 个仓库，所以约定必须**按仓库分开**并标注归属：模型要能
// 判断哪份约定约束哪一部分设计（整体方案、按项目拆分、阶段验证都按仓库分工）。
//
// 约定正文有两个来源，优先级与全仓库一致：
//   - NovaWorkbench 维护的受管副本（权威来源）；
//   - 仓库目录里自带的 CLAUDE.md —— 仅当项目没有受管副本时使用（单仓库阶段
//     在这种情况下由 CLI 从 checkout 直接加载，跨项目阶段工作目录是临时目录，
//     CLI 加载不到，只能由我们带过去）。
//
// 受管副本与仓库自带副本同时存在时以受管副本为准：受管副本由 NovaWorkbench
// 统一维护，仓库自带的那份可能已经过时。

// crossClaudeDocMaxBytes — 单个仓库约定的字节上限。
//
// 比单仓库阶段的 32KB 更紧：跨项目作业要把 N 个仓库的约定一起放进**同一份**
// 提示词，上限若照搬，提示词会随参与仓库数线性膨胀。单仓库阶段的头部（约定
// 与命令所在处）才是模型真正要遵守的部分。
const crossClaudeDocMaxBytes = 8 * 1024

// crossClaudeDocTruncMarker — 截断标记。
//
// 与单仓库阶段的标记不同：这里被截断的可能是仓库自带的副本（完整内容在仓库
// 目录里，模型自己就能读到），不一定是项目详情页上的受管副本。
const crossClaudeDocTruncMarker = "\n\n…（内容过长已截断）\n"

// CrossProjectDoc — 跨项目作业里一个参与仓库的项目级 CLAUDE.md。
type CrossProjectDoc struct {
	// ProjectName — 项目显示名（约定块的归属标签）。
	ProjectName string
	// Content — 约定正文（Markdown）。
	Content string
	// Managed — 正文来自 NovaWorkbench 维护的受管副本。
	Managed bool
	// DiskExists — 该仓库目录中另有一份 CLAUDE.md（受管副本优先时用于消歧）。
	DiskExists bool
}

// BuildCrossProjectConventionSection renders the 「## 项目约定」 section for a
// cross-project run.
//
// 返回空串表示没有参与仓库带约定 —— 调用方据此让提示词逐字节保持原样。
//
// 没有正文的仓库不出一节：一个空标题会让模型以为漏了一份约定。
func BuildCrossProjectConventionSection(docs []CrossProjectDoc) string {
	blocks := make([]string, 0, len(docs))
	for _, d := range docs {
		content := strings.TrimSpace(d.Content)
		if content == "" {
			continue
		}
		name := strings.TrimSpace(d.ProjectName)
		if name == "" {
			name = "（未命名项目）"
		}
		var b strings.Builder
		b.WriteString("### " + name + "\n\n")
		if d.Managed {
			b.WriteString("以下是「" + name + "」项目的权威开发约定，由 NovaWorkbench 统一维护（不落在仓库文件系统中）。\n")
			b.WriteString("整体方案、按项目拆分与阶段验证都必须遵循这些约定。\n")
			if d.DiskExists {
				b.WriteString("（该项目仓库目录中另有一份 CLAUDE.md；若两者冲突，以本块为准。）\n")
			}
		} else {
			b.WriteString("以下是「" + name + "」项目仓库目录里的 CLAUDE.md（该项目没有 NovaWorkbench 维护的受管副本）。\n")
			b.WriteString("整体方案、按项目拆分与阶段验证都必须遵循这些约定。\n")
		}
		b.WriteString("\n")
		b.WriteString(truncateCrossClaudeDoc(content))
		blocks = append(blocks, b.String())
	}
	if len(blocks) == 0 {
		return ""
	}
	return "## 项目约定\n\n" + strings.Join(blocks, "\n")
}

// truncateCrossClaudeDoc keeps the head of an over-long convention document.
//
// The cut is on RUNE boundaries, not byte boundaries: a byte cut can land in
// the middle of a multi-byte Chinese character and leave a broken glyph in the
// prompt (visible to the model as a replacement character).
func truncateCrossClaudeDoc(content string) string {
	if len(content) <= crossClaudeDocMaxBytes {
		return content
	}
	runes := []rune(content)
	keep := 0
	for keep < len(runes) {
		if len(string(runes[:keep+1])) > crossClaudeDocMaxBytes {
			break
		}
		keep++
	}
	return string(runes[:keep]) + crossClaudeDocTruncMarker
}
