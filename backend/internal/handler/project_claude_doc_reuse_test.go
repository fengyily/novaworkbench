package handler

import "testing"

// TestClaudeDocReuseRatio pins the observable counterpart of the
// generation prompt's "do not copy the existing CLAUDE.md" rule.
//
// The prompt is a request; this measures what actually came out. Only the
// PROSE is compared — the commands and the paths are read off the
// repository's build configuration, so a correct document reproduces them
// verbatim and comparing them would flag every correct run.
func TestClaudeDocReuseRatio(t *testing.T) {
	existing := `# smoke-proj

## Project Overview

这是一个最小化的冒烟测试仓库，用于验证项目脚手架的最小可用形态。

## Architecture

后端按 handler / service / model 三层划分，SQL 统一收口在 db 包里。

## Conventions

所有对外接口都走同一条响应信封，错误码由服务端统一定义。

## Commands

- 构建：make build 会产出单个二进制到 dist 目录。
- 测试：make test 运行全部 Go 单元测试。
`

	// A freshly derived document: the same commands and paths (they come
	// from the Makefile), but every sentence re-derived from the code.
	derived := `# smoke-proj

## Project Overview

这是一个最小化的冒烟测试仓库，覆盖前后端联动的最小路径与启动脚本。

## Architecture

后端按接入层 / 领域层 / 持久层划分，所有 SQL 收口在存储适配器里。

## Conventions

所有对外接口共用同一套响应信封，错误码由服务端统一定义。

## Commands

- 构建：make build 会产出单个二进制到 dist 目录。
- 测试：make test 运行全部 Go 单元测试。
`
	if got := claudeDocReuseRatio(existing, derived); got != 0 {
		t.Errorf("reuse ratio for a reworded document = %v, want 0", got)
	}

	// A verbatim copy: every prose line reappears.
	if got := claudeDocReuseRatio(existing, existing); got != 1 {
		t.Errorf("reuse ratio for an identical document = %v, want 1", got)
	}

	// A partial copy: the prose was carried over while the layout was
	// rewritten. That is exactly the "looks derived, is copied" case the
	// warning is for.
	copied := `# smoke-proj

## Overview

这是一个最小化的冒烟测试仓库，用于验证项目脚手架的最小可用形态。

## Architecture

后端按 handler / service / model 三层划分，SQL 统一收口在 db 包里。

## Conventions

所有对外接口都走同一条响应信封，错误码由服务端统一定义。

## Commands

- 构建：make build 会产出单个二进制到 dist 目录。
`
	if got := claudeDocReuseRatio(existing, copied); got < claudeDocReuseWarnRatio {
		t.Errorf("reuse ratio for a partially copied document = %v, want >= %v", got, claudeDocReuseWarnRatio)
	}

	// Too little to judge: a document with a single prose sentence has
	// nothing to copy.
	short := "# t\n\n只有一句话在这里，没有别的内容了。\n"
	if got := claudeDocReuseRatio(short, short); got != 0 {
		t.Errorf("reuse ratio for a document with too few substantial lines = %v, want 0", got)
	}

	// An empty existing document → nothing to compare.
	if got := claudeDocReuseRatio("", derived); got != 0 {
		t.Errorf("reuse ratio with no existing document = %v, want 0", got)
	}
}

// TestClaudeDocSubstantialLines pins what counts as comparable prose:
// free-standing sentences only. Headings, list items and table rows carry
// the layout and the commands — they coincide in any correct document and
// would drown the signal.
func TestClaudeDocSubstantialLines(t *testing.T) {
	doc := `# Title

## Section

这是一条足够长的、值得用来判断是否照抄的说明文字。

- 一条列表项也足够长，但列表项不参与比较。
| 表格 | 单元格 |
| --- | --- |
1. 编号列表项也足够长，同样不参与比较。
> 引用块同样不参与比较。

Go 1.25，标准库 database/sql，三种纯 Go 驱动。
`
	lines := claudeDocSubstantialLines(doc)
	if len(lines) != 2 {
		t.Fatalf("substantial lines = %d (%v), want 2", len(lines), lines)
	}
	if lines[0] != "这是一条足够长的、值得用来判断是否照抄的说明文字。" {
		t.Errorf("first substantial line = %q", lines[0])
	}
	if lines[1] != "Go 1.25，标准库 database/sql，三种纯 Go 驱动。" {
		t.Errorf("second substantial line = %q", lines[1])
	}

	// Fenced code is not prose: a comment inside a code block must not
	// count as a comparable line.
	fence := "```go\n" +
		"// 代码块里的注释足够长，但不参与比较。\n" +
		"```\n"
	if got := claudeDocSubstantialLines(fence); len(got) != 0 {
		t.Errorf("substantial lines inside a code fence = %v, want none", got)
	}
}
