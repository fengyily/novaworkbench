package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestUpdateWorktreeEmptyBranchKeepsPath pins the persistence contract the
// no-branch mode relies on: "empty branch name + a worktree path" is a
// meaningful state, not a half-written row.
//
// The no-branch mode stores exactly that pair — branch_name = "" (no branch
// anywhere) and worktree_path = the detached worktree. A future "fix" that
// turned the empty string into NULL (or dropped the path) would silently
// break the isolation for every no-branch requirement.
func TestUpdateWorktreeEmptyBranchKeepsPath(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_1", "proj_1", "/tmp/proj_1")
	s := NewRequirementService(d, nil)

	created, err := s.Create(model.CreateRequirementReq{ProjectID: "proj_1", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const wtPath = "/tmp/wt/req_x"
	if err := s.UpdateWorktree(created.ID, "", wtPath); err != nil {
		t.Fatalf("UpdateWorktree: %v", err)
	}
	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.BranchName != "" {
		t.Errorf("branch_name = %q, want empty", got.BranchName)
	}
	if got.WorktreePath != wtPath {
		t.Errorf("worktree_path = %q, want %q", got.WorktreePath, wtPath)
	}
}

// TestUpdateNoBranchRoundTrip pins the no_branch flag round-trip. The flag
// is read by half a dozen call sites (the coding prologue, the design
// anchor, the push path, the cleanup path), so a write that does not stick
// would send the requirement down the wrong branch entirely.
func TestUpdateNoBranchRoundTrip(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_1", "proj_1", "/tmp/proj_1")
	s := NewRequirementService(d, nil)

	created, err := s.Create(model.CreateRequirementReq{ProjectID: "proj_1", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.UpdateNoBranch(created.ID, true); err != nil {
		t.Fatalf("UpdateNoBranch(true): %v", err)
	}
	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.NoBranch {
		t.Fatalf("no_branch = false, want true")
	}
	if err := s.UpdateNoBranch(created.ID, false); err != nil {
		t.Fatalf("UpdateNoBranch(false): %v", err)
	}
	got, err = s.Get(created.ID)
	if err != nil {
		t.Fatalf("get (2): %v", err)
	}
	if got.NoBranch {
		t.Fatalf("no_branch = true, want false")
	}
}

// TestUpdateBaseBranchOverwrites pins the "a non-empty value always
// overwrites" semantics of UpdateBaseBranch.
//
// The coding stage writes the base it actually resolved for this run back
// onto the row (so the push stage reads the same base the code was written
// against). That write-back only works if the update is unconditional — a
// "stamp only when empty" behaviour would leave the design stage's stale
// value in place and the code would be developed against one branch but
// pushed onto another.
func TestUpdateBaseBranchOverwrites(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_1", "proj_1", "/tmp/proj_1")
	s := NewRequirementService(d, nil)

	created, err := s.Create(model.CreateRequirementReq{ProjectID: "proj_1", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.UpdateBaseBranch(created.ID, "develop"); err != nil {
		t.Fatalf("UpdateBaseBranch(develop): %v", err)
	}
	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.BaseBranch != "develop" {
		t.Fatalf("base_branch = %q, want %q", got.BaseBranch, "develop")
	}
	// The second write must replace the first one.
	if err := s.UpdateBaseBranch(created.ID, "main"); err != nil {
		t.Fatalf("UpdateBaseBranch(main): %v", err)
	}
	got, err = s.Get(created.ID)
	if err != nil {
		t.Fatalf("get (2): %v", err)
	}
	if got.BaseBranch != "main" {
		t.Fatalf("base_branch = %q, want %q — a non-empty value must always overwrite", got.BaseBranch, "main")
	}
}
