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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
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

// --------------------------------------------------------------------
// workerRunRequest: systemPrompt / disallowedTools pass-through
// --------------------------------------------------------------------

// TestWorkerRunRequest_ForwardsWikiToolDenylist is the regression guard
// for the remote-wiki failure mode: wikiDisallowedTools (notably "Task")
// is what stops Claude from fanning out Explore sub-agents and ending its
// turn on a one-line "等待回收中…" preamble — which finalizeWikiRun then
// rejects, so dropping the field would make remote wiki fail every time.
// The worker has understood `disallowedTools` since before this change
// (agent-worker/server.mjs buildClaudeArgs); only the Go side was silent.
func TestWorkerRunRequest_ForwardsWikiToolDenylist(t *testing.T) {
	body := workerRunRequest(llm.StreamOpts{
		WorkDir:         "/tmp/x",
		Prompt:          "hi",
		PermissionMode:  "plan",
		SystemPrompt:    "你是一位资深软件工程师",
		DisallowedTools: wikiDisallowedTools,
	}, nil, nil)

	if body.SystemPrompt != "你是一位资深软件工程师" {
		t.Errorf("SystemPrompt not forwarded: got %q", body.SystemPrompt)
	}
	if len(body.DisallowedTools) != len(wikiDisallowedTools) {
		t.Fatalf("DisallowedTools: got %#v, want %#v", body.DisallowedTools, wikiDisallowedTools)
	}
	hasTask := false
	for _, tool := range body.DisallowedTools {
		if tool == "Task" {
			hasTask = true
		}
	}
	if !hasTask {
		t.Errorf("DisallowedTools must contain \"Task\" (sub-agent ban), got %#v", body.DisallowedTools)
	}

	// Both fields must survive JSON marshaling under the names the worker
	// destructures (`systemPrompt` / `disallowedTools`).
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"systemPrompt"`, `"disallowedTools"`, `"Task"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("wire format missing %s: %s", key, raw)
		}
	}
}

// TestWorkerRunRequest_OmitsToolFieldsForArchitect pins the "architect and
// coding behaviour is byte-identical" half of the change: both callers
// leave SystemPrompt / DisallowedTools at their zero values, and omitempty
// must keep them off the wire entirely so an older worker destructuring a
// fixed field list behaves exactly as before.
func TestWorkerRunRequest_OmitsToolFieldsForArchitect(t *testing.T) {
	body := workerRunRequest(llm.StreamOpts{
		WorkDir:        "/tmp/x",
		Prompt:         "hi",
		PermissionMode: "plan",
	}, nil, nil)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"systemPrompt", "disallowedTools"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("%s leaked into the wire format for a non-wiki caller: %s", key, raw)
		}
	}
}

// TestRequirementSessionIDs_IncludesWikiSession pins that a kind=wiki
// requirement's conversation id is part of the SFTP up-sync set. Wiki rows
// never populate design_session_id, so without the explicit entry the
// second and later remote wiki runs would find no jsonl to --resume and
// silently restart with no prior context.
func TestRequirementSessionIDs_IncludesWikiSession(t *testing.T) {
	ids := requirementSessionIDs(&model.Requirement{
		AnalysisSessionID: "sid-analysis",
		WikiSessionID:     "sid-wiki",
	}, "sid-wiki")

	// sourceSID == WikiSessionID must dedupe to one entry.
	want := []string{"sid-analysis", "sid-wiki"}
	if len(ids) != len(want) {
		t.Fatalf("got %#v, want %#v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("index %d: got %q, want %q (full %#v)", i, ids[i], want[i], ids)
		}
	}
}
