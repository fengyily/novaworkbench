package handler

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writeTranscriptFile creates a fake Claude session transcript with the given
// raw JSONL lines. Test-only helper for the transcript probe.
func writeTranscriptFile(path string, lines []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// The tests in this file pin the termination-attribution contract that
// replaces the useless "❌ Claude 异常退出: exit status 143" message.
//
// Background: the Claude CLI treats SIGTERM as a *normal* exit (code 143),
// and NovaWorkbench used to SIGTERM the process group during teardown
// BEFORE calling cmd.Wait(). Every early exit therefore reported the exit
// code WE manufactured and blamed the CLI for it.

// TestKillReason_FirstWriteWins pins the "first reason sticks" rule: the
// watchdog / guard / hard-timeout are the real causes and must not be
// overwritten by the later cleanup kill.
func TestKillReason_FirstWriteWins(t *testing.T) {
	var k claudeKillReason
	k.set(killReasonWatchdog)
	k.set(killReasonTeardown)
	k.set(killReasonHardTimeout)
	if got := k.get(); got != killReasonWatchdog {
		t.Fatalf("killReason = %q, want %q (first write must win)", got, killReasonWatchdog)
	}
}

// TestKillReason_EmptyIsIgnored pins that an empty reason never overwrites a
// real one (killReasonNone is the zero value).
func TestKillReason_EmptyIsIgnored(t *testing.T) {
	var k claudeKillReason
	k.set(killReasonHardTimeout)
	k.set(killReasonNone)
	if got := k.get(); got != killReasonHardTimeout {
		t.Fatalf("killReason = %q, want %q", got, killReasonHardTimeout)
	}
}

// TestTerminationMessage_WeKilledItIsNotACliCrash pins the headline fix: when
// WE terminated the process, the message must say so and must NOT present the
// exit code as a CLI crash.
func TestTerminationMessage_WeKilledItIsNotACliCrash(t *testing.T) {
	out := claudeStreamOutcome{
		killReason:       killReasonTeardown,
		stdoutEnded:      "eof",
		streamEventCount: 1,
		elapsed:          733 * time.Millisecond,
	}
	// exit status 143 = the CLI's normal SIGTERM exit code.
	exitErr := &exec.ExitError{}
	msg := terminationMessage(out, 20*time.Minute, exitErr, "")

	if !strings.Contains(msg, "NovaWorkbench 收尾时终止了进程组") {
		t.Errorf("message should say NovaWorkbench terminated it; got:\n%s", msg)
	}
	if strings.Contains(msg, "Claude 异常退出") {
		t.Errorf("message must not blame the CLI when we killed it; got:\n%s", msg)
	}
}

// TestTerminationMessage_CliExitedOnItsOwn pins the other half: when nobody
// killed it, the legacy "Claude 异常退出" wording must survive verbatim —
// downstream log-grep alerts key off it.
func TestTerminationMessage_CliExitedOnItsOwn(t *testing.T) {
	out := claudeStreamOutcome{
		killReason:  killReasonNone,
		stdoutEnded: "eof",
		elapsed:     2 * time.Second,
	}
	exitErr := &exec.ExitError{}
	msg := terminationMessage(out, 20*time.Minute, exitErr, "")
	if !strings.Contains(msg, "Claude 异常退出") {
		t.Errorf("message should keep the legacy wording; got:\n%s", msg)
	}
	if strings.Contains(msg, "NovaWorkbench 收尾时终止了进程组") {
		t.Errorf("message must not claim we terminated it; got:\n%s", msg)
	}
}

// TestTerminationMessage_WatchdogWins pins that the watchdog keeps its own
// wording — the UI has a dedicated banner branch for "流静默".
func TestTerminationMessage_WatchdogWins(t *testing.T) {
	out := claudeStreamOutcome{
		killReason:  killReasonWatchdog,
		stdoutEnded: "eof",
		elapsed:     20 * time.Minute,
	}
	out.stalledByWatchdog = &atomic.Bool{}
	out.stalledByWatchdog.Store(true)

	msg := terminationMessage(out, 20*time.Minute, &exec.ExitError{}, "")
	if !strings.Contains(msg, "看门狗终止") {
		t.Errorf("watchdog wording missing; got:\n%s", msg)
	}
	if strings.Contains(msg, "NovaWorkbench 收尾时终止了进程组") {
		t.Errorf("watchdog case must not use the teardown wording; got:\n%s", msg)
	}
}

// TestTerminationDiagnostics_ContainsEveryField pins the diagnostic line: it
// is the whole point of the fix — the next occurrence must be diagnosable
// from the log alone.
func TestTerminationDiagnostics_ContainsEveryField(t *testing.T) {
	out := claudeStreamOutcome{
		killReason:       killReasonTeardown,
		stdoutEnded:      "eof",
		lastEventType:    "system",
		streamEventCount: 1,
		elapsed:          733 * time.Millisecond,
	}
	line := terminationDiagnostics(out, 143, "")
	for _, want := range []string{"耗时=733ms", "事件数=1", "最后事件类型=system", "stdout结束=eof", "退出码=143", "终止原因=teardown"} {
		if !strings.Contains(line, want) {
			t.Errorf("diagnostic line missing %q; got: %s", want, line)
		}
	}
}

// TestTerminationDiagnostics_EmptyStderrIsExplicit pins that an empty stderr
// shows up as "(空)" rather than as an empty gap in the sentence.
func TestTerminationDiagnostics_EmptyStderrIsExplicit(t *testing.T) {
	line := terminationDiagnostics(claudeStreamOutcome{}, 1, "   ")
	if !strings.Contains(line, "stderr=(空)") {
		t.Errorf("empty stderr should render as (空); got: %s", line)
	}
}

// TestExitCodeOf pins the exit-code extraction: nil → 0, a non-exit error →
// -1 (unknown).
func TestExitCodeOf(t *testing.T) {
	if got := exitCodeOf(nil); got != 0 {
		t.Errorf("exitCodeOf(nil) = %d, want 0", got)
	}
	if got := exitCodeOf(errNotExit{}); got != -1 {
		t.Errorf("exitCodeOf(non-exit error) = %d, want -1", got)
	}
}

type errNotExit struct{}

func (errNotExit) Error() string { return "not an exit error" }

// TestLooksLikeForkInstantCrash pins the fork auto-fallback trigger. Every
// condition must hold; flipping any one of them must disable the retry.
func TestLooksLikeForkInstantCrash(t *testing.T) {
	base := claudeStreamOutcome{
		errMsg:           "Claude 未返回结果即被中止",
		killReason:       killReasonTeardown,
		stdoutEnded:      "eof",
		elapsed:          733 * time.Millisecond,
		sawAssistant:     false,
		streamEventCount: 1,
	}
	probe := claudeTranscriptProbe{Found: true, TotalRecords: 4}

	if !looksLikeForkInstantCrash(base, probe) {
		t.Fatalf("the base signature should trigger the fallback")
	}

	cases := []struct {
		name string
		mut  func(o *claudeStreamOutcome)
	}{
		{"有结果", func(o *claudeStreamOutcome) { o.finalResult = "done" }},
		{"没有错误信息", func(o *claudeStreamOutcome) { o.errMsg = "" }},
		{"拿到了模型输出", func(o *claudeStreamOutcome) { o.sawAssistant = true }},
		{"stdout 以 result 结束", func(o *claudeStreamOutcome) { o.stdoutEnded = "result" }},
		{"看门狗终止的", func(o *claudeStreamOutcome) { o.killReason = killReasonWatchdog }},
		{"跑了很久才失败", func(o *claudeStreamOutcome) { o.elapsed = 2 * time.Minute }},
	}
	for _, tc := range cases {
		o := base
		tc.mut(&o)
		if looksLikeForkInstantCrash(o, probe) {
			t.Errorf("%s: must NOT trigger the fork fallback", tc.name)
		}
	}
}

// TestLooksLikeForkInstantCrash_CliExitedOnItsOwnNeedsTranscriptEvidence pins
// the extra guard: when the CLI exited by itself, the transcript must prove
// that no request was ever issued — otherwise a genuine model-side error
// would be masked by a silent retry.
func TestLooksLikeForkInstantCrash_CliExitedOnItsOwnNeedsTranscriptEvidence(t *testing.T) {
	out := claudeStreamOutcome{
		errMsg:      "Claude 异常退出: exit status 1",
		killReason:  killReasonNone,
		stdoutEnded: "eof",
		elapsed:     400 * time.Millisecond,
	}

	// Transcript proves a request WAS made → not a fork crash.
	if looksLikeForkInstantCrash(out, claudeTranscriptProbe{Found: true, APIRequests: 3}) {
		t.Errorf("a run that did reach the model must not be retried as a fork crash")
	}
	// Transcript proves NO request was made → fork crash.
	if !looksLikeForkInstantCrash(out, claudeTranscriptProbe{Found: true, APIRequests: 0}) {
		t.Errorf("a run with no api-request must be retried as a fork crash")
	}
	// No transcript at all → not enough evidence → no retry.
	if looksLikeForkInstantCrash(out, claudeTranscriptProbe{Found: false}) {
		t.Errorf("without transcript evidence we must not retry")
	}
}

// TestTranscriptProbe_OnlyCountsRecordsInsideTheWindow pins the `since`
// filter — a forked session inherits the parent's whole history, so an
// unfiltered count would answer about the PARENT's traffic.
func TestTranscriptProbe_OnlyCountsRecordsInsideTheWindow(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)

	// The probe derives its path from claudeSessionHome() + the cwd slug.
	workDir := dir
	sid := "sess-1"
	path := claudeTranscriptPath(workDir, sid)
	if path == "" {
		t.Fatalf("transcript path should not be empty")
	}
	if err := writeTranscriptFile(path, []string{
		`{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"content":"parent history"}}`,
		`{"type":"api-request","timestamp":"2026-01-01T00:00:01Z"}`,
		`{"type":"mode","timestamp":"2026-01-01T00:10:00Z"}`,
		`{"type":"assistant","timestamp":"2026-01-01T00:10:01Z"}`,
	}); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	since := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	p := probeClaudeTranscript(workDir, sid, since)
	if !p.Found {
		t.Fatalf("probe should find the transcript")
	}
	if p.TotalRecords != 2 {
		t.Errorf("TotalRecords = %d, want 2 (records after the window start)", p.TotalRecords)
	}
	if p.APIRequests != 0 {
		t.Errorf("APIRequests = %d, want 0 (the parent's request is outside the window)", p.APIRequests)
	}
	if p.Assistants != 1 {
		t.Errorf("Assistants = %d, want 1", p.Assistants)
	}
	if p.RequestLeftMachine() {
		t.Errorf("RequestLeftMachine must be false when no request was made in the window")
	}
}

// TestTranscriptProbe_MissingFile pins that a missing transcript is not a
// crash — it just means "no evidence".
func TestTranscriptProbe_MissingFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	p := probeClaudeTranscript(t.TempDir(), "nope", time.Time{})
	if p.Found {
		t.Errorf("Found should be false for a missing transcript")
	}
	if p.RequestLeftMachine() {
		t.Errorf("RequestLeftMachine must be false when there is no evidence")
	}
	if !strings.Contains(p.Describe(), "未找到") {
		t.Errorf("Describe should mention the transcript was not found; got: %s", p.Describe())
	}
}
