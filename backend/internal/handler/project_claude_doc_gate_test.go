package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// TestClaudeDocGenGatePerProject pins the per-project exclusivity: one
// generation at a time per project.
//
// This is the bug the gate exists for. Two concurrent runs explore the same
// repository at the same time and race on the same document row — the later
// write silently wins, so the user ends up with whichever model happened to
// finish last, and neither run can be attributed to a request.
func TestClaudeDocGenGatePerProject(t *testing.T) {
	var g claudeDocGenGate

	release, ok := g.tryAcquire("proj_a")
	if !ok {
		t.Fatal("the first acquire for a project must succeed")
	}
	if _, ok := g.tryAcquire("proj_a"); ok {
		t.Fatal("a second acquire for the SAME project must be refused")
	}
	// Another project is unaffected — the gate is per project, not global.
	releaseB, ok := g.tryAcquire("proj_b")
	if !ok {
		t.Fatal("a different project must not be blocked")
	}
	releaseB()

	// Releasing frees the slot.
	release()
	if _, ok := g.tryAcquire("proj_a"); !ok {
		t.Fatal("an acquire after the release must succeed")
	}
}

// TestClaudeDocGenGateStaleRelease pins that a late release cannot free the
// slot of a LATER run.
//
// The release func is called from a defer in the generation goroutine. If it
// were to free "the slot for this project" unconditionally, a late release
// would let a second run start while the first one is still writing —
// exactly the race the gate is meant to prevent.
func TestClaudeDocGenGateStaleRelease(t *testing.T) {
	var g claudeDocGenGate

	release1, ok := g.tryAcquire("proj_a")
	if !ok {
		t.Fatal("acquire failed")
	}
	release1()

	release2, ok := g.tryAcquire("proj_a")
	if !ok {
		t.Fatal("acquire after release failed")
	}
	// A stale release of the FIRST run must not free the second run's slot.
	release1()
	if _, ok := g.tryAcquire("proj_a"); ok {
		t.Fatal("a stale release must not free the slot of a later run")
	}
	release2()
	if _, ok := g.tryAcquire("proj_a"); !ok {
		t.Fatal("the slot must be free after the last release")
	}
}

// TestClaudeDocGenGateGlobalCap pins the platform-wide cap: the second
// acquire waits for the slot instead of running alongside the first one.
func TestClaudeDocGenGateGlobalCap(t *testing.T) {
	g := claudeDocGenGate{globalCap: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	waited, err := g.acquireGlobal(ctx)
	if err != nil {
		t.Fatalf("the first acquire failed: %v", err)
	}
	if waited {
		t.Error("the first acquire must not wait")
	}

	done := make(chan error, 1)
	go func() {
		_, err := g.acquireGlobal(ctx)
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("the second acquire must wait for the slot (returned early: %v)", err)
	case <-time.After(150 * time.Millisecond):
	}

	g.releaseGlobal()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the second acquire failed after the release: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the second acquire must proceed once the slot is released")
	}
	g.releaseGlobal()
}

// TestClaudeDocGenerationConcurrency pins the env resolution of the
// platform-wide cap. A value below 1 (or a non-number) falls back to the
// default — a zero cap would wedge every generation forever.
func TestClaudeDocGenerationConcurrency(t *testing.T) {
	t.Setenv(claudeDocGenerationConcurrencyEnv, "3")
	if got := claudeDocGenerationConcurrency(); got != 3 {
		t.Errorf("concurrency = %d, want 3", got)
	}
	t.Setenv(claudeDocGenerationConcurrencyEnv, "0")
	if got := claudeDocGenerationConcurrency(); got != claudeDocGenerationConcurrencyDefault {
		t.Errorf("concurrency = %d, want the default %d", got, claudeDocGenerationConcurrencyDefault)
	}
	t.Setenv(claudeDocGenerationConcurrencyEnv, "not-a-number")
	if got := claudeDocGenerationConcurrency(); got != claudeDocGenerationConcurrencyDefault {
		t.Errorf("concurrency = %d, want the default %d", got, claudeDocGenerationConcurrencyDefault)
	}
}

