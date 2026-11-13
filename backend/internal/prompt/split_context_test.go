package prompt

import (
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// The context block is what makes the split see the requirement's material.
// Its structure is a contract with the model: the three sections must be
// present, at the right heading level, and framed as background rather than
// instructions.
func TestBuildSplitContextBlock_RendersTheThreeSections(t *testing.T) {
	block := BuildSplitContextBlock(SplitContext{
		AttachManifest: "## 📎 已附加文件\n\n下方的文件由用户手动上传…\n\n### 1. 接口文档.pdf  (1 KB)\n**位置**：`/tmp/att/接口文档.pdf`\n",
		DirsManifest:   "## 📂 已授权读取目录\n\n以下目录已通过 `--add-dir` 授权…\n\n- `/Users/x/specs` —— 接口规格\n",
		Referenced: []model.RequirementReferenceContent{{
			RequirementReference: model.RequirementReference{
				RequirementID: "req_ref",
				Note:          "复用其鉴权中间件",
				Origin:        model.ReferenceOriginDirect,
			},
			Description: "关联需求描述",
			DesignDocs:  "# 关联方案",
		}},
	})
	if block == "" {
		t.Fatal("the block must not be empty")
	}
	for _, want := range []string{
		"## 需求上下文",
		"### 📎 已附加文件",
		"### 📂 已授权读取目录",
		"### 关联需求 1",
		"req_ref",
		"复用其鉴权中间件",
		"本需求直接引用",
		"关联需求描述",
		"# 关联方案",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the block is missing %q:\n%s", want, block)
		}
	}

	// The manifest's own per-file heading must be demoted too — otherwise
	// "### 1. 接口文档.pdf" reads as a sibling of the section listing it.
	if strings.Contains(block, "\n### 1. 接口文档.pdf") {
		t.Errorf("the per-file heading was not demoted:\n%s", block)
	}
	if !strings.Contains(block, "\n#### 1. 接口文档.pdf") {
		t.Errorf("the per-file heading should be a level-4 heading:\n%s", block)
	}
}

// A requirement without any material must produce NO block at all, so the
// split prompt stays byte-identical to what it was before the feature.
func TestBuildSplitContextBlock_EmptyInputReturnsNothing(t *testing.T) {
	if got := BuildSplitContextBlock(SplitContext{}); got != "" {
		t.Errorf("an empty context produced a block:\n%s", got)
	}
	if got := BuildSplitContextBlock(SplitContext{AttachManifest: "   ", DirsManifest: "\n"}); got != "" {
		t.Errorf("a blank context produced a block:\n%s", got)
	}
}

// The block ends with exactly one newline: the caller adds the separator and
// the embedded manifests already carry their own trailing newlines, so more
// than one would leave a hole of blank lines in the prompt.
func TestBuildSplitContextBlock_OneTrailingNewline(t *testing.T) {
	block := BuildSplitContextBlock(SplitContext{
		AttachManifest: "## 📎 已附加文件\n\n正文\n\n### 1. a.md\n**位置**：`/x/a.md`\n\n> 提示。\n",
	})
	if !strings.HasSuffix(block, "\n") {
		t.Fatalf("the block must end with a newline:\n%q", block)
	}
	if strings.HasSuffix(block, "\n\n") {
		t.Errorf("the block ends with more than one newline:\n%q", block)
	}
}

// The remote wording exists because a granted directory is recorded on THIS
// machine while the run happens on the agent host.
func TestBuildSplitContextBlock_RemoteNoteOnlyWhenRemote(t *testing.T) {
	dirs := "## 📂 已授权读取目录\n\n- `/Users/x/specs`\n"
	local := BuildSplitContextBlock(SplitContext{DirsManifest: dirs})
	if strings.Contains(local, "Agent 服务器") {
		t.Errorf("a local run must not mention the agent host:\n%s", local)
	}
	remote := BuildSplitContextBlock(SplitContext{DirsManifest: dirs, Remote: true})
	if !strings.Contains(remote, "Agent 服务器") {
		t.Errorf("a remote run must explain the local paths:\n%s", remote)
	}
}

// The referenced requirements come from the same renderer the design stage
// uses, so the split sees the same id / reason / origin / description / plan.
func TestBuildSplitContextBlock_ReusesTheReferenceEntry(t *testing.T) {
	block := BuildSplitContextBlock(SplitContext{
		Referenced: []model.RequirementReferenceContent{
			{
				RequirementReference: model.RequirementReference{
					RequirementID: "req_a",
					Note:          "前置能力",
					Origin:        model.ReferenceOriginParent,
				},
				Description: "描述 A",
			},
			{
				RequirementReference: model.RequirementReference{
					RequirementID: "req_b",
					Origin:        model.ReferenceOriginDirect,
				},
				Description: "描述 B",
			},
		},
	})
	for _, want := range []string{"### 关联需求 1", "### 关联需求 2", "继承自主需求", "本需求直接引用", "- 引用原因：前置能力"} {
		if !strings.Contains(block, want) {
			t.Errorf("the block is missing %q:\n%s", want, block)
		}
	}
}

// A fat reference must not starve the rest: the entries beyond the budget
// are dropped and the drop is visible.
func TestBuildSplitContextBlock_DropsEntriesBeyondTheBudget(t *testing.T) {
	var refs []model.RequirementReferenceContent
	for i := 0; i < 6; i++ {
		refs = append(refs, model.RequirementReferenceContent{
			RequirementReference: model.RequirementReference{RequirementID: "req_" + string(rune('a'+i))},
			Description:          strings.Repeat("描述", 4000),
			DesignDocs:           strings.Repeat("方案", 6000),
		})
	}
	block := BuildSplitContextBlock(SplitContext{Referenced: refs})
	if !strings.Contains(block, "因篇幅限制未展开") {
		t.Errorf("the dropped entries were not reported")
	}
	if runes := len([]rune(block)); runes > splitCtxMaxRunes {
		t.Errorf("the block is %d runes, want <= %d", runes, splitCtxMaxRunes)
	}
}

// The whole block is bounded — an archive with a 10k-line listing must not
// eat the split's context budget.
func TestBuildSplitContextBlock_RespectsTheWholeBudget(t *testing.T) {
	block := BuildSplitContextBlock(SplitContext{
		AttachManifest: "## 📎 已附加文件\n\n" + strings.Repeat("行内容\n", 20000),
		DirsManifest:   "## 📂 已授权读取目录\n\n" + strings.Repeat("- `/x`\n", 5000),
	})
	if runes := len([]rune(block)); runes > splitCtxMaxRunes {
		t.Errorf("the block is %d runes, want <= %d", runes, splitCtxMaxRunes)
	}
	if !strings.Contains(block, "已截断") {
		t.Error("a truncated section must say so")
	}
}

// The block must not be mistaken for an instruction — the material is
// background, the technical plan stays the authority.
func TestBuildSplitContextBlock_FramesTheMaterialAsBackground(t *testing.T) {
	block := BuildSplitContextBlock(SplitContext{DirsManifest: "## 📂 已授权读取目录\n\n- `/x`\n"})
	if !strings.Contains(block, "背景资料") || !strings.Contains(block, "以技术方案为准") {
		t.Errorf("the block is missing the read-only framing:\n%s", block)
	}
}
