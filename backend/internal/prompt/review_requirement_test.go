package prompt

import (
	"strings"
	"testing"
)

// TestBuildReviewPrompt_ScopeBlockReplacesDiffRange pins the contract the
// requirement-level Review depends on: when a scope block is supplied, the
// default "对比区间 + 操作步骤" sections are REPLACED, not merely appended.
//
// If they were merely appended, the reviewer would be told to run
// `git diff base...head` while the scope says the review also covers the
// working tree — and it would grade a stale copy of the code, which is
// exactly the failure the requirement-level Review exists to prevent.
func TestBuildReviewPrompt_ScopeBlockReplacesDiffRange(t *testing.T) {
	scope := "## 本次 Review 的对比范围\n\n- 工作区未提交改动：`git diff HEAD`\n"
	out := BuildReviewPrompt(ReviewPromptInput{
		ReviewType: "requirement",
		Branch:     "feat/req_x",
		BaseBranch: "main",
		HeadRef:    "feat/req_x",
		BaseRef:    "origin/main",
		ScopeBlock: scope,
	})

	if !strings.Contains(out, scope) {
		t.Fatalf("scope block missing from the prompt:\n%s", out)
	}
	if strings.Contains(out, "## 操作步骤") {
		t.Fatalf("the default 操作步骤 section must be replaced by the scope block:\n%s", out)
	}
	if strings.Contains(out, "git diff origin/main...feat/req_x` 查看本次全部改动") {
		t.Fatalf("the default diff-range step must not survive:\n%s", out)
	}
	if !strings.Contains(out, "请对以下需求实现的改动进行代码 Review。") {
		t.Fatalf("requirement-level intro missing:\n%s", out)
	}
}

// TestBuildReviewPrompt_DefaultPathUnchanged guards the project-level
// Review: an empty scope block must leave the prompt exactly as it was.
func TestBuildReviewPrompt_DefaultPathUnchanged(t *testing.T) {
	out := BuildReviewPrompt(ReviewPromptInput{
		ReviewType: "branch",
		Branch:     "feat/x",
		BaseBranch: "main",
		HeadRef:    "origin/feat/x",
		BaseRef:    "origin/main",
	})
	if !strings.Contains(out, "## 操作步骤") {
		t.Fatalf("the default 操作步骤 section must be present for a project-level review:\n%s", out)
	}
	if !strings.Contains(out, "git diff origin/main...origin/feat/x` 查看本次全部改动") {
		t.Fatalf("the default diff step must be present:\n%s", out)
	}
	if !strings.Contains(out, "请对以下分支的改动进行代码 Review。") {
		t.Fatalf("branch intro missing:\n%s", out)
	}
}

// TestBuildFixPrompt_CarriesTheFinding guards the fix sub-task prompt: it
// must name the severity, the location and the suggestion, and it must NOT
// carry the review's output contract (a fix ends in code, not in a report).
func TestBuildFixPrompt_CarriesTheFinding(t *testing.T) {
	out := BuildFixPrompt(FixPromptInput{
		Severity:        SevP0,
		Ref:             "P0-1",
		Title:           "分页参数未校验",
		Detail:          "page 可能为负数",
		Suggestion:      "加一个下界检查",
		File:            "backend/internal/service/foo.go",
		Line:            "12-20",
		RequirementTitle: "分页改造",
	})

	for _, want := range []string{"P0", "P0-1", "分页参数未校验", "backend/internal/service/foo.go", "12-20", "加一个下界检查", "分页改造"} {
		if !strings.Contains(out, want) {
			t.Fatalf("fix prompt missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "```json") {
		t.Fatalf("the fix prompt must not carry the review's trailing JSON contract:\n%s", out)
	}
	if !strings.Contains(out, "不要新建分支") {
		t.Fatalf("the fix prompt must pin the branch:\n%s", out)
	}
}
