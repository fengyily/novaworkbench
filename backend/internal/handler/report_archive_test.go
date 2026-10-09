package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/service"
)

// newReportArchiveTestHandler wires a ReportArchiveHandler with a real
// SQLite DB and a Gateway constructed with llmCfg=nil. The nil LLM channel
// forces ExtractReportKnowledge to return an error, which is exactly the
// "fallback to raw archive" path the production handler hits for users
// without an HTTP LLM configured — the most important regression target
// in this feature.
func newReportArchiveTestHandler(t *testing.T) (*ReportArchiveHandler, *db.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: path})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { d.Close(); os.Remove(path) })
	if _, err := d.Exec("INSERT INTO projects (id, name, local_path) VALUES ('proj_1', 'Test', '/tmp/proj1')"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := d.Exec("INSERT INTO requirements (id, project_id, title, status, kind) VALUES ('req_1', 'proj_1', 'test req', 'done', 'requirement')"); err != nil {
		t.Fatalf("seed requirement: %v", err)
	}
	// coding_plan is a TEXT NOT NULL DEFAULT '' column added via ALTER; an
	// explicit UPDATE keeps the seeded value visible in the same test.
	if _, err := d.Exec("UPDATE requirements SET coding_plan='fake plan content' WHERE id='req_1'"); err != nil {
		t.Fatalf("seed coding_plan: %v", err)
	}
	if _, err := d.Exec("INSERT INTO sub_tasks (id, requirement_id, title, prompt, status, artifact) VALUES ('st_1', 'req_1', 'sub', 'do', 'done', 'fake artifact content')"); err != nil {
		t.Fatalf("seed sub_task: %v", err)
	}

	reqSvc := service.NewRequirementService(d, nil)
	subSvc := service.NewSubTaskService(d, nil)
	archSvc := service.NewReportArchiveService(d)
	// llm.New(nil, nil) → llmCfg is nil → ExtractReportKnowledge returns
	// an error → handler falls back to raw archive. Also avoids a real
	// network call to an LLM endpoint.
	gw := llm.New(nil, nil)
	h := NewReportArchiveHandler(reqSvc, subSvc, archSvc, gw, nil)
	return h, d
}

func TestArchiveDevReport_EmptyCodingPlan_Returns400(t *testing.T) {
	h, d := newReportArchiveTestHandler(t)
	if _, err := d.Exec("UPDATE requirements SET coding_plan='' WHERE id='req_1'"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_1/dev-report/archive", nil)
	req.SetPathValue("id", "req_1")
	w := httptest.NewRecorder()
	h.ArchiveDevReport(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d, body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("NO_REPORT")) {
		t.Errorf("expected NO_REPORT code in body, got %s", w.Body.String())
	}
}

func TestArchiveDevReport_NoLLM_FallsBackToRaw_Returns200(t *testing.T) {
	h, d := newReportArchiveTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_1/dev-report/archive", nil)
	req.SetPathValue("id", "req_1")
	w := httptest.NewRecorder()
	h.ArchiveDevReport(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (raw-archive fallback must always succeed), got %d, body=%s", w.Code, w.Body.String())
	}
	var resp APIResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success {
		t.Errorf("expected success=true, got %+v", resp)
	}
	// The fallback marker must show up in the data payload.
	raw, _ := json.Marshal(resp.Data)
	if !bytes.Contains(raw, []byte("原文归档")) {
		t.Errorf("expected 原文归档 fallback marker in data, got %s", raw)
	}
	// And a knowledge row should have been persisted.
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM knowledge WHERE source_type='dev_report' AND source_ref='req_1'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 dev_report row, got %d", n)
	}
}

func TestArchiveSubTaskReport_WrongRequirement_Returns404(t *testing.T) {
	h, _ := newReportArchiveTestHandler(t)
	// st_1 belongs to req_1; ask for it under req_other → 404.
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_other/sub-tasks/st_1/archive", nil)
	req.SetPathValue("id", "req_other")
	req.SetPathValue("sid", "st_1")
	w := httptest.NewRecorder()
	h.ArchiveSubTaskReport(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for cross-requirement sub-task, got %d, body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("NOT_FOUND")) {
		t.Errorf("expected NOT_FOUND code, got %s", w.Body.String())
	}
}

func TestArchiveSubTaskReport_EmptyArtifact_Returns400(t *testing.T) {
	h, d := newReportArchiveTestHandler(t)
	if _, err := d.Exec("UPDATE sub_tasks SET artifact='' WHERE id='st_1'"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_1/sub-tasks/st_1/archive", nil)
	req.SetPathValue("id", "req_1")
	req.SetPathValue("sid", "st_1")
	w := httptest.NewRecorder()
	h.ArchiveSubTaskReport(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d, body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("NO_REPORT")) {
		t.Errorf("expected NO_REPORT code, got %s", w.Body.String())
	}
}

func TestArchiveSubTaskReport_NoLLM_FallsBackToRaw_Returns200(t *testing.T) {
	h, d := newReportArchiveTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_1/sub-tasks/st_1/archive", nil)
	req.SetPathValue("id", "req_1")
	req.SetPathValue("sid", "st_1")
	w := httptest.NewRecorder()
	h.ArchiveSubTaskReport(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM knowledge WHERE source_type='subtask_report' AND source_ref='st_1'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 subtask_report row, got %d", n)
	}
}

func TestListArchives_ReturnsBothTypes(t *testing.T) {
	h, _ := newReportArchiveTestHandler(t)
	// Archive both: the first call falls back to raw archive (no LLM) and
	// persists two knowledge rows; ListArchives should return both ids.
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_1/dev-report/archive", nil)
	req.SetPathValue("id", "req_1")
	w := httptest.NewRecorder()
	h.ArchiveDevReport(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("setup dev archive: %d, %s", w.Code, w.Body.String())
	}
	req2 := httptest.NewRequest(http.MethodPost, "/api/requirements/req_1/sub-tasks/st_1/archive", nil)
	req2.SetPathValue("id", "req_1")
	req2.SetPathValue("sid", "st_1")
	w = httptest.NewRecorder()
	h.ArchiveSubTaskReport(w, req2)
	if w.Code != http.StatusOK {
		t.Fatalf("setup subtask archive: %d, %s", w.Code, w.Body.String())
	}

	req3 := httptest.NewRequest(http.MethodGet, "/api/requirements/req_1/report-archives", nil)
	req3.SetPathValue("id", "req_1")
	w = httptest.NewRecorder()
	h.ListArchives(w, req3)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d, %s", w.Code, w.Body.String())
	}
	var resp APIResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, _ := json.Marshal(resp.Data)
	if !strings.Contains(string(raw), `"req_1"`) || !strings.Contains(string(raw), `"st_1"`) {
		t.Errorf("expected both req_1 and st_1 in items map, got %s", raw)
	}
}
