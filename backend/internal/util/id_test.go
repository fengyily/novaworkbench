package util

import (
	"strings"
	"testing"
)

// A requirement ID is "<prefix>_<16 hex chars>" — 8 random bytes hex-encoded,
// so the rendered handle is 20 characters ("req_" + 16). The project-detail
// requirements table sizes its ID column from that length, and the column
// used to be laid out for 12 characters because the doc comment claimed 8
// hex digits. Lock the contract so a change to the default byte count can
// never silently shrink the handle under a fixed-width column again.
func TestNewIDShape(t *testing.T) {
	const wantHexLen = 16

	id := NewID("req")
	prefix, hex, ok := strings.Cut(id, "_")
	if !ok {
		t.Fatalf("NewID(%q) = %q, want a <prefix>_<hex> shape", "req", id)
	}
	if prefix != "req" {
		t.Errorf("prefix = %q, want %q", prefix, "req")
	}
	if len(hex) != wantHexLen {
		t.Errorf("hex part = %q (%d chars), want %d", hex, len(hex), wantHexLen)
	}
	if len(id) != len("req_")+wantHexLen {
		t.Errorf("id length = %d, want %d", len(id), len("req_")+wantHexLen)
	}
	for _, c := range hex {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("id = %q contains a non lowercase-hex rune %q", id, c)
			break
		}
	}
}

// An empty prefix yields a bare hex string — the session-token shape.
func TestNewIDEmptyPrefix(t *testing.T) {
	id := NewID("")
	if strings.Contains(id, "_") {
		t.Errorf("NewID(\"\") = %q, want no separator", id)
	}
	if len(id) != 16 {
		t.Errorf("NewID(\"\") length = %d, want 16", len(id))
	}
}

// The optional byte count overrides the default; 0 keeps the default.
func TestNewIDByteOverride(t *testing.T) {
	hexLen := func(id string) int {
		_, hex, _ := strings.Cut(id, "_")
		return len(hex)
	}
	if got := hexLen(NewID("x", 4)); got != 8 {
		t.Errorf("NewID(\"x\", 4) hex length = %d, want 8", got)
	}
	if got := hexLen(NewID("x", 0)); got != 16 {
		t.Errorf("NewID(\"x\", 0) hex length = %d, want 16 (default kept)", got)
	}
}
