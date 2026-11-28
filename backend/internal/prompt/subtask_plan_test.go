package prompt

import (
	"strings"
	"testing"
)

// TestSubTaskPlanInstructionsBlock pins the three load-bearing fragments of
// the plan-phase instruction block:
//
//   - the plan-file path (the backend captures the plan from that Write);
//   - the read-only constraint (the CLI enforces it, but the prompt has to
//     say it or the model keeps trying to edit files);
//   - the required section list (the plan is the product of this phase —
//     without a fixed shape the card has nothing to render).
func TestSubTaskPlanInstructionsBlock(t *testing.T) {
	for _, want := range []string{
		"~/.claude/plans/",
		"不要修改任何项目文件",
		"验收点",
		"改动点",
		"风险",
	} {
		if !strings.Contains(SubTaskPlanInstructionsBlock, want) {
			t.Errorf("SubTaskPlanInstructionsBlock is missing %q", want)
		}
	}
	// It must NOT ask for the requirement-level design furniture — a single
	// sub-task has no page to lay out.
	for _, unwanted := range []string{"ASCII", "布局图"} {
		if strings.Contains(SubTaskPlanInstructionsBlock, unwanted) {
			t.Errorf("SubTaskPlanInstructionsBlock should not mention %q", unwanted)
		}
	}
}

// TestSubTaskPlanDevBlock pins the injection block: an empty plan yields an
// empty string (the caller must not inject an empty heading), and a real plan
// is wrapped with the heading that tells the implementer to follow it.
func TestSubTaskPlanDevBlock(t *testing.T) {
	if got := SubTaskPlanDevBlock(""); got != "" {
		t.Errorf("SubTaskPlanDevBlock(\"\") = %q, want empty", got)
	}
	if got := SubTaskPlanDevBlock("   \n  "); got != "" {
		t.Errorf("SubTaskPlanDevBlock(whitespace) = %q, want empty", got)
	}
	got := SubTaskPlanDevBlock("# 方案\n\n## 目标\n做一件事")
	if !strings.Contains(got, "## 实施方案") {
		t.Errorf("SubTaskPlanDevBlock is missing the heading: %q", got)
	}
	if !strings.Contains(got, "## 目标\n做一件事") {
		t.Errorf("SubTaskPlanDevBlock dropped the plan body: %q", got)
	}
}
