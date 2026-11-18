package service

import (
	"errors"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestResolveAutoBase pins the base-branch resolution chain — the rule that
// decides which branch a requirement run forks from.
//
// The chain has five tiers plus a caller override:
//
//	tier 1  incoming override          (explicit — only when it differs from auto)
//	tier 2  persisted explicit choice  (explicit)
//	tier 3  the parent requirement's branch (parent)
//	tier 4  this requirement's own stored value (requirement)
//	tier 5  the project's default branch (project)
//	tier 6  the hard-coded "main"       (default)
//
// The two subtle cases are pinned explicitly below:
//
//   - tier 1 fires only when the incoming value DIFFERS from auto, so the
//     frontend echoing the effective value back is not read as a choice;
//   - tier 4 does NOT reuse a project-derived value, so changing
//     projects.default_branch still moves existing requirements onto the
//     new default instead of freezing them on the old one.
//
// The rules live in the service package because the READ path needs them
// too (it computes the effective base the frontend seeds its picker with);
// the handler keeps a thin wrapper so the runtime path runs the very same
// function.
func TestResolveAutoBase(t *testing.T) {
	cases := []struct {
		name         string
		req          *model.Requirement
		projDefault  string
		parentBranch string
		wantBase     string
		wantSource   string
	}{
		{
			name:         "tier2 persisted explicit wins over the parent branch",
			req:          &model.Requirement{ID: "req_C", ParentID: "req_P", BaseBranch: "develop", BaseBranchSource: model.BaseSourceExplicit},
			projDefault:  "main",
			parentBranch: "feat/req_P",
			wantBase:     "develop",
			wantSource:   model.BaseSourceExplicit,
		},
		{
			name:        "tier2 persisted explicit is what auto resolves to",
			req:         &model.Requirement{ID: "req_C", BaseBranch: "develop", BaseBranchSource: model.BaseSourceExplicit},
			projDefault: "main",
			wantBase:    "develop",
			wantSource:  model.BaseSourceExplicit,
		},
		{
			name:         "tier3 child inherits the parent branch",
			req:          &model.Requirement{ID: "req_C", ParentID: "req_P"},
			projDefault:  "main",
			parentBranch: "feat/req_P",
			wantBase:     "feat/req_P",
			wantSource:   model.BaseSourceParent,
		},
		{
			name:         "tier3 parent branch beats a project-derived stored value",
			req:          &model.Requirement{ID: "req_C", ParentID: "req_P", BaseBranch: "main", BaseBranchSource: model.BaseSourceProject},
			projDefault:  "main",
			parentBranch: "feat/req_P",
			wantBase:     "feat/req_P",
			wantSource:   model.BaseSourceParent,
		},
		{
			name:         "tier4 non-child reuses its own stored value",
			req:          &model.Requirement{ID: "req_C", BaseBranch: "release/1.0", BaseBranchSource: model.BaseSourceRequirement},
			projDefault:  "main",
			parentBranch: "",
			wantBase:     "release/1.0",
			wantSource:   model.BaseSourceRequirement,
		},
		{
			name:         "tier4 does not reuse a project-derived value",
			req:          &model.Requirement{ID: "req_C", BaseBranch: "main", BaseBranchSource: model.BaseSourceProject},
			projDefault:  "main",
			parentBranch: "",
			wantBase:     "main",
			wantSource:   model.BaseSourceProject,
		},
		{
			name:         "tier4 does not reuse a default-sourced value",
			req:          &model.Requirement{ID: "req_C", BaseBranch: "main", BaseBranchSource: model.BaseSourceDefault},
			projDefault:  "develop",
			parentBranch: "",
			wantBase:     "develop",
			wantSource:   model.BaseSourceProject,
		},
		{
			name:         "legacy row without a source re-resolves to the project default",
			req:          &model.Requirement{ID: "req_C", BaseBranch: "main"},
			projDefault:  "develop",
			parentBranch: "",
			wantBase:     "develop",
			wantSource:   model.BaseSourceProject,
		},
		{
			name:         "tier6 empty project default falls back to main",
			req:          &model.Requirement{ID: "req_C"},
			projDefault:  "",
			parentBranch: "",
			wantBase:     "main",
			wantSource:   model.BaseSourceDefault,
		},
		{
			name:         "nil requirement degrades to the project default",
			req:          nil,
			projDefault:  "main",
			parentBranch: "",
			wantBase:     "main",
			wantSource:   model.BaseSourceProject,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotBase, gotSource := ResolveAutoBase(c.req, c.projDefault, c.parentBranch)
			if gotBase != c.wantBase {
				t.Fatalf("base = %q, want %q (source %q)", gotBase, c.wantBase, gotSource)
			}
			if gotSource != c.wantSource {
				t.Fatalf("source = %q, want %q", gotSource, c.wantSource)
			}
		})
	}
}

