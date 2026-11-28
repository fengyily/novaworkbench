package util

import (
	"strings"
	"unicode/utf8"
)

// TruncateRunes cuts s to at most maxRunes runes (not bytes) and appends the
// suffix when the cut actually happened.
//
// Byte-slicing a Chinese string cuts a character in half and emits invalid
// UTF-8 into the prompt — a class of bug that has already bitten the
// sub-task context builder. Anything that renders user- or model-authored
// Markdown into a prompt must go through a rune-aware cut.
//
// maxRunes <= 0 returns "" (an empty budget means "nothing fits").
func TruncateRunes(s string, maxRunes int, suffix string) string {
	if maxRunes <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + suffix
}

// TruncateRunesByBytes cuts s to at most maxBytes BYTES while guaranteeing the
// cut lands on a rune boundary, so the result is always valid UTF-8.
//
// It exists because several context budgets in this codebase are documented as
// *byte* budgets (e.g. the 32 KB sub-task context cap). Switching those to a
// rune budget would silently change the budget semantics; what was wrong was
// the cut point, not the unit. This helper keeps the byte accounting exact
// while fixing the mojibake.
//
// maxBytes <= 0 returns "".
func TruncateRunesByBytes(s string, maxBytes int, suffix string) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	// Walk back to the start of the rune that straddles the cut point.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	// A continuation byte at the very end means the rune *starting* before the
	// cut is incomplete; utf8.RuneStart already handles that, but guard the
	// degenerate empty result.
	if cut <= 0 {
		return suffix
	}
	return s[:cut] + suffix
}

// NormalizeWhitespace collapses every run of whitespace (including the
// full-width space U+3000) into a single ASCII space and trims the result.
//
// Used when comparing free-form text that two producers wrote slightly
// differently — e.g. a model-authored heading and a backend-authored one.
func NormalizeWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '　' {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}
