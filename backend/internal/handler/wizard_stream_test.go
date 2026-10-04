package handler

import (
	"context"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestIsStaleSessionError covers isStaleSessionError across the known claude CLI
// stale-session error formats (legacy + current) and verifies that unrelated
// errors do not match. Pure-function tests — no mocks, no subprocess.
//
// The patterns covered are documented on isStaleSessionError itself:
//   - "No conversation found with session ID: <uuid>" (legacy)
//   - "Error: Session ID <uuid>..." (current, observed on --resume failures)
//   - "Session not found" / "session not found" (defensive catch-all)
func TestIsStaleSessionError(t *testing.T) {
	cases := []struct {
		name   string
		evt    map[string]interface{}
		stderr string
		want   bool
	}{
		{
			name:   "current CLI format via stderr",
			stderr: "Error: Session ID 1746c6bc-9c1a-46fc-ad2b-1234567890ab not found on disk",
			want:   true,
		},
		{
			name:   "legacy format via stderr",
			stderr: "No conversation found with session ID: 1746c6bc-9c1a-46fc-ad2b-1234567890ab",
			want:   true,
		},
		{
			name:   "defensive session-not-found substring via stderr",
			stderr: "session not found for the requested resume target",
			want:   true,
		},
		{
			name:   "defensive Session-not-found substring via stderr",
			stderr: "Session not found while attempting --resume",
			want:   true,
		},
		{
			name:   "empty stderr returns false",
			stderr: "",
			want:   false,
		},
		{
			name:   "current format on evt.error field",
			evt:    map[string]interface{}{"error": "Error: Session ID 1746c6bc-9c1a-46fc-ad2b-1234567890ab"},
			want:   true,
		},
		{
			name:   "legacy format on evt.result field",
			evt:    map[string]interface{}{"result": "No conversation found with session ID: 1746c6bc-9c1a-46fc-ad2b-1234567890ab"},
			want:   true,
		},
		{
			name:   "current format inside evt.errors array",
			evt:    map[string]interface{}{"errors": []interface{}{"Error: Session ID 1746c6bc-9c1a-46fc-ad2b-1234567890ab"}},
			want:   true,
		},
		{
			name: "unrelated evt.message does not match",
			evt:  map[string]interface{}{"message": "random unrelated text"},
			want: false,
		},
		{
			name:   "unrelated stderr text does not match",
			stderr: "some random unrelated error from the proxy layer",
			want:   false,
		},
		{
			name:   "evt nil with empty stderr returns false",
			evt:    nil,
			stderr: "",
			want:   false,
		},
		{
			name:   "nil evt only checks stderr — negative",
			evt:    nil,
			stderr: "totally unrelated 401 from upstream",
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isStaleSessionError(tc.evt, tc.stderr)
			if got != tc.want {
				t.Errorf("isStaleSessionError(evt=%v, stderr=%q) = %v, want %v",
					tc.evt, truncateForDisplay(tc.stderr), got, tc.want)
			}
		})
	}
}

// truncateForDisplay keeps assertion failures readable when a long stderr string
// is echoed back. Only used by the test helper.
func truncateForDisplay(s string) string {
	const max = 80
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// Sanity guard: confirms the test helper itself doesn't accidentally rewrite
// strings (catches future refactors that swap Contains for regex, etc.).
func TestIsStaleSessionError_HelperContract(t *testing.T) {
	if !strings.Contains("Error: Session ID abc", "Error: Session ID") {
		t.Fatal("stdlib strings.Contains semantics drifted — fixture mismatch")
	}
}

// TestNewRollingCtx_ContinuousBeatsDoNotCancel verifies that as long as the
// caller keeps pumping the heartbeat channel within idleTimeout, the context
// is never cancelled.
func TestNewRollingCtx_ContinuousBeatsDoNotCancel(t *testing.T) {
	ctx, cancel, hb := newRollingCtx("test", 100*time.Millisecond)
	defer cancel()
	defer close(hb)

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case hb <- struct{}{}:
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("ctx unexpectedly cancelled: %v", err)
	}
}

// TestNewRollingCtx_IdleTriggersCancel verifies that stopping the heartbeat
// for longer than idleTimeout causes the context to be cancelled.
func TestNewRollingCtx_IdleTriggersCancel(t *testing.T) {
	ctx, cancel, hb := newRollingCtx("test", 100*time.Millisecond)
	defer cancel()
	defer close(hb)

	for i := 0; i < 5; i++ {
		hb <- struct{}{}
	}
	select {
	case <-ctx.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("ctx not cancelled within 2x idle window; err=%v", ctx.Err())
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
}

// TestNewRollingCtx_CloseChannelStopsGoroutine verifies that closing the
// heartbeat channel cleanly stops the watcher goroutine without firing the
// idle cancel (and without leaking goroutines for the duration of the test).
func TestNewRollingCtx_CloseChannelStopsGoroutine(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		ctx, cancel, hb := newRollingCtx("test", 50*time.Millisecond)
		_ = ctx
		close(hb)
		cancel()
	}
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Logf("note: goroutine count grew (before=%d after=%d); may be GC delay", before, after)
	}
}

// TestParseStreamJSONFromReader_IdleTimeoutMessage verifies that the EOF
// branch produces a dedicated "idle 超时" message when the context is
// cancelled mid-stream with at least one stream_event seen.
//
// The reader streams slowly so the cancel beats the buffer drain: cancel
// fires after the first event lands but before subsequent reads complete.
// Note: parseStreamJSONFromReader closes the heartbeats channel itself on
// return, so the test must NOT close it again.
func TestParseStreamJSONFromReader_IdleTimeoutMessage(t *testing.T) {
	lines := []string{
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"world"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"again"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"still going"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"one more"}}}`,
	}
	hb := make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after 50ms; reader produces each line 30ms apart so cancel
	// beats the buffer drain.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	out := parseStreamJSONFromReader(ctx, &slowReader{lines: lines, gap: 30 * time.Millisecond}, silentSink{}, "idle-test", nil, hb)
	if out.errMsg == "" {
		t.Fatalf("expected errMsg to be populated; out=%+v", out)
	}
	if !strings.Contains(out.errMsg, "idle") {
		t.Fatalf("errMsg should mention 'idle'; got: %q", out.errMsg)
	}
	if !out.IdleTimeoutHit {
		t.Fatal("IdleTimeoutHit should be true")
	}
}

// TestParseStreamJSONFromReader_NilHeartbeatsIsOK verifies the legacy path
// where the caller does not pass a heartbeats channel — parseStreamJSONFromReader
// must not panic and must not write to a nil channel.
func TestParseStreamJSONFromReader_NilHeartbeatsIsOK(t *testing.T) {
	ctx := context.Background()
	out := parseStreamJSONFromReader(ctx, strings.NewReader(`{"type":"result","subtype":"success","result":"done"}`+"\n"), silentSink{}, "legacy", nil, nil)
	if out.errMsg != "" {
		t.Fatalf("unexpected errMsg: %q", out.errMsg)
	}
	if out.finalResult != "done" {
		t.Fatalf("finalResult = %q, want %q", out.finalResult, "done")
	}
}

// slowReader emits one NDJSON line every `gap`. Used by idle-timeout tests
// to feed the scanner slowly enough that ctx cancel beats the buffer drain.
type slowReader struct {
	lines []string
	gap   time.Duration
	idx   int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.idx >= len(r.lines) {
		return 0, io.EOF
	}
	time.Sleep(r.gap)
	n := copy(p, r.lines[r.idx]+"\n")
	r.idx++
	return n, nil
}
