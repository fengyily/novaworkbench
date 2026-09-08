package service

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCapTitleRuneSafe locks down the regression behind the
// "DB_ERROR: insert adjustment sub_task: invalid byte sequence for encoding
// \"UTF8\": 0xef" symptom. Byte-slicing at 80 landed mid-rune whenever the
// prefix + parent title contained ASCII mixed with CJK / fullwidth punctuation.
func TestCapTitleRuneSafe(t *testing.T) {
	// Case 1 — short input is returned unchanged.
	if got := capTitle("hello", 80); got != "hello" {
		t.Errorf("short unchanged: got %q", got)
	}

	// Case 2 — the exact prefix + parent title shape from CreateAdjustment.
	// Parent title length 72 ASCII bytes plus the 8-byte "调整: " prefix
	// gives 80 bytes total, so without the fix len > 80 was false and the
	// bug didn't fire. Add a single fullwidth colon to push past the
	// threshold; without the rune-safe cap, [:80] lands on 0xEF.
	parent := strings.Repeat("a", 72) + "：补充"
	adjust := capTitle("调整: "+parent, 80)
	if !utf8.ValidString(adjust) {
		t.Errorf("adjust title not valid UTF-8: % x", adjust)
	}
	if utf8.RuneCountInString(adjust) > 80 {
		t.Errorf("adjust title exceeded rune cap: %d runes", utf8.RuneCountInString(adjust))
	}

	// Case 3 — Create path with a runaway user-supplied title. Pure-CJK
	// 100-rune input must come out at exactly 80 runes, byte-length
	// therefore up to 240 (which still fits TEXT on all three dialects).
	cjk := strings.Repeat("开", 100)
	got := capTitle(cjk, 80)
	if utf8.RuneCountInString(got) != 80 {
		t.Errorf("cjk cap: got %d runes, want 80", utf8.RuneCountInString(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("cjk cap produced invalid UTF-8: % x", got)
	}
}