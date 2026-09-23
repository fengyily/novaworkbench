package service

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func insertClaudeConfig(t *testing.T, d interface {
	Exec(string, ...any) (sql.Result, error)
}, name, baseURL, token, model string) string {
	t.Helper()
	id := fmt.Sprintf("claude_test_%d", time.Now().UnixNano())
	_, err := d.Exec(
		`INSERT INTO claude_configs (id, name, base_url, auth_token, models, default_model, is_active, created_at, updated_at)
		 VALUES (?, ?, ?, ?, '[]', ?, 0, ?, ?)`,
		id, name, baseURL, token, model, time.Now(), time.Now(),
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return id
}

// L1
func TestClaudeConfigService_TestConnection_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("expected Bearer Authorization, got %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"data":[{"id":"claude-sonnet-4-5"}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	db := newTestDB(t)
	id := insertClaudeConfig(t, db, "t", srv.URL, "tok", "claude-sonnet-4-5")
	svc := NewClaudeConfigService(db)
	model, err := svc.TestConnection(context.Background(), id)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "claude-sonnet-4-5" {
		t.Errorf("expected claude-sonnet-4-5, got %q", model)
	}
}

// L2
func TestClaudeConfigService_TestConnection_401(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	db := newTestDB(t)
	id := insertClaudeConfig(t, db, "t", srv.URL, "bad-token", "")
	svc := NewClaudeConfigService(db)
	_, err := svc.TestConnection(context.Background(), id)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.HasPrefix(err.Error(), "LLM_TOKEN_INVALID:") {
		t.Errorf("missing LLM_TOKEN_INVALID prefix: %q", err.Error())
	}
}

// L3
func TestClaudeConfigService_TestConnection_ConnectError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(mux)
	url := srv.URL
	srv.Close() // close before call → dial error

	db := newTestDB(t)
	id := insertClaudeConfig(t, db, "t", url, "tok", "")
	svc := NewClaudeConfigService(db)
	_, err := svc.TestConnection(context.Background(), id)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.HasPrefix(err.Error(), "LLM_CONNECT_FAILED:") {
		t.Errorf("missing LLM_CONNECT_FAILED prefix: %q", err.Error())
	}
}

// L4
func TestClaudeConfigService_TestConnection_NotFound(t *testing.T) {
	svc := NewClaudeConfigService(newTestDB(t))
	_, err := svc.TestConnection(context.Background(), "claude_does_not_exist")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.HasPrefix(err.Error(), "LLM_CONFIG_NOT_FOUND:") {
		t.Errorf("missing LLM_CONFIG_NOT_FOUND prefix: %q", err.Error())
	}
}