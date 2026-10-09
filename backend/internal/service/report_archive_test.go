package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/db"
)

// newReportArchiveTestDB wraps the shared newTestDB and seeds a project +
// requirement row the report-archive tests can hang dev_report / subtask_report
// knowledge rows from. Keeps each test isolated to a fresh DB so a stray
// row from a prior case never leaks.
func newReportArchiveTestDB(t *testing.T) (*db.DB, string) {
	t.Helper()
	d := newTestDB(t)
	if _, err := d.Exec("INSERT INTO projects (id, name, local_path, description) VALUES ('proj_1', 'Test', '/tmp/proj1', '')"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := d.Exec("INSERT INTO requirements (id, project_id, kind, title, description, status) VALUES ('req_1', 'proj_1', 'feature', 'test req', 'desc', 'done')"); err != nil {
		t.Fatalf("seed requirement: %v", err)
	}
	return d, "proj_1"
}

func TestReportArchiveService_Upsert_InsertThenUpdate(t *testing.T) {
	d, projectID := newReportArchiveTestDB(t)
	svc := NewReportArchiveService(d)

	// First Upsert: should INSERT a new knowledge row.
	kb1, err := svc.Upsert(projectID, SourceTypeDevReport, "req_1", "v1 title", "v1 content")
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if kb1 == nil || kb1.ID == "" {
		t.Fatalf("first upsert returned empty kb: %+v", kb1)
	}
	if kb1.Title != "v1 title" || kb1.Content != "v1 content" {
		t.Errorf("first upsert content mismatch: title=%q content=%q", kb1.Title, kb1.Content)
	}
	if !kb1.IsApproved {
		t.Error("first upsert: IsApproved should be true (the row participates in knowledge injection immediately)")
	}
	if kb1.IsReviewed {
		t.Error("first upsert: IsReviewed should be false (lands in the 待 Review queue)")
	}

	// Second Upsert with the same (project_id, source_ref, source_type): should
	// UPDATE the existing row, not INSERT a duplicate.
	kb2, err := svc.Upsert(projectID, SourceTypeDevReport, "req_1", "v2 title", "v2 content")
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if kb2.ID != kb1.ID {
		t.Errorf("id changed across upsert: kb1=%s kb2=%s (upsert should reuse id)", kb1.ID, kb2.ID)
	}
	if kb2.Title != "v2 title" || kb2.Content != "v2 content" {
		t.Errorf("second upsert content not refreshed: title=%q content=%q", kb2.Title, kb2.Content)
	}

	// Verify only one knowledge row exists for this (project, source_ref, source_type).
	var n int
	if err := d.QueryRow(
		"SELECT COUNT(*) FROM knowledge WHERE project_id=? AND source_ref=? AND source_type=?",
		projectID, "req_1", SourceTypeDevReport,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 knowledge row after re-upsert, got %d", n)
	}
}

func TestReportArchiveService_ListForRequirement_BothTypes(t *testing.T) {
	d, projectID := newReportArchiveTestDB(t)
	if _, err := d.Exec("INSERT INTO sub_tasks (id, requirement_id, title, prompt, status) VALUES ('st_1', 'req_1', 'sub 1', 'do x', 'done')"); err != nil {
		t.Fatalf("seed sub_task: %v", err)
	}
	svc := NewReportArchiveService(d)
	if _, err := svc.Upsert(projectID, SourceTypeDevReport, "req_1", "dev", "dev content"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Upsert(projectID, SourceTypeSubtaskReport, "st_1", "sub", "sub content"); err != nil {
		t.Fatal(err)
	}
	// Distraction row: a dev_report for a DIFFERENT requirement, must NOT
	// show up when listing req_1.
	if _, err := svc.Upsert(projectID, SourceTypeDevReport, "req_other", "other", "other content"); err != nil {
		t.Fatal(err)
	}

	got, err := svc.ListForRequirement("req_1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 entries (req_1, st_1), got %d: %+v", len(got), got)
	}
	if _, ok := got["req_1"]; !ok {
		t.Error("missing dev_report entry for req_1")
	}
	if _, ok := got["st_1"]; !ok {
		t.Error("missing subtask_report entry for st_1")
	}
	if _, ok := got["req_other"]; ok {
		t.Error("should NOT include other requirement's archive (req_other)")
	}
}

func TestReportArchiveService_DeleteBySourceRefs(t *testing.T) {
	d, projectID := newReportArchiveTestDB(t)
	svc := NewReportArchiveService(d)
	// Three rows: 2 subtask_report + 1 dev_report. DeleteBySourceRefs with
	// type=subtask_report + refs=[st_1, st_2] must remove the 2 subtask rows
	// and leave the dev_report row intact.
	if _, err := svc.Upsert(projectID, SourceTypeSubtaskReport, "st_1", "a", "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Upsert(projectID, SourceTypeSubtaskReport, "st_2", "c", "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Upsert(projectID, SourceTypeDevReport, "req_1", "e", "f"); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteBySourceRefs(SourceTypeSubtaskReport, []string{"st_1", "st_2"}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM knowledge").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row remaining (dev_report), got %d", n)
	}
	var remainingType string
	if err := d.QueryRow("SELECT source_type FROM knowledge LIMIT 1").Scan(&remainingType); err != nil {
		t.Fatalf("read remaining: %v", err)
	}
	if remainingType != SourceTypeDevReport {
		t.Errorf("remaining row's source_type = %q, want %q", remainingType, SourceTypeDevReport)
	}

	// No-op on empty refs must not error (caller passes subIDs straight
	// through without a guard).
	if err := svc.DeleteBySourceRefs(SourceTypeSubtaskReport, nil); err != nil {
		t.Errorf("empty refs delete: %v", err)
	}
	if err := svc.DeleteBySourceRefs(SourceTypeSubtaskReport, []string{}); err != nil {
		t.Errorf("zero-len refs delete: %v", err)
	}
}

func TestSubTaskService_DeleteByIDs_CleansUpKnowledge(t *testing.T) {
	d, projectID := newReportArchiveTestDB(t)
	// Two sub_tasks under req_1, each with a subtask_report archive.
	if _, err := d.Exec("INSERT INTO sub_tasks (id, requirement_id, title, prompt, status) VALUES ('st_1', 'req_1', 's1', 'p1', 'done'), ('st_2', 'req_1', 's2', 'p2', 'done')"); err != nil {
		t.Fatalf("seed sub_tasks: %v", err)
	}
	archSvc := NewReportArchiveService(d)
	if _, err := archSvc.Upsert(projectID, SourceTypeSubtaskReport, "st_1", "t1", "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := archSvc.Upsert(projectID, SourceTypeSubtaskReport, "st_2", "t2", "c2"); err != nil {
		t.Fatal(err)
	}

	// Sanity: 2 knowledge rows present.
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM knowledge WHERE source_type='subtask_report'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("precondition: expected 2 subtask_report rows, got %d", n)
	}

	// Wire the archiveSvc into SubTaskService and delete both sub_tasks.
	subSvc := NewSubTaskService(d, archSvc)
	if err := subSvc.DeleteByIDs([]string{"st_1", "st_2"}); err != nil {
		t.Fatalf("delete by ids: %v", err)
	}

	// Both sub_tasks and their subtask_report knowledge rows must be gone.
	var stCount, kbCount int
	d.QueryRow("SELECT COUNT(*) FROM sub_tasks").Scan(&stCount)
	d.QueryRow("SELECT COUNT(*) FROM knowledge WHERE source_type='subtask_report'").Scan(&kbCount)
	if stCount != 0 {
		t.Errorf("expected 0 sub_tasks, got %d", stCount)
	}
	if kbCount != 0 {
		t.Errorf("expected 0 subtask_report knowledge rows after DeleteByIDs, got %d", kbCount)
	}
}
