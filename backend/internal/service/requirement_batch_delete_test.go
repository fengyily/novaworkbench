package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/db"
)

// countRows counts rows in table where requirement_id = id. Used to assert
// that Delete leaves no orphans behind.
func countRows(t *testing.T, d *db.DB, table, id string) int {
	t.Helper()
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE requirement_id=?", id).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestRequirementBatchDeleteRemovesAllRows pins the happy path: every
// selected row goes away and the untouched one stays.
func TestRequirementBatchDeleteRemovesAllRows(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_bd", "proj_bd", "/tmp/proj_bd")
	s := NewRequirementService(d, nil)
	a, b, c := mkReq(t, s, "proj_bd", "A"), mkReq(t, s, "proj_bd", "B"), mkReq(t, s, "proj_bd", "C")

	res := s.BatchDelete([]string{a.ID, b.ID})
	if res.Deleted != 2 || len(res.Failed) != 0 {
		t.Fatalf("res = %+v, want deleted=2 failed=0", res)
	}

	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM requirements WHERE id IN (?, ?)`, a.ID, b.ID).Scan(&n); err != nil {
		t.Fatalf("count deleted: %v", err)
	}
	if n != 0 {
		t.Fatalf("requirements rows after batch delete = %d, want 0", n)
	}
	if _, err := s.Get(c.ID); err != nil {
		t.Fatalf("untouched requirement %s was deleted: %v", c.ID, err)
	}
}

// TestRequirementBatchDeleteDetachesChildren is the load-bearing one:
// deleting a parent must NOT cascade. The child survives and is promoted to
// top level. Batch delete inherits this by looping over Delete, and this
// test is what keeps that inheritance from silently regressing.
func TestRequirementBatchDeleteDetachesChildren(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_bd", "proj_bd", "/tmp/proj_bd")
	s := NewRequirementService(d, nil)
	a, b := mkReq(t, s, "proj_bd", "parent"), mkReq(t, s, "proj_bd", "child")
	if _, err := s.SetParent(b.ID, a.ID); err != nil {
		t.Fatalf("attach: %v", err)
	}

	res := s.BatchDelete([]string{a.ID})
	if res.Deleted != 1 || len(res.Failed) != 0 {
		t.Fatalf("res = %+v, want deleted=1 failed=0", res)
	}
	if _, err := s.Get(a.ID); err == nil {
		t.Fatalf("parent %s still exists after delete", a.ID)
	}
	got, err := s.Get(b.ID)
	if err != nil {
		t.Fatalf("child was cascade-deleted: %v", err)
	}
	if got.ParentID != "" {
		t.Fatalf("child parent_id = %q, want \"\" (child must be promoted to top level)", got.ParentID)
	}
}

// TestRequirementBatchDeleteCleansChildTables pins the orphan-cleanup fix in
// Delete. SQLite never enables PRAGMA foreign_keys, so the ON DELETE CASCADE
// declared on these tables silently no-ops — without explicit cleanup every
// deleted requirement leaves rows behind, which batch delete multiplies by
// the number of selected rows.
func TestRequirementBatchDeleteCleansChildTables(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_bd", "proj_bd", "/tmp/proj_bd")
	s := NewRequirementService(d, nil)
	r := mkReq(t, s, "proj_bd", "with children")

	// Only id + requirement_id are required; every other column has a
	// default. requirement_attachments.sha256 is NOT NULL with no default,
	// so it must be supplied.
	if _, err := d.Exec(`INSERT INTO sub_tasks (id, requirement_id) VALUES ('st_1', ?)`, r.ID); err != nil {
		t.Fatalf("seed sub_tasks: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO orchestration_batches (id, requirement_id) VALUES ('ob_1', ?)`, r.ID); err != nil {
		t.Fatalf("seed orchestration_batches: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO requirement_attachments (id, requirement_id, filename, stored_path, sha256)
		 VALUES ('att_1', ?, 'a.log', '/tmp/a.log', 'deadbeef')`, r.ID); err != nil {
		t.Fatalf("seed requirement_attachments: %v", err)
	}

	if res := s.BatchDelete([]string{r.ID}); res.Deleted != 1 {
		t.Fatalf("res = %+v, want deleted=1", res)
	}

	for _, tbl := range []string{
		"sub_tasks", "orchestration_batches", "requirement_attachments",
		"refinement_chats", "coding_chats", "scheduled_tasks", "design_revisions",
	} {
		if n := countRows(t, d, tbl, r.ID); n != 0 {
			t.Fatalf("%s rows after delete = %d, want 0 (orphan left behind)", tbl, n)
		}
	}
}

// TestRequirementBatchDeleteReportsPartialFailure pins that a bad id is
// reported per-row instead of rolling back the rows that already succeeded.
// (Contrast with KnowledgeService.BatchReview, which discards per-row
// errors entirely — that's the anti-pattern this endpoint must not copy.)
func TestRequirementBatchDeleteReportsPartialFailure(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_bd", "proj_bd", "/tmp/proj_bd")
	s := NewRequirementService(d, nil)
	good := mkReq(t, s, "proj_bd", "good")

	res := s.BatchDelete([]string{good.ID, "req_missing"})
	if res.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (the valid id must survive a sibling's failure)", res.Deleted)
	}
	if len(res.Failed) != 1 {
		t.Fatalf("failed = %+v, want exactly one entry", res.Failed)
	}
	if res.Failed[0].ID != "req_missing" || res.Failed[0].Message == "" {
		t.Fatalf("failed[0] = %+v, want id=req_missing with a non-empty message", res.Failed[0])
	}
	if _, err := s.Get(good.ID); err == nil {
		t.Fatalf("%s still exists — a sibling failure rolled the whole batch back", good.ID)
	}
}

// TestRequirementBatchDeleteEmptyInput pins that degenerate input is a
// no-op rather than a panic or a spurious failure entry.
func TestRequirementBatchDeleteEmptyInput(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_bd", "proj_bd", "/tmp/proj_bd")
	s := NewRequirementService(d, nil)

	for _, ids := range [][]string{nil, {}, {"", "   "}} {
		res := s.BatchDelete(ids)
		if res.Deleted != 0 || len(res.Failed) != 0 {
			t.Fatalf("BatchDelete(%q) = %+v, want deleted=0 failed=0", ids, res)
		}
	}
}