// TestParentBranchBase pins the degradation rules of the parent-branch
// lookup. Every failure mode must collapse to "" so a missing parent can
// never block a run — the caller falls back to the project default branch.
func TestParentBranchBase(t *testing.T) {
	lookupErr := errors.New("requirement not found")

	// lookupCalled records whether the injected lookup was reached, so the
	// "no parent" cases can assert we never even hit the DB.
	var lookupCalled bool
	neverCalled := func(string) (*model.Requirement, error) {
		lookupCalled = true
		return nil, nil
	}

	cases := []struct {
		name        string
		req         *model.Requirement
		lookup      func(string) (*model.Requirement, error)
		want        string
		wantNoDBHit bool
	}{
		{
			name:        "nil requirement",
			req:         nil,
			lookup:      neverCalled,
			want:        "",
			wantNoDBHit: true,
		},
		{
			name:        "no parent id",
			req:         &model.Requirement{ID: "req_C"},
			lookup:      neverCalled,
			want:        "",
			wantNoDBHit: true,
		},
		{
			name: "parent lookup fails",
			req:  &model.Requirement{ID: "req_C", ParentID: "req_P"},
			lookup: func(string) (*model.Requirement, error) {
				return nil, lookupErr
			},
			want: "",
		},
		{
			name: "parent row is nil",
			req:  &model.Requirement{ID: "req_C", ParentID: "req_P"},
			lookup: func(string) (*model.Requirement, error) {
				return nil, nil
			},
			want: "",
		},
		{
			name: "parent runs in no-branch mode",
			req:  &model.Requirement{ID: "req_C", ParentID: "req_P"},
			lookup: func(string) (*model.Requirement, error) {
				return &model.Requirement{ID: "req_P", NoBranch: true, BranchName: "feat/req_P"}, nil
			},
			want: "",
		},
		{
			name: "parent has no branch yet",
			req:  &model.Requirement{ID: "req_C", ParentID: "req_P"},
			lookup: func(string) (*model.Requirement, error) {
				return &model.Requirement{ID: "req_P"}, nil
			},
			want: "",
		},
		{
			name: "parent branch name is blank",
			req:  &model.Requirement{ID: "req_C", ParentID: "req_P"},
			lookup: func(string) (*model.Requirement, error) {
				return &model.Requirement{ID: "req_P", BranchName: "   "}, nil
			},
			want: "",
		},
		{
			name: "parent has a branch",
			req:  &model.Requirement{ID: "req_C", ParentID: "req_P"},
			lookup: func(string) (*model.Requirement, error) {
				return &model.Requirement{ID: "req_P", BranchName: "feat/req_P"}, nil
			},
			want: "feat/req_P",
		},
		// ── 需求拆分 semantics: the parent's DEVELOPMENT branch wins.
		//
		// A split master keeps its own dev branch (feat/req_<master>) as the
		// stack bottom, and every child branches off it — the stacked-PR
		// model. The base is only the fallback for a parent that has no
		// development branch of its own.
		{
			name: "parent development branch wins over the parent base",
			req:  &model.Requirement{ID: "req_C", ParentID: "req_P"},
			lookup: func(string) (*model.Requirement, error) {
				return &model.Requirement{ID: "req_P", BaseBranch: "release/x", BranchName: "feat/req_P"}, nil
			},
			want: "feat/req_P",
		},
		{
			name: "parent base only (parent has no development branch)",
			req:  &model.Requirement{ID: "req_C", ParentID: "req_P"},
			lookup: func(string) (*model.Requirement, error) {
				return &model.Requirement{ID: "req_P", BaseBranch: "release/x"}, nil
			},
			want: "release/x",
		},
		{
			name: "legacy parent without a base falls back to its development branch",
			req:  &model.Requirement{ID: "req_C", ParentID: "req_P"},
			lookup: func(string) (*model.Requirement, error) {
				return &model.Requirement{ID: "req_P", BranchName: "feat/req_P"}, nil
			},
			want: "feat/req_P",
		},
		{
			name: "parent with neither base nor development branch",
			req:  &model.Requirement{ID: "req_C", ParentID: "req_P"},
			lookup: func(string) (*model.Requirement, error) {
				return &model.Requirement{ID: "req_P", BaseBranch: "  ", BranchName: "  "}, nil
			},
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lookupCalled = false
			if got := ParentBranchBase(c.lookup, c.req); got != c.want {
				t.Fatalf("ParentBranchBase() = %q, want %q", got, c.want)
			}
			if c.wantNoDBHit && lookupCalled {
				t.Fatal("lookup must not be called when there is no parent")
			}
		})
	}
}

