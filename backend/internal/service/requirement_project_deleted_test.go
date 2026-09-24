package service

import (
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// TestListHidesRequirementsOfDeletedProject is the regression pin for
// "项目删除后，需求还在列表中": a project's requirements must disappear from the
// global List/Calendar views the moment the project is soft-deleted (trash)
// or purged, and must reappear unchanged if the project is restored — the
// requirement rows themselves are never physically removed on soft-delete.
func TestListHidesRequirementsOfDeletedProject(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_live", "proj_live", "/tmp/proj_live")
	reqSvc := NewRequirementService(d)
	projSvc := NewProjectService(d, nil)

	if _, err := reqSvc.Create(model.CreateRequirementReq{ProjectID: "proj_live", Title: "a", Description: ""}); err != nil {
		t.Fatalf("create r1: %v", err)
	}
	if _, err := reqSvc.Create(model.CreateRequirementReq{ProjectID: "proj_live", Title: "b", Description: ""}); err != nil {
		t.Fatalf("create r2: %v", err)
	}

	from := time.Now().AddDate(0, 0, -1)
	to := time.Now().AddDate(0, 0, 2)

	// Active project → both queries see both requirements.
	if list, err := reqSvc.List("", "", "", "", ""); err != nil || len(list) != 2 {
		t.Fatalf("List (active): err=%v n=%d, want 2", err, len(list))
	}
	if cal, err := reqSvc.Calendar(from, to, "", ""); err != nil || len(cal) != 2 {
		t.Fatalf("Calendar (active): err=%v n=%d, want 2", err, len(cal))
	}

	// Soft-delete the project → both queries hide its requirements.
	if err := projSvc.Remove("proj_live", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if list, err := reqSvc.List("", "", "", "", ""); err != nil || len(list) != 0 {
		t.Fatalf("List (soft-deleted): err=%v n=%d, want 0", err, len(list))
	}
	if cal, err := reqSvc.Calendar(from, to, "", ""); err != nil || len(cal) != 0 {
		t.Fatalf("Calendar (soft-deleted): err=%v n=%d, want 0", err, len(cal))
	}

	// Simulate a restore → requirements come back untouched (proving nothing
	// on the requirement side was physically deleted).
	if _, err := d.Exec(`UPDATE projects SET deleted_at = NULL WHERE id = ?`, "proj_live"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if list, err := reqSvc.List("", "", "", "", ""); err != nil || len(list) != 2 {
		t.Fatalf("List (restored): err=%v n=%d, want 2", err, len(list))
	}
	if cal, err := reqSvc.Calendar(from, to, "", ""); err != nil || len(cal) != 2 {
		t.Fatalf("Calendar (restored): err=%v n=%d, want 2", err, len(cal))
	}
}

// TestPurgeDeletesRequirementRows pins root cause 2: Purge must physically
// remove a project's requirement rows even on SQLite, where the schema's
// ON DELETE CASCADE never fires (PRAGMA foreign_keys stays off). Before the
// fix this assertion failed — the requirements row survived as an orphan.
func TestPurgeDeletesRequirementRows(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_purge", "proj_purge", "/tmp/proj_purge")
	reqSvc := NewRequirementService(d)
	projSvc := NewProjectService(d, nil)

	if _, err := reqSvc.Create(model.CreateRequirementReq{ProjectID: "proj_purge", Title: "x", Description: ""}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Purge requires the project to be in the trash first.
	if err := projSvc.Remove("proj_purge", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := projSvc.Purge("proj_purge"); err != nil {
		t.Fatalf("Purge: %v", err)
	}

	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM requirements WHERE project_id = ?`, "proj_purge").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("requirements rows after purge = %d, want 0 (orphans left behind)", n)
	}
}
