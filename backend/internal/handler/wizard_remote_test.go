// Package handler tests the fallback session-file scanner used when
// the candidate list derived from project metadata misses the
// cwd-encoded slug the analyst actually used.
//
// Regression guard for req_d92d397bb4ae6286: when an analyst chat
// runs in a subdir / workspace symlink / drifted project path, the
// resulting .jsonl lands in a slug dir the architect stage's
// claudeProjectsSlugDir doesn't probe. findSessionJsonlFallback
// must locate it via full-table scan so the remote --resume can
// succeed.
package handler

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFindSessionJsonlFallback covers:
//   - empty sid → no-op
//   - no projects dir → no-op
//   - single hit → returned
//   - multiple hits (renamed project) → most-recently-modified wins
//   - non-matching files in the same dir → ignored
//
// Note: claudeSessionHome() reads $CLAUDE_CONFIG_DIR first, then
// $NOVA_CLAUDE_HOME, then ~/.claude. Tests point CLAUDE_CONFIG_DIR
// at t.TempDir() and explicitly clear NOVA_CLAUDE_HOME so that any
// value leaking from the host environment can't shadow the temp dir.
func TestFindSessionJsonlFallback(t *testing.T) {
	const sid = "07ff6a81-312e-4d8e-8489-3fa4d98c47c1"

	t.Run("empty sid returns false", func(t *testing.T) {
		got, ok := findSessionJsonlFallback("")
		if ok || got != "" {
			t.Fatalf("want (\"\", false), got (%q, %v)", got, ok)
		}
	})

	t.Run("no projects dir returns false", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", home)
		t.Setenv("NOVA_CLAUDE_HOME", "")
		if _, ok := findSessionJsonlFallback(sid); ok {
			t.Fatal("expected no hit with empty projects dir")
		}
	})

	t.Run("single matching slug dir returns the jsonl", func(t *testing.T) {
		home := t.TempDir()
		slugDir := filepath.Join(home, "projects", "--users-f1-test-proj-p")
		if err := os.MkdirAll(slugDir, 0o755); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(slugDir, sid+".jsonl")
		if err := os.WriteFile(want, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CLAUDE_CONFIG_DIR", home)
		t.Setenv("NOVA_CLAUDE_HOME", "")
		got, ok := findSessionJsonlFallback(sid)
		if !ok {
			t.Fatalf("expected hit, got none")
		}
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("multiple hits prefer most-recently-modified", func(t *testing.T) {
		home := t.TempDir()

		// Stale hit: written first, then we set its mtime back by 1h
		// so the fresh hit is unambiguously newer. Using an explicit
		// time delta avoids flake when both files were written in
		// the same OS time tick.
		staleDir := filepath.Join(home, "projects", "--old-slug")
		if err := os.MkdirAll(staleDir, 0o755); err != nil {
			t.Fatal(err)
		}
		staleFile := filepath.Join(staleDir, sid+".jsonl")
		if err := os.WriteFile(staleFile, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-1 * time.Hour)
		if err := os.Chtimes(staleFile, past, past); err != nil {
			t.Fatalf("chtimes stale: %v", err)
		}

		// Fresh hit: written after, mtime = now. Must be the one
		// the helper returns.
		freshDir := filepath.Join(home, "projects", "--users-f1-test-proj-p--subdir")
		if err := os.MkdirAll(freshDir, 0o755); err != nil {
			t.Fatal(err)
		}
		freshFile := filepath.Join(freshDir, sid+".jsonl")
		if err := os.WriteFile(freshFile, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := os.Chtimes(freshFile, now, now); err != nil {
			t.Fatalf("chtimes fresh: %v", err)
		}

		t.Setenv("CLAUDE_CONFIG_DIR", home)
		t.Setenv("NOVA_CLAUDE_HOME", "")
		got, ok := findSessionJsonlFallback(sid)
		if !ok {
			t.Fatal("expected hit, got none")
		}
		if got != freshFile {
			t.Fatalf("got %q, want fresh %q", got, freshFile)
		}
	})

	t.Run("non-matching jsonl in same dir ignored", func(t *testing.T) {
		home := t.TempDir()
		slugDir := filepath.Join(home, "projects", "--some-other-slug")
		if err := os.MkdirAll(slugDir, 0o755); err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(slugDir, "different-sid.jsonl")
		if err := os.WriteFile(other, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CLAUDE_CONFIG_DIR", home)
		t.Setenv("NOVA_CLAUDE_HOME", "")
		if _, ok := findSessionJsonlFallback(sid); ok {
			t.Fatal("did not expect a hit for sid not present on disk")
		}
	})
}
