package prompt

import (
	"strings"
	"testing"
)

// TestOutputBudgetClause pins the load-bearing fragments of the output-budget
// block. The wording is the only thing the model sees, so the constraints it
// actually has to convey must not silently disappear from a refactor.
func TestOutputBudgetClause(t *testing.T) {
	for _, want := range []string{"300 行", "分段", "Edit", "截断"} {
		if !strings.Contains(OutputBudgetClause, want) {
			t.Errorf("OutputBudgetClause is missing %q", want)
		}
	}
}

// TestOutputBudgetClauseIsAppendOnly guards the "prompt wording elsewhere is
// unchanged" property: the block must be a self-contained tail, not something
// a caller has to splice into an existing sentence.
func TestOutputBudgetClauseIsAppendOnly(t *testing.T) {
	if !strings.HasPrefix(OutputBudgetClause, "\n") && !strings.HasPrefix(OutputBudgetClause, "#") {
		t.Errorf("OutputBudgetClause should start on its own line/heading, got %q", OutputBudgetClause[:20])
	}
	if strings.Contains(OutputBudgetClause, "%s") {
		t.Error("OutputBudgetClause must not contain format verbs (it is used verbatim)")
	}
}
