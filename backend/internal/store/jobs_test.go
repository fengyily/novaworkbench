package store

import (
	"strings"
	"testing"
)

// TestCleanThinkTags covers the MiniMax-M3 stream pollution fix: the helper
// must strip the leaked internal-tag wrappers AND their surrounding
// whitespace, while leaving every other <...> construct untouched.
//
// The literal leaked-tag strings are built via string concatenation rather
// than embedded inline, so the test fixture itself doesn't depend on the
// exact zero-width / spacing convention used by the model — we explicitly
// construct the variants we want to assert against.
func TestCleanThinkTags(t *testing.T) {
	// Local aliases keep each case a one-liner and make the leakage shape
	// obvious when reading the test table.
	closeTag := "</" + "mm:think>"
	openTag := "<" + "mm:think" + ">"
	selfClose := "<" + "mm:think" + "/>"
	closeWithAttr := "</" + "mm:think id=\"x\">"

	cases := []struct {
		name string
		in   string
		want string
	}{
		// Empty input is a no-op so callers can use the empty-string signal to
		// skip a log line entirely.
		{"empty", "", ""},

		// The pollution pattern observed in the wild: a `⏺` model bullet
		// followed by nothing but stacked empty closing tags. After
		// cleaning, the content is empty → the caller drops the log line.
		{"closing-only stack", "⏺ " + closeTag + closeTag + closeTag, ""},

		// Real content with leaked open/close tags around it: the content
		// survives, the tags + surrounding whitespace do not.
		{
			name: "wrapping real text",
			in:   "hello" + openTag + " world",
			want: "hello world",
		},
		{
			name: "attribute-bearing closing tag",
			in:   openTag + "abc" + closeWithAttr,
			want: "abc",
		},

		// Tolerant of whitespace inside the tag — observed variants in the
		// wild include `<  /  mm:think  >` with spaces.
		{
			name: "whitespace inside tag",
			in:   "<  /  mm:think  >stuff",
			want: "stuff",
		},

		// Case-insensitive match (the (?i) flag covers UpperCase leaks).
		{
			name: "uppercase tag",
			in:   "<MM:THINK>x</MM:THINK>",
			want: "x",
		},

		// Self-closing variant.
		{
			name: "self-closing tag",
			in:   "before" + selfClose + "after",
			want: "beforeafter",
		},

		// Non-matches: a `<think>python` substring (different namespace) and a
		// legitimate code-like <div> construct must NOT be touched. The regex
		// uses a \b boundary so it only fires on the exact "mm:think" name.
		{
			name: "unrelated think tag untouched",
			in:   "<think>python is fun",
			want: "<think>python is fun",
		},
		{
			name: "unrelated html tag untouched",
			in:   "<div class=\"x\">data</div>",
			want: "<div class=\"x\">data</div>",
		},

		// Multi-line content with a leaked tag mid-stream.
		{
			name: "multiline with leaked tag",
			in:   "first line\n" + closeTag + "\nlast line",
			want: "first line\n\nlast line",
		},

		// Pure whitespace surviving the cleaning collapses to "" so callers
		// can detect "no real content".
		{
			name: "whitespace-only after cleaning",
			in:   "   " + openTag + "  " + closeTag + "   ",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CleanThinkTags(tc.in)
			if got != tc.want {
				t.Errorf("CleanThinkTags(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCleanThinkTagsIdempotent guards against accidentally introducing a
// multi-pass regex where the first pass unmasks a second tag. Running the
// helper twice must produce the same result as running it once.
func TestCleanThinkTagsIdempotent(t *testing.T) {
	openTag := "<" + "mm:think>"
	closeTag := "</" + "mm:think>"
	inputs := []string{
		"",
		"plain text",
		"hello" + openTag + " world" + closeTag,
		"⏺ " + closeTag + closeTag,
		strings.Repeat(openTag+closeTag, 100),
	}
	for _, in := range inputs {
		once := CleanThinkTags(in)
		twice := CleanThinkTags(once)
		if once != twice {
			t.Errorf("not idempotent for %q: once=%q twice=%q", in, once, twice)
		}
	}
}

// TestCleanThinkTagsDoesNotCorruptJSON is a defensive check for the concern
// raised in the design plan: the regex must not destroy legitimate JSON
// payloads that happen to contain angle brackets (e.g. an HTML snippet in a
// user message). The exact substring "mm:think" is the only thing that
// triggers a match, so a JSON payload with random <...> pairs passes through
// unchanged.
func TestCleanThinkTagsDoesNotCorruptJSON(t *testing.T) {
	jsonPayload := `{"type":"usage","input_tokens":42,"note":"see <foo> and <bar>"}`
	if got := CleanThinkTags(jsonPayload); got != jsonPayload {
		t.Errorf("JSON payload mutated: got=%q, want=%q", got, jsonPayload)
	}
}