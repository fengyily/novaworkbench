package handler

import (
	"bytes"
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

// newAcceptanceHandlerTest wires the acceptance handler over a throwaway
// SQLite database.
//
// The AI run itself is NOT exercised here (it needs the claude CLI) — this
// test covers the preflight gate, which is where a wrong answer would cost
// the user a wasted run.
func newAcceptanceHandlerTest(t *testing.T) (*AcceptanceHandler, *db.DB, string) {
	t.Helper()
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	projectID := "proj_acc_test"
	now := time.Now()
	if _, err := d.Exec(
		`INSERT INTO projects (id,name,local_path,status,added_at,updated_at) VALUES (?,?,?, 'active', ?, ?)`,
		projectID, "acceptance test", filepath.Join(t.TempDir(), "repo"), now, now); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	projSvc := service.NewProjectService(d, nil, nil)
	reqSvc := service.NewRequirementService(d, nil)
	reviewSvc := service.NewCodeReviewService(d, service.NewCodeReviewAttachmentService(d))
	h := NewAcceptanceHandler(reqSvc, projSvc, reviewSvc, nil, nil,
		store.NewJobStore(16), nil, nil)
	return h, d, projectID
}

// seedAcceptanceRequirement inserts a requirement row directly.
func seedAcceptanceRequirement(t *testing.T, d *db.DB, projectID, id, title string, delegated bool) {
	t.Helper()
	flag := 0
	if delegated {
		flag = 1
	}
	if _, err := d.Exec(
		`INSERT INTO requirements (id, project_id, title, dev_delegated, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, projectID, title, flag, time.Now(), time.Now()); err != nil {
		t.Fatalf("seed requirement %s: %v", id, err)
	}
}

// seedAcceptanceChild inserts a child requirement row.
func seedAcceptanceChild(t *testing.T, d *db.DB, projectID, id, parentID, title, status, worktree string) {
	t.Helper()
	if _, err := d.Exec(
		`INSERT INTO requirements (id, project_id, title, status, parent_id, split_order, worktree_path, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?)`,
		id, projectID, title, status, parentID, worktree, time.Now(), time.Now()); err != nil {
		t.Fatalf("seed child %s: %v", id, err)
	}
}

// callAcceptance runs one request through the handler and decodes the envelope.
func callAcceptance(t *testing.T, fn func(w http.ResponseWriter, r *http.Request), method, target, reqID string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(nil))
	req.SetPathValue("id", reqID)
	rec := httptest.NewRecorder()
	fn(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// errorCode digs the error code out of the JSON envelope.
func errorCode(t *testing.T, envelope map[string]any) string {
	t.Helper()
	errObj, ok := envelope["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := errObj["code"].(string)
	return code
}

// TestAcceptanceStart_Preflight covers every gate of the start endpoint.
//
// Each gate must answer with ITS OWN error code: a user who starts a核对 on
// a requirement that was never split has to be told that, not "服务不可用".
func TestAcceptanceStart_Preflight(t *testing.T) {
	h, d, projectID := newAcceptanceHandlerTest(t)

	// 1. The requirement does not exist.
	code, env := callAcceptance(t, h.StartCheck, http.MethodPost,
		"/api/requirements/req_missing/acceptance-check", "req_missing")
	if code != http.StatusNotFound || errorCode(t, env) != "REQUIREMENT_NOT_FOUND" {
		t.Fatalf("missing requirement → %d %v", code, env)
	}

	// 2. The requirement was never split (dev_delegated = 0).
	seedAcceptanceRequirement(t, d, projectID, "req_plain", "普通需求", false)
	code, env = callAcceptance(t, h.StartCheck, http.MethodPost,
		"/api/requirements/req_plain/acceptance-check", "req_plain")
	if code != http.StatusConflict || errorCode(t, env) != "ACCEPTANCE_NOT_DELEGATED" {
		t.Fatalf("not delegated → %d %v", code, env)
	}

	// 3. Delegated, but no children.
	seedAcceptanceRequirement(t, d, projectID, "req_nochild", "拆了但没子需求", true)
	code, env = callAcceptance(t, h.StartCheck, http.MethodPost,
		"/api/requirements/req_nochild/acceptance-check", "req_nochild")
	if code != http.StatusConflict || errorCode(t, env) != "ACCEPTANCE_NO_CHILDREN" {
		t.Fatalf("no children → %d %v", code, env)
	}

	// 4. Children exist, but none of them entered development.
	seedAcceptanceRequirement(t, d, projectID, "req_noimpl", "子需求都没开工", true)
	seedAcceptanceChild(t, d, projectID, "req_noimpl_c1", "req_noimpl", "子需求一", "draft", "")
	code, env = callAcceptance(t, h.StartCheck, http.MethodPost,
		"/api/requirements/req_noimpl/acceptance-check", "req_noimpl")
	if code != http.StatusConflict || errorCode(t, env) != "ACCEPTANCE_NO_IMPLEMENTATION" {
		t.Fatalf("no implementation → %d %v", code, env)
	}

	// 5. A child that is done counts as implemented even without a worktree
	//    (its worktree may have been cleaned up) — the gate must let it pass.
	seedAcceptanceRequirement(t, d, projectID, "req_donechild", "子需求已完成", true)
	seedAcceptanceChild(t, d, projectID, "req_donechild_c1", "req_donechild", "子需求一", "done", "")
	if !anyChildImplemented([]*model.Requirement{{Status: "done"}}) {
		t.Fatalf("a done child must count as implemented")
	}
}

// TestAcceptanceStart_InProgress covers the concurrency gate: one
// acceptance run per master requirement at a time.
func TestAcceptanceStart_InProgress(t *testing.T) {
	h, d, projectID := newAcceptanceHandlerTest(t)

	seedAcceptanceRequirement(t, d, projectID, "req_busy", "正在核对", true)
	seedAcceptanceChild(t, d, projectID, "req_busy_c1", "req_busy", "子需求一", "developing",
		filepath.Join(t.TempDir(), "child1"))

	// Simulate a run that is already in flight.
	if _, ok := h.locks.acquire("req_busy", "job_running"); !ok {
		t.Fatalf("the lock must be acquirable on a fresh handler")
	}

	code, env := callAcceptance(t, h.StartCheck, http.MethodPost,
		"/api/requirements/req_busy/acceptance-check", "req_busy")
	if code != http.StatusConflict || errorCode(t, env) != "ACCEPTANCE_IN_PROGRESS" {
		t.Fatalf("in progress → %d %v", code, env)
	}
}

// TestAcceptanceLocks covers the gate's own contract.
func TestAcceptanceLocks(t *testing.T) {
	l := newAcceptanceLocks()
	if _, running := l.peek("req_a"); running {
		t.Fatalf("a fresh gate must not report a running job")
	}
	if existing, ok := l.acquire("req_a", "job_1"); !ok || existing != "" {
		t.Fatalf("first acquire → %q / %v", existing, ok)
	}
	if existing, ok := l.acquire("req_a", "job_2"); ok || existing != "job_1" {
		t.Fatalf("second acquire → %q / %v", existing, ok)
	}
	if id, running := l.peek("req_a"); !running || id != "job_1" {
		t.Fatalf("peek → %q / %v", id, running)
	}
	l.release("req_a")
	if _, running := l.peek("req_a"); running {
		t.Fatalf("release must clear the gate")
	}
	// A different requirement is unaffected by the release.
	if _, ok := l.acquire("req_b", "job_3"); !ok {
		t.Fatalf("a different requirement must be able to acquire the gate")
	}
}

// TestAcceptanceListAndGet covers the read endpoints.
func TestAcceptanceListAndGet(t *testing.T) {
	h, d, projectID := newAcceptanceHandlerTest(t)
	reviewSvc := service.NewCodeReviewService(d, service.NewCodeReviewAttachmentService(d))

	seedAcceptanceRequirement(t, d, projectID, "req_read", "可读的需求", true)
	rv, err := reviewSvc.Create(&model.CodeReview{
		ProjectID:     projectID,
		ReviewType:    model.ReviewTypeAcceptance,
		RequirementID: "req_read",
	})
	if err != nil {
		t.Fatalf("create acceptance row: %v", err)
	}
	t.Cleanup(func() { _ = reviewSvc.Delete(rv.ID) })

	code, env := callAcceptance(t, h.ListChecks, http.MethodGet,
		"/api/requirements/req_read/acceptance-check", "req_read")
	if code != http.StatusOK {
		t.Fatalf("list → %d %v", code, env)
	}
	rows, _ := env["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("list returned %d rows, want 1", len(rows))
	}

	// The single-row endpoint resolves it.
	req := httptest.NewRequest(http.MethodGet, "/api/requirements/req_read/acceptance-check/"+rv.ID, nil)
	req.SetPathValue("id", "req_read")
	req.SetPathValue("cid", rv.ID)
	rec := httptest.NewRecorder()
	h.GetCheck(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get → %d %s", rec.Code, rec.Body.String())
	}

	// A row belonging to ANOTHER requirement must not be readable here.
	other := httptest.NewRequest(http.MethodGet, "/api/requirements/req_other/acceptance-check/"+rv.ID, nil)
	other.SetPathValue("id", "req_other")
	other.SetPathValue("cid", rv.ID)
	rec = httptest.NewRecorder()
	h.GetCheck(rec, other)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-requirement get → %d %s", rec.Code, rec.Body.String())
	}
}

// TestDecodeStringArray covers the JSON-array column decoding.
func TestDecodeStringArray(t *testing.T) {
	if got := decodeStringArray(""); got != nil {
		t.Fatalf("empty → %v", got)
	}
	if got := decodeStringArray("[]"); got != nil {
		t.Fatalf("[] → %v", got)
	}
	got := decodeStringArray(`["a", " b ", "", "c"]`)
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("array → %v", got)
	}
	// A malformed column degrades to one plain value instead of an error.
	got = decodeStringArray("not json")
	if len(got) != 1 || got[0] != "not json" {
		t.Fatalf("malformed → %v", got)
	}
}
