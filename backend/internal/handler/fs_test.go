package handler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestFsHandler builds an FsHandler with a stubbed LocalPathProvider that
// returns the given paths. No DB needed.
func newTestFsHandler(localPaths []string) *FsHandler {
	paths := localPaths
	return NewFsHandler(func() []string { return paths })
}

// TestResolveAllowedPath_UnderWorkspace: ~/workspace/** should pass when
// $HOME/workspace exists on the test host.
func TestResolveAllowedPath_UnderWorkspace(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir available")
	}
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	defer os.RemoveAll(workspace)

	h := newTestFsHandler(nil)
	got, errCode, errMsg := h.resolveAllowedPath("~/workspace")
	if errCode != "" {
		t.Fatalf("unexpected error: %s %s", errCode, errMsg)
	}
	if !strings.HasPrefix(got, workspace) {
		t.Errorf("got %q, want under %q", got, workspace)
	}
}

// TestResolveAllowedPath_EtcRejected: /etc must always be rejected.
func TestResolveAllowedPath_EtcRejected(t *testing.T) {
	h := newTestFsHandler(nil)
	_, errCode, errMsg := h.resolveAllowedPath("/etc")
	if errCode != "PATH_NOT_ALLOWED" {
		t.Errorf("got errCode=%q msg=%q, want PATH_NOT_ALLOWED", errCode, errMsg)
	}
}

// TestResolveAllowedPath_DotDotEscape: ~/workspace/../../etc must be rejected.
func TestResolveAllowedPath_DotDotEscape(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir available")
	}
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	defer os.RemoveAll(workspace)

	h := newTestFsHandler(nil)
	_, errCode, _ := h.resolveAllowedPath("~/workspace/../../etc")
	if errCode != "PATH_NOT_ALLOWED" {
		t.Errorf("got errCode=%q, want PATH_NOT_ALLOWED", errCode)
	}
}

// TestResolveAllowedPath_ProjectLocalPath: paths inside a registered project
// local_path should pass; sibling paths should fail.
func TestResolveAllowedPath_ProjectLocalPath(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projA")
	if err := os.MkdirAll(proj, 0755); err != nil {
		t.Fatalf("mkdir proj: %v", err)
	}
	h := newTestFsHandler([]string{proj})

	// Inside: pass
	if _, ec, em := h.resolveAllowedPath(proj); ec != "" {
		t.Errorf("expected %q to pass, got %s %s", proj, ec, em)
	}
	sub := filepath.Join(proj, "sub")
	if _, ec, em := h.resolveAllowedPath(sub); ec != "" {
		t.Errorf("expected %q to pass, got %s %s", sub, ec, em)
	}
	// Sibling: fail
	projB := filepath.Join(dir, "projB")
	if _, ec, _ := h.resolveAllowedPath(projB); ec != "PATH_NOT_ALLOWED" {
		t.Errorf("expected %q to be rejected as PATH_NOT_ALLOWED, got %s", projB, ec)
	}
}

// TestResolveAllowedPath_SymlinkEscape: a symlink inside ~/workspace pointing
// at /etc must be rejected after EvalSymlinks resolution.
func TestResolveAllowedPath_SymlinkEscape(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir available")
	}
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	defer os.RemoveAll(workspace)

	link := filepath.Join(workspace, "evil-link")
	if err := os.Symlink("/etc", link); err != nil {
		t.Skipf("cannot create symlink (likely Windows): %v", err)
	}

	h := newTestFsHandler(nil)
	_, errCode, _ := h.resolveAllowedPath(link)
	if errCode != "PATH_NOT_ALLOWED" {
		t.Errorf("expected symlink escape to be rejected, got errCode=%q", errCode)
	}
}

// TestResolveAllowedPath_NulByte: a NUL byte in path must be rejected.
func TestResolveAllowedPath_NulByte(t *testing.T) {
	h := newTestFsHandler(nil)
	_, errCode, _ := h.resolveAllowedPath("/etc\x00/passwd")
	if errCode != "INVALID_PATH" {
		t.Errorf("got errCode=%q, want INVALID_PATH", errCode)
	}
}