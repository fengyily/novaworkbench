package service

import "testing"

// TestWorktreeLimiterSameDirSerial pins the core guarantee of the
// per-working-directory gate: two sub-tasks of the SAME requirement (they
// share one worktree) must not run at the same time.
//
// This is the gate that fixes the "the agent keeps answering the same thing"
// symptom: two children editing one directory invalidate each other's probe
// results, so the model re-verifies and re-states the same conclusion.
func TestWorktreeLimiterSameDirSerial(t *testing.T) {
	l := NewWorktreeLimiter()

	if !l.TryAcquire("/worktrees/req_a") {
		t.Fatal("first TryAcquire on a free directory must succeed")
	}
	if l.TryAcquire("/worktrees/req_a") {
		t.Fatal("second TryAcquire on the SAME directory must fail — two agents, one tree")
	}
	if got := l.Active("/worktrees/req_a"); got != 1 {
		t.Errorf("Active = %d, want 1", got)
	}

	// A DIFFERENT directory is unaffected: cross-worktree parallelism is the
	// whole point of the gate.
	if !l.TryAcquire("/worktrees/req_b") {
		t.Fatal("TryAcquire on a DIFFERENT directory must succeed")
	}

	l.Release("/worktrees/req_a")
	if !l.TryAcquire("/worktrees/req_a") {
		t.Fatal("after Release the directory must be acquirable again")
	}
}

// TestWorktreeLimiterReleaseIdempotent makes sure a double release cannot
// grant an extra slot (which would let three agents into one tree).
func TestWorktreeLimiterReleaseIdempotent(t *testing.T) {
	l := NewWorktreeLimiter()
	if !l.TryAcquire("/w") {
		t.Fatal("TryAcquire must succeed")
	}
	l.Release("/w")
	l.Release("/w")
	l.Release("/never-acquired")
	if got := l.Active("/w"); got != 0 {
		t.Errorf("Active = %d, want 0 (a double release must not grant an extra slot)", got)
	}
	if !l.TryAcquire("/w") {
		t.Fatal("TryAcquire must succeed after the releases")
	}
	if l.TryAcquire("/w") {
		t.Fatal("the directory must still be exclusive")
	}
}
