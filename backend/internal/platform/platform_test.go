package platform

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests are the first to exercise the platform package. Every impl
// is pointed at an httptest server by constructing the unexported struct
// directly (New() would reject the fake host or hardcode api.github.com).
// That is why githubClient grew a baseURL field — see github.go.

// capturedReq records what the fake server saw, so assertions read off the
// actual wire format rather than off the client's intent.
//
// path is the RAW (still-escaped) path. r.URL.Path would decode %2F back to
// "/" and hide whether the project path was actually escaped — which is
// precisely the thing the GitLab assertion needs to check.
type capturedReq struct {
	path   string
	auth   string
	method string
	body   map[string]any
}

func newIssueServer(t *testing.T, got *capturedReq, status int, resp string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.EscapedPath()
		got.auth = r.Header.Get("Authorization")
		if tok := r.Header.Get("PRIVATE-TOKEN"); tok != "" {
			got.auth = "PRIVATE-TOKEN:" + tok
		}
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &got.body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGithubCreateIssue(t *testing.T) {
	var got capturedReq
	srv := newIssueServer(t, &got, http.StatusCreated,
		`{"number":42,"title":"login fails","body":"boom","state":"open",
		  "html_url":"https://github.com/o/r/issues/42","user":{"login":"nova"},
		  "labels":[{"name":"bug"}]}`)

	c := &githubClient{baseURL: srv.URL, token: "tkn"}
	iss, err := c.CreateIssue(context.Background(), "https://github.com/o/r", "login fails", "boom", []string{"bug"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if want := "/repos/o/r/issues"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	if want := "token tkn"; got.auth != want {
		t.Errorf("Authorization = %q, want %q", got.auth, want)
	}
	if got.body["title"] != "login fails" || got.body["body"] != "boom" {
		t.Errorf("payload = %v, want title/body round-tripped", got.body)
	}
	if lbls, ok := got.body["labels"].([]any); !ok || len(lbls) != 1 || lbls[0] != "bug" {
		t.Errorf("labels = %v, want [bug]", got.body["labels"])
	}
	if iss.Number != 42 || iss.HTMLURL != "https://github.com/o/r/issues/42" || iss.Author != "nova" {
		t.Errorf("issue = %+v, want number/url/author populated", iss)
	}
	if len(iss.Labels) != 1 || iss.Labels[0] != "bug" {
		t.Errorf("issue.Labels = %v, want [bug]", iss.Labels)
	}
}

// A blank labels slice must be OMITTED, not sent as [] — GitHub rejects an
// empty labels array on issue creation.
func TestGithubCreateIssueOmitsEmptyLabels(t *testing.T) {
	var got capturedReq
	srv := newIssueServer(t, &got, http.StatusCreated, `{"number":1,"html_url":"u"}`)

	c := &githubClient{baseURL: srv.URL, token: "t"}
	if _, err := c.CreateIssue(context.Background(), "https://github.com/o/r", "t", "b", nil); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if _, present := got.body["labels"]; present {
		t.Errorf("payload contains labels key for a nil slice: %v", got.body)
	}
}

func TestGithubCreateIssueError(t *testing.T) {
	var got capturedReq
	srv := newIssueServer(t, &got, http.StatusUnauthorized,
		`{"message":"Bad credentials"}`)

	c := &githubClient{baseURL: srv.URL, token: "bad"}
	_, err := c.CreateIssue(context.Background(), "https://github.com/o/r", "t", "b", nil)
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}
	// The status AND the drained body both belong in the message — the body
	// is what tells an operator the token expired vs. lacks a scope.
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("err = %v, want status 401 and the response body", err)
	}
}

// parseRepo must keep working for the plain github.com forms while also
// accepting a host-agnostic HTTPS URL (GitHub Enterprise / httptest).
func TestGithubParseRepo(t *testing.T) {
	c := &githubClient{}
	cases := []struct{ in, owner, repo string }{
		{"https://github.com/owner/repo", "owner", "repo"},
		{"https://github.com/owner/repo.git", "owner", "repo"},
		{"git@github.com:owner/repo.git", "owner", "repo"},
		{"https://ghe.corp.example/owner/repo.git", "owner", "repo"},
	}
	for _, tc := range cases {
		o, r, err := c.parseRepo(tc.in)
		if err != nil {
			t.Errorf("parseRepo(%q): %v", tc.in, err)
			continue
		}
		if o != tc.owner || r != tc.repo {
			t.Errorf("parseRepo(%q) = %q/%q, want %q/%q", tc.in, o, r, tc.owner, tc.repo)
		}
	}
	if _, _, err := c.parseRepo("https://example.com/only-one-segment"); err == nil {
		t.Error("expected an error for a single-segment path")
	}
}

func TestGitlabCreateIssue(t *testing.T) {
	var got capturedReq
	srv := newIssueServer(t, &got, http.StatusCreated,
		`{"iid":7,"title":"t","description":"d","state":"opened",
		  "web_url":"https://gitlab.com/g/p/-/issues/7","author":{"username":"nova"},
		  "labels":["bug","autotest"]}`)

	c := &gitlabClient{baseURL: srv.URL, token: "glpat"}
	iss, err := c.CreateIssue(context.Background(), "https://gitlab.com/g/p", "t", "d", []string{"bug", "autotest"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	if want := "/api/v4/projects/g%2Fp/issues"; got.path != want {
		t.Errorf("path = %q, want %q (project path must be URL-escaped)", got.path, want)
	}
	if want := "PRIVATE-TOKEN:glpat"; got.auth != want {
		t.Errorf("auth = %q, want %q", got.auth, want)
	}
	// GitLab takes labels as a comma-separated STRING, not an array.
	if got.body["labels"] != "bug,autotest" {
		t.Errorf("labels = %v, want the csv string %q", got.body["labels"], "bug,autotest")
	}
	if got.body["description"] != "d" {
		t.Errorf("body key must be `description` on GitLab, got %v", got.body)
	}
	if iss.Number != 7 || iss.HTMLURL != "https://gitlab.com/g/p/-/issues/7" || len(iss.Labels) != 2 {
		t.Errorf("issue = %+v, want iid/url/labels populated", iss)
	}
}

func TestGitlabCreateIssueError(t *testing.T) {
	var got capturedReq
	srv := newIssueServer(t, &got, http.StatusForbidden, `{"message":"403 Forbidden"}`)

	c := &gitlabClient{baseURL: srv.URL, token: "t"}
	_, err := c.CreateIssue(context.Background(), "https://gitlab.com/g/p", "t", "d", nil)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want a 403 surfaced in the message", err)
	}
}

func TestGiteaCreateIssue(t *testing.T) {
	var got capturedReq
	srv := newIssueServer(t, &got, http.StatusCreated,
		`{"number":9,"title":"t","body":"[autotest]\n\nd","state":"open",
		  "html_url":"https://gitea.corp/o/r/issues/9","user":{"login":"nova"}}`)

	c := &giteaClient{baseURL: srv.URL, token: "gt"}
	iss, err := c.CreateIssue(context.Background(), "https://gitea.corp/o/r", "t", "d", []string{"bug"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	if want := "/api/v1/repos/o/r/issues"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	if want := "token gt"; got.auth != want {
		t.Errorf("Authorization = %q, want %q", got.auth, want)
	}
	// Gitea only accepts numeric label ids, so names are dropped entirely
	// and the [autotest] marker carries the intent in the body instead.
	if _, present := got.body["labels"]; present {
		t.Errorf("payload must NOT contain labels for Gitea: %v", got.body)
	}
	if !strings.HasPrefix(got.body["body"].(string), autotestMarker) {
		t.Errorf("body = %q, want the %q marker prefix", got.body["body"], autotestMarker)
	}
	if iss.Number != 9 || iss.Author != "nova" {
		t.Errorf("issue = %+v, want number/author populated", iss)
	}
}

func TestGiteaCreateIssueError(t *testing.T) {
	var got capturedReq
	srv := newIssueServer(t, &got, http.StatusInternalServerError, `{"message":"boom"}`)

	c := &giteaClient{baseURL: srv.URL, token: "t"}
	_, err := c.CreateIssue(context.Background(), "https://gitea.corp/o/r", "t", "d", nil)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want a 500 surfaced in the message", err)
	}
}

// New() must keep the old defaults: GitHub still gets api.github.com when
// no base_url is configured, GitLab still falls back to gitlab.com.
func TestNewDefaults(t *testing.T) {
	gh, err := New("github", "", "t")
	if err != nil {
		t.Fatalf("New(github): %v", err)
	}
	if got := gh.(*githubClient).baseURL; got != "https://api.github.com" {
		t.Errorf("github baseURL = %q, want https://api.github.com", got)
	}

	gl, err := New("gitlab", "", "t")
	if err != nil {
		t.Fatalf("New(gitlab): %v", err)
	}
	if got := gl.(*gitlabClient).baseURL; got != "https://gitlab.com" {
		t.Errorf("gitlab baseURL = %q, want https://gitlab.com", got)
	}

	if _, err := New("gitea", "", "t"); err == nil {
		t.Error("gitea without a base_url must still be an error")
	}
	if _, err := New("bitbucket", "", "t"); err == nil {
		t.Error("an unknown platform must be rejected")
	}
}
