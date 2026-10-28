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
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// newReqDirTestHandler wires a RequirementDirHandler with a real SQLite DB
// (migrations applied by db.Init) and one seeded requirement. The returned
// attachment service is the one the handler consults for the synthetic
// "auto" row — the tests that need it upload a file through it.
//
// The attachment root (a package-global in the service layer) is pointed at a
// temp dir so an upload in a test never writes into the real
// ~/.novaworkbench/attachments.
func newReqDirTestHandler(t *testing.T) (*RequirementDirHandler, *db.DB) {
	t.Helper()
	oldRoot := service.AttachmentRoot()
	service.SetAttachmentRoot(t.TempDir())
	t.Cleanup(func() { service.SetAttachmentRoot(oldRoot) })

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
	attachSvc := service.NewAttachmentService(d)
	return NewRequirementDirHandler(service.NewRequirementDirService(d), attachSvc), d
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

// --- synthetic "auto" row: the requirement's attachment directory ---

// listDirs pulls the rows out of a List response.
func listDirs(t *testing.T, h *RequirementDirHandler) []requirementDirView {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/requirements/req_1/dirs", nil)
	req.SetPathValue("id", "req_1")
	w := httptest.NewRecorder()
	h.List(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("List status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	data, code, msg := decodeDirEnvelope(t, w.Body.Bytes())
	if code != "" {
		t.Fatalf("List error = %s/%s", code, msg)
	}
	var views []requirementDirView
	if err := json.Unmarshal(data, &views); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return views
}

// TestRequirementDirListIncludesAutoAttachmentRow pins the contract that the
// panel shows the attachment directory: it is handed to claude as --add-dir on
// every run (handler/attachment_grant.go), so the user must be able to see it.
func TestRequirementDirListIncludesAutoAttachmentRow(t *testing.T) {
	h, d := newReqDirTestHandler(t)

	// No attachment yet → no auto row.
	for _, v := range listDirs(t, h) {
		if v.Auto {
			t.Fatalf("a requirement without attachments must not list an auto row, got %+v", v)
		}
	}

	attachSvc := service.NewAttachmentService(d)
	if _, err := attachSvc.Upload("req_1", model.AttachmentUpload{
		Filename:   "error.log",
		Reader:     strings.NewReader("boom"),
		UploadedBy: "u1",
	}); err != nil {
		t.Fatalf("upload attachment: %v", err)
	}

	var auto *requirementDirView
	for _, v := range listDirs(t, h) {
		if v.Auto {
			vv := v
			auto = &vv
		}
	}
	if auto == nil {
		t.Fatal("expected an auto row once the requirement has an attachment")
	}
	if auto.ID != "auto:attachment" {
		t.Fatalf("auto row id = %q, want %q", auto.ID, "auto:attachment")
	}
	if auto.Reason != "attachment" {
		t.Fatalf("auto row reason = %q, want %q", auto.Reason, "attachment")
	}
	if want := filepath.Join(service.AttachmentRoot(), "req_1"); auto.Path != want {
		t.Fatalf("auto row path = %q, want %q", auto.Path, want)
	}
}

// TestRequirementDirListHasNoAutoRowWithoutAttachments is the other half: the
// synthetic row must not appear for a requirement with no attachments, and it
// must not be mistaken for a user grant.
func TestRequirementDirListHasNoAutoRowWithoutAttachments(t *testing.T) {
	h, _ := newReqDirTestHandler(t)

	views := listDirs(t, h)
	if len(views) != 0 {
		t.Fatalf("expected no rows at all, got %+v", views)
	}
}

// TestRequirementDirDeleteAutoRowIsNotFound pins that the synthetic row cannot
// be revoked — it is not a grant, so DELETE answers 404 instead of silently
// pretending to revoke something.
func TestRequirementDirDeleteAutoRowIsNotFound(t *testing.T) {
	h, _ := newReqDirTestHandler(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/requirements/req_1/dirs/auto:attachment", nil)
	req.SetPathValue("id", "req_1")
	req.SetPathValue("dir_id", "auto:attachment")
	w := httptest.NewRecorder()
	h.Delete(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("Delete(auto row) status = %d, want 404, body=%s", w.Code, w.Body.String())
	}
}
