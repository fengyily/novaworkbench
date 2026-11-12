package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/store"
)

// newTestDB opens a fresh in-memory-backed sqlite file for one test. The
// project has no shared test harness (CLAUDE.md: "There is no test suite
// yet"), so each test brings up its own DB + schema.
func newTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: path})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { d.Close(); os.Remove(path) })
	return d
}

func TestStageModelPersistRoundTrip(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_1", "proj_1", "/tmp/proj_1")
	reqSvc := NewRequirementService(d, nil)

	created, err := reqSvc.Create(model.CreateRequirementReq{ProjectID: "proj_1", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Write each stage's effective model on the success path.
	if err := reqSvc.UpdateAnalystModel(created.ID, "claude-sonnet-4-5"); err != nil {
		t.Fatalf("UpdateAnalystModel: %v", err)
	}
	if err := reqSvc.UpdateArchitectModel(created.ID, "默认模型"); err != nil {
		t.Fatalf("UpdateArchitectModel: %v", err)
	}
	if err := reqSvc.UpdateDeveloperModel(created.ID, "claude-opus-4-1"); err != nil {
		t.Fatalf("UpdateDeveloperModel: %v", err)
	}
	// Reviewer stays unwritten (review is persisted on job_logs, not the
	// requirement) — it must read back as the zero value "".

	got, err := reqSvc.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AnalystModel != "claude-sonnet-4-5" {
		t.Errorf("AnalystModel = %q, want claude-sonnet-4-5", got.AnalystModel)
	}
	if got.ArchitectModel != "默认模型" {
		t.Errorf("ArchitectModel = %q, want 默认模型", got.ArchitectModel)
	}
	if got.DeveloperModel != "claude-opus-4-1" {
		t.Errorf("DeveloperModel = %q, want claude-opus-4-1", got.DeveloperModel)
	}
	if got.ReviewerModel != "" {
		t.Errorf("ReviewerModel = %q, want empty (review is on job_logs)", got.ReviewerModel)
	}

	// List must return the same values (the SELECT column list is shared).
	list, err := reqSvc.List("proj_1", "", "", "", "")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	if list[0].AnalystModel != "claude-sonnet-4-5" || list[0].DeveloperModel != "claude-opus-4-1" {
		t.Errorf("list row: analyst=%q developer=%q", list[0].AnalystModel, list[0].DeveloperModel)
	}
}

func TestJobLogModelRoundTrip(t *testing.T) {
	d := newTestDB(t)
	jobSvc := NewJobLogService(d)

	now := time.Now()
	lines := []store.LogLine{{Type: "message", Content: "review body"}}
	if err := jobSvc.Save("job_abc", "", "done", 0, now, now, lines, "claude-haiku"); err != nil {
		t.Fatalf("save: %v", err)
	}

	status, exitCode, startedAt, finishedAt, model, errorKind, gotLines, err := jobSvc.Get("job_abc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if model != "claude-haiku" {
		t.Errorf("model = %q, want claude-haiku", model)
	}
	if status != "done" || exitCode != 0 {
		t.Errorf("status=%q exitCode=%d", status, exitCode)
	}
	if errorKind != "" {
		t.Errorf("errorKind = %q, want empty for a successful job", errorKind)
	}
	if len(gotLines) != 1 || gotLines[0].Content != "review body" {
		t.Errorf("lines = %+v", gotLines)
	}
	if startedAt == "" || finishedAt == "" {
		t.Errorf("timestamps empty: started=%q finished=%q", startedAt, finishedAt)
	}
}

// TestJobLogErrorKindRoundTrip pins the durable failure reason. Before the
// error_kind column existed, a failed job was persisted as status=done /
// exit_code=0 — the record contradicted the failure the user actually saw.
func TestJobLogErrorKindRoundTrip(t *testing.T) {
	d := newTestDB(t)
	jobSvc := NewJobLogService(d)

	now := time.Now()
	lines := []store.LogLine{{Type: "error", Content: "❌ Claude 异常退出: exit status 143"}}
	if err := jobSvc.Save("job_fail", "", "error", 1, now, now, lines, "", "stalled"); err != nil {
		t.Fatalf("save: %v", err)
	}

	status, exitCode, _, _, _, errorKind, gotLines, err := jobSvc.Get("job_fail")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if status != "error" || exitCode != 1 {
		t.Errorf("status=%q exitCode=%d, want error/1", status, exitCode)
	}
	if errorKind != "stalled" {
		t.Errorf("errorKind = %q, want stalled", errorKind)
	}
	if len(gotLines) != 1 || gotLines[0].Type != "error" {
		t.Errorf("lines = %+v", gotLines)
	}

	// A second Save on the same job must update the row, not duplicate it.
	if err := jobSvc.Save("job_fail", "", "error", 1, now, now, lines, "", "terminated"); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	_, _, _, _, _, errorKind, _, err = jobSvc.Get("job_fail")
	if err != nil {
		t.Fatalf("get after re-save: %v", err)
	}
	if errorKind != "terminated" {
		t.Errorf("errorKind after re-save = %q, want terminated", errorKind)
	}
}
