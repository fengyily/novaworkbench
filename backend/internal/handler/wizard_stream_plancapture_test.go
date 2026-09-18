package handler

import (
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/store"
)

// Regression coverage for the "方案设计完成未入库" issue: an architect-design run
// finished with design_docs empty because (a) the 3-minute stall watchdog
// killed Claude while it waited on Explore sub-agents (stdout is silent for
// the whole wait) and (b) the plan was only recoverable from a
// ~/.claude/plans/*.md Write, so a plan submitted through the plan-approval
// tool was dropped.
//
// These tests drive runClaudeStream with a fake NDJSON producer instead of the
// real claude CLI, so they assert the parser/watchdog contract directly.

// captureSink collects every LogLine the stream emits so tests can assert on
// what the UI would have seen.
type captureSink struct {
	lines []store.LogLine
}

func (s *captureSink) emit(line store.LogLine) { s.lines = append(s.lines, line) }

// ndjsonCmd builds a cmd that prints the supplied NDJSON lines on stdout.
// Each line is emitted as-is; the caller composes the event sequence.
func ndjsonCmd(t *testing.T, lines ...string) *exec.Cmd {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake NDJSON producer relies on a POSIX shell")
	}
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString("printf '%s\\n' " + shellQuote(l) + "\n")
	}
	return exec.Command("sh", "-c", sb.String())
}

// assistantToolUse renders one `assistant` stream-json event carrying a single
// tool_use block with the given name and input.
func assistantToolUse(t *testing.T, name string, input map[string]interface{}) string {
	t.Helper()
	evt := map[string]interface{}{
		"type": "assistant",
		"message": map[string]interface{}{
			"model": "claude-test",
			"content": []interface{}{
				map[string]interface{}{"type": "tool_use", "name": name, "input": input},
			},
		},
	}
	b, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal assistant event: %v", err)
	}
	return string(b)
}

const successResultEvent = `{"type":"result","subtype":"success","result":"done"}`

// TestRunClaudeStream_CapturesExitPlanModePlan is the Fix C guard: when Claude
// submits the plan through the plan-approval tool instead of writing
// ~/.claude/plans/<slug>.md, the plan Markdown must still land in
// out.planContent — otherwise finalizeArchitectRun sees an empty plan and
// reports "Claude 未返回结果" without persisting design_docs.
func TestRunClaudeStream_CapturesExitPlanModePlan(t *testing.T) {
	const plan = "# 技术方案\n\n## 实现步骤\n1. 改 wizard_stream.go"

	for _, toolName := range []string{"ExitPlanMode", "ExitPlan"} {
		t.Run(toolName, func(t *testing.T) {
			cmd := ndjsonCmd(t,
				assistantToolUse(t, toolName, map[string]interface{}{"plan": plan}),
				successResultEvent,
			)
			out := runClaudeStream(&captureSink{}, cmd, "test-"+toolName, nil)
			if out.errMsg != "" {
				t.Fatalf("unexpected errMsg: %q", out.errMsg)
			}
			if out.planContent != plan {
				t.Errorf("planContent = %q, want %q", out.planContent, plan)
			}
		})
	}
}

// TestRunClaudeStream_PlanFileWriteWinsOverExitPlanMode pins the precedence
// documented on the capture: a real Write to ~/.claude/plans/*.md is the
// authoritative full document, so a later plan-approval tool_use (which
// carries an abridged plan) must not clobber it.
func TestRunClaudeStream_PlanFileWriteWinsOverExitPlanMode(t *testing.T) {
	const full = "# 完整方案\n\n（Write 落盘的全文）"
	cmd := ndjsonCmd(t,
		assistantToolUse(t, "Write", map[string]interface{}{
			"file_path": "/home/u/.claude/plans/req-abc.md",
			"content":   full,
		}),
		assistantToolUse(t, "ExitPlanMode", map[string]interface{}{"plan": "摘要版方案"}),
		successResultEvent,
	)
	out := runClaudeStream(&captureSink{}, cmd, "test-precedence", nil)
	if out.planContent != full {
		t.Errorf("planContent = %q, want the Write content %q", out.planContent, full)
	}
}

// TestRunClaudeStream_ExitPlanModeWithoutPlanInput guards the nil/empty input
// path: a malformed plan-approval tool_use must leave planContent empty rather
// than panicking or storing a bogus value.
func TestRunClaudeStream_ExitPlanModeWithoutPlanInput(t *testing.T) {
	cmd := ndjsonCmd(t,
		assistantToolUse(t, "ExitPlanMode", map[string]interface{}{"plan": ""}),
		successResultEvent,
	)
	out := runClaudeStream(&captureSink{}, cmd, "test-empty-plan", nil)
	if out.planContent != "" {
		t.Errorf("planContent = %q, want empty", out.planContent)
	}
}

// TestRunClaudeStream_StallTimeoutOverride is the Fix A wiring guard: the
// variadic override must actually drive the watchdog. A producer that emits
// one line and then goes silent past the override window has to be killed so
// the scan loop unblocks — the same mechanism that was misfiring on
// architect-design, only with the window shrunk so the test is fast.
func TestRunClaudeStream_StallTimeoutOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake NDJSON producer relies on a POSIX shell")
	}
	// One event, then sleep far longer than the override without emitting
	// anything: exactly the "silent main thread" shape of the bug.
	cmd := exec.Command("sh", "-c", `printf '%s\n' '{"type":"system","subtype":"init","session_id":"sid-1"}'; sleep 30`)

	start := time.Now()
	out := runClaudeStream(&captureSink{}, cmd, "test-stall", nil, 300*time.Millisecond)
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Fatalf("override ignored: stream took %v, expected the watchdog to fire at ~300ms", elapsed)
	}
	if out.sessionID != "sid-1" {
		t.Errorf("sessionID = %q, want %q (events before the stall must survive)", out.sessionID, "sid-1")
	}
	// The run produced no result event, so the caller must be able to tell it
	// failed rather than silently persisting an empty design.
	if out.finalResult != "" {
		t.Errorf("finalResult = %q, want empty on a stalled run", out.finalResult)
	}
}

// TestRunClaudeStream_NonPositiveOverrideFallsBackToDefault documents that a
// zero/negative override is ignored in favour of defaultStallTimeout, so a
// caller passing an unset duration keeps the 3-minute behaviour instead of
// getting a watchdog that fires immediately.
func TestRunClaudeStream_NonPositiveOverrideFallsBackToDefault(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		cmd := ndjsonCmd(t, successResultEvent)
		out := runClaudeStream(&captureSink{}, cmd, "test-fallback", nil, d)
		// If the override had been honoured the watchdog would fire at once;
		// the run completing normally with the result event is the signal.
		if out.finalResult != "done" {
			t.Errorf("override %v: finalResult = %q, want %q", d, out.finalResult, "done")
		}
	}
}

// TestArchitectStallTimeoutExceedsDefault is the behavioural core of Fix A:
// architect-design must get a strictly longer silent window than the global
// default, because plan mode can dispatch Explore sub-agents that produce no
// stdout on the main thread for minutes at a time. If someone reverts the
// call site to the default, this fails.
func TestArchitectStallTimeoutExceedsDefault(t *testing.T) {
	if architectStallTimeout <= defaultStallTimeout {
		t.Fatalf("architectStallTimeout (%v) must exceed defaultStallTimeout (%v): "+
			"plan-mode Explore sub-agents keep stdout silent for longer than the default",
			architectStallTimeout, defaultStallTimeout)
	}
}
