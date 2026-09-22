package handler

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestWorktreePathMatches is the table-driven coverage for the drift guard
// every wizard entry point now runs before trusting req.WorktreePath. The
// regression we're guarding against: a row whose worktree_path points at
// another requirement's directory survives a project move / DB migration
// and silently re-uses the foreign directory on the next coding pass
// (req_c46e8d66491ae3a2 → req_cd5079181af7335a).
//
// The helper is pure (no DB, no git), so the test sets $HOME via
// t.Setenv("HOME", t.TempDir()) to make worktreeRoot deterministic and then
// walks every interesting case. Cleaning is via filepath.Clean on both
// sides so trailing slashes / redundant segments don't cause false
// negatives — the production code at wizard_coding.go:1496 and friends
// relies on the helper, not on caller-side cleaning.
func TestWorktreePathMatches(t *testing.T) {
	const reqA = "req_aaaaaaaaaaaa"
	const reqB = "req_bbbbbbbbbbbb"
	home := t.TempDir()
	t.Setenv("HOME", home)

	// worktreeRoot("projectPath") = $HOME/.novaworkbench/worktrees/<basename(projectPath)>
	projectPath := "/Users/f1/project/novaworkbench"
	expectedA := WorktreePath(projectPath, reqA) // $HOME/.novaworkbench/worktrees/novaworkbench/req_aaaaaaaaaaaa
	expectedB := WorktreePath(projectPath, reqB)

	cases := []struct {
		name      string
		reqID     string
		stored    string
		projPath  string
		wantOK    bool
		wantExpEq string // value of expected must match; either expectedA or expectedB
	}{
		{
			name:      "empty stored path → no match (never anchored yet)",
			reqID:     reqA,
			stored:    "",
			projPath:  projectPath,
			wantOK:    false,
			wantExpEq: expectedA,
		},
		{
			name:      "stored == expected → match",
			reqID:     reqA,
			stored:    expectedA,
			projPath:  projectPath,
			wantOK:    true,
			wantExpEq: expectedA,
		},
		{
			name:      "stored is another reqID's directory → no match (cross-requirement contamination)",
			reqID:     reqA,
			stored:    expectedB,
			projPath:  projectPath,
			wantOK:    false,
			wantExpEq: expectedA,
		},
		{
			name:      "project local_path moved → stored refers to old root → no match",
			reqID:     reqA,
			stored:    expectedA, // path computed against the OLD projectPath
			projPath:  "/tmp/new-project", // different basename → different worktreeRoot
			wantOK:    false,
			wantExpEq: WorktreePath("/tmp/new-project", reqA),
		},
		{
			name:      "trailing slash on stored → still matches after filepath.Clean",
			reqID:     reqA,
			stored:    expectedA + string(filepath.Separator),
			projPath:  projectPath,
			wantOK:    true,
			wantExpEq: expectedA,
		},
		{
			name:      "stored with redundant /./ segment → still matches after filepath.Clean",
			reqID:     reqA,
			stored:    strings.Replace(expectedA, string(filepath.Separator)+reqA, string(filepath.Separator)+"."+string(filepath.Separator)+reqA, 1),
			projPath:  projectPath,
			wantOK:    true,
			wantExpEq: expectedA,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, got := WorktreePathMatches(tc.reqID, tc.stored, tc.projPath)
			if ok != tc.wantOK {
				t.Errorf("WorktreePathMatches(%q, %q, %q) ok = %v, want %v (expected %q)",
					tc.reqID, tc.stored, tc.projPath, ok, tc.wantOK, got)
			}
			if got != tc.wantExpEq {
				t.Errorf("WorktreePathMatches(%q, %q, %q) expected = %q, want %q",
					tc.reqID, tc.stored, tc.projPath, got, tc.wantExpEq)
			}
		})
	}
}
