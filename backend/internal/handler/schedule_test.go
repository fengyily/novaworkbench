package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// Regression: when the frontend posts a datetime-local string like
// "2026-09-07T23:30" from a CST (UTC+8) user to a server running in UTC
// (the Docker default), the old parseRunAt parsed the bare string as
// *server local* and stored 2026-09-07 23:30 UTC, which the user then
// saw as 2026-09-08 07:30 CST — the "9-7 23:30 变成 9-8 7:40" bug.
//
// The fix is two-sided:
//   1. The frontend now sends an RFC3339 string with the user's offset
//      (e.g. "2026-09-07T23:30:00+08:00"). The absolute moment stored
//      should match what the user typed in their local time.
//   2. parseRunAt must try RFC3339 first, since that's the unambiguous
//      representation. The bare datetime-local fallback is kept only for
//      backward compat and is documented as server-local.
//
// This test pins down both: parseRunAt preserves the absolute moment of
// an RFC3339 input (offset and all), and round-tripping through JSON
// (the same code path ScheduledTask.run_at travels back to the browser)
// keeps the offset so the frontend's `new Date(task.run_at)` lands on
// the original local time.

func TestParseRunAt_RFC3339WithOffset_IsUnaffectedByServerTZ(t *testing.T) {
	// Pin server TZ to UTC so the test is deterministic regardless of
	// the dev box's local time.
	prev := time.Local
	time.Local = time.UTC
	defer func() { time.Local = prev }()

	// User in CST+8 picks "9-7 23:30" in the picker; frontend converts
	// to "...T23:30:00+08:00" and POSTs that.
	const userInput = "2026-09-07T23:30:00+08:00"
	got, err := parseRunAt(userInput)
	if err != nil {
		t.Fatalf("parseRunAt(%q) error: %v", userInput, err)
	}

	// Absolute moment must match 23:30 CST = 15:30 UTC, NOT 23:30 UTC.
	wantUTC := time.Date(2026, 9, 7, 15, 30, 0, 0, time.UTC)
	if !got.Equal(wantUTC) {
		t.Fatalf("parseRunAt(%q) = %s, want %s (CST 23:30 → UTC 15:30)", userInput, got, wantUTC)
	}

	// JSON round-trip (the same way ScheduledTask.run_at reaches the
	// browser) must keep the +08:00 offset — not collapse to "Z". If the
	// offset collapses, the frontend's `new Date(...)` would re-shift
	// the moment back to server local and re-introduce the bug.
	b, err := json.Marshal(model.ScheduledTask{RunAt: got})
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}
	var roundTripped model.ScheduledTask
	if err := json.Unmarshal(b, &roundTripped); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}
	if !roundTripped.RunAt.Equal(wantUTC) {
		t.Fatalf("round-trip moment = %s, want %s", roundTripped.RunAt, wantUTC)
	}
	if got.Location().String() == "UTC" {
		t.Fatalf("parseRunAt(%q) lost the +08:00 offset (location = UTC); "+
			"the frontend needs the offset to land on the user's local time", userInput)
	}
}

func TestParseRunAt_DateTimeLocal_FallsBackToServerLocal(t *testing.T) {
	// Bare datetime-local is the legacy path. We keep it as server-local
	// for backward compatibility, but the new RFC3339 path is the one
	// the frontend uses. This test just locks the legacy behavior so we
	// notice if anyone changes it accidentally.
	prev := time.Local
	time.Local = time.UTC
	defer func() { time.Local = prev }()

	got, err := parseRunAt("2026-09-07T23:30")
	if err != nil {
		t.Fatalf("parseRunAt error: %v", err)
	}
	want := time.Date(2026, 9, 7, 23, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("parseRunAt bare datetime-local = %s, want %s (server-local)", got, want)
	}
}

func TestParseRunAt_RejectsGarbage(t *testing.T) {
	if _, err := parseRunAt("not-a-date"); err == nil {
		t.Fatalf("parseRunAt should reject non-date input")
	}
}