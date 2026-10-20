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
