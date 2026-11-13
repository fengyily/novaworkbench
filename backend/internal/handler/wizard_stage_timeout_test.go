package handler

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// The stage timeout policy exists because a WALL-CLOCK deadline killed runs
// that were actively producing output: job_8c64ea6b and job_9db58f29
// (2026-11-13) both died at 耗时=30m0.04s with a tool_call on the very same
// millisecond. These tests pin the contract that makes that impossible.

// TestNewStageRun_ActivitySlidesTheDeadline is the core regression guard:
// a stage that keeps producing output must survive well past the idle
// window, because the deadline slides on every beat.
func TestNewStageRun_ActivitySlidesTheDeadline(t *testing.T) {
	// A short idle window so the test is fast; the property under test is
	// "beats push the deadline out", not the specific duration.
	t.Setenv("NOVA_STAGE_IDLE_TIMEOUT", "200ms")
	t.Setenv("NOVA_STAGE_MAX_TOTAL_TIMEOUT", "10s")

	stage := newStageRun("test")
	defer stage.cancel()

	// Beat faster than the idle window for well past it. The old
	// wall-clock deadline would have fired at 200ms here.
	deadline := time.Now().Add(900 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case stage.hb <- struct{}{}:
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := stage.ctx.Err(); err != nil {
		t.Fatalf("stage ctx died while output was flowing: %v", err)
	}
}

// TestNewStageRun_IdleCancelsWithCause pins that a stage that produces
// NOTHING is still bounded — and that the cancellation carries errStageIdle,
// so the run can be attributed to our idle window rather than to "nobody".
func TestNewStageRun_IdleCancelsWithCause(t *testing.T) {
	t.Setenv("NOVA_STAGE_IDLE_TIMEOUT", "150ms")
	t.Setenv("NOVA_STAGE_MAX_TOTAL_TIMEOUT", "10s")

	stage := newStageRun("test")
	defer stage.cancel()

	select {
	case <-stage.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("idle stage was never cancelled")
	}
	if !errors.Is(context.Cause(stage.ctx), errStageIdle) {
		t.Fatalf("cancel cause = %v, want errStageIdle", context.Cause(stage.ctx))
	}
}

// TestNewStageRun_AbsoluteCapFires pins that the generous absolute cap is
// still a real bound: a run that keeps beating forever must still die.
func TestNewStageRun_AbsoluteCapFires(t *testing.T) {
	t.Setenv("NOVA_STAGE_IDLE_TIMEOUT", "10s")
	t.Setenv("NOVA_STAGE_MAX_TOTAL_TIMEOUT", "300ms")

	stage := newStageRun("test")
	defer stage.cancel()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case stage.hb <- struct{}{}:
		default:
		}
		if stage.ctx.Err() != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := stage.ctx.Err(); err == nil {
		t.Fatal("absolute cap never fired")
	}
	if !errors.Is(stage.ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx err = %v, want DeadlineExceeded", stage.ctx.Err())
	}
}

// TestDurationFromEnv pins the env parsing contract: unset → default,
// valid → parsed, invalid/non-positive → default (a typo must not silently
// disable the timeout).
func TestDurationFromEnv(t *testing.T) {
	const key = "NOVA_TEST_DURATION"
	def := 7 * time.Minute

	t.Setenv(key, "")
	if got := durationFromEnv(key, def); got != def {
		t.Fatalf("unset: got %v, want %v", got, def)
	}

	t.Setenv(key, "90s")
	if got := durationFromEnv(key, def); got != 90*time.Second {
		t.Fatalf("valid: got %v, want 90s", got)
	}

	t.Setenv(key, "not-a-duration")
	if got := durationFromEnv(key, def); got != def {
		t.Fatalf("invalid: got %v, want %v", got, def)
	}

	t.Setenv(key, "0")
	if got := durationFromEnv(key, def); got != def {
		t.Fatalf("zero: got %v, want %v", got, def)
	}
}

// TestStageTimeouts_DefaultsAreGenerous pins the shipped defaults. The
// absolute cap must be far above the old 30-minute wall (that wall was the
// bug), and the idle window must stay a real bound.
func TestStageTimeouts_DefaultsAreGenerous(t *testing.T) {
	os.Unsetenv("NOVA_STAGE_IDLE_TIMEOUT")
	os.Unsetenv("NOVA_STAGE_MAX_TOTAL_TIMEOUT")

	if got := stageIdleTimeout(); got != defaultStageIdleTimeout {
		t.Fatalf("idle default = %v, want %v", got, defaultStageIdleTimeout)
	}
	if got := stageMaxTotalTimeout(); got != defaultStageMaxTotalTimeout {
		t.Fatalf("max total default = %v, want %v", got, defaultStageMaxTotalTimeout)
	}
	if stageMaxTotalTimeout() <= 30*time.Minute {
		t.Fatalf("absolute cap %v must exceed the old 30m wall", stageMaxTotalTimeout())
	}
}

// TestTerminationDiagnostics_CountsTotalAndStreamEvents pins that the
// diagnostic line no longer lies. It used to print the stream_event subset
// under the label "事件数" — and the LOCAL path never incremented any
// counter at all, so every local failure reported "事件数=0 最后事件类型=-"
// no matter how much output the run had produced.
func TestTerminationDiagnostics_CountsTotalAndStreamEvents(t *testing.T) {
	out := claudeStreamOutcome{
		killReason:       killReasonTeardown,
		stdoutEnded:      "eof",
		lastEventType:    "assistant",
		eventCount:       42,
		streamEventCount: 7,
		elapsed:          733 * time.Millisecond,
	}
	line := terminationDiagnostics(out, 143, "")
	for _, want := range []string{"事件数=42", "stream事件数=7", "最后事件类型=assistant", "终止原因=teardown"} {
		if !strings.Contains(line, want) {
			t.Errorf("diagnostic line missing %q; got: %s", want, line)
		}
	}
}
