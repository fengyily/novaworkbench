package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

func mkReq(t *testing.T, s *RequirementService, proj, title string) *model.Requirement {
	t.Helper()
	r, err := s.Create(model.CreateRequirementReq{ProjectID: proj, Title: title})
	if err != nil {
		t.Fatalf("create %s: %v", title, err)
	}
	return r
}

func TestSetParentRules(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_a", "proj_a", "/tmp/proj_a")
	seedProject(t, d, "proj_b", "proj_b", "/tmp/proj_b")
	s := NewRequirementService(d, nil)
	a, b, c := mkReq(t, s, "proj_a", "A"), mkReq(t, s, "proj_a", "B"), mkReq(t, s, "proj_a", "C")
	x := mkReq(t, s, "proj_b", "X")

	got, err := s.SetParent(b.ID, a.ID)
	if err != nil || got.ParentID != a.ID {
		t.Fatalf("attach: err=%v parent=%q", err, got.ParentID)
	}
	if _, err := s.SetParent(a.ID, a.ID); err != ErrParentSelf {
		t.Fatalf("self: %v", err)
	}
	if _, err := s.SetParent(c.ID, "req_missing"); err != ErrParentNotFound {
		t.Fatalf("missing: %v", err)
	}
	if _, err := s.SetParent(c.ID, x.ID); err != ErrParentCrossProject {
		t.Fatalf("cross project: %v", err)
	}
	if _, err := s.SetParent(c.ID, b.ID); err != ErrParentIsChild {
		t.Fatalf("target is child: %v", err)
	}
	if _, err := s.SetParent(a.ID, c.ID); err != ErrSelfHasChildren {
		t.Fatalf("self has children: %v", err)
	}

	list, err := s.List("", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range list {
		switch r.ID {
		case a.ID:
			if r.ChildCount != 1 {
				t.Fatalf("child_count=%d want 1", r.ChildCount)
			}
		case b.ID:
			if r.ParentID != a.ID {
				t.Fatalf("list parent_id=%q", r.ParentID)
			}
		}
	}

	// Archived children do not count.
	if _, err := d.Exec("UPDATE requirements SET status='archived' WHERE id=?", b.ID); err != nil {
		t.Fatal(err)
	}
	list, _ = s.List("", "", "", "", "")
	for _, r := range list {
		if r.ID == a.ID && r.ChildCount != 0 {
			t.Fatalf("archived child counted: %d", r.ChildCount)
		}
	}
	if _, err := d.Exec("UPDATE requirements SET status='draft' WHERE id=?", b.ID); err != nil {
		t.Fatal(err)
	}

	got, err = s.SetParent(b.ID, "")
	if err != nil || got.ParentID != "" {
		t.Fatalf("detach: err=%v parent=%q", err, got.ParentID)
	}
}

func TestDeleteParentDetachesChildren(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_a", "proj_a", "/tmp/proj_a")
	s := NewRequirementService(d, nil)
	a, b := mkReq(t, s, "proj_a", "A"), mkReq(t, s, "proj_a", "B")
	if _, err := s.SetParent(b.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := s.Get(b.ID)
	if err != nil || got.ParentID != "" {
		t.Fatalf("child after delete: err=%v parent=%q", err, got.ParentID)
	}
}

func TestUpdateProjectSwitchClearsHierarchy(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_a", "proj_a", "/tmp/proj_a")
	seedProject(t, d, "proj_b", "proj_b", "/tmp/proj_b")
	s := NewRequirementService(d, nil)
	a, b := mkReq(t, s, "proj_a", "A"), mkReq(t, s, "proj_a", "B")
	if _, err := s.SetParent(b.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(a.ID, model.CreateRequirementReq{ProjectID: "proj_b", Title: "A"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(b.ID)
	if got.ParentID != "" {
		t.Fatalf("child kept parent across projects: %q", got.ParentID)
	}
}

// TestSetParentRecomputesBase pins what happens to a requirement's base
// branch when it is re-homed under a (new) master.
//
// A requirement that follows its master must follow the NEW master's branch —
// otherwise the child keeps developing on the old master's line while the UI
// claims it follows the new one.
//
// Two guards keep the rewrite from destroying something the user owns or
// something the code already sits on:
//   - an EXPLICIT base is the user's own choice — never rewritten;
//   - a requirement that already developed has a worktree cut from its old
//     base — moving the base now would desync the working tree from the PR
//     target.
//
// Detaching (parent_id = "") never rewrites the base: the code already sits
// on that line, so moving the target would break the PR rather than fix it.
func TestSetParentRecomputesBase(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_a", "proj_a", "/tmp/proj_a")
	s := NewRequirementService(d, nil)

	masterA := mkReq(t, s, "proj_a", "master A")
	masterB := mkReq(t, s, "proj_a", "master B")
	if err := s.UpdateWorktree(masterA.ID, "feat/req_"+masterA.ID, ""); err != nil {
		t.Fatalf("stamp master A branch: %v", err)
	}
	if err := s.UpdateWorktree(masterB.ID, "feat/req_"+masterB.ID, ""); err != nil {
		t.Fatalf("stamp master B branch: %v", err)
	}

	// (1) A fresh requirement attached to a master inherits the master's
	//     development branch.
	child := mkReq(t, s, "proj_a", "child")
	got, err := s.SetParent(child.ID, masterA.ID)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if got.BaseBranch != "feat/req_"+masterA.ID {
		t.Fatalf("child base_branch = %q, want %q", got.BaseBranch, "feat/req_"+masterA.ID)
	}
	if got.BaseBranchSource != model.BaseSourceParent {
		t.Fatalf("child base_branch_source = %q, want %q", got.BaseBranchSource, model.BaseSourceParent)
	}

	// (2) Re-homing under another master moves the base along.
	got, err = s.SetParent(child.ID, masterB.ID)
	if err != nil {
		t.Fatalf("re-home: %v", err)
	}
	if got.BaseBranch != "feat/req_"+masterB.ID {
		t.Fatalf("re-homed base_branch = %q, want %q", got.BaseBranch, "feat/req_"+masterB.ID)
	}
	if got.BaseBranchSource != model.BaseSourceParent {
		t.Fatalf("re-homed base_branch_source = %q, want %q", got.BaseBranchSource, model.BaseSourceParent)
	}

	// (3) An EXPLICIT base is the user's own choice — re-homing must not
	//     touch it.
	explicit := mkReq(t, s, "proj_a", "explicit")
	if err := s.UpdateBaseBranchWithSource(explicit.ID, "develop", model.BaseSourceExplicit); err != nil {
		t.Fatalf("stamp explicit base: %v", err)
	}
	got, err = s.SetParent(explicit.ID, masterA.ID)
	if err != nil {
		t.Fatalf("attach explicit: %v", err)
	}
	if got.BaseBranch != "develop" || got.BaseBranchSource != model.BaseSourceExplicit {
		t.Fatalf("explicit row rewritten: base_branch=%q source=%q", got.BaseBranch, got.BaseBranchSource)
	}

	// (4) A requirement that already developed keeps its base — its
	//     worktree is cut from that line.
	developed := mkReq(t, s, "proj_a", "developed")
	if err := s.UpdateBaseBranchWithSource(developed.ID, "feat/req_"+masterA.ID, model.BaseSourceParent); err != nil {
		t.Fatalf("stamp developed base: %v", err)
	}
	if err := s.UpdateWorktree(developed.ID, "feat/req_"+developed.ID, "/tmp/wt"); err != nil {
		t.Fatalf("stamp developed worktree: %v", err)
	}
	got, err = s.SetParent(developed.ID, masterB.ID)
	if err != nil {
		t.Fatalf("attach developed: %v", err)
	}
	if got.BaseBranch != "feat/req_"+masterA.ID {
		t.Fatalf("developed row rewritten: base_branch = %q, want %q", got.BaseBranch, "feat/req_"+masterA.ID)
	}

	// (5) Detaching never rewrites the base.
	got, err = s.SetParent(child.ID, "")
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	if got.BaseBranch != "feat/req_"+masterB.ID {
		t.Fatalf("detached row rewritten: base_branch = %q, want %q", got.BaseBranch, "feat/req_"+masterB.ID)
	}
}
