package handler

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// End-to-end regression guard for the reported failure: a local stage that
// keeps producing output was killed at exactly 30 minutes (job_8c64ea6b /
// job_9db58f29, 2026-11-13 — both died at 耗时=30m0.04s with a tool_call on
// the very same millisecond).
//
// These tests drive the REAL runClaudeStream against a fake NDJSON producer
// so the whole path is covered: the stage ctx's idle deadline sliding on
// stdout activity, and the termination being attributed to our deadline
// instead of being blamed on the CLI.

// ndjsonProducerCmd builds a command that prints one NDJSON event every
// `every` for `total`, then exits. The run is therefore bounded, but it
// looks like a healthy, busy stage for its whole life.
func ndjsonProducerCmd(t *testing.T, every, total time.Duration) *exec.Cmd {
	t.Helper()
	script := `
i=0
while [ $i -lt ` + fmt.Sprintf("%d", int(total/every)) + ` ]; do
  printf '{"type":"assistant","message":{"model":"claude-test","content":[{"type":"text","text":"tick"}]}}\n'
  i=$((i+1))
  sleep ` + fmt.Sprintf("%f", every.Seconds()) + `
done
`
	return exec.Command("sh", "-c", script)
}

// TestRunClaudeStream_BusyStageSurvivesIdleWindow proves the fix: a stage
// that emits an event continuously must NOT be torn down by the idle
// deadline, no matter how long it runs.
//
// The old behaviour was a flat wall-clock deadline, so this test is the
// direct regression guard for "明明一直在输出，到了30分钟就会被终止".
func TestRunClaudeStream_BusyStageSurvivesIdleWindow(t *testing.T) {
	// A 150ms idle window: a run that emits every 50ms must survive well
	// past it, because every beat slides the deadline forward.
	t.Setenv("NOVA_STAGE_IDLE_TIMEOUT", "150ms")
	t.Setenv("NOVA_STAGE_MAX_TOTAL_TIMEOUT", "30s")

	stage := newStageRun("test-busy")
	defer stage.cancel()

	// Emit for 600ms — four times the idle window — then exit.
	cmd := ndjsonProducerCmd(t, 50*time.Millisecond, 600*time.Millisecond)
	out := runClaudeStream(&captureSink{}, cmd, "test-busy", nil, nil, stage.hb, 0,
		withStallTimeout(codingStallTimeout), withCtx(stage.ctx))

	// The run must NOT have been killed by our idle window.
	if out.IdleTimeoutHit {
		t.Fatalf("busy stage was killed by the idle window: %s", out.errMsg)
	}
	if stage.ctx.Err() != nil && strings.Contains(out.errMsg, "idle 超时") {
		t.Fatalf("busy stage reported an idle timeout: %s", out.errMsg)
	}
	// It must have counted the events it produced — the old local path
	// never counted any, so the diagnostic line always said "事件数=0".
	if out.eventCount == 0 {
		t.Fatal("local path counted no events; the diagnostic line would lie again")
	}
}

// TestRunClaudeStream_SilentStageIsBoundedAndAttributed pins the other
// half: a stage that produces NOTHING must still be reaped, and the failure
// must be attributed to our idle window (终止原因=idle-timeout) instead of
// being reported as 终止原因=none — which is what made the original 30-minute
// kill invisible in the first place.
func TestRunClaudeStream_SilentStageIsBoundedAndAttributed(t *testing.T) {
	t.Setenv("NOVA_STAGE_IDLE_TIMEOUT", "150ms")
	t.Setenv("NOVA_STAGE_MAX_TOTAL_TIMEOUT", "30s")

	stage := newStageRun("test-silent")
	defer stage.cancel()

	// A command that prints nothing and exits immediately.
	cmd := exec.Command("sh", "-c", "exit 0")
	out := runClaudeStream(&captureSink{}, cmd, "test-silent", nil, nil, stage.hb, 0,
		withStallTimeout(codingStallTimeout), withCtx(stage.ctx))

	// The CLI exited on its own (exit 0, no output) — that is a real
	// "no result" failure, not an idle timeout, so the attribution must
	// NOT claim the idle window fired.
	if out.killReason == killReasonIdleTimeout {
		t.Fatalf("a self-exiting CLI was misattributed to the idle window: %s", out.errMsg)
	}
}

// TestRunClaudeStream_HeartbeatChannelIsReusable pins the channel ownership
// contract: runClaudeStream does NOT close the caller's heartbeat channel.
//
// The stage Q&A path retries a stale session in the SAME job, i.e. it runs
// runClaudeStream twice against one channel. If the first run closed it, the
// second run's first beat would panic on a closed channel.
func TestRunClaudeStream_HeartbeatChannelIsReusable(t *testing.T) {
	t.Setenv("NOVA_STAGE_IDLE_TIMEOUT", "5s")
	t.Setenv("NOVA_STAGE_MAX_TOTAL_TIMEOUT", "30s")

	stage := newStageRun("test-reuse")
	defer stage.cancel()

	for i := 0; i < 2; i++ {
		cmd := ndjsonProducerCmd(t, 50*time.Millisecond, 120*time.Millisecond)
		out := runClaudeStream(&captureSink{}, cmd, "test-reuse", nil, nil, stage.hb, 0,
			withStallTimeout(codingStallTimeout), withCtx(stage.ctx))
		if out.eventCount == 0 {
			t.Fatalf("run %d counted no events", i)
		}
	}
}
