package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

// seedProject inserts a bare-bones project row directly via SQL so the
// test doesn't drag in the PlatformTokenService that NewProjectService
// requires. We only need the row + its dependents to exist; the columns
// not exercised here stay at their SQL defaults.
func seedProject(t *testing.T, d *db.DB, id, name, localPath string) {
	t.Helper()
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, 'ready', 'main')`,
		id, name, localPath); err != nil {
		t.Fatalf("seed project %s: %v", id, err)
	}
}

// TestRequirementClearWorktree pins the helper every drift guard now relies
// on. The regression we're guarding against: a stale worktree_path on a
// requirement row survives across project moves / DB migrations and is
// silently reused by the next coding pass, leading Claude to edit the wrong
// directory (req_c46e8d66491ae3a2 → req_cd5079181af7335a).
//
// ClearWorktree must wipe BOTH branch_name AND worktree_path in one UPDATE
// (anchorWorktree checks the pair for matches==false), and it must not
// touch any other column. It must also be a no-op (no error, 0 rows
// affected) on a non-existent reqID so cleanup-from-stale-handlers is
// idempotent.
func TestRequirementClearWorktree(t *testing.T) {
	d := newTestDB(t)
	reqSvc := NewRequirementService(d)

	seedProject(t, d, "proj_1", "p", t.TempDir())

	// Setup: a row with worktree_path + branch_name set (post-coding state).
	created, err := reqSvc.Create(model.CreateRequirementReq{ProjectID: "proj_1", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := reqSvc.UpdateWorktree(created.ID, "feat/"+created.ID, "/tmp/some-worktree"); err != nil {
		t.Fatalf("UpdateWorktree: %v", err)
	}
	if err := reqSvc.UpdateCodingPlan(created.ID, "# plan"); err != nil {
		t.Fatalf("UpdateCodingPlan (sentinel — must NOT be wiped): %v", err)
	}

	// Sanity: the row carries the values we just set.
	pre, err := reqSvc.Get(created.ID)
	if err != nil {
		t.Fatalf("get pre: %v", err)
	}
	if pre.WorktreePath == "" || pre.BranchName == "" {
		t.Fatalf("setup failed: worktree_path=%q branch_name=%q", pre.WorktreePath, pre.BranchName)
	}

	// Act.
	if err := reqSvc.ClearWorktree(created.ID); err != nil {
		t.Fatalf("ClearWorktree: %v", err)
	}

	// Assert: both columns wiped, coding_plan preserved (ClearWorktree
	// must NOT touch unrelated columns — anchorWorktree / drift guard only
	// own branch_name + worktree_path).
	post, err := reqSvc.Get(created.ID)
	if err != nil {
		t.Fatalf("get post: %v", err)
	}
	if post.WorktreePath != "" {
		t.Errorf("worktree_path = %q, want empty", post.WorktreePath)
	}
	if post.BranchName != "" {
		t.Errorf("branch_name = %q, want empty", post.BranchName)
	}
	if post.CodingPlan != "# plan" {
		t.Errorf("coding_plan = %q, want %q (ClearWorktree must not touch unrelated columns)", post.CodingPlan, "# plan")
	}

	// Idempotent / no-op on a non-existent row — does NOT return an error.
	if err := reqSvc.ClearWorktree("req_does_not_exist"); err != nil {
		t.Errorf("ClearWorktree on missing row returned error: %v", err)
	}
}

// TestProjectUpdateBasicInfoCascadesWorktreeClear pins the cascade in
// project.UpdateBasicInfo: when the project's local_path actually moves
// (the only case we care about — same-path PATCHes must NOT wipe unrelated
// requirements' worktrees), every requirement of that project has its
// branch_name + worktree_path cleared.
//
// The cascade matters because anchorWorktree / execStartCoding compute the
// expected worktree_path as WorktreePath(projectPath, reqID); once
// projectPath changes, every persisted worktree_path from before the move
// is guaranteed to mismatch. Without the cascade, the next wizard call on
// any of those requirements would either silently land in the old worktree
// (if the directory still exists) or fail the WorktreePathMatches drift
// guard at every entry point.
//
// ProjectService requires a PlatformTokenService that we don't need here,
// so we wire a non-nil empty one to satisfy the constructor. The cascade
// itself only reads/writes requirements + projects, so the platform service
// stays untouched throughout the test.
func TestProjectUpdateBasicInfoCascadesWorktreeClear(t *testing.T) {
	d := newTestDB(t)
	platformSvc := NewPlatformTokenService(d)
	projSvc := NewProjectService(d, platformSvc)
	reqSvc := NewRequirementService(d)

	oldDir := t.TempDir()
	newDir := t.TempDir()
	seedProject(t, d, "proj_p", "p", oldDir)
	created, err := projSvc.Get("proj_p")
	if err != nil || created == nil {
		t.Fatalf("get proj_p: %v", err)
	}

	// Two requirements of this project, both with worktree state set.
	r1, err := reqSvc.Create(model.CreateRequirementReq{ProjectID: created.ID, Title: "r1", Description: ""})
	if err != nil {
		t.Fatalf("create r1: %v", err)
	}
	r2, err := reqSvc.Create(model.CreateRequirementReq{ProjectID: created.ID, Title: "r2", Description: ""})
	if err != nil {
		t.Fatalf("create r2: %v", err)
	}
	if err := reqSvc.UpdateWorktree(r1.ID, "feat/r1", oldDir+"/.novaworkbench/worktrees/p/"+r1.ID); err != nil {
		t.Fatalf("seed r1 worktree: %v", err)
	}
	if err := reqSvc.UpdateWorktree(r2.ID, "feat/r2", oldDir+"/.novaworkbench/worktrees/p/"+r2.ID); err != nil {
		t.Fatalf("seed r2 worktree: %v", err)
	}

	// Other-project control: a different project + its own requirement,
	// which must NOT be cleared when our project moves.
	otherDir := t.TempDir()
	seedProject(t, d, "proj_other", "other", otherDir)
	rOther, err := reqSvc.Create(model.CreateRequirementReq{ProjectID: "proj_other", Title: "rOther", Description: ""})
	if err != nil {
		t.Fatalf("create rOther: %v", err)
	}
	if err := reqSvc.UpdateWorktree(rOther.ID, "feat/other", otherDir+"/.novaworkbench/worktrees/other/"+rOther.ID); err != nil {
		t.Fatalf("seed rOther worktree: %v", err)
	}

	// Move the project (same name / remoteURL / everything, NEW local_path).
	if err := projSvc.UpdateBasicInfo(created.ID, "p", "", "go", newDir, "main"); err != nil {
		t.Fatalf("UpdateBasicInfo move: %v", err)
	}

	// Both r1 and r2 must be wiped.
	got1, _ := reqSvc.Get(r1.ID)
	if got1.WorktreePath != "" || got1.BranchName != "" {
		t.Errorf("r1 not cleared after project move: worktree_path=%q branch_name=%q", got1.WorktreePath, got1.BranchName)
	}
	got2, _ := reqSvc.Get(r2.ID)
	if got2.WorktreePath != "" || got2.BranchName != "" {
		t.Errorf("r2 not cleared after project move: worktree_path=%q branch_name=%q", got2.WorktreePath, got2.BranchName)
	}

	// Control row in the OTHER project must be untouched.
	gotOther, _ := reqSvc.Get(rOther.ID)
	if gotOther.WorktreePath == "" || gotOther.BranchName == "" {
		t.Errorf("rOther was wrongly cleared: worktree_path=%q branch_name=%q", gotOther.WorktreePath, gotOther.BranchName)
	}
}

// TestProjectUpdateBasicInfoNoMoveDoesNotClear is the negative case: when
// UpdateBasicInfo is called with the SAME local_path (e.g. PATCH on the
// project name / description only), no requirement rows must be touched.
// Without this guard, every minor project rename would invalidate every
// requirement's worktree, forcing anchorWorktree to rebuild on the next
// coding round for no reason.
func TestProjectUpdateBasicInfoNoMoveDoesNotClear(t *testing.T) {
	d := newTestDB(t)
	platformSvc := NewPlatformTokenService(d)
	projSvc := NewProjectService(d, platformSvc)
	reqSvc := NewRequirementService(d)

	dir := t.TempDir()
	seedProject(t, d, "proj_p", "p", dir)
	r, err := reqSvc.Create(model.CreateRequirementReq{ProjectID: "proj_p", Title: "r", Description: ""})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := reqSvc.UpdateWorktree(r.ID, "feat/r", "/some/persisted/path"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Same path, only the name changes.
	if err := projSvc.UpdateBasicInfo("proj_p", "p-renamed", "", "go", dir, "main"); err != nil {
		t.Fatalf("UpdateBasicInfo same-path: %v", err)
	}

	got, _ := reqSvc.Get(r.ID)
	if got.WorktreePath != "/some/persisted/path" || got.BranchName != "feat/r" {
		t.Errorf("same-path PATCH wrongly cleared: worktree_path=%q branch_name=%q", got.WorktreePath, got.BranchName)
	}
}
