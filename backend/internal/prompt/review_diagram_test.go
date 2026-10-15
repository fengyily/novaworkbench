package prompt

import (
	"strings"
	"testing"
)

// A review report is read the way a design doc is read — the shape of the
// change first — so the task prompt has to ask for diagrams, name the
// Mermaid flavours per situation, and give the model an explicit way out for
// a one-line change (without the out it draws a two-node flowchart for a
// typo fix).
func TestBuildReviewPrompt_AsksForDiagrams(t *testing.T) {
	out := BuildReviewPrompt(ReviewPromptInput{
		ReviewType: "branch",
		Branch:     "feat/x",
		BaseBranch: "main",
		HeadRef:    "origin/feat/x",
		BaseRef:    "origin/main",
	})
	for _, want := range []string{
		"## 改动概览图",
		"```mermaid",
		"sequenceDiagram",
		"flowchart",
		"stateDiagram-v2",
		"Mermaid 11.x 兼容约束",
		"「无」",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("review prompt is missing %q", want)
		}
	}
}

// Ordering is load-bearing: handler.extractTrailingJSON takes the LAST
// ```json fence and otherwise brace-matches backwards from the end of the
// report, so a Mermaid block emitted after the contract block would strand
// prose behind the JSON and defeat both paths. The prompt must therefore
// pin diagrams to the body, ahead of the contract.
func TestBuildReviewPrompt_DiagramDirectivePrecedesJSONContract(t *testing.T) {
	out := BuildReviewPrompt(ReviewPromptInput{ReviewType: "branch", Branch: "feat/x"})
	directive := strings.Index(out, "## 输出图示（Mermaid）")
	contract := strings.Index(out, "必须在最末尾追加一个 json 代码围栏块")
	if directive < 0 || contract < 0 {
		t.Fatalf("missing section: directive=%d contract=%d", directive, contract)
	}
	if directive > contract {
		t.Fatal("the Mermaid directive must come before the trailing-JSON contract")
	}
	if !strings.Contains(out, "位置在文末 json 块**之前**") {
		t.Error("the prompt never tells the model to keep diagrams ahead of the JSON block")
	}
}

// A follow-up explains one finding in depth — often a call chain or a race —
// so it may draw, but it still must not emit the grading contract.
func TestBuildFollowupPrompt_InvitesDiagramsButNotJSON(t *testing.T) {
	out := BuildFollowupPrompt(FollowupPromptInput{
		Resumed:  true,
		Question: "这个并发问题具体怎么触发？",
	})
	if !strings.Contains(out, "```mermaid") {
		t.Error("follow-up prompt should offer Mermaid for call chains / timing")
	}
	if strings.Contains(out, "```json") {
		t.Error("follow-ups must not request the grading JSON block")
	}
}
