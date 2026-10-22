package service

import (
	"path/filepath"
	"strings"
	"testing"
)

// ServeArtifact joins a URL-supplied path onto the run directory and serves
// the result. That join is the whole attack surface, so these cases are the
// security contract of the endpoint, not incidental coverage.

func TestSafeRelativePathRejectsTraversal(t *testing.T) {
	runDir := filepath.Join("/ws", "runs", "trun_1")

	allowed := []struct{ in, want string }{
		{"artifacts/shot.png", "artifacts/shot.png"},
		{"artifacts/sub/trace.zip", "artifacts/sub/trace.zip"},
		{"report.json", "report.json"},
	}
	for _, tc := range allowed {
		got, ok := SafeRelativePath(tc.in, runDir)
		if !ok {
			t.Errorf("SafeRelativePath(%q) rejected a legitimate path", tc.in)
			continue
		}
		if got != tc.want {
			t.Errorf("SafeRelativePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	denied := []string{
		"../secrets.txt",
		"../../../../etc/passwd",
		"artifacts/../../../../etc/passwd",
		"..",
		"/etc/passwd",
		"",
	}
	for _, in := range denied {
		if got, ok := SafeRelativePath(in, runDir); ok {
			t.Errorf("SafeRelativePath(%q) = %q, want it REJECTED", in, got)
		}
	}
}

// A sibling directory sharing a name prefix is the case a strings.HasPrefix
// guard silently allows.
func TestSafeRelativePathRejectsPrefixLookalike(t *testing.T) {
	runDir := filepath.Join("/ws", "runs", "trun_1")
	if got, ok := SafeRelativePath(filepath.Join("/ws", "runs", "trun_1-evil", "x.png"), runDir); ok {
		t.Errorf("SafeRelativePath allowed a prefix-lookalike path: %q", got)
	}
}

// Normalizing first means "a/./b" and "a//b" resolve to the same file rather
// than 404ing for a path the report legitimately produced.
func TestSafeRelativePathNormalizes(t *testing.T) {
	runDir := filepath.Join("/ws", "runs", "trun_1")
	got, ok := SafeRelativePath("artifacts/./sub//shot.png", runDir)
	if !ok {
		t.Fatal("a normalizable path was rejected")
	}
	if strings.Contains(got, "//") || strings.Contains(got, "/./") {
		t.Errorf("= %q, want a normalized path", got)
	}
	if !strings.HasSuffix(got, "shot.png") {
		t.Errorf("= %q", got)
	}
}

// A backslash separator must not become a bypass on the platforms where it
// is a path separator.
func TestSafeRelativePathBackslash(t *testing.T) {
	runDir := filepath.Join("/ws", "runs", "trun_1")
	if got, ok := SafeRelativePath(`..\..\..\etc\passwd`, runDir); ok {
		t.Errorf("SafeRelativePath allowed a backslash traversal: %q", got)
	}
}
