package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTestingWorkspaceDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir, err := TestingWorkspaceDir("proj_abc", "tenv_xyz")
	if err != nil {
		t.Fatalf("TestingWorkspaceDir: %v", err)
	}
	want := filepath.Join(home, ".novaworkbench", "testing", "proj_abc", "tenv_xyz")
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	// Must exist on return — the handler spawns npm with cmd.Dir = this path
	// and would fail with ENOENT otherwise.
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("workspace not created: %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("%s is not a directory", dir)
	}
}

func TestTestingWorkspaceDirRejectsTraversal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bad := []string{"", ".", "..", "../escape", "a/b", `a\b`, "x\x00y"}
	for _, id := range bad {
		if _, err := TestingWorkspaceDir(id, "tenv_1"); err == nil {
			t.Errorf("projectID %q was accepted, want an error", id)
		}
		if _, err := TestingWorkspaceDir("proj_1", id); err == nil {
			t.Errorf("envID %q was accepted, want an error", id)
		}
	}
}

// The path-traversal guard must actually keep us under the testing root —
// assert on the resolved path, not just on the error.
func TestTestingWorkspaceDirStaysUnderRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".novaworkbench", "testing")

	dir, err := TestingWorkspaceDir("proj_ok", "tenv_ok")
	if err != nil {
		t.Fatalf("TestingWorkspaceDir: %v", err)
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	if strings.HasPrefix(rel, "..") {
		t.Errorf("workspace %q escaped root %q", dir, root)
	}
	if rel != filepath.Join("proj_ok", "tenv_ok") {
		t.Errorf("rel = %q, want proj_ok/tenv_ok", rel)
	}
}

func TestTestingRootDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root, err := TestingRootDir()
	if err != nil {
		t.Fatalf("TestingRootDir: %v", err)
	}
	if want := filepath.Join(home, ".novaworkbench", "testing"); root != want {
		t.Errorf("root = %q, want %q", root, want)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("root not created: %v", err)
	}
}
