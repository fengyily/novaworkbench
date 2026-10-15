package prompt

import (
	"strings"
	"testing"
)

// A resumed turn must stay small: the session already holds the diff and the
// report, so re-pasting them would only burn context. It must also never ask
// for the trailing JSON block — that contract belongs to the review itself,
// and a second one would be parsed over the stored findings.
func TestBuildFollowupPrompt_ResumedIsLean(t *testing.T) {
	out := BuildFollowupPrompt(FollowupPromptInput{
		Resumed:     true,
		Branch:      "feat/x",
		BaseBranch:  "main",
		Description: "把 X 接口改成分页返回",
		ReportMD:    "## 总体评价\n不建议合入",
		Question:    "P0 第二条为什么会导致数据损坏？",
	})
	if strings.Contains(out, "不建议合入") {
		t.Fatal("resumed prompt re-pasted the report")
	}
	if strings.Contains(out, "把 X 接口改成分页返回") {
		t.Fatal("resumed prompt re-pasted the description")
	}
	if !strings.Contains(out, "P0 第二条为什么会导致数据损坏？") {
		t.Fatal("the question itself is missing")
	}
	if strings.Contains(out, "```json") {
		t.Fatal("follow-ups must not request the grading JSON block")
	}
}

// A fresh turn is self-contained: without the report in the prompt the model
// would improvise an answer about a review it never saw.
func TestBuildFollowupPrompt_FreshCarriesContext(t *testing.T) {
	out := BuildFollowupPrompt(FollowupPromptInput{
		Branch:      "feat/x",
		BaseBranch:  "main",
		HeadRef:     "origin/feat/x",
		BaseRef:     "origin/main",
		Description: "把 X 接口改成分页返回",
		ReportMD:    "## 总体评价\n不建议合入",
		History: []FollowupTurn{
			{Question: "第一个问题", Answer: "第一个回答"},
		},
		Question: "那第二条呢？",
	})
	for _, want := range []string{
		"不建议合入",                       // the report
		"把 X 接口改成分页返回",                // the goal it was graded against
		"origin/main...origin/feat/x", // the diff range
		"第一个问题", "第一个回答",              // the thread so far
		"那第二条呢？",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("fresh prompt is missing %q", want)
		}
	}
}

// An unanswered (still running or errored) turn is dropped by the handler, but
// the builder must also tolerate one with a blank answer without emitting a
// dangling "回答 N：" label.
func TestBuildFollowupPrompt_BlankAnswerOmitsLabel(t *testing.T) {
	out := BuildFollowupPrompt(FollowupPromptInput{
		ReportMD: "报告",
		History:  []FollowupTurn{{Question: "问题", Answer: "   "}},
		Question: "新问题",
	})
	if strings.Contains(out, "回答 1") {
		t.Fatal("blank answer should not render an answer label")
	}
	if !strings.Contains(out, "追问 1") {
		t.Fatal("the earlier question should still be replayed")
	}
}
