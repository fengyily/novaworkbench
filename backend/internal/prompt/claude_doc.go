package prompt

import (
	"fmt"
	"strings"
)

// ProjectClaudeDocHeading is the section title of the injected project-level
// CLAUDE.md block. The exact string is asserted by tests and referenced by
// the UI, so it must stay stable.
const ProjectClaudeDocHeading = "## 项目级约定（CLAUDE.md · NovaWorkbench 维护）"

// projectClaudeDocMaxBytes caps the injected document. A CLAUDE.md is
// usually a few KB, but a machine-generated one can run long; the prompt
// budget matters more than completeness, so we keep the head (where the
// conventions and the commands live) and drop the tail with an explicit
// marker instead of silently truncating.
const projectClaudeDocMaxBytes = 32 * 1024

// projectClaudeDocTruncMarker tells the model that what it sees is partial —
// otherwise a truncated list of conventions reads as "these are ALL the
// rules", and the model would confidently follow a half-document.
const projectClaudeDocTruncMarker = "\n\n…（内容过长已截断，完整内容见项目详情页）\n"

// BuildProjectClaudeDocBlock renders the project-level CLAUDE.md as a
// self-contained prompt section.
//
// diskExists says whether the working directory the model will see already
// contains a CLAUDE.md. When it does, the Claude CLI auto-loads that file,
// so the model would otherwise see two documents and have to guess which one
// wins — the block states explicitly that this one is authoritative.
//
// An empty content returns an empty string: the caller then appends
// nothing and the prompt stays byte-for-byte identical to the pre-feature
// behavior.
func BuildProjectClaudeDocBlock(content string, diskExists bool) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(ProjectClaudeDocHeading)
	b.WriteString("\n\n")
	b.WriteString("以下是本项目的权威开发约定，由 NovaWorkbench 统一维护（不落在仓库文件系统中）。\n")
	b.WriteString("方案设计、开发实现、子任务执行、代码审查都必须遵循这些约定。\n")
	if diskExists {
		b.WriteString("（仓库目录中另有一份 CLAUDE.md；若两者冲突，以本块为准。）\n")
	}
	b.WriteString("\n")
	b.WriteString(truncateClaudeDoc(trimmed))
	b.WriteString("\n")
	return b.String()
}

// truncateClaudeDoc keeps the head of an over-long document.
//
// The cut is done on RUNE boundaries, not byte boundaries: a byte cut can
// land in the middle of a multi-byte Chinese character and leave a broken
// glyph in the prompt (visible as a replacement character to the model).
func truncateClaudeDoc(content string) string {
	if len(content) <= projectClaudeDocMaxBytes {
		return content
	}
	runes := []rune(content)
	// Walk forward until the byte length of the kept prefix would exceed the
	// budget. len(string(runes[:n])) is the byte length.
	keep := 0
	for keep < len(runes) {
		if len(string(runes[:keep+1])) > projectClaudeDocMaxBytes {
			break
		}
		keep++
	}
	return string(runes[:keep]) + projectClaudeDocTruncMarker
}

// BuildClaudeDocGenerationPrompt is the contract for the "生成 CLAUDE.md"
// run: read the repository, then emit the document itself.
//
// Two things matter here:
//  1. the run is READ-ONLY — the tool layer already blocks Write/Edit/Bash,
//     and the prompt repeats it so the model does not spend turns trying;
//  2. the output is the document body and nothing else — no fence, no
//     "here is your CLAUDE.md" preamble — because the backend stores the
//     text verbatim.
func BuildClaudeDocGenerationPrompt(projectName, focus string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("请为项目「%s」生成一份 CLAUDE.md（项目级 AI 开发指南）。\n\n", strings.TrimSpace(projectName)))
	b.WriteString("## 探索要求\n")
	b.WriteString("1. 只读探索本仓库：使用 Read / Glob / Grep 了解目录结构、构建方式、分层约定与技术选型。\n")
	b.WriteString("2. 禁止创建或修改任何文件；禁止执行会改变仓库状态的命令。\n")
	b.WriteString("3. 命令必须来自仓库中真实存在的配置（如 Makefile、package.json、go.mod、CI 配置），不得编造。\n")
	if f := strings.TrimSpace(focus); f != "" {
		b.WriteString(fmt.Sprintf("4. 用户额外关注点：%s\n", f))
	}
	b.WriteString("\n## 文档结构\n")
	b.WriteString("请按以下小节组织（缺少依据的小节可以省略，不要凭空编造）：\n")
	b.WriteString("- `## Project Overview`：项目用途与整体结构\n")
	b.WriteString("- `## Tech Stack`：语言、框架、关键依赖\n")
	b.WriteString("- `## Architecture`：分层与模块职责\n")
	b.WriteString("- `## Commands`：构建 / 测试 / 运行的真实命令\n")
	b.WriteString("- `## Conventions`：代码风格与本仓库必须遵守的约定\n")
	b.WriteString("- `## Important Files`：关键入口文件\n")
	b.WriteString("- `## Development Workflow`：一次改动从开发到验证的流程\n")
	b.WriteString("- `## Testing`：测试方式与位置\n")
	b.WriteString("\n## 输出要求\n")
	b.WriteString("只输出 CLAUDE.md 的 Markdown 正文本身，不要使用 ``` 代码围栏包裹，不要写任何开场白或结束语。\n")
	return b.String()
}
