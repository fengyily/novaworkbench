package prompt

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestBuildProjectClaudeDocBlock pins the shape of the injected block:
// the heading, the body, and the disambiguation sentence that only appears
// when the working directory already ships a CLAUDE.md.
func TestBuildProjectClaudeDocBlock(t *testing.T) {
	if got := BuildProjectClaudeDocBlock("", false); got != "" {
		t.Errorf("empty content → %q, want empty string", got)
	}
	if got := BuildProjectClaudeDocBlock("   \n\t ", false); got != "" {
		t.Errorf("blank content → %q, want empty string", got)
	}

	block := BuildProjectClaudeDocBlock("# 约定\n\n用 Go 1.25。", false)
	if !strings.Contains(block, ProjectClaudeDocHeading) {
		t.Errorf("block missing heading:\n%s", block)
	}
	if !strings.Contains(block, "# 约定\n\n用 Go 1.25。") {
		t.Errorf("block missing content:\n%s", block)
	}
	if strings.Contains(block, "以本块为准") {
		t.Errorf("block mentions a disk conflict although diskExists=false:\n%s", block)
	}

	block = BuildProjectClaudeDocBlock("# 约定", true)
	if !strings.Contains(block, "以本块为准") {
		t.Errorf("block missing the conflict note although diskExists=true:\n%s", block)
	}
}

// TestBuildProjectClaudeDocBlockTruncates is the guard for the two ways the
// truncation could go wrong: splitting a multi-byte character (garbled
// text for the model) and silently dropping the tail (the model would
// believe the visible part is the complete set of conventions).
func TestBuildProjectClaudeDocBlockTruncates(t *testing.T) {
	// ~40KB of Chinese: every rune is 3 bytes, so the byte budget is
	// crossed in the middle of the string.
	content := strings.Repeat("项目约定内容。", 4000)
	block := BuildProjectClaudeDocBlock(content, false)
	if !strings.Contains(block, "已截断") {
		t.Error("truncation marker missing")
	}
	if len(block) >= len(content) {
		t.Errorf("block was not truncated: len=%d content len=%d", len(block), len(content))
	}
	if !utf8.ValidString(block) {
		t.Error("block is not valid UTF-8 — the cut landed inside a rune")
	}
	if strings.ContainsRune(block, utf8.RuneError) {
		t.Error("block contains a replacement character — the cut landed inside a rune")
	}
	// The head must survive: the conventions and commands usually live at
	// the top of the document.
	if !strings.Contains(block, "项目约定内容。") {
		t.Error("the head of the document was dropped")
	}
}

// TestBuildClaudeDocGenerationPrompt pins the generation contract: read-only
// exploration, no invented commands, and the body as the only output.
func TestBuildClaudeDocGenerationPrompt(t *testing.T) {
	p := BuildClaudeDocGenerationPrompt("NovaWorkbench", "重点关注后端分层", false)
	for _, want := range []string{
		"NovaWorkbench",
		"重点关注后端分层",
		"Read",
		"Glob",
		"禁止创建或修改任何文件",
		"不得编造",
		"不要使用 ``` 代码围栏",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("generation prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "```") && !strings.Contains(p, "不要使用 ``` 代码围栏") {
		t.Errorf("generation prompt contains a stray fence:\n%s", p)
	}
}

// TestBuildClaudeDocGenerationPromptExistingDocRule pins the rule that keeps
// the generation from degenerating into a copy of the CLAUDE.md that is
// already on disk.
//
// The Claude CLI auto-loads that file into the session as project memory —
// as instructions, not as a file the model chose to read — so without an
// explicit rule the model has no reason to prefer the code over the text
// it was just handed.
//
// The rule must appear only when such a file is actually there: telling a
// model "there may be a CLAUDE.md" when the repository has none sends it
// looking for a document that does not exist.
func TestBuildClaudeDocGenerationPromptExistingDocRule(t *testing.T) {
	without := BuildClaudeDocGenerationPrompt("NovaWorkbench", "", false)
	if strings.Contains(without, "既有 CLAUDE.md") {
		t.Errorf("the existing-document rule appears although the repo has no CLAUDE.md:\n%s", without)
	}

	with := BuildClaudeDocGenerationPrompt("NovaWorkbench", "", true)
	for _, want := range []string{
		"既有 CLAUDE.md",
		"不要照抄它",
		"以代码为准",
		"必须在仓库中找到依据",
	} {
		if !strings.Contains(with, want) {
			t.Errorf("generation prompt missing %q:\n%s", want, with)
		}
	}
	// The rule must sit BEFORE the section layout, so the model reads it
	// while it is still deciding what to write — not after it has already
	// paraphrased the old document.
	if strings.Index(with, "既有 CLAUDE.md") > strings.Index(with, "## 文档结构") {
		t.Errorf("the existing-document rule comes after the section layout:\n%s", with)
	}
}
