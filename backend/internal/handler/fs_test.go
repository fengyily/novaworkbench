package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newTestFsHandler builds an FsHandler with a stubbed LocalPathProvider that
// returns the given paths. No DB needed.
func newTestFsHandler(localPaths []string) *FsHandler {
	paths := localPaths
	return NewFsHandler(func() []string { return paths })
}

// TestResolveAllowedPath_UnderWorkspace: ~/workspace/** should pass when
// $HOME/workspace exists on the test host.
func TestResolveAllowedPath_UnderWorkspace(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir available")
	}
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	defer os.RemoveAll(workspace)

	h := newTestFsHandler(nil)
	got, errCode, errMsg := h.resolveAllowedPath("~/workspace")
	if errCode != "" {
		t.Fatalf("unexpected error: %s %s", errCode, errMsg)
	}
	if !strings.HasPrefix(got, workspace) {
		t.Errorf("got %q, want under %q", got, workspace)
	}
}

// TestResolveAllowedPath_EtcRejected: /etc must always be rejected.
func TestResolveAllowedPath_EtcRejected(t *testing.T) {
	h := newTestFsHandler(nil)
	_, errCode, errMsg := h.resolveAllowedPath("/etc")
	if errCode != "PATH_NOT_ALLOWED" {
		t.Errorf("got errCode=%q msg=%q, want PATH_NOT_ALLOWED", errCode, errMsg)
	}
}

// TestResolveAllowedPath_DotDotEscape: ~/workspace/../../etc must be rejected.
func TestResolveAllowedPath_DotDotEscape(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir available")
	}
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	defer os.RemoveAll(workspace)

	h := newTestFsHandler(nil)
	_, errCode, _ := h.resolveAllowedPath("~/workspace/../../etc")
	if errCode != "PATH_NOT_ALLOWED" {
		t.Errorf("got errCode=%q, want PATH_NOT_ALLOWED", errCode)
	}
}

// TestResolveAllowedPath_ProjectLocalPath: paths inside a registered project
// local_path should pass; sibling paths should fail.
func TestResolveAllowedPath_ProjectLocalPath(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projA")
	if err := os.MkdirAll(proj, 0755); err != nil {
		t.Fatalf("mkdir proj: %v", err)
	}
	h := newTestFsHandler([]string{proj})

	// Inside: pass
	if _, ec, em := h.resolveAllowedPath(proj); ec != "" {
		t.Errorf("expected %q to pass, got %s %s", proj, ec, em)
	}
	sub := filepath.Join(proj, "sub")
	if _, ec, em := h.resolveAllowedPath(sub); ec != "" {
		t.Errorf("expected %q to pass, got %s %s", sub, ec, em)
	}
	// Sibling: fail
	projB := filepath.Join(dir, "projB")
	if _, ec, _ := h.resolveAllowedPath(projB); ec != "PATH_NOT_ALLOWED" {
		t.Errorf("expected %q to be rejected as PATH_NOT_ALLOWED, got %s", projB, ec)
	}
}

// TestResolveAllowedPath_SymlinkEscape: a symlink inside ~/workspace pointing
// at /etc must be rejected after EvalSymlinks resolution.
func TestResolveAllowedPath_SymlinkEscape(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir available")
	}
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	defer os.RemoveAll(workspace)

	link := filepath.Join(workspace, "evil-link")
	if err := os.Symlink("/etc", link); err != nil {
		t.Skipf("cannot create symlink (likely Windows): %v", err)
	}

	h := newTestFsHandler(nil)
	_, errCode, _ := h.resolveAllowedPath(link)
	if errCode != "PATH_NOT_ALLOWED" {
		t.Errorf("expected symlink escape to be rejected, got errCode=%q", errCode)
	}
}

// TestResolveAllowedPath_NulByte: a NUL byte in path must be rejected.
func TestResolveAllowedPath_NulByte(t *testing.T) {
	h := newTestFsHandler(nil)
	_, errCode, _ := h.resolveAllowedPath("/etc\x00/passwd")
	if errCode != "INVALID_PATH" {
		t.Errorf("got errCode=%q, want INVALID_PATH", errCode)
	}
}

// runGitIn runs `git <args...>` in `dir` and fatals on error. Mirrors the
// helper in review_refs_test.go.
func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, string(out))
	}
}

// callGitBranches invokes FsHandler.GitBranches with the given query string
// (e.g. "?path=/foo&refresh=1") and returns the recorded response.
func callGitBranches(h *FsHandler, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/fs/git-branches"+query, nil)
	rec := httptest.NewRecorder()
	h.GitBranches(rec, req)
	return rec
}

