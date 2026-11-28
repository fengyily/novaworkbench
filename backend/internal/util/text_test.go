package util

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Byte-slicing a Chinese string cuts a character in half. The prompt used to
// carry invalid UTF-8 whenever a plan or an artifact digest was truncated
// mid-character — the model sees mojibake and the truncation point looks
// random.

func TestTruncateRunes_CutsOnRuneBoundary(t *testing.T) {
	s := strings.Repeat("中", 100)
	out := TruncateRunes(s, 10, "…")
	if !utf8.ValidString(out) {
		t.Fatalf("output is not valid UTF-8: %q", out)
	}
	if got := len([]rune(out)); got != 11 {
		t.Fatalf("rune count = %d, want 11 (10 + suffix)", got)
	}
}

func TestTruncateRunes_NoCutWhenShort(t *testing.T) {
	if got := TruncateRunes("短", 10, "…"); got != "短" {
		t.Fatalf("got %q", got)
	}
	if got := TruncateRunes("短", 0, "…"); got != "" {
		t.Fatalf("zero budget = %q, want empty", got)
	}
}

func TestTruncateRunesByBytes_KeepsByteBudget(t *testing.T) {
	// Each Chinese rune is 3 bytes. A 10-byte budget must not exceed 10 bytes.
	s := strings.Repeat("中", 100)
	out := TruncateRunesByBytes(s, 10, "…")
	if !utf8.ValidString(out) {
		t.Fatalf("output is not valid UTF-8: %q", out)
	}
	if len(out) > 10+len("…") {
		t.Fatalf("output is %d bytes, over budget: %q", len(out), out)
	}
	if !strings.HasSuffix(out, "…") {
		t.Fatalf("missing truncation marker: %q", out)
	}
	// 10 bytes = 3 whole runes (9 bytes) + 1 leftover byte that must be
	// dropped rather than split.
	if got := len([]rune(strings.TrimSuffix(out, "…"))); got != 3 {
		t.Fatalf("kept %d runes, want 3", got)
	}
}

func TestTruncateRunesByBytes_NoCutWhenShort(t *testing.T) {
	if got := TruncateRunesByBytes("abc", 10, "…"); got != "abc" {
		t.Fatalf("got %q", got)
	}
	if got := TruncateRunesByBytes("abc", 0, "…"); got != "" {
		t.Fatalf("zero budget = %q, want empty", got)
	}
}

func TestTruncateRunesByBytes_ASCIIBoundary(t *testing.T) {
	out := TruncateRunesByBytes("abcdef", 3, "…")
	if out != "abc…" {
		t.Fatalf("got %q, want %q", out, "abc…")
	}
}

func TestNormalizeWhitespace(t *testing.T) {
	cases := map[string]string{
		"  a  b  ":   "a b",
		"a　　b":      "a b",
		"a\n\tb":    "a b",
		"":           "",
		"single":     "single",
		"   leading": "leading",
	}
	for in, want := range cases {
		if got := NormalizeWhitespace(in); got != want {
			t.Errorf("NormalizeWhitespace(%q) = %q, want %q", in, got, want)
		}
	}
}
