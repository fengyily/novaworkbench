package handler

import (
	"strings"
	"testing"
)

// TestCodingPromptsCarryOutputBudgetClause pins that every prompt which
// drives the claude child to WRITE files carries the output-budget
// constraint.
//
// The clause is the PREVENTION layer for the "model emitted one huge file
// and got truncated" failure. If a new coding entry point is added without
// it, that failure silently becomes more likely again — so the wiring is
// pinned here rather than left to review.
func TestCodingPromptsCarryOutputBudgetClause(t *testing.T) {
	cases := []struct {
		name string
		got  string
	}{
		{"agentDirectPrompt", agentDirectPrompt("req_1", "标题", "leadIn", "/tmp/work", "gitBlock")},
		{"developerDecomposePrompt", developerDecomposePrompt("标题", "leadIn", "/tmp/work", "gitBlock")},
		{"outputLimitResumePrompt", outputLimitResumePrompt()},
	}
	for _, tc := range cases {
		if !strings.Contains(tc.got, "300 行") {
			t.Errorf("%s is missing the output-budget constraint", tc.name)
		}
		if !strings.Contains(tc.got, "分段") {
			t.Errorf("%s is missing the 'write in pieces' instruction", tc.name)
		}
	}
}

// TestCodingPromptsKeepTheirOriginalWording pins that the injection is
// APPEND-only: the persona header, the decomposition steps and the sentinel
// must survive verbatim, because other code keys off them (the orchestrator
// short-circuits on the absence of the trigger phrase; the sub-task parser
// looks for [SUBTASKS_READY]).
func TestCodingPromptsKeepTheirOriginalWording(t *testing.T) {
	direct := agentDirectPrompt("req_1", "标题", "leadIn", "/tmp/work", "gitBlock")
	for _, want := range []string{"现在切换到「Agent 开发者」角色", "需求 ID：req_1", "工作目录：/tmp/work", "gitBlock"} {
		if !strings.Contains(direct, want) {
			t.Errorf("agentDirectPrompt lost %q", want)
		}
	}

	decompose := developerDecomposePrompt("标题", "leadIn", "/tmp/work", "gitBlock")
	for _, want := range []string{"[SUBTASKS_READY]", "leadIn", "gitBlock"} {
		if !strings.Contains(decompose, want) {
			t.Errorf("developerDecomposePrompt lost %q", want)
		}
	}
}
