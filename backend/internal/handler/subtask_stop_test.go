package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// The Stop endpoint used to be a dead end for exactly one case: a sub-task
// whose JobStore job no longer exists (the backend restarted — the store is
// in-memory — or the row's run died with it).
//
// StopSubTask answered 409 JOB_GONE and wrote NOTHING, so the row stayed
// 'running'. And 'running' is the one status the SubTaskPanel offers no
// button for: 继续/重做 are rendered only on 'error'/'stopped' rows and 删除
// only on 'error' (frontend/src/components/SubTaskPanel.tsx:1227, :1371,
// :1372). The user could neither stop, nor re-run, nor delete the card —
// "点停止，它又变回运行中".
//
// These tests pin the new contract: Stop always lands the row in a state the
// user can act on.

func newStopTestHandler(t *testing.T) (*WizardHandler, *db.DB, *service.RequirementService, *service.SubTaskService, string) {
	t.Helper()
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES ('proj_stop', 'p', '/tmp/p', 'ready', 'main')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	reqSvc := service.NewRequirementService(d, nil)
	subTaskSvc := service.NewSubTaskService(d, nil)
	row, err := reqSvc.Create(model.CreateRequirementReq{
		ProjectID:   "proj_stop",
		Title:       "req",
		Description: "d",
	})
	if err != nil {
		t.Fatalf("create requirement: %v", err)
	}
	h := &WizardHandler{
		db:         d,
		reqSvc:     reqSvc,
		subTaskSvc: subTaskSvc,
		jobs:       store.NewJobStore(50),
	}
	return h, d, reqSvc, subTaskSvc, row.ID
}

// seedRunningSubTask creates a sub-task row in the 'running' state with the
// given job id — the shape of a row whose run died with the backend.
func seedRunningSubTask(t *testing.T, svc *service.SubTaskService, reqID, jobID string) *model.SubTask {
	t.Helper()
	st, err := svc.Create(reqID, "[P1-4] fix", "prompt", "", "", "", 0, "", "", "", "", "")
	if err != nil {
		t.Fatalf("create sub-task: %v", err)
	}
	if _, won, err := svc.MarkRunning(st.ID); err != nil || !won {
		t.Fatalf("MarkRunning: won=%v err=%v", won, err)
	}
	if err := svc.UpdateJobID(st.ID, jobID); err != nil {
		t.Fatalf("UpdateJobID: %v", err)
	}
	return st
}

func postStop(t *testing.T, h *WizardHandler, reqID, sid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/"+reqID+"/sub-tasks/"+sid+"/stop", nil)
	req.SetPathValue("id", reqID)
	req.SetPathValue("sid", sid)
	rec := httptest.NewRecorder()
	h.StopSubTask(rec, req)
	return rec
}

// TestStopSubTask_JobMissing_LandsRowOnStopped is the regression test for the
// reported symptom: the JobStore has no job for this row (the backend
// restarted), and Stop must still leave the row in a state the UI can act on.
func TestStopSubTask_JobMissing_LandsRowOnStopped(t *testing.T) {
	h, _, _, subTaskSvc, reqID := newStopTestHandler(t)
	st := seedRunningSubTask(t, subTaskSvc, reqID, "job_gone_after_restart")

	rec := postStop(t, h, reqID, st.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !payload.Success {
		t.Errorf("response not successful: %s", rec.Body.String())
	}
	if payload.Data.Status != model.SubTaskStatusStopped {
		t.Errorf("response status = %q, want stopped", payload.Data.Status)
	}

	got, err := subTaskSvc.Get(st.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != model.SubTaskStatusStopped {
		t.Fatalf("row status = %q, want stopped (a 'running' row is a dead end for the user)", got.Status)
	}
	if !strings.Contains(got.Artifact, "后台任务已不存在") {
		t.Errorf("artifact = %q, want the explanation why the run is over", got.Artifact)
	}
	if !strings.HasPrefix(got.Artifact, "⏹ 用户中止于 ") {
		t.Errorf("artifact = %q, want the standard stop banner", got.Artifact)
	}
}

// TestStopSubTask_JobTerminal_LandsRowOnStopped covers the sibling case: the
// job is still in the store but already finished, so the runner's final row
// write never landed. The row must not stay 'running'.
func TestStopSubTask_JobTerminal_LandsRowOnStopped(t *testing.T) {
	h, _, _, subTaskSvc, reqID := newStopTestHandler(t)
	job := h.jobs.Create(reqID)
	job.Finish(0, store.JobDone)
	st := seedRunningSubTask(t, subTaskSvc, reqID, job.ID)

	rec := postStop(t, h, reqID, st.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	got, err := subTaskSvc.Get(st.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != model.SubTaskStatusStopped {
		t.Errorf("row status = %q, want stopped", got.Status)
	}
}

// TestStopSubTask_LiveJob_CancelsAndStops is the unchanged happy path: a
// live job is cancelled and the row is stopped with the plain banner.
func TestStopSubTask_LiveJob_CancelsAndStops(t *testing.T) {
	h, _, _, subTaskSvc, reqID := newStopTestHandler(t)
	job := h.jobs.Create(reqID)
	// Stand in for the runner's unwind: the real cancel func aborts the
	// claude process, and the runner then finishes the job.
	cancelled := false
	job.SetCmd(nil, func() {
		cancelled = true
		job.Finish(0, store.JobDone)
	})
	st := seedRunningSubTask(t, subTaskSvc, reqID, job.ID)

	rec := postStop(t, h, reqID, st.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	if !cancelled {
		t.Error("the job's cancel func was not invoked; Stop must cancel the run")
	}
	if _, status, _ := job.Snapshot(); status == store.JobRunning {
		t.Error("the job is still running; Stop must cancel it")
	}
	got, err := subTaskSvc.Get(st.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != model.SubTaskStatusStopped {
		t.Errorf("row status = %q, want stopped", got.Status)
	}
	if strings.Contains(got.Artifact, "后台任务已不存在") {
		t.Errorf("artifact = %q, want the plain banner for a live run", got.Artifact)
	}
}

// TestStopSubTask_NotRunning_Still409 pins the unchanged behaviour: stopping
// a row that is not running is a no-op with a 409.
func TestStopSubTask_NotRunning_Still409(t *testing.T) {
	h, _, _, subTaskSvc, reqID := newStopTestHandler(t)
	st, err := subTaskSvc.Create(reqID, "done row", "prompt", "", "", "", 0, "", "", "", "", "")
	if err != nil {
		t.Fatalf("create sub-task: %v", err)
	}
	if perr := subTaskSvc.Finish(st.ID, model.SubTaskStatusDone, "报告", "", model.SubTaskTokens{}, 0, time.Time{}); perr != nil {
		t.Fatalf("Finish: %v", perr)
	}

	rec := postStop(t, h, reqID, st.ID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	got, err := subTaskSvc.Get(st.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != model.SubTaskStatusDone {
		t.Errorf("row status = %q, want done (a finished row must not be rewritten)", got.Status)
	}
}
