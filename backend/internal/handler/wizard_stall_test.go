package handler

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestResolveStallTimeout pins the four documented branches of the variadic
// override resolver: missing → default; explicit zero / negative → default;
// positive override → that value. Matches the contract baked into the
// plan-agent's variadic design: non-positive overrides and extra elements
// are ignored so callers can't accidentally request a 0-second watchdog.
func TestResolveStallTimeout(t *testing.T) {
	cases := []struct {
		name string
		in   []time.Duration
		want time.Duration
	}{
		{"nil override falls back to default", nil, defaultStallTimeout},
		{"zero override falls back to default", []time.Duration{0}, defaultStallTimeout},
		{"negative override falls back to default", []time.Duration{-1 * time.Second}, defaultStallTimeout},
		{"positive override wins", []time.Duration{20 * time.Minute}, 20 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveStallTimeout(tc.in)
			if got != tc.want {
				t.Fatalf("resolveStallTimeout(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestStallErrorMessage_Watchdog verifies the watchdog-fired branch returns
// the user-facing "流静默超时" message and includes the effective window so
// the user can see how long the watchdog waited before pulling the trigger.
func TestStallErrorMessage_Watchdog(t *testing.T) {
	msg := stallErrorMessage(true, 20*time.Minute, errors.New("signal: terminated"), "")
	if !strings.HasPrefix(msg, "Claude 流静默超过 20m0s") {
		t.Fatalf("expected stalled prefix, got %q", msg)
	}
	if !strings.Contains(msg, "看门狗终止") {
		t.Fatalf("expected '看门狗终止' in message, got %q", msg)
	}
}

// TestStallErrorMessage_GenericExit verifies that when the watchdog did NOT
// fire, the legacy "异常退出" message is preserved verbatim — including
// formatting through err.Error() — so existing user-facing logs / search
// queries don't break.
func TestStallErrorMessage_GenericExit(t *testing.T) {
	msg := stallErrorMessage(false, 3*time.Minute, errors.New("signal: terminated"), "")
	want := "Claude 异常退出: signal: terminated"
	if msg != want {
		t.Fatalf("got %q, want %q", msg, want)
	}
}

// TestStallErrorMessage_StderrTakesPrecedence mirrors the legacy branch in
// runClaudeStream where a non-empty stderr trim shadows the err.Error()
// string — the watchdog-fired path should never see this branch, but we pin
// the behavior so a future refactor doesn't silently drop it.
func TestStallErrorMessage_StderrTakesPrecedence(t *testing.T) {
	msg := stallErrorMessage(false, 3*time.Minute, errors.New("signal: terminated"), "custom stderr from CLI")
	want := "Claude 异常退出: custom stderr from CLI"
	if msg != want {
		t.Fatalf("got %q, want %q", msg, want)
	}
}

// TestRunStallWatchdog_FiresOnArmedSilence verifies the production failure
// mode the watchdog is meant to catch: a run that produced at least one
// stdout line (arming the watchdog) and then went silent for the full
// stallTimeout. The killFn must fire, and the returned *atomic.Bool must
// flip to true. 50ms keeps the test fast while still leaving enough slack
// for CI scheduler jitter.
//
// Note: runStallWatchdog only "arms" after the first heartbeat (matching
// the inline implementation the helper replaced — see wizard_stream.go).
// We prime it with one beat before going silent so the test exercises the
// real "stream went silent after producing events" path.
func TestRunStallWatchdog_FiresOnArmedSilence(t *testing.T) {
	heartbeats := make(chan struct{}, 1)
	killed := atomic.Bool{}
	stalled := runStallWatchdog("test", 50*time.Millisecond, heartbeats, func() { killed.Store(true) })

	// Prime the watchdog so it arms and starts counting from now.
	heartbeats <- struct{}{}

	// Give the goroutine a moment past the timer window.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if killed.Load() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !killed.Load() {
		t.Fatalf("killFn was not invoked within 500ms (stallTimeout=50ms)")
	}
	if stalled == nil || !stalled.Load() {
		t.Fatalf("stalled flag should be true after kill")
	}
}

// TestRunStallWatchdog_DoesNotFireWithBeats verifies that steady beats reset
// the timer and the watchdog never fires within the observation window.
// This is the core guarantee that prevents the watchdog from pre-empting
// a healthy long-running job.
func TestRunStallWatchdog_DoesNotFireWithBeats(t *testing.T) {
	heartbeats := make(chan struct{}, 1)
	killed := atomic.Bool{}
	stalled := runStallWatchdog("test", 50*time.Millisecond, heartbeats, func() { killed.Store(true) })

	// Beat every 20ms for 250ms — well past the 50ms window several times over.
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		<-ticker.C
		select {
		case heartbeats <- struct{}{}:
		default:
		}
	}
	if killed.Load() {
		t.Fatalf("killFn should not fire while beats keep coming")
	}
	if stalled != nil && stalled.Load() {
		t.Fatalf("stalled flag should remain false while beats keep coming")
	}
	// Stop the watchdog cleanly so the goroutine returns before the test ends.
	close(heartbeats)
}
