package service

import (
	"errors"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// Creating a sub-requirement must be ONE atomic insert: the child row and
// its parent link land together, or nothing lands at all. The alternative
// (insert, then SetParent) leaves an orphan top-level row behind whenever
// the second call fails — the user sees a requirement appear with no
// hierarchy, which is worse than an error message.
func TestCreateWithParent(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_a", "proj_a", "/tmp/proj_a")
	seedProject(t, d, "proj_b", "proj_b", "/tmp/proj_b")
	s := NewRequirementService(d, nil)

	parent := mkReq(t, s, "proj_a", "parent")

	child, err := s.Create(model.CreateRequirementReq{
		ProjectID:   "proj_a",
		Title:       "child",
		Description: "child body",
		ParentID:    parent.ID,
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if child.ParentID != parent.ID {
		t.Fatalf("parent_id = %q, want %q", child.ParentID, parent.ID)
	}
	if child.Status != "draft" {
		t.Fatalf("status = %q, want draft", child.Status)
	}

	// The child must be visible under its master, and the master must report
	// it — the same child_count contract the attach path produces.
	list, err := s.List("proj_a", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	sawChild, sawParent := false, false
	for _, r := range list {
		switch r.ID {
		case child.ID:
			sawChild = true
			if r.ParentID != parent.ID {
				t.Fatalf("list child parent_id = %q", r.ParentID)
			}
		case parent.ID:
			sawParent = true
			if r.ChildCount != 1 {
				t.Fatalf("child_count = %d, want 1", r.ChildCount)
			}
		}
	}
	if !sawChild || !sawParent {
		t.Fatalf("list missing rows: child=%v parent=%v", sawChild, sawParent)
	}

	// A rejected parent must leave NOTHING behind — no orphan top-level row.
	before, err := s.List("proj_a", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Create(model.CreateRequirementReq{
		ProjectID:   "proj_a",
		Title:       "orphan?",
		Description: "must not exist",
		ParentID:    "req_missing",
	})
	if !errors.Is(err, ErrParentNotFound) {
		t.Fatalf("missing parent: %v", err)
	}
	after, err := s.List("proj_a", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("orphan row left behind: %d -> %d", len(before), len(after))
	}
}

func TestCreateWithParentRejects(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_a", "proj_a", "/tmp/proj_a")
	seedProject(t, d, "proj_b", "proj_b", "/tmp/proj_b")
	s := NewRequirementService(d, nil)

	parent := mkReq(t, s, "proj_a", "parent")
	child := mkReq(t, s, "proj_a", "child")
	// `child` becomes a child itself, so it can serve as an illegal master.
	if _, err := s.SetParent(child.ID, parent.ID); err != nil {
		t.Fatalf("attach: %v", err)
	}
	other := mkReq(t, s, "proj_b", "other project")

	// Cross-project master.
	if _, err := s.Create(model.CreateRequirementReq{
		ProjectID: "proj_a", Title: "x", Description: "x", ParentID: other.ID,
	}); !errors.Is(err, ErrParentCrossProject) {
		t.Fatalf("cross project: %v", err)
	}
	// A master that is itself a child — the hierarchy is one level deep.
	if _, err := s.Create(model.CreateRequirementReq{
		ProjectID: "proj_a", Title: "x", Description: "x", ParentID: child.ID,
	}); !errors.Is(err, ErrParentIsChild) {
		t.Fatalf("parent is child: %v", err)
	}
	// Archived master — a closed chapter must not grow new work.
	if _, err := d.Exec("UPDATE requirements SET status='archived' WHERE id=?", parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(model.CreateRequirementReq{
		ProjectID: "proj_a", Title: "x", Description: "x", ParentID: parent.ID,
	}); !errors.Is(err, ErrParentArchived) {
		t.Fatalf("archived parent: %v", err)
	}
	// No parent at all — unchanged behaviour, still a top-level row.
	plain, err := s.Create(model.CreateRequirementReq{ProjectID: "proj_a", Title: "plain", Description: "d"})
	if err != nil {
		t.Fatalf("create top level: %v", err)
	}
	if plain.ParentID != "" {
		t.Fatalf("top level parent_id = %q", plain.ParentID)
	}
}

// The archived-master rule is a CREATE-time rule only. Re-homing an existing
// row under an archived master keeps its historical behaviour, so the
// attach path the UI has always used cannot start failing under users.
func TestSetParentStillAcceptsArchivedMaster(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_a", "proj_a", "/tmp/proj_a")
	s := NewRequirementService(d, nil)
	parent := mkReq(t, s, "proj_a", "parent")
	child := mkReq(t, s, "proj_a", "child")
	if _, err := d.Exec("UPDATE requirements SET status='archived' WHERE id=?", parent.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.SetParent(child.ID, parent.ID)
	if err != nil {
		t.Fatalf("attach to archived master: %v", err)
	}
	if got.ParentID != parent.ID {
		t.Fatalf("parent_id = %q", got.ParentID)
	}
}

// qa_turn_count is a derived List column: how many stage-Q&A turns the
// requirement has been asked, across both stages.
func TestListQATurnCount(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_a", "proj_a", "/tmp/proj_a")
	s := NewRequirementService(d, nil)
	req := mkReq(t, s, "proj_a", "req")

	if _, err := d.Exec(
		"INSERT INTO requirement_qa_turns (id,requirement_id,stage,question,answer,status) VALUES (?,?,?,?,?,?)",
		"qat_1", req.ID, "design", "q1", "a1", "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(
		"INSERT INTO requirement_qa_turns (id,requirement_id,stage,question,answer,status) VALUES (?,?,?,?,?,?)",
		"qat_2", req.ID, "coding", "q2", "a2", "done"); err != nil {
		t.Fatal(err)
	}
	// A turn of another requirement must not be counted.
	other := mkReq(t, s, "proj_a", "other")
	if _, err := d.Exec(
		"INSERT INTO requirement_qa_turns (id,requirement_id,stage,question,answer,status) VALUES (?,?,?,?,?,?)",
		"qat_3", other.ID, "design", "q3", "a3", "done"); err != nil {
		t.Fatal(err)
	}

	list, err := s.List("proj_a", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range list {
		switch r.ID {
		case req.ID:
			if r.QATurnCount != 2 {
				t.Fatalf("qa_turn_count = %d, want 2", r.QATurnCount)
			}
		case other.ID:
			if r.QATurnCount != 1 {
				t.Fatalf("other qa_turn_count = %d, want 1", r.QATurnCount)
			}
		}
	}
}

// TestCreateChildInheritsParentBase pins the base-branch inheritance of a
// HAND-MADE child requirement.
//
// A child requirement must develop on top of the branch its master develops
// on (feat/req_<master>) — that is the stack bottom of the stacked-PR model
// and the reason the child's PR targets the master's branch instead of the
// project default.
//
// The source marker is ALWAYS "parent", never "explicit": the user never
// picked a base on THIS requirement, they are following the master. An
// explicit pick later still wins (tier 2 of the resolution chain).
func TestCreateChildInheritsParentBase(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_a", "proj_a", "/tmp/proj_a")
	s := NewRequirementService(d, nil)

	// A master that already developed owns feat/req_<id>.
	master := mkReq(t, s, "proj_a", "master")
	masterBranch := "feat/req_" + master.ID
	if err := s.UpdateWorktree(master.ID, masterBranch, ""); err != nil {
		t.Fatalf("stamp master branch: %v", err)
	}

	child, err := s.Create(model.CreateRequirementReq{
		ProjectID:   "proj_a",
		Title:       "child",
		Description: "child body",
		ParentID:    master.ID,
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if child.BaseBranch != masterBranch {
		t.Fatalf("child base_branch = %q, want %q", child.BaseBranch, masterBranch)
	}
	if child.BaseBranchSource != model.BaseSourceParent {
		t.Fatalf("child base_branch_source = %q, want %q", child.BaseBranchSource, model.BaseSourceParent)
	}
	// The read path must hand the frontend the very same value, so an
	// untouched picker echoes it back and is NOT read as an explicit choice.
	if child.BaseBranchEffective != masterBranch {
		t.Fatalf("child base_branch_effective = %q, want %q", child.BaseBranchEffective, masterBranch)
	}

	// A master that has NOT developed yet has no branch of its own: the
	// child inherits the master's base instead.
	freshMaster := mkReq(t, s, "proj_a", "fresh master")
	if err := s.UpdateBaseBranchWithSource(freshMaster.ID, "develop", model.BaseSourceExplicit); err != nil {
		t.Fatalf("stamp master base: %v", err)
	}
	child2, err := s.Create(model.CreateRequirementReq{
		ProjectID: "proj_a",
		Title:     "child2",
		ParentID:  freshMaster.ID,
	})
	if err != nil {
		t.Fatalf("create child2: %v", err)
	}
	if child2.BaseBranch != "develop" {
		t.Fatalf("child2 base_branch = %q, want %q", child2.BaseBranch, "develop")
	}
	if child2.BaseBranchSource != model.BaseSourceParent {
		t.Fatalf("child2 base_branch_source = %q, want %q", child2.BaseBranchSource, model.BaseSourceParent)
	}

	// A master with neither a branch nor a base: the child falls back to the
	// project default — but the source still says "parent", because the
	// value was derived from the master, not picked by the user.
	bareMaster := mkReq(t, s, "proj_a", "bare master")
	child3, err := s.Create(model.CreateRequirementReq{
		ProjectID: "proj_a",
		Title:     "child3",
		ParentID:  bareMaster.ID,
	})
	if err != nil {
		t.Fatalf("create child3: %v", err)
	}
	if child3.BaseBranch != "main" {
		t.Fatalf("child3 base_branch = %q, want %q", child3.BaseBranch, "main")
	}
	if child3.BaseBranchSource != model.BaseSourceParent {
		t.Fatalf("child3 base_branch_source = %q, want %q", child3.BaseBranchSource, model.BaseSourceParent)
	}

	// A TOP-LEVEL requirement inherits nothing: no base, no source. The
	// resolver then falls back to the project default, which keeps following
	// projects.default_branch.
	top := mkReq(t, s, "proj_a", "top")
	if top.BaseBranch != "" || top.BaseBranchSource != "" {
		t.Fatalf("top-level row: base_branch=%q source=%q, want both empty",
			top.BaseBranch, top.BaseBranchSource)
	}
}
