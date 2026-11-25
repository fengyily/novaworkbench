package handler

import (
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// The plan-mode sub-task runs the CLI in two distinct shapes:
//
//	plan 段 —— --permission-mode plan (read-only), architect persona, a
//	            read-only exploration prompt, and the plan captured into
//	            sub_task_plans;
//	auto 段 —— --dangerously-skip-permissions, executor persona, the
//	            implementation prompt, and the plan injected as the spec.
//
// The three helpers below carry that distinction. They are pure so they can
// be pinned without spawning a claude process.

// TestSubTaskPlanMode pins the mode gate: ONLY the exact value 'plan' turns
// a row into a plan row. An empty mode (a legacy row) and 'auto' both mean
// the historical implementation run.
func TestSubTaskPlanMode(t *testing.T) {
	cases := []struct {
		mode string
		want bool
	}{
		{"", false},
		{model.SubTaskModeAuto, false},
		{model.SubTaskModePlan, true},
		{"plan ", true},  // trimmed
		{" plan", true},  // trimmed
		{"PLAN", false},  // case-sensitive: only the exact value counts
		{"planning", false},
	}
	for _, tc := range cases {
		st := &model.SubTask{Mode: tc.mode}
		if got := subTaskPlanMode(st); got != tc.want {
			t.Errorf("subTaskPlanMode(%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
	// A nil row is not a plan row (the helper must not panic).
	if subTaskPlanMode(nil) {
		t.Error("subTaskPlanMode(nil) = true, want false")
	}
}

// TestSubTaskPlanPermissionMode pins the CLI permission mode: "plan" only for
// a plan row, "" for everything else. "" is what llm.Gateway.streamArgs turns
// into --dangerously-skip-permissions.
func TestSubTaskPlanPermissionMode(t *testing.T) {
	if got := subTaskPlanPermissionMode(&model.SubTask{Mode: model.SubTaskModePlan}); got != "plan" {
		t.Errorf("plan row permission mode = %q, want %q", got, "plan")
	}
	if got := subTaskPlanPermissionMode(&model.SubTask{Mode: model.SubTaskModeAuto}); got != "" {
		t.Errorf("auto row permission mode = %q, want empty", got)
	}
	if got := subTaskPlanPermissionMode(&model.SubTask{Mode: ""}); got != "" {
		t.Errorf("legacy row permission mode = %q, want empty", got)
	}
}

// TestSubTaskPlanPrompt pins the plan-phase prompt: it carries the plan
// header and the read-only instruction block, and it does NOT carry the two
// fragments that would contradict a read-only run — the executor's "implement
// it now" line and the three-section phase instruction.
func TestSubTaskPlanPrompt(t *testing.T) {
	prompt := subTaskPlanPrompt("给用户表加一个 index")

	if !strings.Contains(prompt, "给用户表加一个 index") {
		t.Error("the plan prompt dropped the sub-task body")
	}
	if !strings.Contains(prompt, "## 方案（plan 模式）") {
		t.Error("the plan prompt is missing its header")
	}
	if !strings.Contains(prompt, "不要修改任何项目文件") {
		t.Error("the plan prompt is missing the read-only constraint")
	}
	if !strings.Contains(prompt, "~/.claude/plans/") {
		t.Error("the plan prompt is missing the plan-file write instruction")
	}
	if strings.Contains(prompt, "你是执行者") {
		t.Error("the plan prompt must not tell the model to implement the sub-task")
	}
	if strings.Contains(prompt, phaseInstructionsBlock) {
		t.Error("the plan prompt must not carry the three-section phase instruction")
	}
}
