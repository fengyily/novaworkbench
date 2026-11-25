package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// The plan-mode sub-task endpoints: read the plan, and convert a finished
// plan row into its implementation phase.
//
// The conversion is the load-bearing one: it flips `status` and `mode` in a
// single guarded UPDATE, so a second concurrent click loses the race and
// gets a 409 instead of spawning a second claude process on the same row.

func newPlanModeTestHandler(t *testing.T) (*WizardHandler, *db.DB, *service.SubTaskService, string) {
	t.Helper()
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES ('proj_plan', 'p', '/tmp/p', 'ready', 'main')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	reqSvc := service.NewRequirementService(d, nil)
	subTaskSvc := service.NewSubTaskService(d, nil)
	row, err := reqSvc.Create(model.CreateRequirementReq{
		ProjectID:   "proj_plan",
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
	return h, d, subTaskSvc, row.ID
}

// seedPlanSubTask creates a sub-task row in the given mode / status and (when
// plan is non-empty) stores a plan for it.
func seedPlanSubTask(t *testing.T, d *db.DB, svc *service.SubTaskService, reqID, mode, status, plan string) *model.SubTask {
	t.Helper()
	row, err := svc.Create(
		reqID, "child", "do the thing", "", "sid-src",
		"", 0, "", model.SubTaskSessionModeFork, "", model.SubTaskSourceManual,
		model.SubTaskReportModeAuto, mode,
	)
	if err != nil {
		t.Fatalf("create sub-task: %v", err)
	}
	if status != model.SubTaskStatusPending {
		if _, err := d.Exec(`UPDATE sub_tasks SET status=? WHERE id=?`, status, row.ID); err != nil {
			t.Fatalf("seed status: %v", err)
		}
	}
	if plan != "" {
		if err := svc.SavePlan(row.ID, plan); err != nil {
			t.Fatalf("save plan: %v", err)
		}
	}
	return row
}

// TestGetSubTaskPlan pins the read endpoint: the plan comes back, a row that
// belongs to another requirement is a 404, and a row without a plan is a 200
// with an empty body (NOT a 404 — the card renders "no plan", not an error).
func TestGetSubTaskPlan(t *testing.T) {
	h, d, svc, reqID := newPlanModeTestHandler(t)

	row := seedPlanSubTask(t, d, svc, reqID, model.SubTaskModePlan, model.SubTaskStatusDone, "## 目标\n做一件事")

	// The happy path.
	rec := httptest.NewRecorder()
	h.GetSubTaskPlan(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("id", reqID)
	req.SetPathValue("sid", row.ID)
	rec = httptest.NewRecorder()
	h.GetSubTaskPlan(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GetSubTaskPlan status = %d, want 200", rec.Code)
	}
	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			Content string `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !payload.Success {
		t.Fatalf("success = false, body = %s", rec.Body.String())
	}
	if payload.Data.Content != "## 目标\n做一件事" {
		t.Errorf("content = %q, want the saved plan", payload.Data.Content)
	}

	// A row that belongs to another requirement is a 404.
	otherReq := httptest.NewRequest(http.MethodGet, "/", nil)
	otherReq.SetPathValue("id", "req_somewhere_else")
	otherReq.SetPathValue("sid", row.ID)
	rec = httptest.NewRecorder()
	h.GetSubTaskPlan(rec, otherReq)
	if rec.Code != http.StatusNotFound {
		t.Errorf("foreign requirement status = %d, want 404", rec.Code)
	}

	// A row with no plan is a 200 with an empty body.
	noPlan := seedPlanSubTask(t, d, svc, reqID, model.SubTaskModeAuto, model.SubTaskStatusDone, "")
	emptyReq := httptest.NewRequest(http.MethodGet, "/", nil)
	emptyReq.SetPathValue("id", reqID)
	emptyReq.SetPathValue("sid", noPlan.ID)
	rec = httptest.NewRecorder()
	h.GetSubTaskPlan(rec, emptyReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-plan status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"content":""`) {
		t.Errorf("no-plan body = %s, want an empty content", rec.Body.String())
	}
}

// TestStartSubTaskDev pins the conversion: a finished plan row flips to
// pending/auto in place, the plan survives, and every ineligible row is
// refused with a 409.
func TestStartSubTaskDev(t *testing.T) {
	h, d, svc, reqID := newPlanModeTestHandler(t)

	call := func(t *testing.T, sid string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
		req.SetPathValue("id", reqID)
		req.SetPathValue("sid", sid)
		rec := httptest.NewRecorder()
		h.StartSubTaskDev(rec, req)
		return rec
	}

	// --- the happy path -----------------------------------------------------
	row := seedPlanSubTask(t, d, svc, reqID, model.SubTaskModePlan, model.SubTaskStatusDone, "## 改动点\n改 foo.go")
	rec := call(t, row.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("StartSubTaskDev status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			JobID      string `json:"job_id"`
			SubTaskID string `json:"sub_task_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !payload.Success {
		t.Fatalf("success = false, body = %s", rec.Body.String())
	}
	if payload.Data.JobID == "" {
		t.Error("job_id is empty")
	}
	if payload.Data.SubTaskID != row.ID {
		t.Errorf("sub_task_id = %q, want %q — the row must be re-armed in place", payload.Data.SubTaskID, row.ID)
	}

	got, err := svc.Get(row.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != model.SubTaskStatusPending {
		t.Errorf("status = %q, want %q", got.Status, model.SubTaskStatusPending)
	}
	if got.Mode != model.SubTaskModeAuto {
		t.Errorf("mode = %q, want %q", got.Mode, model.SubTaskModeAuto)
	}
	plan, err := svc.GetPlan(row.ID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if plan != "## 改动点\n改 foo.go" {
		t.Errorf("plan = %q, want it preserved", plan)
	}

	// A second click is refused — the guarded UPDATE matches no row.
	rec = call(t, row.ID)
	if rec.Code != http.StatusConflict {
		t.Errorf("second StartSubTaskDev status = %d, want 409", rec.Code)
	}

	// --- an ordinary (auto) row is refused ---------------------------------
	autoRow := seedPlanSubTask(t, d, svc, reqID, model.SubTaskModeAuto, model.SubTaskStatusDone, "## 目标")
	if rec := call(t, autoRow.ID); rec.Code != http.StatusConflict {
		t.Errorf("auto row status = %d, want 409", rec.Code)
	}

	// --- a failed plan row is refused ---------------------------------------
	errRow := seedPlanSubTask(t, d, svc, reqID, model.SubTaskModePlan, model.SubTaskStatusError, "## 目标")
	if rec := call(t, errRow.ID); rec.Code != http.StatusConflict {
		t.Errorf("error row status = %d, want 409", rec.Code)
	}

	// --- a plan row with no plan is refused ---------------------------------
	emptyRow := seedPlanSubTask(t, d, svc, reqID, model.SubTaskModePlan, model.SubTaskStatusDone, "")
	if rec := call(t, emptyRow.ID); rec.Code != http.StatusConflict {
		t.Errorf("plan-less row status = %d, want 409", rec.Code)
	}
}

// TestStartSubTaskDev_FreshSessionNeedsNoSourceSession pins the session
// rule: only a FORK-mode implementation phase needs a parent session to
// fork from.
//
// The with_context / bare modes start a fresh session instead (the runner
// clears the source session for those), so they must not be refused just
// because the requirement has no coding session. A plan-mode row created
// with 「带上下文」 has exactly that shape — refusing it would leave the
// user with a plan they can never act on.
func TestStartSubTaskDev_FreshSessionNeedsNoSourceSession(t *testing.T) {
	h, d, svc, reqID := newPlanModeTestHandler(t)

	// A plan-mode row whose session mode is with_context, on a requirement
	// that has NO coding session at all.
	row := seedPlanSubTask(t, d, svc, reqID, model.SubTaskModePlan, model.SubTaskStatusDone, "## 目标\n做一件事")
	if _, err := d.Exec(`UPDATE sub_tasks SET session_mode=? WHERE id=?`,
		model.SubTaskSessionModeWithContext, row.ID); err != nil {
		t.Fatalf("seed session_mode: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	req.SetPathValue("id", reqID)
	req.SetPathValue("sid", row.ID)
	rec := httptest.NewRecorder()
	h.StartSubTaskDev(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("StartSubTaskDev status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	got, err := svc.Get(row.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != model.SubTaskStatusPending || got.Mode != model.SubTaskModeAuto {
		t.Errorf("row = status %q mode %q, want pending/auto", got.Status, got.Mode)
	}
	// A fresh-session row carries no source session id (the audit trail
	// mirrors StartSubTask).
	if got.SourceSessionID != "" {
		t.Errorf("source_session_id = %q, want empty for a fresh-session row", got.SourceSessionID)
	}
	// The plan must survive the conversion.
	plan, err := svc.GetPlan(row.ID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if plan != "## 目标\n做一件事" {
		t.Errorf("plan = %q, want it preserved", plan)
	}
}
