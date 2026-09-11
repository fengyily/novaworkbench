package handler

import (
	"strings"
	"testing"
)

// TestWorkerCategoryHintRunningAsRoot guards the Chinese fix hint surfaced
// when nova-agent-worker emits the `running_as_root` error category. Without
// the case the wizard's job panel would show the raw `--dangerously-skip-
// permissions cannot be used with root/sudo privileges for security reasons`
// stderr with no actionable next step, leaving the operator wondering why
// their coding run died at the first CLI invocation.
//
// Pair this test with the worker's classifyError regex change — the two
// halves must land together, otherwise one side silently degrades to the
// `unknown` fallback or a no-op hint.
func TestWorkerCategoryHintRunningAsRoot(t *testing.T) {
	const stderr = "--dangerously-skip-permissions cannot be used with root/sudo privileges for security reasons"

	got := workerCategoryHint("running_as_root", stderr)
	if got == "" {
		t.Fatalf("workerCategoryHint(running_as_root, %q) returned empty; expected a Chinese fix hint", stderr)
	}
	// Anchor on "普通用户" — the actionable knob the user has to turn next.
	// Failing here means someone shortened the hint and lost the actual fix.
	if !strings.Contains(got, "普通用户") {
		t.Fatalf("workerCategoryHint(running_as_root, %q) = %q; expected the hint to mention \"普通用户\"", stderr, got)
	}
	// And on the dangerous flag itself, so the user can map the hint back to
	// the exact CLI invocation that rejected them.
	if !strings.Contains(got, "--dangerously-skip-permissions") {
		t.Fatalf("workerCategoryHint(running_as_root, %q) = %q; expected the hint to mention the rejected flag", stderr, got)
	}
	// The one-click fix — the install flow now auto-provisions a non-root user
	// and switches the SSH username, so the hint must steer the user there
	// first rather than only to manual steps.
	if !strings.Contains(got, "安装依赖") {
		t.Fatalf("workerCategoryHint(running_as_root, %q) = %q; expected the hint to mention 安装依赖", stderr, got)
	}

	// Unknown categories must still return "" so the wizard's existing
	// "no specific hint" branch keeps working. A regression here would
	// surface a stale hint for genuinely new error categories that the
	// worker hasn't been taught to classify yet.
	if extra := workerCategoryHint("foo_bar_baz", "anything"); extra != "" {
		t.Fatalf("workerCategoryHint(foo_bar_baz, ...) = %q; expected empty string for unknown category", extra)
	}
}