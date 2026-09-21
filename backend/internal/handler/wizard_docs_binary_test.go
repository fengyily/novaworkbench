package handler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCollectProjectContext_SkipsBinaryAndKeepsText is the regression test
// for the fork/exec EINVAL bug: a pre-read file containing a NUL byte
// would propagate into the analyst prompt and kill cmd.Start() with
// EINVAL (syscall.ByteSliceFromString rejects NUL before fork). The
// binary-detection sniff in collectProjectContext must drop such files
// while leaving genuine text files (e.g. CLAUDE.md) untouched.
//
// The file names match a title keyword so they would have hit the keyword
// filter — the previous extension-based skipExts could only exclude them
// via a hardcoded extension whitelist, which misses no-extension binaries
// entirely.
func TestCollectProjectContext_SkipsBinaryAndKeepsText(t *testing.T) {
	dir := t.TempDir()

	// Genuine text config doc that MUST still appear in the prompt.
	claudeMD := "# Project\n\nThis is a normal CLAUDE.md describing the project.\n"
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(claudeMD), 0o644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}

	// No-extension, <200KB file containing a NUL byte. Its name matches the
	// title keyword "payload" below — so it would have been picked up by
	// the keyword filter pre-fix. Without the binary sniff it would be
	// embedded verbatim into the prompt and break the analyst turn with
	// EINVAL.
	binary := []byte("head\x00tail\x00more-binary-content")
	if err := os.WriteFile(filepath.Join(dir, "payload"), binary, 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	// Text file with same shape (matches keyword) — control case proving
	// we don't drop legitimate text files.
	textMatch := "package main\nfunc main() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "payload_helper.txt"), []byte(textMatch), 0o644); err != nil {
		t.Fatalf("write payload_helper.txt: %v", err)
	}

	docBlock, readFiles, _ := collectProjectContext(dir, "payload spec")

	if strings.Contains(docBlock, "\x00") {
		t.Fatalf("docBlock contains NUL byte after binary detection — would propagate to argv and EINVAL the spawn\n--- docBlock ---\n%s", docBlock)
	}

	// Binary file must be absent from readFiles.
	for _, rf := range readFiles {
		if rf == "payload" {
			t.Errorf("binary file 'payload' was included in readFiles (would inject NUL into prompt)")
		}
	}

	// docBlock must not mention the binary file at all (we drop it
	// before the markdown template wraps the content).
	if strings.Contains(docBlock, "### payload\n") {
		t.Errorf("docBlock contains a heading for the dropped binary file")
	}

	// CLAUDE.md (priority doc) MUST still be present.
	if !strings.Contains(docBlock, "CLAUDE.md") {
		t.Errorf("docBlock lost CLAUDE.md (priority doc must always be included)")
	}
	if !strings.Contains(docBlock, "normal CLAUDE.md") {
		t.Errorf("docBlock lost CLAUDE.md content")
	}

	// text-match control file SHOULD be present (proves we aren't
	// over-dropping).
	if !strings.Contains(docBlock, "payload_helper.txt") {
		t.Errorf("docBlock lost the text control file 'payload_helper.txt' — binary sniff is over-eager")
	}
}

// TestLooksBinary covers the helper directly so future regressions of the
// sniff itself are caught even if collectProjectContext's contract
// changes.
func TestLooksBinary(t *testing.T) {
	// Build a 8KB non-zero prefix to use in two boundary cases.
	prefix := make([]byte, 8*1024)
	for i := range prefix {
		prefix[i] = 'a'
	}
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"empty", []byte{}, false},
		{"plain text", []byte("hello world\n"), false},
		{"utf8 multiline", []byte("中文多行\n# heading\n"), false},
		{"single NUL head", []byte("\x00hello"), true},
		{"NUL mid-stream", append([]byte("xxxxxx"), 0), true},
		// 8KB of 'a' followed by a NUL: the sniff only inspects the first
		// 8KB, so it must NOT report this as binary (mirrors git's
		// heuristic — most large files have a NUL somewhere past the
		// head).
		{"NUL only after 8KB boundary", append(prefix, 0), false},
		{"NUL within first 8KB", append(prefix[:8*1024-1], 0, 'a', 'b'), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksBinary(tc.in); got != tc.want {
				t.Errorf("looksBinary(%d bytes) = %v, want %v", len(tc.in), got, tc.want)
			}
		})
	}
}
