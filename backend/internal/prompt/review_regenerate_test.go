package prompt

import (
	"strings"
	"testing"
)

func sampleRegenerateInput(resumed bool) RegeneratePromptInput {
	return RegeneratePromptInput{
		Resumed:     resumed,
		ReviewType:  "branch",
		Branch:      "feat/pagination",
		BaseBranch:  "main",
		HeadRef:     "origin/feat/pagination",
		BaseRef:     "origin/main",
		Description: "把 X 接口改成游标分页",
		ReportMD:    "## 总体评价\n建议修复后合入",
		History: []FollowupTurn{
			{Question: "P0-1 真的会触发吗？", Answer: "经核对该分支不会执行到，判断有误"},
		},
	}
}

// The regenerated report is parsed by the same handler.extractTrailingJSON →
// parseFindings path as the first one, so it must carry the same contract.
func TestBuildRegeneratePrompt_CarriesTheReviewContract(t *testing.T) {
	out := BuildRegeneratePrompt(sampleRegenerateInput(false))

	for _, want := range []string{
		"```json",
		`"goal_met":"yes|partial|no"`,
		"goal_checks",
		"不使用 P2",
		"## 改动概览图",
		"JSON 必须是单个合法对象，且是整个回复的最后一段内容。",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("regenerate prompt is missing %q", want)
		}
	}
}

// Ordering is load-bearing: extractTrailingJSON takes the LAST ```json fence
// and otherwise brace-matches backwards from the end, so a Mermaid directive
// emitted after the contract would teach the model to break both paths.
func TestBuildRegeneratePrompt_MermaidDirectiveComesBeforeTheJSONContract(t *testing.T) {
	out := BuildRegeneratePrompt(sampleRegenerateInput(false))

	mermaid := strings.Index(out, "## 输出图示（Mermaid）")
	contract := strings.Index(out, "**报告正文结束后，必须在最末尾追加一个 json 代码围栏块**")
	if mermaid < 0 || contract < 0 {
		t.Fatalf("directive/contract missing: mermaid=%d contract=%d", mermaid, contract)
	}
	if mermaid > contract {
		t.Fatalf("mermaid directive at %d comes after the json contract at %d", mermaid, contract)
	}
	if !strings.Contains(out, "位置在文末 json 块**之前**") {
		t.Error("the explicit 'diagrams before the json block' rule is gone")
	}
}

// The 追问 thread is the entire reason to regenerate — it has to reach the
// model, along with the instruction to act on it rather than append to it.
func TestBuildRegeneratePrompt_EmbedsTheFollowupThread(t *testing.T) {
	out := BuildRegeneratePrompt(sampleRegenerateInput(false))

	for _, want := range []string{
		"P0-1 真的会触发吗？",
		"经核对该分支不会执行到，判断有误",
		"把追问中确认的结论写进报告",
		"纠正被推翻的结论",
		"不要凭追问臆造新问题",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("regenerate prompt is missing %q", want)
		}
	}
}

// A resumed run already holds the diff and the report in its session;
// re-pasting them would only burn context. The thread still goes in —
// the earlier turns are what the rewrite is based on.
func TestBuildRegeneratePrompt_ResumedOmitsTheReportButKeepsTheThread(t *testing.T) {
	resumed := BuildRegeneratePrompt(sampleRegenerateInput(true))

	if strings.Contains(resumed, "上一版 Review 报告原文") {
		t.Error("resumed prompt re-pastes the report it is already holding")
	}
	if strings.Contains(resumed, "## Review 上下文") {
		t.Error("resumed prompt re-states context the session already has")
	}
	if !strings.Contains(resumed, "P0-1 真的会触发吗？") {
		t.Error("resumed prompt dropped the follow-up thread")
	}
	if !strings.Contains(resumed, "```json") {
		t.Error("resumed prompt dropped the output contract")
	}

	fresh := BuildRegeneratePrompt(sampleRegenerateInput(false))
	if !strings.Contains(fresh, "上一版 Review 报告原文") {
		t.Error("fresh prompt must carry the report it is rewriting")
	}
	if !strings.Contains(fresh, "git diff origin/main...origin/feat/pagination") {
		t.Error("fresh prompt must name the diff range so the model can re-read it")
	}
}

// The attachment directive is what turns an uploaded run log into quotable
// evidence; a regeneration that dropped it would quietly downgrade every
// goal_check that cited one.
func TestBuildRegeneratePrompt_KeepsAttachmentDirectiveWhenFilesExist(t *testing.T) {
	in := sampleRegenerateInput(false)
	in.HasAttachments = true
	out := BuildRegeneratePrompt(in)

	if !strings.Contains(out, "附件（功能验证材料）") {
		t.Error("attachment directive missing from a regeneration that has attachments")
	}
	if strings.Contains(BuildRegeneratePrompt(sampleRegenerateInput(false)), "附件（功能验证材料）") {
		t.Error("attachment directive leaked into a regeneration with no attachments")
	}
}

// goal_checks is the structured evidence behind the one-word 目标达成 verdict
// (requirement: "功能验证，可能需要提供附件…确认分支改动有没有达到目的").
func TestBuildReviewPrompt_AsksForGoalCheckEvidence(t *testing.T) {
	out := BuildReviewPrompt(ReviewPromptInput{
		ReviewType:     "branch",
		Branch:         "feat/x",
		BaseBranch:     "main",
		HeadRef:        "origin/feat/x",
		BaseRef:        "origin/main",
		Description:    "把 X 接口改成游标分页",
		HasAttachments: true,
	})

	for _, want := range []string{
		`"goal_checks"`,
		"criterion",
		"evidence",
		"`unknown` 表示现有材料不足以判断",
		"grep -nEi 'error|panic|fail|exception|traceback'",
		"unzip -p",
		"以代码为准",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("review prompt is missing %q", want)
		}
	}
}

// One stored column, two meanings: a branch review's description is the
// 改动目标 the report grades 目标达成 against, a PR review's is 额外审查要求.
// Labelling the latter as the former would have the reviewer grade the
// change against the wrong text.
func TestBuildRegeneratePrompt_SeparatesGoalFromExtraRequirements(t *testing.T) {
	branch := BuildRegeneratePrompt(sampleRegenerateInput(false))
	if !strings.Contains(branch, "用户描述的改动目标（判断「目标是否达成」的唯一依据）") {
		t.Error("a branch review's description must be presented as the 改动目标")
	}
	if strings.Contains(branch, "额外审查要求") {
		t.Error("a branch review has no 额外审查要求 slot")
	}

	in := sampleRegenerateInput(false)
	in.ReviewType = "pr"
	in.Description = ""
	in.ExtraRequirements = "重点看并发安全"
	pr := BuildRegeneratePrompt(in)
	if !strings.Contains(pr, "### 额外审查要求\n\n重点看并发安全") {
		t.Error("a PR review's text must be presented as 额外审查要求")
	}
	if strings.Contains(pr, "用户描述的改动目标（判断「目标是否达成」的唯一依据）") {
		t.Error("a PR review has no 改动目标 to grade against")
	}
}