// TestAttachEffectiveBaseFillsInheritedBase pins the READ half of the
// contract: a child requirement's effective base is the branch its parent
// develops on, and a top-level requirement's is the project default.
//
// The effective base is what the frontend seeds its BASE picker with, so it
// MUST equal the auto value the runtime resolver computes — otherwise the
// picker would send back something the resolver reads as an explicit choice.
func TestAttachEffectiveBaseFillsInheritedBase(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_a", "proj_a", "/tmp/proj_a")
	s := NewRequirementService(d, nil)

	parent := mkReq(t, s, "proj_a", "parent")
	if err := s.UpdateWorktree(parent.ID, "feat/req_"+parent.ID, ""); err != nil {
		t.Fatalf("stamp parent branch: %v", err)
	}
	child, err := s.Create(model.CreateRequirementReq{
		ProjectID: "proj_a",
		Title:     "child",
		ParentID:  parent.ID,
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	top := mkReq(t, s, "proj_a", "top")

	gotChild, err := s.Get(child.ID)
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	if gotChild.BaseBranchEffective != "feat/req_"+parent.ID {
		t.Fatalf("child base_branch_effective = %q, want %q",
			gotChild.BaseBranchEffective, "feat/req_"+parent.ID)
	}
	// The whole point: the value the frontend sends back IS the auto value,
	// so tier 1 of the resolution chain ("the caller overrode the base")
	// never fires and the inheritance survives the round-trip.
	autoBase, autoSource := ResolveAutoBase(gotChild, "main", "feat/req_"+parent.ID)
	if autoBase != gotChild.BaseBranchEffective {
		t.Fatalf("auto base = %q, effective base = %q — the frontend would echo a value the resolver reads as an explicit choice",
			autoBase, gotChild.BaseBranchEffective)
	}
	if autoSource != model.BaseSourceParent {
		t.Fatalf("auto source = %q, want %q", autoSource, model.BaseSourceParent)
	}

	gotTop, err := s.Get(top.ID)
	if err != nil {
		t.Fatalf("get top: %v", err)
	}
	if gotTop.BaseBranchEffective != "main" {
		t.Fatalf("top-level base_branch_effective = %q, want %q", gotTop.BaseBranchEffective, "main")
	}

	// The list projection must carry the same value — it selects and scans
	// independently of Get, so a missing backfill there would only show up
	// on the list endpoint.
	list, err := s.List("proj_a", "", "", "", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	sawChild, sawTop := false, false
	for _, r := range list {
		switch r.ID {
		case child.ID:
			sawChild = true
			if r.BaseBranchEffective != "feat/req_"+parent.ID {
				t.Fatalf("list child base_branch_effective = %q", r.BaseBranchEffective)
			}
		case top.ID:
			sawTop = true
			if r.BaseBranchEffective != "main" {
				t.Fatalf("list top base_branch_effective = %q", r.BaseBranchEffective)
			}
		}
	}
	if !sawChild || !sawTop {
		t.Fatalf("list missing rows: child=%v top=%v", sawChild, sawTop)
	}
}

// TestNoBranchMasterFallsBackToProjectDefault pins the no_branch rule on
// BOTH sides of the seam.
//
// A master in "develop on base" mode has no branch of its own — it commits
// straight onto its base. There is therefore nothing for a child to
// inherit, and both the creation path and the read path must say so: the
// child falls back to the PROJECT DEFAULT.
//
// When the two disagreed, the child row carried a base the resolver would
// never reproduce (the master's own base), the frontend seeded its picker
// with the resolver's value instead, and the next design / coding run
// overwrote the persisted base — the child's PR target silently drifted
// away from the line the child actually developed on.
func TestNoBranchMasterFallsBackToProjectDefault(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_a", "proj_a", "/tmp/proj_a")
	s := NewRequirementService(d, nil)

	master := mkReq(t, s, "proj_a", "no-branch master")
	// The master develops on "develop" — NOT the project default ("main").
	if err := s.UpdateBaseBranchWithSource(master.ID, "develop", model.BaseSourceExplicit); err != nil {
		t.Fatalf("stamp master base: %v", err)
	}
	if err := s.UpdateNoBranch(master.ID, true); err != nil {
		t.Fatalf("stamp no_branch: %v", err)
	}
	masterRow, err := s.Get(master.ID)
	if err != nil {
		t.Fatalf("get master: %v", err)
	}

	child, err := s.Create(model.CreateRequirementReq{
		ProjectID: "proj_a",
		Title:     "child",
		ParentID:  master.ID,
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	// The master's base is NOT inherited — a no_branch master has no branch
	// of its own, so the child falls back to the project default.
	if child.BaseBranch != "main" {
		t.Fatalf("child base_branch = %q, want %q", child.BaseBranch, "main")
	}
	if child.BaseBranchSource != model.BaseSourceParent {
		t.Fatalf("child base_branch_source = %q, want %q", child.BaseBranchSource, model.BaseSourceParent)
	}

	// The read path must agree with the creation path — otherwise the
	// frontend would seed the picker with a value the resolver never
	// reproduces, and the next run would rewrite the persisted base.
	if child.BaseBranchEffective != "main" {
		t.Fatalf("child base_branch_effective = %q, want %q", child.BaseBranchEffective, "main")
	}
	auto, _ := ResolveAutoBase(child, "main", ParentBranchBase(
		func(string) (*model.Requirement, error) { return masterRow, nil }, child))
	if auto != child.BaseBranchEffective {
		t.Fatalf("auto base = %q, effective base = %q — the creation path and the resolver disagree",
			auto, child.BaseBranchEffective)
	}

	// A master that still carries a stale branch_name (it developed before
	// switching to no-branch mode) contributes nothing either: no_branch
	// wins over the branch it no longer develops on.
	if err := s.UpdateWorktree(master.ID, "feat/req_"+master.ID, ""); err != nil {
		t.Fatalf("stamp stale master branch: %v", err)
	}
	staleChild, err := s.Create(model.CreateRequirementReq{
		ProjectID: "proj_a",
		Title:     "child2",
		ParentID:  master.ID,
	})
	if err != nil {
		t.Fatalf("create child2: %v", err)
	}
	if staleChild.BaseBranch != "main" {
		t.Fatalf("child2 base_branch = %q, want %q", staleChild.BaseBranch, "main")
	}
	if staleChild.BaseBranchEffective != "main" {
		t.Fatalf("child2 base_branch_effective = %q, want %q", staleChild.BaseBranchEffective, "main")
	}
}
