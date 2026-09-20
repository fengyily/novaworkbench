package handler

import "testing"

// TestDefaultSubTaskConcurrency_IsOne guards the regression: a regression
// that bumped the default back to 4 would re-introduce the cross-project
// sub-task fan-out the user reported as "并发". Per-project gate
// (DefaultSubTaskProjectConcurrency) stays at 1; only the process-wide
// OOM ceiling is at stake. Override via NOVA_SUBTASK_CONCURRENCY env.
func TestDefaultSubTaskConcurrency_IsOne(t *testing.T) {
	if DefaultSubTaskConcurrency != 1 {
		t.Fatalf("DefaultSubTaskConcurrency = %d, want 1 (strict serial "+
			"across projects; override via NOVA_SUBTASK_CONCURRENCY)",
			DefaultSubTaskConcurrency)
	}
}
