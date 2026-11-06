package handler

import (
	"errors"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestRequirementBranchName pins the branch-name convention every
// requirement develops on: feat/req_<id> for requirement / idea / wiki /
// legacy rows, fix/req_<id> for issues.
//
// The req_ prefix is part of the contract (the user sees it in the URL and
// the UI), so the expected values keep it verbatim.
func TestRequirementBranchName(t *testing.T) {
	cases := []struct {
		name string
		kind string
		want string
	}{
		{"requirement", "requirement", "feat/req_x"},
		{"idea", "idea", "feat/req_x"},
		{"wiki", "wiki", "feat/req_x"},
		{"legacy empty kind", "", "feat/req_x"},
		{"issue", "issue", "fix/req_x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := requirementBranchName(c.kind, "req_x"); got != c.want {
				t.Fatalf("requirementBranchName(%q, %q) = %q, want %q", c.kind, "req_x", got, c.want)
			}
		})
	}
}

// TestIsRequirementBranch guards the predicate that decides whether we may
// publish a branch to origin on the user's behalf. Only branches this app
// created for a requirement qualify; anything else (the project default,
// a release line, a hand-made branch) belongs to the user.
//
// The suffix length is deliberately NOT constrained — requirement ids are
// req_ plus 8 or 16 hex characters depending on the caller, and legacy
// rows may carry longer ids.
func TestIsRequirementBranch(t *testing.T) {
	cases := []struct {
		name   string
		branch string
		want   bool
	}{
		{"feat requirement branch", "feat/req_abc", true},
		{"fix requirement branch", "fix/req_abc", true},
		{"feat with long id", "feat/req_92a0381bc53bb526", true},
		{"fix with long id", "fix/req_92a0381bc53bb526", true},
		{"project default", "main", false},
		{"develop", "develop", false},
		{"release line", "release/1.0", false},
		{"prefix without id", "feat/", false},
		{"fix prefix without id", "fix/", false},
		{"empty", "", false},
		{"similar but not prefixed", "feature/req_abc", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isRequirementBranch(c.branch); got != c.want {
				t.Fatalf("isRequirementBranch(%q) = %v, want %v", c.branch, got, c.want)
			}
		})
	}
}

