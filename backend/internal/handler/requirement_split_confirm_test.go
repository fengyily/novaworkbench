package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// End-to-end regression test for the "点创建子需求时一直是『创建中…』" bug.
//
// The confirm request prepares the stack-bottom branch and PUSHES it to
// origin. When origin is unreachable, the old git helper never returned (the
// remote helper grandchild held the stdout pipe open past the context
// deadline), so the HTTP request never completed and the UI sat on
// "创建中…" forever.
//
// The test drives the real ConfirmSplit handler against a repo whose origin
// is a server that accepts the connection and never answers. The request
// MUST come back — with a clear error — instead of hanging.

func TestConfirmSplit_UnreachableRemoteDoesNotHang(t *testing.T) {
	requireGit(t)

	addr, stop := blackholeListener(t)
	defer stop()

	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	stackGit(t, repo, "init", "-b", "main")
	stackGit(t, repo, "config", "user.email", "t@example.com")
	stackGit(t, repo, "config", "user.name", "t")
	stackGit(t, repo, "commit", "--allow-empty", "-m", "init")
	// origin points at the silent server.
	stackGit(t, repo, "remote", "add", "origin", "http://"+addr+"/repo.git")

	h, _, _, _ := newStackTestHandler(t, repo, "req_x")

	body := `{"tasks":[{"title":"子需求 A","description":"d","scope":[],"acceptance":["a"],"boundary":"","order":1,"depends_on":[]}],"source_branch":"main"}`
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_x/split/confirm", strings.NewReader(body))
	req.SetPathValue("id", "req_x")
	rec := httptest.NewRecorder()

	// The handler must answer within a bounded time. The budget is generous
	// (the git steps have their own timeouts) — the point is that it
	// ANSWERS.
	const budget = 3 * time.Minute

	done := make(chan int, 1)
	go func() {
		h.ConfirmSplit(rec, req)
		done <- rec.Code
	}()

	select {
	case code := <-done:
		// A failure is the expected outcome (the remote is unreachable), but
		// it must be a REAL answer, not a silent success.
		if code == http.StatusOK {
			t.Fatalf("confirm = 200 — the unreachable remote should have failed the push")
		}
		if !strings.Contains(rec.Body.String(), "SPLIT_") {
			t.Errorf("body = %q, want a stable SPLIT_* error code", rec.Body.String())
		}
	case <-time.After(budget):
		t.Fatalf("ConfirmSplit did not return within %s — the request would hang the UI on 创建中…", budget)
	}
}

// The same guarantee for the whole stack-bottom preparation: an unreachable
// remote must produce an error, not a hang.
func TestPrepareSplitStackBase_UnreachableRemoteDoesNotHang(t *testing.T) {
	requireGit(t)

	addr, stop := blackholeListener(t)
	defer stop()

	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	stackGit(t, repo, "init", "-b", "main")
	stackGit(t, repo, "config", "user.email", "t@example.com")
	stackGit(t, repo, "config", "user.name", "t")
	stackGit(t, repo, "commit", "--allow-empty", "-m", "init")
	stackGit(t, repo, "remote", "add", "origin", "http://"+addr+"/repo.git")

	h, _, proj, row := newStackTestHandler(t, repo, "req_x")

	const budget = 3 * time.Minute
	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, _, _, err := h.prepareSplitStackBase(row, proj, "main")
		done <- result{err: err}
	}()

	select {
	case res := <-done:
		// Either outcome is acceptable — what matters is that it ANSWERS.
		// (A local-only repo may still succeed: the push is the part that
		// needs the network, and its failure is reported as an error.)
		if res.err != nil {
			var se *splitStackError
			if !errors.As(res.err, &se) {
				t.Errorf("error = %v, want a *splitStackError with a stable code", res.err)
			}
		}
	case <-time.After(budget):
		t.Fatalf("prepareSplitStackBase did not return within %s", budget)
	}
}

// A reachable origin that simply does NOT have the picked branch is a
// different problem from an unreachable origin — the user must be told
// "pick another branch", not "check your network".
//
// This distinction is easy to get wrong: both make the ls-remote produce no
// sha, but only one of them means "origin did not answer".
func TestPrepareSplitStackBase_ReachableOriginWithoutTheBranch(t *testing.T) {
	requireGit(t)

	// A local bare repo as origin — fully reachable, no network involved.
	origin := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	stackGit(t, origin, "init", "-b", "main")
	stackGit(t, origin, "config", "user.email", "t@example.com")
	stackGit(t, origin, "config", "user.name", "t")
	stackGit(t, origin, "commit", "--allow-empty", "-m", "init")

	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	stackGit(t, repo, "init", "-b", "main")
	stackGit(t, repo, "config", "user.email", "t@example.com")
	stackGit(t, repo, "config", "user.name", "t")
	stackGit(t, repo, "commit", "--allow-empty", "-m", "init")
	stackGit(t, repo, "remote", "add", "origin", origin)

	h, _, proj, row := newStackTestHandler(t, repo, "req_x")

	// "nosuch" exists neither locally nor on origin.
	_, _, _, err := h.prepareSplitStackBase(row, proj, "nosuch")
	if err == nil {
		t.Fatal("expected an error for a branch that exists nowhere")
	}
	var se *splitStackError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a *splitStackError", err)
	}
	if se.code != "SPLIT_SOURCE_BRANCH_INVALID" {
		t.Errorf("code = %q, want SPLIT_SOURCE_BRANCH_INVALID (the origin answered fine)", se.code)
	}
}
