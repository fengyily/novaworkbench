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

// Regression: the create-requirement form pre-creates a draft row on mount
// and submits via PUT /api/requirements/{draft_id}. When the draft is gone by
// submit time (deleted from another tab, or the page outlived a database
// switch/restart) the handler used to answer 500 INTERNAL "requirement not
// found" — which the UI renders as "服务器内部错误" and gives the user no way
// to recover. It must be a 404 NOT_FOUND so the client can fall back to
// creating a fresh row.
func TestRequirementUpdateMissingReturns404(t *testing.T) {
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "t.db")})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	defer d.Close()

	h := NewRequirementHandler(service.NewRequirementService(d, nil), nil, nil, nil, nil, nil)

	body := `{"title":"t","description":"d","priority":"medium","kind":"requirement"}`
	req := httptest.NewRequest(http.MethodPut, "/api/requirements/req_missing", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Update(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success bool `json:"success"`
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}
	if resp.Success || resp.Error.Code != "NOT_FOUND" {
		t.Fatalf("response = %+v, want success=false code=NOT_FOUND", resp)
	}
	if !strings.Contains(resp.Error.Message, "requirement not found") {
		t.Fatalf("message = %q, want it to mention \"requirement not found\"", resp.Error.Message)
	}
}
