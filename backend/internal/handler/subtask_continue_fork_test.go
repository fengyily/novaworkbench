package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// TestContinueSubTaskForksIntoANewSession pins the session semantics of
// 「继续执行」.
//
// The OLD behavior resumed the row's own session IN PLACE (--resume <old>,
// no --fork-session), so the new turn was appended to the very JSONL the
// previous attempt wrote. When that previous attempt was an ORPHAN — a
// backend that was SIGKILLed leaves the claude child running — two processes
// were appending to the same file, and the model re-derived the same
// conclusion over and over.
//
// The new behavior forks: --resume <old> --fork-session --session-id <new>.
// The model still inherits the previous attempt's conversation, but this turn
// lands in a file of its own.
//
// It also pins the audit fix: source_session_id must keep pointing at the
// row's ORIGINAL source (the requirement's coding session), NOT at the row's
// own session id — the old code overwrote it with the row's own id, so the
// row claimed to be its own parent.
func TestContinueSubTaskForksIntoANewSession(t *testing.T) {
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Skipf("db init: %v", err)
	}
	defer d.Close()

	const reqID = "req_continue_fork"
	const stID = "st_continue_fork"
	const origSession = "sid_original"
	const ownSession = "sid_own"

	if _, err := d.Exec(`INSERT INTO requirements (id, title, project_id, coding_session_id)
		VALUES (?, '继续执行需求', 'proj_x', ?)`, reqID, origSession); err != nil {
		t.Fatalf("seed requirement: %v", err)
	}
	now := time.Now()
	if _, err := d.Exec(`INSERT INTO sub_tasks
		(id, requirement_id, title, prompt, status, session_id, source_session_id,
		 job_id, artifact, model, source, batch_id, input_tokens, output_tokens,
		 created_at, updated_at)
		VALUES (?, ?, 't', 'p', 'error', ?, ?, '', '', '', 'manual', '', 1, 2, ?, ?)`,
		stID, reqID, ownSession, origSession, now, now); err != nil {
		t.Fatalf("seed sub_task: %v", err)
	}

	subTaskSvc := service.NewSubTaskService(d, nil)
	reqSvc := service.NewRequirementService(d, nil)
	jobs := store.NewJobStore(50)
	jobLogSvc := service.NewJobLogService(d)
	runner := NewSubTaskRunner(nil, subTaskSvc, reqSvc, jobs, nil, nil, jobLogSvc, nil, nil, nil, nil, nil, nil, 0, nil, nil, nil)
	h := NewWizardHandler(d, nil, reqSvc, nil, nil, jobs, nil, nil, nil, nil, nil, nil, nil, subTaskSvc, runner, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/requirements/"+reqID+"/sub-tasks/"+stID+"/continue", nil)
	req.SetPathValue("id", reqID)
	req.SetPathValue("sid", stID)
	rec := httptest.NewRecorder()
	h.ContinueSubTask(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			JobID     string `json:"job_id"`
			SubTaskID string `json:"sub_task_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}
	if resp.Data.JobID == "" {
		t.Error("the response carries no job_id — the row would not be dispatchable")
	}

	got, err := subTaskSvc.Get(stID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// The new session id must be a FRESH one — the old id would mean the
	// new turn is appended to the previous attempt's file again.
	if got.SessionID == ownSession {
		t.Error("session_id is unchanged — the continue resumed the previous attempt's file in place")
	}
	if got.SessionID == "" {
		t.Error("session_id is empty — the run would have no session to write into")
	}
	// The audit chain: source_session_id keeps pointing at the ORIGINAL
	// source, not at the row's own session id.
	if got.SourceSessionID != origSession {
		t.Errorf("SourceSessionID = %q, want %q (the row's own id must not become its source)",
			got.SourceSessionID, origSession)
	}
	if got.SourceSessionID == got.SessionID {
		t.Error("source_session_id equals session_id — the row claims to be its own parent")
	}
}

// TestContinueSubTaskKeepsTheOriginalSource is the companion of the test
// above for the ROW-LEVEL bookkeeping: after a continue the row must still
// point at the requirement's coding session, so a later 「重做」 can fork
// from the same origin.
func TestContinueSubTaskKeepsTheOriginalSource(t *testing.T) {
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Skipf("db init: %v", err)
	}
	defer d.Close()

	const reqID = "req_continue_src"
	const stID = "st_continue_src"
	const origSession = "sid_origin"

	if _, err := d.Exec(`INSERT INTO requirements (id, title, project_id, coding_session_id)
		VALUES (?, '需求', 'proj_x', ?)`, reqID, origSession); err != nil {
		t.Fatalf("seed requirement: %v", err)
	}
	now := time.Now()
	if _, err := d.Exec(`INSERT INTO sub_tasks
		(id, requirement_id, title, prompt, status, session_id, source_session_id,
		 job_id, artifact, model, source, batch_id, input_tokens, output_tokens,
		 created_at, updated_at)
		VALUES (?, ?, 't', 'p', 'error', 'sid_prev', ?, '', '', '', 'manual', '', 1, 2, ?, ?)`,
		stID, reqID, origSession, now, now); err != nil {
		t.Fatalf("seed sub_task: %v", err)
	}

	subTaskSvc := service.NewSubTaskService(d, nil)
	reqSvc := service.NewRequirementService(d, nil)
	jobs := store.NewJobStore(50)
	jobLogSvc := service.NewJobLogService(d)
	runner := NewSubTaskRunner(nil, subTaskSvc, reqSvc, jobs, nil, nil, jobLogSvc, nil, nil, nil, nil, nil, nil, 0, nil, nil, nil)
	h := NewWizardHandler(d, nil, reqSvc, nil, nil, jobs, nil, nil, nil, nil, nil, nil, nil, subTaskSvc, runner, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/requirements/"+reqID+"/sub-tasks/"+stID+"/continue", nil)
	req.SetPathValue("id", reqID)
	req.SetPathValue("sid", stID)
	rec := httptest.NewRecorder()
	h.ContinueSubTask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	got, err := subTaskSvc.Get(stID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SessionID == "sid_prev" {
		t.Error("the continue reused the previous attempt's session id")
	}
	if got.SourceSessionID != origSession {
		t.Errorf("SourceSessionID = %q, want %q", got.SourceSessionID, origSession)
	}
	// The row must be dispatchable (a job id) — otherwise it sits at
	// 「排队中」 forever.
	if got.JobID == "" {
		t.Error("the row carries no job_id — it would sit at 排队中 forever")
	}
	_ = model.SubTaskStatusRunning
}
