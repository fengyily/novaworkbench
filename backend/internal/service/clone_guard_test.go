package service

import (
	"os"
	"path/filepath"
	"testing"
)

// Tests for the clone-failure cleanup guard.
//
// Background (2026-10-24): cloneRepo unconditionally ran `os.RemoveAll(dest)`
// whenever `git clone` failed. `dest` is either a project's local_path (a live
// checkout with the user's work in it) or <home>/workspace/<repo>. During a
// github.com outage that turned "the clone could not reach the remote" into
// "your working copy is gone". The guard now requires all of:
//   - the directory did not exist before this call,
//   - it contains nothing but the partial .git/ a failed clone leaves behind,
//   - it lives strictly inside <home>/workspace.
//
// These tests pin each condition independently.

// TestRepoName_RejectsTraversalSentinels: repoName feeds
// filepath.Join(home, "workspace", repoName(url)), and Join CLEANS its result.
// "." would collapse to the workspace root and ".." to $HOME — both must be
// rejected rather than silently yielding a path that later gets cloned into
// and, on failure, removed.
func TestRepoName_RejectsTraversalSentinels(t *testing.T) {
	bad := []string{"", ".", "..", "../..", "owner/..", "a\\b"}
	for _, url := range bad {
		if got := repoName(url); got != "" {
			t.Errorf("repoName(%q) = %q, want \"\" (unsafe)", url, got)
		}
	}

	good := map[string]string{
		"git@github.com:VivalinkCloud/vivalink-sdk.git": "vivalink-sdk",
		"https://github.com/o/repo.git":                 "repo",
		"https://github.com/o/repo/":                    "repo",
		"https://github.com/o/repo":                     "repo",
	}
	for url, want := range good {
		if got := repoName(url); got != want {
			t.Errorf("repoName(%q) = %q, want %q", url, got, want)
		}
	}
}

// TestIsHalfClonedTree distinguishes the wreckage of a failed clone from a
// directory that holds real work.
func TestIsHalfClonedTree(t *testing.T) {
	t.Run("partial git dir only", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if !isHalfClonedTree(dir) {
			t.Error("a directory containing only .git/ is a half-clone")
		}
	})

	t.Run("git dir plus empty worktree dirs", func(t *testing.T) {
		dir := t.TempDir()
		for _, p := range []string{".git", "sub"} {
			if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if !isHalfClonedTree(dir) {
			t.Error("empty directories alongside .git/ are still a half-clone")
		}
	})

	t.Run("real checkout is not a half-clone", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if isHalfClonedTree(dir) {
			t.Error("a directory with source files must never be treated as a half-clone")
		}
	})

	t.Run("missing dir", func(t *testing.T) {
		if isHalfClonedTree(filepath.Join(t.TempDir(), "nope")) {
			t.Error("a missing directory must not report as a half-clone")
		}
	})
}

// TestIsInsideDefaultWorkspace: only paths strictly under <home>/workspace may
// be cleaned up. The workspace root itself and anything outside it are off
// limits — deleting the root takes every project with it.
func TestIsInsideDefaultWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows

	ws := filepath.Join(home, "workspace")
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(ws, "repo"), true},
		{ws, false},   // the root itself
		{home, false}, // $HOME
		{filepath.Join(home, "other"), false},
		{"/tmp/elsewhere", false},
	}
	for _, c := range cases {
		if got := isInsideDefaultWorkspace(c.path); got != c.want {
			t.Errorf("isInsideDefaultWorkspace(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