// fakeClaudeBin writes a stand-in for the claude CLI.
//
// It accepts the CLI's argv, sleeps for a moment (so a test can observe a
// run that is IN FLIGHT) and exits without producing a result event. That
// keeps the tests hermetic: no model is invoked and no token is spent.
func fakeClaudeBin(t *testing.T, sleepSeconds int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake claude CLI is a POSIX shell script")
	}
	path := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\nsleep " + strconv.Itoa(sleepSeconds) + "\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return path
}

// TestGenerateRejectsConcurrentRunForSameProject is the end-to-end guard
// for the P1: a second "generate" for a project that is already generating
// must be refused with 409 GENERATION_IN_PROGRESS instead of starting a
// second claude process.
func TestGenerateRejectsConcurrentRunForSameProject(t *testing.T) {
	rig := newClaudeDocGenerateRig(t, 2)

	// First request: admitted, the run starts.
	rec := rig.doGenerate(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("the first generate must be admitted (status=%d, body: %s)", rec.Code, rec.Body.String())
	}
	var first struct {
		Success bool `json:"success"`
		Data    struct {
			JobID string `json:"job_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if !first.Success {
		t.Fatalf("the first generate failed: %s", rec.Body.String())
	}
	jobID := first.Data.JobID
	if jobID == "" {
		t.Fatalf("the first generate must return a job id: %s", rec.Body.String())
	}

	// Second request while the first is in flight: refused.
	rec = rig.doGenerate(t)
	if rec.Code != http.StatusConflict {
		t.Fatalf("the second generate must be refused with 409 (status=%d, body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "GENERATION_IN_PROGRESS") {
		t.Errorf("body = %s, want the GENERATION_IN_PROGRESS code", rec.Body.String())
	}

	// Once the run is over the project is admissible again — a leaked slot
	// would block the project forever.
	waitForClaudeDocJobDone(t, rig.h.jobs, jobID)
	rec = rig.doGenerate(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("the generate after the run finished must be admitted (status=%d, body: %s)", rec.Code, rec.Body.String())
	}
}

// waitForClaudeDocJobDone waits until the job reaches a terminal state.
func waitForClaudeDocJobDone(t *testing.T, jobs *store.JobStore, jobID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := jobs.Get(jobID)
		if ok {
			_, status, _ := job.Snapshot()
			if status != store.JobRunning {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish in time", jobID)
}

// claudeDocGenerateRig is a project + handler wired for the generation
// endpoints, with a stand-in claude CLI.
type claudeDocGenerateRig struct {
	h    *ProjectClaudeDocHandler
	mux  *http.ServeMux
	proj string
}

func newClaudeDocGenerateRig(t *testing.T, fakeSleepSeconds int) *claudeDocGenerateRig {
	t.Helper()
	bin := fakeClaudeBin(t, fakeSleepSeconds)
	// The gateway resolves the CLI path at construction time.
	t.Setenv("CLAUDE_BIN", bin)

	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	svc := service.NewProjectService(d, nil, nil)
	const proj = "proj_claude_doc_gate"
	// A non-git directory: the base-branch sync is skipped, so the run
	// goes straight to the (fake) CLI.
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path) VALUES (?, ?, ?)`,
		proj, "gate project", t.TempDir()); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	h := NewProjectClaudeDocHandler(svc, llm.New(nil, nil), store.NewJobStore(8))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/projects/{id}/claude-doc/generate", h.Generate)
	return &claudeDocGenerateRig{h: h, mux: mux, proj: proj}
}

func (r *claudeDocGenerateRig) doGenerate(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/projects/"+r.proj+"/claude-doc/generate", strings.NewReader(`{"focus":""}`))
	req.SetPathValue("id", r.proj)
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}
