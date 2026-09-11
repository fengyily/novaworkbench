package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// TestScheduleCreateThenListRoundTrip pins the user-visible flow:
//
//	POST /api/schedules          → 201 + row
//	GET  /api/schedules          → 200 + [row]      (the new row is in the list)
//	POST /api/schedules (same)   → 409 ALREADY_SCHEDULED
//
// The middle assertion is the bug guard: the user reported "after
// creating a scheduled task, the task list doesn't show it" — yet a
// subsequent create of the same (requirement, task_type) correctly
// errors with "already exists pending". That combination is impossible
// unless the List query fails to surface the row just inserted.
//
// If this test fails, the regression is in the handler / service / SQL
// path. If it passes, the user's reported symptom cannot be caused by
// the backend and must be in the deployed binary, the frontend bundle,
// the auth/session layer, or the runtime config.
func TestScheduleCreateThenListRoundTrip(t *testing.T) {
	dir, _ := os.MkdirTemp("", "sched-int-")
	defer os.RemoveAll(dir)
	cfg := db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")}
	d, err := db.Init(cfg)
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	defer d.Close()

	reqID := seedRequirementForSchedule(t, d)

	schedSvc := service.NewScheduledTaskService(d)
	reqSvc := service.NewRequirementService(d)

	h := NewScheduleHandler(schedSvc, reqSvc)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/schedules", h.Create)
	mux.HandleFunc("GET /api/schedules", h.List)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 1) Create the row.
	createBody, _ := json.Marshal(map[string]any{
		"requirement_id": reqID,
		"task_type":      model.SchedTypeDesign,
		"run_at":         time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339),
		"model":          "",
		"read_knowledge": false,
	})
	createResp, err := http.Post(srv.URL+"/api/schedules", "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatalf("create POST: %v", err)
	}
	body, _ := io.ReadAll(createResp.Body)
	createResp.Body.Close()
	if createResp.StatusCode != 201 {
		t.Fatalf("Create: expected 201, got %d body=%s", createResp.StatusCode, string(body))
	}
	var createdEnv struct {
		Success bool                 `json:"success"`
		Data    *model.ScheduledTask `json:"data"`
	}
	if err := json.Unmarshal(body, &createdEnv); err != nil {
		t.Fatalf("decode create body: %v body=%s", err, string(body))
	}
	if createdEnv.Data == nil || createdEnv.Data.ID == "" {
		t.Fatalf("created id empty in body=%s", string(body))
	}
	createdID := createdEnv.Data.ID

	// 2) List — the row we just created MUST appear.
	listResp, err := http.Get(srv.URL + "/api/schedules")
	if err != nil {
		t.Fatalf("list GET: %v", err)
	}
	listBody, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if listResp.StatusCode != 200 {
		t.Fatalf("List: expected 200, got %d body=%s", listResp.StatusCode, string(listBody))
	}
	var listEnv struct {
		Success bool                  `json:"success"`
		Data    []model.ScheduledTask `json:"data"`
	}
	if err := json.Unmarshal(listBody, &listEnv); err != nil {
		t.Fatalf("decode list body: %v", err)
	}
	found := false
	for _, r := range listEnv.Data {
		if r.ID == createdID {
			found = true
			if r.Status != model.SchedStatusPending {
				t.Errorf("List row status = %q, want %q", r.Status, model.SchedStatusPending)
			}
			break
		}
	}
	if !found {
		t.Fatalf("List did not contain the row just created (id=%s); saw %d other row(s): body=%s",
			createdID, len(listEnv.Data), string(listBody))
	}

	// 3) Re-create the same (req, task_type) — expect 409 ALREADY_SCHEDULED.
	reResp, err := http.Post(srv.URL+"/api/schedules", "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatalf("recreate POST: %v", err)
	}
	reBody, _ := io.ReadAll(reResp.Body)
	reResp.Body.Close()
	if reResp.StatusCode != 409 {
		t.Fatalf("Re-create: expected 409, got %d body=%s", reResp.StatusCode, string(reBody))
	}
}

// seedRequirementForSchedule inserts the bare minimum rows the schedule
// Create handler expects (project + requirement FK chain). It bypasses
// the project / requirement services so the test doesn't drag in path
// validation, platform tokens, or git init logic that the services
// enforce for the real POST /api/projects and POST /api/requirements
// paths. The schedule flow only cares that the requirement exists and
// resolves through RequirementService.Get.
func seedRequirementForSchedule(t *testing.T, d *db.DB) string {
	t.Helper()
	projID := "proj_seed_sched"
	reqID := "req_seed_sched"
	if _, err := d.Exec(`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
		projID, "Seed", "/tmp/seed", "ready", "main"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO requirements (id, project_id, title) VALUES (?, ?, ?)`,
		reqID, projID, "Seed req"); err != nil {
		t.Fatalf("insert requirement: %v", err)
	}
	return reqID
}