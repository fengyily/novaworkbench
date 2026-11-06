package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestUpdateBaseBranchWithSource covers the paired write of base_branch +
// base_branch_source. The two columns must move together: the resolver
// reads the source to tell "the user chose this" apart from "this was
// merely the project default", and a half-written pair would silently
// change which tier the resolver picks.
func TestUpdateBaseBranchWithSource(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_a", "proj_a", "/tmp/proj_a")
	s := NewRequirementService(d, nil)
	r := mkReq(t, s, "proj_a", "A")

	// A freshly created requirement has no base branch and no source.
	got, err := s.Get(r.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.BaseBranch != "" || got.BaseBranchSource != "" {
		t.Fatalf("fresh row: base_branch=%q source=%q, want both empty",
			got.BaseBranch, got.BaseBranchSource)
	}

	// Write an explicit choice — both columns must land.
	if err := s.UpdateBaseBranchWithSource(r.ID, "develop", model.BaseSourceExplicit); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = s.Get(r.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.BaseBranch != "develop" || got.BaseBranchSource != model.BaseSourceExplicit {
		t.Fatalf("after update: base_branch=%q source=%q, want %q / %q",
			got.BaseBranch, got.BaseBranchSource, "develop", model.BaseSourceExplicit)
	}

	// Re-writing with a different source must overwrite BOTH — a stale
	// source pointing at the previous value is exactly the drift the
	// single-statement write exists to prevent.
	if err := s.UpdateBaseBranchWithSource(r.ID, "feat/req_parent", model.BaseSourceParent); err != nil {
		t.Fatalf("re-update: %v", err)
	}
	got, err = s.Get(r.ID)
	if err != nil {
		t.Fatalf("get after re-update: %v", err)
	}
	if got.BaseBranch != "feat/req_parent" || got.BaseBranchSource != model.BaseSourceParent {
		t.Fatalf("after re-update: base_branch=%q source=%q, want %q / %q",
			got.BaseBranch, got.BaseBranchSource, "feat/req_parent", model.BaseSourceParent)
	}

	// The List projection must expose the same column — it selects and
	// scans independently of Get, so a missing column there would only
	// show up on the list endpoint.
	list, err := s.List("", "", "", "", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, item := range list {
		if item.ID != r.ID {
			continue
		}
		found = true
		if item.BaseBranch != "feat/req_parent" || item.BaseBranchSource != model.BaseSourceParent {
			t.Fatalf("list row: base_branch=%q source=%q, want %q / %q",
				item.BaseBranch, item.BaseBranchSource, "feat/req_parent", model.BaseSourceParent)
		}
	}
	if !found {
		t.Fatalf("requirement %s missing from List", r.ID)
	}
}
