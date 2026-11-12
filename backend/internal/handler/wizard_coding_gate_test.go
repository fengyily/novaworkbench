package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/service"
)

// newGateTestHandler wires a WizardHandler against a throwaway SQLite DB with
// one project and the given requirements already seeded.
//
// The rows are inserted with raw SQL so the test owns the exact column
// values (dev_delegated, status, parent_id) that the gate reads — Create()
// would normalize several of them away.
func newGateTestHandler(t *testing.T, reqs [][3]string) (*WizardHandler, *service.RequirementService, *db.DB) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	const projID = "proj_gate"
	if _, err := d.Exec(`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
		projID, "P", "/tmp/p", "ready", "main"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	// reqs entries are {id, status, devDeleged} — "0"/"1" for the flag.
	for _, r := range reqs {
		if _, err := d.Exec(
			`INSERT INTO requirements (id, project_id, title, kind, status, dev_delegated) VALUES (?, ?, ?, 'requirement', ?, ?)`,
			r[0], projID, r[0], r[1], r[2]); err != nil {
			t.Fatalf("insert requirement %s: %v", r[0], err)
		}
	}
	reqSvc := service.NewRequirementService(d, nil)
	return &WizardHandler{reqSvc: reqSvc}, reqSvc, d
}

// A master whose development was handed down to its children must NOT be
// promoted into "developing" — and it must not be left in a dirty
// "developing" state either.
func TestGateCodingEntry_DevDelegatedMasterRejected(t *testing.T) {
	h, reqSvc, _ := newGateTestHandler(t, [][3]string{
		{"req_master", "designed", "1"},
	})
	req, err := reqSvc.Get("req_master")
	if err != nil {
		t.Fatalf("get master: %v", err)
	}

	_, af := h.gateCodingEntry(req)
	if af == nil {
		t.Fatal("gateCodingEntry allowed a dev_delegated requirement into development")
	}
	if af.Code != "GATE_DEV_DELEGATED" {
		t.Errorf("code = %q, want GATE_DEV_DELEGATED", af.Code)
	}
	if af.Status != 409 {
		t.Errorf("status = %d, want 409", af.Status)
	}

	after, err := reqSvc.Get("req_master")
	if err != nil {
		t.Fatalf("get master after gate: %v", err)
	}
	if after.Status != "designed" {
		t.Errorf("status = %q after a rejected gate, want it unchanged at 'designed'", after.Status)
	}
}

// A child whose prerequisite is not done may not start developing.
func TestGateCodingEntry_UnmetDependencyRejected(t *testing.T) {
	h, reqSvc, d := newGateTestHandler(t, [][3]string{
		{"req_prereq", "draft", "0"},
		{"req_child", "designed", "0"},
	})
	if _, err := d.Exec(
		`INSERT INTO requirement_dependencies (id, requirement_id, depends_on_id, batch_id, created_at) VALUES ('dep_1', ?, ?, 'spb_x', CURRENT_TIMESTAMP)`,
		"req_child", "req_prereq"); err != nil {
		t.Fatalf("insert edge: %v", err)
	}

	req, err := reqSvc.Get("req_child")
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	_, af := h.gateCodingEntry(req)
	if af == nil {
		t.Fatal("gateCodingEntry allowed a child with an unmet prerequisite into development")
	}
	if af.Code != "GATE_DEPENDENCY_NOT_MET" {
		t.Errorf("code = %q, want GATE_DEPENDENCY_NOT_MET", af.Code)
	}
	if af.Status != 409 {
		t.Errorf("status = %d, want 409", af.Status)
	}

	after, err := reqSvc.Get("req_child")
	if err != nil {
		t.Fatalf("get child after gate: %v", err)
	}
	if after.Status != "designed" {
		t.Errorf("status = %q after a rejected gate, want it unchanged at 'designed'", after.Status)
	}
}

// Once every prerequisite is done, the gate lets the child through and
// promotes it to "developing".
func TestGateCodingEntry_DependenciesSatisfiedPasses(t *testing.T) {
	h, reqSvc, d := newGateTestHandler(t, [][3]string{
		{"req_prereq", "done", "0"},
		{"req_child", "designed", "0"},
	})
	if _, err := d.Exec(
		`INSERT INTO requirement_dependencies (id, requirement_id, depends_on_id, batch_id, created_at) VALUES ('dep_1', ?, ?, 'spb_x', CURRENT_TIMESTAMP)`,
		"req_child", "req_prereq"); err != nil {
		t.Fatalf("insert edge: %v", err)
	}

	req, err := reqSvc.Get("req_child")
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	updated, af := h.gateCodingEntry(req)
	if af != nil {
		t.Fatalf("gateCodingEntry rejected a child whose prerequisites are done: %v", af)
	}
	if updated == nil || updated.Status != "developing" {
		t.Fatalf("status = %v, want 'developing'", updated)
	}
}

// A requirement with no dependencies and no delegation passes the gate
// untouched — the new gates must not change the pre-existing behaviour.
func TestGateCodingEntry_NoDependenciesUnaffected(t *testing.T) {
	h, reqSvc, _ := newGateTestHandler(t, [][3]string{
		{"req_plain", "designed", "0"},
	})
	req, err := reqSvc.Get("req_plain")
	if err != nil {
		t.Fatalf("get req: %v", err)
	}
	updated, af := h.gateCodingEntry(req)
	if af != nil {
		t.Fatalf("gateCodingEntry rejected a plain requirement: %v", af)
	}
	if updated == nil || updated.Status != "developing" {
		t.Fatalf("status = %v, want 'developing'", updated)
	}
}

// The gate must not fire on a requirement whose dev_delegated column was
// never set (the default for every pre-split row).
func TestGateCodingEntry_LegacyRowNotDelegated(t *testing.T) {
	h, reqSvc, _ := newGateTestHandler(t, [][3]string{
		{"req_legacy", "designed", "0"},
	})
	req, err := reqSvc.Get("req_legacy")
	if err != nil {
		t.Fatalf("get req: %v", err)
	}
	if req.DevDelegated {
		t.Fatal("dev_delegated = true on a freshly inserted row")
	}
	if _, af := h.gateCodingEntry(req); af != nil {
		t.Fatalf("gateCodingEntry rejected a legacy row: %v", af)
	}
}

// The HTTP start-coding path is the PRIMARY way a user starts development,
// and it does NOT go through gateCodingEntry (that one only covers the
// scheduled and LaunchSpec paths). The split gates must apply here too —
// otherwise a delegated master would still develop.
func TestStartCoding_RefusesDelegatedMaster(t *testing.T) {
	h, _, _ := newGateTestHandler(t, [][3]string{
		{"req_master", "designed", "1"},
	})

	body := strings.NewReader(`{"requirement_id":"req_master"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/wizard/start-coding", body)
	rec := httptest.NewRecorder()
	h.StartCoding(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != "GATE_DEV_DELEGATED" {
		t.Errorf("code = %q, want GATE_DEV_DELEGATED", env.Error.Code)
	}
}

// Same for the dependency gate: a dependent child may not start coding
// before its prerequisite is done.
func TestStartCoding_RefusesUnmetDependency(t *testing.T) {
	h, _, d := newGateTestHandler(t, [][3]string{
		{"req_prereq", "draft", "0"},
		{"req_child", "designed", "0"},
	})
	if _, err := d.Exec(
		`INSERT INTO requirement_dependencies (id, requirement_id, depends_on_id, batch_id, created_at) VALUES ('dep_1', ?, ?, 'spb_x', CURRENT_TIMESTAMP)`,
		"req_child", "req_prereq"); err != nil {
		t.Fatalf("insert edge: %v", err)
	}

	body := strings.NewReader(`{"requirement_id":"req_child"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/wizard/start-coding", body)
	rec := httptest.NewRecorder()
	h.StartCoding(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != "GATE_DEPENDENCY_NOT_MET" {
		t.Errorf("code = %q, want GATE_DEPENDENCY_NOT_MET", env.Error.Code)
	}
}