// TestGitBranches_NoRefresh_BackwardCompat: without refresh=1 the response
// must contain only `branches`. `fetch_warning` must be absent so legacy
// callers (and existing JSON consumers) keep decoding the same shape.
func TestGitBranches_NoRefresh_BackwardCompat(t *testing.T) {
	dir := t.TempDir()
	runGitIn(t, dir, "init", "--initial-branch=main")
	runGitIn(t, dir, "config", "user.email", "t@e")
	runGitIn(t, dir, "config", "user.name", "t")
	runGitIn(t, dir, "commit", "--allow-empty", "-m", "init")
	runGitIn(t, dir, "checkout", "-b", "feat/x")

	h := newTestFsHandler([]string{dir})
	rec := callGitBranches(h, "?path="+dir)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// writeJSON wraps the body in {success, data, error}; decode the data block.
	var envelope struct {
		Success bool                     `json:"success"`
		Data    map[string]interface{}   `json:"data"`
		Error   map[string]interface{}   `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !envelope.Success {
		t.Fatalf("expected success=true, got %+v", envelope)
	}
	if _, ok := envelope.Data["fetch_warning"]; ok {
		t.Errorf("expected no fetch_warning without refresh, got %v", envelope.Data)
	}
	branches, ok := envelope.Data["branches"].([]interface{})
	if !ok || len(branches) == 0 {
		t.Errorf("expected non-empty branches, got %v", envelope.Data["branches"])
	}
}

// TestGitBranches_Refresh_FetchFails: with refresh=1 on a repo that has no
// `origin` remote, `git fetch origin --prune` fails (exit=128). The handler
// must still return 200 with `branches` and a non-empty `fetch_warning` —
// fetch failure is intentionally non-fatal.
func TestGitBranches_Refresh_FetchFails(t *testing.T) {
	dir := t.TempDir()
	runGitIn(t, dir, "init", "--initial-branch=main")
	runGitIn(t, dir, "config", "user.email", "t@e")
	runGitIn(t, dir, "config", "user.name", "t")
	runGitIn(t, dir, "commit", "--allow-empty", "-m", "init")
	// Intentionally NOT adding an origin so fetch must fail.

	h := newTestFsHandler([]string{dir})
	rec := callGitBranches(h, "?path="+dir+"&refresh=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (fetch failure is non-fatal), got %d: %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Success bool                   `json:"success"`
		Data    map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	warning, ok := envelope.Data["fetch_warning"].(string)
	if !ok || warning == "" {
		t.Errorf("expected non-empty fetch_warning, got %v (ok=%v)", envelope.Data["fetch_warning"], ok)
	}
	branches, ok := envelope.Data["branches"].([]interface{})
	if !ok || len(branches) == 0 {
		t.Errorf("expected non-empty branches even when fetch fails, got %v", envelope.Data["branches"])
	}
}

// TestGitBranches_Refresh_OK: with refresh=1 on a repo with a working bare
// origin, fetch succeeds and the response contains `branches` and an empty
// `fetch_warning` is not set (since `payload["fetch_warning"]` is omitted when
// the fetch has no error).
func TestGitBranches_Refresh_OK(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	work := filepath.Join(root, "work")
	runGitIn(t, root, "init", "--bare", "--initial-branch=main", origin)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, work, "clone", origin, work)
	runGitIn(t, work, "config", "user.email", "t@e")
	runGitIn(t, work, "config", "user.name", "t")
	runGitIn(t, work, "commit", "--allow-empty", "-m", "init")
	runGitIn(t, work, "push", "origin", "main")
	// Add a second branch and push, so remote-tracking ref exists.
	runGitIn(t, work, "checkout", "-b", "feat/pushed")
	runGitIn(t, work, "push", "origin", "feat/pushed")

	h := newTestFsHandler([]string{work})
	rec := callGitBranches(h, "?path="+work+"&refresh=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Success bool                   `json:"success"`
		Data    map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := envelope.Data["fetch_warning"]; ok {
		t.Errorf("expected no fetch_warning on success, got %v", envelope.Data)
	}
	branches, ok := envelope.Data["branches"].([]interface{})
	if !ok || len(branches) == 0 {
		t.Fatalf("expected non-empty branches, got %v", envelope.Data["branches"])
	}
	seen := map[string]bool{}
	for _, b := range branches {
		if s, ok := b.(string); ok {
			seen[s] = true
		}
	}
	if !seen["main"] || !seen["feat/pushed"] {
		t.Errorf("expected main and feat/pushed in branches, got %v", seen)
	}
}