// TestResolveEffectiveBase pins the base-branch resolution chain — the rule
// that decides which branch a requirement run forks from.
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
func TestResolveEffectiveBase(t *testing.T) {
	cases := []struct {
		name         string
		req          *model.Requirement
		projDefault  string
		incoming     string
		parentBranch string
		wantBase     string
		wantSource   string
	}{
		{
			name:        "tier1 incoming differs from auto is explicit",
			req:         &model.Requirement{ID: "req_C"},
			projDefault: "main",
			incoming:    "develop",
			wantBase:    "develop",
			wantSource:  model.BaseSourceExplicit,
		},
		{
			name:        "tier1 incoming equals auto is not explicit",
			req:         &model.Requirement{ID: "req_C"},
			projDefault: "main",
			incoming:    "main",
			wantBase:    "main",
			wantSource:  model.BaseSourceProject,
		},
		{
			name:        "empty incoming falls through to auto",
			req:         &model.Requirement{ID: "req_C"},
			projDefault: "main",
			incoming:    "",
			wantBase:    "main",
			wantSource:  model.BaseSourceProject,
		},
		{
			name:         "tier2 persisted explicit wins over the parent branch",
			req:          &model.Requirement{ID: "req_C", ParentID: "req_P", BaseBranch: "develop", BaseBranchSource: model.BaseSourceExplicit},
			projDefault:  "main",
			incoming:     "",
			parentBranch: "feat/req_P",
			wantBase:     "develop",
			wantSource:   model.BaseSourceExplicit,
		},
		{
			name:        "tier2 persisted explicit is what auto resolves to",
			req:         &model.Requirement{ID: "req_C", BaseBranch: "develop", BaseBranchSource: model.BaseSourceExplicit},
			projDefault: "main",
			incoming:    "develop",
			wantBase:    "develop",
			wantSource:  model.BaseSourceExplicit,
		},
		{
			name:         "tier3 child inherits the parent branch",
			req:          &model.Requirement{ID: "req_C", ParentID: "req_P"},
			projDefault:  "main",
			incoming:     "",
			parentBranch: "feat/req_P",
			wantBase:     "feat/req_P",
			wantSource:   model.BaseSourceParent,
		},
		{
			name:         "tier3 parent branch beats a project-derived stored value",
			req:          &model.Requirement{ID: "req_C", ParentID: "req_P", BaseBranch: "main", BaseBranchSource: model.BaseSourceProject},
			projDefault:  "main",
			incoming:     "",
			parentBranch: "feat/req_P",
			wantBase:     "feat/req_P",
			wantSource:   model.BaseSourceParent,
		},
		{
			name:         "tier4 non-child reuses its own stored value",
			req:          &model.Requirement{ID: "req_C", BaseBranch: "release/1.0", BaseBranchSource: model.BaseSourceRequirement},
			projDefault:  "main",
			incoming:     "",
			parentBranch: "",
			wantBase:     "release/1.0",
			wantSource:   model.BaseSourceRequirement,
		},
		{
			name:         "tier4 does not reuse a project-derived value",
			req:          &model.Requirement{ID: "req_C", BaseBranch: "main", BaseBranchSource: model.BaseSourceProject},
			projDefault:  "main",
			incoming:     "",
			parentBranch: "",
			wantBase:     "main",
			wantSource:   model.BaseSourceProject,
		},
		{
			name:         "tier4 does not reuse a default-sourced value",
			req:          &model.Requirement{ID: "req_C", BaseBranch: "main", BaseBranchSource: model.BaseSourceDefault},
			projDefault:  "develop",
			incoming:     "",
			parentBranch: "",
			wantBase:     "develop",
			wantSource:   model.BaseSourceProject,
		},
		{
			name:         "legacy row without a source re-resolves to the project default",
			req:          &model.Requirement{ID: "req_C", BaseBranch: "main"},
			projDefault:  "develop",
			incoming:     "",
			parentBranch: "",
			wantBase:     "develop",
			wantSource:   model.BaseSourceProject,
		},
		{
			name:         "tier6 empty project default falls back to main",
			req:          &model.Requirement{ID: "req_C"},
			projDefault:  "",
			incoming:     "",
			parentBranch: "",
			wantBase:     "main",
			wantSource:   model.BaseSourceDefault,
		},
		{
			name:         "nil requirement degrades to the project default",
			req:          nil,
			projDefault:  "main",
			incoming:     "",
			parentBranch: "",
			wantBase:     "main",
			wantSource:   model.BaseSourceProject,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotBase, gotSource := resolveEffectiveBase(c.req, c.projDefault, c.incoming, c.parentBranch)
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lookupCalled = false
			if got := parentBranchBaseWith(c.lookup, c.req); got != c.want {
				t.Fatalf("parentBranchBaseWith() = %q, want %q", got, c.want)
			}
			if c.wantNoDBHit && lookupCalled {
				t.Fatal("lookup must not be called when there is no parent")
			}
		})
	}
}

// TestResolveEffectiveBaseForNilSafe pins that the WizardHandler wrapper
// tolerates a missing requirement, a missing project and an unwired
// requirement service — the resolver is called from stage prologues where
// any of those can be absent, and a panic there would take the whole run
// down.
func TestResolveEffectiveBaseForNilSafe(t *testing.T) {
	h := &WizardHandler{}

	base, source := h.resolveEffectiveBaseFor(nil, nil, "")
	if base != "main" || source != model.BaseSourceDefault {
		t.Fatalf("resolveEffectiveBaseFor(nil, nil, \"\") = (%q, %q), want (%q, %q)",
			base, source, "main", model.BaseSourceDefault)
	}

	base, source = h.resolveEffectiveBaseFor(&model.Requirement{ID: "req_C"}, &model.Project{DefaultBranch: "develop"}, "")
	if base != "develop" || source != model.BaseSourceProject {
		t.Fatalf("resolveEffectiveBaseFor(req, proj) = (%q, %q), want (%q, %q)",
			base, source, "develop", model.BaseSourceProject)
	}

	base, source = h.resolveEffectiveBaseFor(&model.Requirement{ID: "req_C"}, nil, "release/1.0")
	if base != "release/1.0" || source != model.BaseSourceExplicit {
		t.Fatalf("resolveEffectiveBaseFor(req, nil, incoming) = (%q, %q), want (%q, %q)",
			base, source, "release/1.0", model.BaseSourceExplicit)
	}
}
