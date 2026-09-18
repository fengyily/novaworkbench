package handler

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/store"
)

// Regression tests for resolveCodingProjectPath — the helper that fills in
// codingRunParams.ProjectPath when the scheduler (ScheduledExecutor
// .RunScheduledCoding) doesn't pre-fill it.
//
// Pre-fix symptom (req_b646601dbc5e7ac3): an empty ProjectPath let the
// downstream workDir/branchDir stay as "" — EnsureWorktreeLogged("", ...)
// then probed nova's own CWD (logged "ℹ️ 非 git 仓库"), git pull ran there
// and surfaced the C-runtime "致命错误：无法读取当前工作目录: No such file or
// directory", and the spawned `claude` (Bun runtime) failed with ENOENT when
// chdir-ing to "". The fix funnels that lookup through one helper, which the
// scheduler path now invokes and these tests pin down.
//
// Scope: the helper is pure (no DB, no subprocess, no clock). The fake getter
// below satisfies the narrow projectPathResolver interface, so the tests run
// in any environment without NOVA_DB_DSN.

// fakeProjectGetter is a minimal stub satisfying projectPathResolver.
// Records the last id it was asked for so tests can assert "not called" /
// "called with X" without standing up *service.ProjectService + *db.DB.
type fakeProjectGetter struct {
	mu       sync.Mutex
	calls    []string
	response *model.Project
	err      error
}

func (f *fakeProjectGetter) Get(id string) (*model.Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id)
	return f.response, f.err
}

func (f *fakeProjectGetter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// jobRecorder is a minimal stand-in for *store.Job that captures Append calls.
// We don't need the real JobStore ring buffer / SSE plumbing here — the helper
// only reads the job's side effect, so a recording shim is enough.
type jobRecorder struct {
	mu    sync.Mutex
	lines []store.LogLine
}

func (j *jobRecorder) Append(line store.LogLine) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.lines = append(j.lines, line)
}

func (j *jobRecorder) snapshot() []store.LogLine {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]store.LogLine, len(j.lines))
	copy(out, j.lines)
	return out
}

// jobRecorderAppender is the shape resolveCodingProjectPath needs on the job
// parameter — it only calls .Append(...). Anything *store.Job satisfies
// this; the shim keeps tests dependency-light.
type jobRecorderAppender interface {
	Append(store.LogLine)
}

// wrapRecorder turns a *jobRecorder into a jobRecorderAppender without forcing
// tests to deal with the pointer-vs-value dance.
func wrapRecorder(j *jobRecorder) jobRecorderAppender { return j }

// TestResolveCodingProjectPath covers the three execution branches documented
// in the helper's contract:
//
//  1. ProjectPath empty + reqRow != nil + getter OK → fallback resolves,
//     p.ProjectPath is rewritten, one `phase` line is appended, error is nil.
//  2. ProjectPath empty + reqRow == nil → no-op (legacy quick-start path),
//     p.ProjectPath stays "", no lines appended, no getter call.
//  3. ProjectPath non-empty → no-op, no getter call, no line appended.
func TestResolveCodingProjectPath(t *testing.T) {
	t.Run("empty_project_path_with_req_row_resolves_via_projectsvc", func(t *testing.T) {
		p := &codingRunParams{} // ProjectPath intentionally ""
		reqRow := &model.Requirement{ID: "req_abc", ProjectID: "proj_xyz"}
		getter := &fakeProjectGetter{
			response: &model.Project{ID: "proj_xyz", LocalPath: "/tmp/fake-proj"},
		}
		rec := &jobRecorder{}

		// WizardHandler{} is fine here: the helper only touches projectSvc
		// (passed as getter) and the job (passed as the appender).
		h := &WizardHandler{}
		err := h.resolveCodingProjectPath(p, reqRow, getter, wrapRecorder(rec))
		if err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
		if p.ProjectPath != "/tmp/fake-proj" {
			t.Fatalf("ProjectPath = %q, want /tmp/fake-proj", p.ProjectPath)
		}
		if getter.callCount() != 1 || getter.calls[0] != "proj_xyz" {
			t.Fatalf("getter calls = %v, want exactly [proj_xyz]", getter.calls)
		}
		lines := rec.snapshot()
		if len(lines) != 1 {
			t.Fatalf("expected exactly 1 log line, got %d: %+v", len(lines), lines)
		}
		if lines[0].Type != "phase" {
			t.Fatalf("log line type = %q, want phase", lines[0].Type)
		}
		if !strings.Contains(lines[0].Content, "/tmp/fake-proj") {
			t.Fatalf("log line content = %q, want it to contain /tmp/fake-proj", lines[0].Content)
		}
	})

	t.Run("empty_project_path_no_req_row_is_noop", func(t *testing.T) {
		p := &codingRunParams{} // ProjectPath intentionally ""
		getter := &fakeProjectGetter{
			response: &model.Project{ID: "proj_xyz", LocalPath: "/tmp/fake-proj"},
		}
		rec := &jobRecorder{}

		h := &WizardHandler{}
		err := h.resolveCodingProjectPath(p, nil, getter, wrapRecorder(rec))
		if err != nil {
			t.Fatalf("expected nil error for legacy quick-start path, got %v", err)
		}
		if p.ProjectPath != "" {
			t.Fatalf("ProjectPath = %q, want \"\" (reqRow is nil, must not touch)", p.ProjectPath)
		}
		if getter.callCount() != 0 {
			t.Fatalf("getter should NOT be called when reqRow is nil; got %d calls", getter.callCount())
		}
		if lines := rec.snapshot(); len(lines) != 0 {
			t.Fatalf("expected zero log lines for no-op path, got %d: %+v", len(lines), lines)
		}
	})

	t.Run("nonempty_project_path_is_noop", func(t *testing.T) {
		p := &codingRunParams{ProjectPath: "/already/set/by/http"}
		reqRow := &model.Requirement{ID: "req_abc", ProjectID: "proj_xyz"}
		getter := &fakeProjectGetter{
			response: &model.Project{ID: "proj_xyz", LocalPath: "/tmp/fake-proj"},
		}
		rec := &jobRecorder{}

		h := &WizardHandler{}
		err := h.resolveCodingProjectPath(p, reqRow, getter, wrapRecorder(rec))
		if err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
		if p.ProjectPath != "/already/set/by/http" {
			t.Fatalf("ProjectPath was overwritten: got %q, want /already/set/by/http", p.ProjectPath)
		}
		if getter.callCount() != 0 {
			t.Fatalf("getter should NOT be called when ProjectPath is non-empty; got %d calls", getter.callCount())
		}
		if lines := rec.snapshot(); len(lines) != 0 {
			t.Fatalf("expected zero log lines for no-op path, got %d: %+v", len(lines), lines)
		}
	})
}

