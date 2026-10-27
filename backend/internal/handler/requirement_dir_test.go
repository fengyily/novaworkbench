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
	"github.com/novaworkbench/backend/internal/service"
)

// newReqDirTestHandler wires a RequirementDirHandler with a real SQLite DB
// (migrations applied by db.Init) and one seeded requirement.
func newReqDirTestHandler(t *testing.T) (*RequirementDirHandler, *db.DB) {
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
	if _, err := d.Exec("INSERT INTO requirements (id, project_id, title) VALUES ('req_1', 'proj_1', '目录授权')"); err != nil {
		t.Fatalf("seed requirement: %v", err)
	}
	return NewRequirementDirHandler(service.NewRequirementDirService(d)), d
}

// decodeDirEnvelope pulls the {success, data, error} envelope out of a
// response body.
func decodeDirEnvelope(t *testing.T, body []byte) (data json.RawMessage, errCode string, errMsg string) {
	t.Helper()
	var env struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope %s: %v", body, err)
	}
	return env.Data, env.Error.Code, env.Error.Message
}

func TestRequirementDirAddListDelete(t *testing.T) {
	h, _ := newReqDirTestHandler(t)
	dir := t.TempDir()

	// --- add ---
	body, _ := json.Marshal(map[string]string{"path": dir, "label": "参考日志"})
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_1/dirs", bytes.NewReader(body))
	req.SetPathValue("id", "req_1")
	w := httptest.NewRecorder()
	h.Add(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("Add status = %d, want 201, body=%s", w.Code, w.Body.String())
	}
	data, code, msg := decodeDirEnvelope(t, w.Body.Bytes())
	if code != "" {
		t.Fatalf("Add error = %s/%s", code, msg)
	}
	var added struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(data, &added); err != nil {
		t.Fatalf("decode added: %v", err)
	}
	if added.Path != dir {
		t.Fatalf("Add path = %q, want %q", added.Path, dir)
	}
	if added.ID == "" {
		t.Fatal("Add returned an empty id")
	}

	// --- list ---
	req = httptest.NewRequest(http.MethodGet, "/api/requirements/req_1/dirs", nil)
	req.SetPathValue("id", "req_1")
	w = httptest.NewRecorder()
	h.List(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("List status = %d, want 200", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(dir)) {
		t.Fatalf("List body = %s, want it to mention %q", w.Body.String(), dir)
	}
	// The list projection carries an `exists_locally` flag so the UI can
	// flag a grant whose directory is missing from THIS host. It says
	// nothing about the agent host, which is where the grant may resolve.
	if !bytes.Contains(w.Body.Bytes(), []byte(`"exists_locally":true`)) {
		t.Fatalf("List body = %s, want an exists_locally flag", w.Body.String())
	}

	// --- delete ---
	req = httptest.NewRequest(http.MethodDelete, "/api/requirements/req_1/dirs/"+added.ID, nil)
	req.SetPathValue("id", "req_1")
	req.SetPathValue("dir_id", added.ID)
	w = httptest.NewRecorder()
	h.Delete(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("Delete status = %d, want 204, body=%s", w.Code, w.Body.String())
	}

	// --- delete again → 404 ---
	req = httptest.NewRequest(http.MethodDelete, "/api/requirements/req_1/dirs/"+added.ID, nil)
	req.SetPathValue("id", "req_1")
	req.SetPathValue("dir_id", added.ID)
	w = httptest.NewRecorder()
	h.Delete(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("Delete(again) status = %d, want 404", w.Code)
	}
}

// TestRequirementDirAddAcceptsUnresolvablePaths pins the add-time contract
// at the HTTP boundary: a path this host cannot resolve is still granted,
// because the claude session that consumes the grant may run on an Agent
// server where the path resolves. Only the filesystem root is rejected.
func TestRequirementDirAddAcceptsUnresolvablePaths(t *testing.T) {
	h, _ := newReqDirTestHandler(t)

	file := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	accepted := []struct {
		name string
		path string
	}{
		{name: "missing directory", path: filepath.Join(t.TempDir(), "nope")},
		{name: "file not directory", path: file},
		{name: "agent-server-only path", path: "/srv/agent/workspace/specs"},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"path": tc.path})
			req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_1/dirs", bytes.NewReader(body))
			req.SetPathValue("id", "req_1")
			w := httptest.NewRecorder()
			h.Add(w, req)
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201, body=%s", w.Code, w.Body.String())
			}
		})
	}

	// The filesystem root is still rejected — that rule is about the shape
	// of the path, not about whether it resolves on this host.
	body, _ := json.Marshal(map[string]string{"path": "/"})
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_1/dirs", bytes.NewReader(body))
	req.SetPathValue("id", "req_1")
	w := httptest.NewRecorder()
	h.Add(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
	_, code, _ := decodeDirEnvelope(t, w.Body.Bytes())
	if code != "ROOT_PATH_REJECTED" {
		t.Fatalf("error code = %q, want ROOT_PATH_REJECTED", code)
	}
}

func TestRequirementDirAddEmptyPath(t *testing.T) {
	h, _ := newReqDirTestHandler(t)
	body, _ := json.Marshal(map[string]string{"path": "   "})
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_1/dirs", bytes.NewReader(body))
	req.SetPathValue("id", "req_1")
	w := httptest.NewRecorder()
	h.Add(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
}

func TestRequirementDirListEmptyIsArray(t *testing.T) {
	h, _ := newReqDirTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/requirements/req_1/dirs", nil)
	req.SetPathValue("id", "req_1")
	w := httptest.NewRecorder()
	h.List(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	data, _, _ := decodeDirEnvelope(t, w.Body.Bytes())
	if !strings.HasPrefix(strings.TrimSpace(string(data)), "[") {
		t.Fatalf("data = %s, want a JSON array (never null)", data)
	}
}

// TestDirsManifestFor pins the prompt block: the model has to be told which
// directories were authorized, or it has no reason to look there.
func TestDirsManifestFor(t *testing.T) {
	h, _ := newReqDirTestHandler(t)
	dir := t.TempDir()

	body, _ := json.Marshal(map[string]string{"path": dir, "label": "参考日志"})
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_1/dirs", bytes.NewReader(body))
	req.SetPathValue("id", "req_1")
	w := httptest.NewRecorder()
	h.Add(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("Add status = %d, want 201", w.Code)
	}

	block := dirsManifestFor(h.svc, "req_1")
	if block == "" {
		t.Fatal("dirsManifestFor returned an empty block for a granted directory")
	}
	if !strings.Contains(block, dir) {
		t.Fatalf("block = %q, want it to name the granted path", block)
	}
	if !strings.Contains(block, "参考日志") {
		t.Fatalf("block = %q, want it to carry the user's note", block)
	}

	// A requirement with no grants must produce no block, so callers can
	// append it unconditionally without changing the prompt.
	empty := dirsManifestFor(h.svc, "req_missing")
	if empty != "" {
		t.Fatalf("dirsManifestFor(no grants) = %q, want empty", empty)
	}
	// A nil service must not panic.
	if got := dirsManifestFor(nil, "req_1"); got != "" {
		t.Fatalf("dirsManifestFor(nil svc) = %q, want empty", got)
	}
	if got := resolveAddDirs(nil, "req_1"); got != nil {
		t.Fatalf("resolveAddDirs(nil svc) = %v, want nil", got)
	}
}
