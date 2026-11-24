package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMaterializeClaudeDocWritesTheManagedCopy pins the materialization:
// the managed copy has to land on disk as a real CLAUDE.md, because that is
// the only shape the Claude CLI loads as project memory.
func TestMaterializeClaudeDocWritesTheManagedCopy(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { SetClaudeDocMemoryRoot("") })
	SetClaudeDocMemoryRoot(filepath.Join(root, "memory"))

	dir, err := MaterializeClaudeDoc("proj_1", "# 约定\n\n分层：handler / service。")
	if err != nil {
		t.Fatalf("MaterializeClaudeDoc: %v", err)
	}
	if dir == "" {
		t.Fatal("MaterializeClaudeDoc returned no directory")
	}
	path := filepath.Join(dir, "CLAUDE.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the materialized copy: %v", err)
	}
	if !strings.Contains(string(data), "分层：handler / service。") {
		t.Errorf("materialized content = %q, want the managed document", string(data))
	}
	// The copy must live under NovaWorkbench's own directory — never inside
	// the user's checkout, where `git add -A` would commit it into their
	// branch.
	if !strings.HasPrefix(dir, root) {
		t.Errorf("materialized dir = %q, want it under %q", dir, root)
	}
}

// TestMaterializeClaudeDocIsIdempotent pins the no-op on unchanged content.
//
// A run that starts many sessions must not keep touching the file: the
// mtime stays put, which keeps the CLI's "instruction files were re-read"
// notice quiet, and the write itself costs nothing.
func TestMaterializeClaudeDocIsIdempotent(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { SetClaudeDocMemoryRoot("") })
	SetClaudeDocMemoryRoot(filepath.Join(root, "memory"))

	const content = "# 约定"
	dir, err := MaterializeClaudeDoc("proj_1", content)
	if err != nil {
		t.Fatalf("MaterializeClaudeDoc: %v", err)
	}
	path := filepath.Join(dir, "CLAUDE.md")
	first, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// A second materialization with the same content must not rewrite.
	if _, err := MaterializeClaudeDoc("proj_1", content); err != nil {
		t.Fatalf("MaterializeClaudeDoc (again): %v", err)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Errorf("the file was rewritten for unchanged content (%s -> %s)", first.ModTime(), second.ModTime())
	}

	// Different content DOES overwrite.
	if _, err := MaterializeClaudeDoc("proj_1", "# 改过的约定"); err != nil {
		t.Fatalf("MaterializeClaudeDoc (changed): %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "改过的约定") {
		t.Errorf("content = %q, want the updated document", string(data))
	}
}

// TestMaterializeClaudeDocRejectsEmptyContent pins the guard: an empty
// document must not produce a file (an empty CLAUDE.md would still be
// loaded by the CLI, and the caller would believe the conventions are in
// the context).
func TestMaterializeClaudeDocRejectsEmptyContent(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { SetClaudeDocMemoryRoot("") })
	SetClaudeDocMemoryRoot(filepath.Join(root, "memory"))

	if _, err := MaterializeClaudeDoc("proj_1", "   \n\t "); err == nil {
		t.Error("MaterializeClaudeDoc accepted empty content")
	}
}

// TestClaudeDocMemoryDirIsPerProject pins the per-project separation: two
// projects must never share a directory, or one project's document would
// replace the other's.
func TestClaudeDocMemoryDirIsPerProject(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { SetClaudeDocMemoryRoot("") })
	SetClaudeDocMemoryRoot(root)

	a := ClaudeDocMemoryDir("proj_1")
	b := ClaudeDocMemoryDir("proj_2")
	if a == b {
		t.Errorf("two projects share the memory directory %q", a)
	}
	if !strings.HasPrefix(a, root) {
		t.Errorf("memory dir = %q, want it under %q", a, root)
	}
}

// TestClaudeDocMemoryDirSanitizesTheProjectID pins the path-traversal
// guard. A project id is normally generated, but the value can also come
// from a restored database or a hand-written configuration — and a path
// separator in it would let the materialized file escape the memory root.
func TestClaudeDocMemoryDirSanitizesTheProjectID(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { SetClaudeDocMemoryRoot("") })
	SetClaudeDocMemoryRoot(root)

	dir := ClaudeDocMemoryDir("../../etc/passwd")
	if !strings.HasPrefix(dir, root) {
		t.Fatalf("memory dir = %q, want it under %q", dir, root)
	}
	// The separator is neutralized, so the id cannot walk out of the root.
	if strings.Contains(filepath.ToSlash(dir), "/../") {
		t.Errorf("memory dir = %q, want no parent-directory segment", dir)
	}
	// And it is a SINGLE segment: the id became one path component.
	if got := filepath.Base(dir); got != ".._.._etc_passwd" {
		t.Errorf("memory dir base = %q, want the sanitized id", got)
	}
}

// TestRemoveClaudeDocMemoryDropsTheCopy pins the cleanup: a deleted
// managed document must not linger on disk, or the CLI would keep loading
// it as project memory.
func TestRemoveClaudeDocMemoryDropsTheCopy(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { SetClaudeDocMemoryRoot("") })
	SetClaudeDocMemoryRoot(filepath.Join(root, "memory"))

	dir, err := MaterializeClaudeDoc("proj_1", "# 约定")
	if err != nil {
		t.Fatalf("MaterializeClaudeDoc: %v", err)
	}
	if err := RemoveClaudeDocMemory("proj_1"); err != nil {
		t.Fatalf("RemoveClaudeDocMemory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("the materialized copy still exists after removal")
	}
	// Removing an absent document is a no-op, not an error.
	if err := RemoveClaudeDocMemory("proj_1"); err != nil {
		t.Errorf("RemoveClaudeDocMemory (again): %v", err)
	}
}

// TestClaudeMdLoadSet pins the files the CLI loads from a directory.
//
// The set is what the conflict detection has to cover: the CLI reads more
// than <dir>/CLAUDE.md, and a detector that only looked at that one file
// would miss the .claude/ variant and the local one.
func TestClaudeMdLoadSet(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md", filepath.Join(".claude", "CLAUDE.md")} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// A file that is NOT part of the load set must not show up.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}

	got := ClaudeMdLoadSet(dir)
	want := map[string]bool{
		filepath.Join(dir, "CLAUDE.md"):            true,
		filepath.Join(dir, "CLAUDE.local.md"):      true,
		filepath.Join(dir, "AGENTS.md"):            true,
		filepath.Join(dir, ".claude", "CLAUDE.md"): true,
	}
	if len(got) != len(want) {
		t.Fatalf("load set = %v, want %d entries", got, len(want))
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected load-set entry %q", p)
		}
	}
}

// TestClaudeMdLoadSetSkipsMissingFiles pins the existence filter: the
// result is the set the CLI will ACTUALLY pick up.
func TestClaudeMdLoadSetSkipsMissingFiles(t *testing.T) {
	dir := t.TempDir()
	if got := ClaudeMdLoadSet(dir); len(got) != 0 {
		t.Errorf("load set = %v, want empty for a directory without instruction files", got)
	}
	if got := ClaudeMdLoadSet(""); got != nil {
		t.Errorf("load set = %v, want nil for an empty directory", got)
	}
}