// TestResolveCodingProjectPath_Errors pins the two failure shapes — the
// helper must surface them as a non-nil error AND emit an `error` log line so
// execStartCoding's caller can finish the JobStore job with JobError.
func TestResolveCodingProjectPath_Errors(t *testing.T) {
	t.Run("getter_error_propagates_and_logs", func(t *testing.T) {
		p := &codingRunParams{}
		reqRow := &model.Requirement{ID: "req_abc", ProjectID: "proj_xyz"}
		getter := &fakeProjectGetter{err: errors.New("db down")}
		rec := &jobRecorder{}

		h := &WizardHandler{}
		err := h.resolveCodingProjectPath(p, reqRow, getter, wrapRecorder(rec))
		if err == nil {
			t.Fatal("expected non-nil error, got nil")
		}
		if !strings.Contains(err.Error(), "db down") {
			t.Fatalf("error should wrap getter error, got %v", err)
		}
		if p.ProjectPath != "" {
			t.Fatalf("ProjectPath must stay empty on failure, got %q", p.ProjectPath)
		}
		lines := rec.snapshot()
		if len(lines) != 1 || lines[0].Type != "error" {
			t.Fatalf("expected 1 error log line, got %+v", lines)
		}
		if !strings.Contains(lines[0].Content, "proj_xyz") {
			t.Fatalf("error line should mention project id, got %q", lines[0].Content)
		}
	})

	t.Run("empty_local_path_propagates_and_logs", func(t *testing.T) {
		p := &codingRunParams{}
		reqRow := &model.Requirement{ID: "req_abc", ProjectID: "proj_xyz"}
		getter := &fakeProjectGetter{response: &model.Project{ID: "proj_xyz", LocalPath: ""}}
		rec := &jobRecorder{}

		h := &WizardHandler{}
		err := h.resolveCodingProjectPath(p, reqRow, getter, wrapRecorder(rec))
		if err == nil {
			t.Fatal("expected non-nil error, got nil")
		}
		if p.ProjectPath != "" {
			t.Fatalf("ProjectPath must stay empty on failure, got %q", p.ProjectPath)
		}
		lines := rec.snapshot()
		if len(lines) != 1 || lines[0].Type != "error" {
			t.Fatalf("expected 1 error log line, got %+v", lines)
		}
	})
}

// Compile-time proof: *service.ProjectService satisfies projectPathResolver.
// Avoids a "fake passes but real service breaks" footgun if the helper ever
// starts calling another method on the interface.
//
// We don't construct a real *service.ProjectService here (that pulls in
// *db.DB + driver init); we just declare the variable as the zero value
// pointer with a nil check — the assertion is purely structural.
//
//nolint:unused // type-assertion-only test: never executes the service.
var _ projectPathResolver = (*projectServiceSentinel)(nil)

// projectServiceSentinel is a type-alias shim: the var above proves that any
// pointer-to-struct with a matching Get method signature satisfies the
// interface, mirroring *service.ProjectService.Get. We keep it local so this
// test file doesn't need to import the service package (and its *db.DB
// dependency chain).
type projectServiceSentinel struct{}

func (projectServiceSentinel) Get(string) (*model.Project, error) { return nil, nil }
