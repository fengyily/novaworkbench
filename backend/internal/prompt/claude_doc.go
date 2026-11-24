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
// This is the PROMPT-MODE rendering: the document is spliced into the user
// prompt. The default path does NOT use it — the managed copy travels
// through the Claude CLI's own project-memory channel (see
// handler/claude_doc_injection.go), which frames it as authoritative
// project instructions.
//
// diskExists says whether the working directory the model will see already
// contains a CLAUDE.md. It only matters in prompt mode, where the CLI may
// still load the repository's own copy alongside this block — the block then
// says which one wins.
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
// Three things matter here:
//  1. the run is READ-ONLY — the tool layer already blocks Write/Edit/Bash,
//     and the prompt repeats it so the model does not spend turns trying;
//  2. the output is the document body and nothing else — no fence, no
//     "here is your CLAUDE.md" preamble — because the backend stores the
//     text verbatim;
//  3. when the repository already ships a CLAUDE.md, the model is told
//     that the CLI auto-loaded it and that it must NOT be copied.
//
// DiskDocExists must describe the directory the model will actually see
// (the generation root), not the user's checkout — that is where the CLI
// picks the file up from.
//
// InSnapshot tells the model that the generation root is a THROWAWAY
// snapshot of the base branch (a linked worktree) rather than the user's
// own checkout. Without that instruction the model faithfully documents
// what it sees — including the scratch directory path and the `.git`
// indirection a linked worktree carries — and those details then live
// forever in a document that is injected into every later AI stage.
func BuildClaudeDocGenerationPrompt(projectName, focus string, diskDocExists, inSnapshot bool) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("请为项目「%s」生成一份 CLAUDE.md（项目级 AI 开发指南）。\n\n", strings.TrimSpace(projectName)))
	b.WriteString("## 探索要求\n")
	b.WriteString("1. 只读探索本仓库：使用 Read / Glob / Grep 了解目录结构、构建方式、分层约定与技术选型。\n")
	b.WriteString("2. 禁止创建或修改任何文件；禁止执行会改变仓库状态的命令。\n")
	b.WriteString("3. 命令必须来自仓库中真实存在的配置（如 Makefile、package.json、go.mod、CI 配置），不得编造。\n")
	if f := strings.TrimSpace(focus); f != "" {
		b.WriteString(fmt.Sprintf("4. 用户额外关注点：%s\n", f))
	}
	if diskDocExists {
		b.WriteString(claudeDocExistingDocRule)
	}
	if inSnapshot {
		b.WriteString(claudeDocSnapshotRule)
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

// claudeDocExistingDocRule is the paragraph that keeps the generation from
// degenerating into a copy of the document that is already on disk.
//
// The Claude CLI auto-loads `<repo>/CLAUDE.md` into the session as project
// memory — it arrives as INSTRUCTIONS, not as a file the model chose to
// read. Without this rule the model has no reason to prefer the code over
// that text, and the obvious (cheap) way to satisfy "write a CLAUDE.md" is
// to paraphrase what it was just handed: a document that looks freshly
// derived but is really a copy of a possibly-stale one. That defeats the
// point of the feature — the point is to describe THIS code base.
//
// So the rule says three things: the file is a prior version and not a
// source of truth, every statement must be re-derived from the code, and
// the code wins on any conflict.
const claudeDocExistingDocRule = "" +
	"\n## 既有 CLAUDE.md 的处理（重要）\n" +
	"本次运行的 Claude CLI 会自动把仓库根目录的 `CLAUDE.md` 读进上下文（你可能已经看到它）。\n" +
	"它只是**此前的旧版本**，可能已经过时，也可能与当前代码不符：\n" +
	"- 不要照抄它，也不要把它当作事实来源；\n" +
	"- 只依据你在代码里核实到的事实（源文件、构建配置、CI 配置）来写这份文档；\n" +
	"- 旧文档里的每一条命令、路径、分层约定都必须在仓库中找到依据，找不到就不要写；\n" +
	"- 旧文档与代码冲突时以代码为准，并按代码的实际形态描述；\n" +
	"- 旧文档中经核实仍然成立的约定可以保留，但要按代码的实际形态重写，不要复述原文。\n"

// claudeDocSnapshotRule keeps the snapshot's own mechanics out of the
// document.
//
// The generation runs in a throwaway worktree pinned to the freshly synced
// base-branch commit — that is what makes "generate from the latest main"
// possible without touching the user's working tree. But a linked worktree
// is NOT shaped like a normal checkout: its `.git` is a FILE that points at
// the real repository's worktree metadata, and it sits in a temporary
// directory whose name is random.
//
// The model reads what it sees and writes it down. Left alone it produces a
// document that says things like "the working copy at this path is a linked
// worktree (.git is a file containing gitdir: /tmp/nova-claude-doc-XXXX)".
// That text is: (a) a statement about the scratch checkout rather than about
// the project, (b) wrong for the user's own checkout, and (c) carrying a
// machine-specific path into an asset that is injected into every later AI
// stage and survives every future regeneration.
//
// So the rule names the snapshot for what it is and bans describing it: the
// document describes the PROJECT, as a normal checkout of the base branch
// would look.
const claudeDocSnapshotRule = "" +
	"\n## 当前工作目录是临时快照（重要）\n" +
	"你当前浏览的目录是该项目主分支某个提交的**临时只读快照**（linked worktree），不是用户日常使用的那份检出：\n" +
	"- 这里的 `.git` 是一个指向真实仓库的**文件**而不是目录，这是快照机制造成的，不代表项目本身如此；\n" +
	"- 不要把快照写进文档：不要提临时目录路径、`.git` 指向的 gitdir 路径、worktree / 快照 / 临时检出等字样；\n" +
	"- 文档要描述的是**项目本身**（主分支上的代码与配置）。凡是涉及仓库形态的表述，按普通检出的形态来写（例如「仓库根目录」「`.git` 目录」），不要写「当前工作目录是 linked worktree」这类与项目无关的实现细节；\n" +
	"- 只有当源码或配置中确实能核实到相关事实时（例如远程仓库地址、分支命名约定），才写与 Git 相关的项目约定。\n"
