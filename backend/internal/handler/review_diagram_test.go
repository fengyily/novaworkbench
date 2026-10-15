package handler

import (
	"strings"
	"testing"
)

// Reports now carry Mermaid (prompt.BuildReviewPrompt asks for a 改动概览图),
// which puts two new demands on the finish path:
//
//  1. a diagram the Mermaid 11 lexer would reject must be repaired before it
//     is persisted, otherwise the report view shows a red "Syntax error in
//     text" card where the picture should be;
//  2. the braces inside a diagram must not be mistaken for the trailing
//     grading contract.
func TestBuildReviewOutcome_SanitizesMermaid(t *testing.T) {
	report := "## 总体评价\n\n建议修复后合入。\n\n## 改动概览图\n\n" +
		"```mermaid\nflowchart TD\n  X[renderKeep(n)<br/>binary search]\n```\n\n" +
		"```json\n{\"goal_met\":\"partial\",\"findings\":[{\"severity\":\"P1\",\"title\":\"t\"}]}\n```\n"

	out := buildReviewOutcome("done", report)

	if !strings.Contains(out.ResultMD, `X["renderKeep(n)<br/>binary search"]`) {
		t.Errorf("broken Mermaid label was persisted unrepaired:\n%s", out.ResultMD)
	}
	if strings.Contains(out.ResultMD, "goal_met") {
		t.Error("the contract block leaked into result_md")
	}
	if out.GoalMet != "partial" || out.P1 != 1 {
		t.Errorf("grading lost: goal_met=%q p1=%d", out.GoalMet, out.P1)
	}
}

// A diagram-only report (model skipped the contract) still has to come back
// whole: the degraded path keeps the full text, and the rhombus braces of a
// flowchart must not be brace-matched into a phantom contract block.
func TestBuildReviewOutcome_DiagramBracesAreNotAContract(t *testing.T) {
	report := "## 总体评价\n\n可以合入。\n\n```mermaid\nflowchart TD\n  A --> B{是否命中缓存}\n  B --> C[返回]\n```\n"

	out := buildReviewOutcome("done", report)

	if out.FindingsJSON != "" || out.GoalMet != "" {
		t.Errorf("a flowchart was parsed as the grading contract: %+v", out)
	}
	if !strings.Contains(out.ResultMD, "B{是否命中缓存}") || !strings.Contains(out.ResultMD, "C[返回]") {
		t.Errorf("the diagram was truncated:\n%s", out.ResultMD)
	}
}
