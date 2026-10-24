package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

func newRunSvc(t *testing.T) (*TestRunService, *TestPlanService, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	d := newTestDB(t)
	seedProject(t, d, "proj_1", "proj 1", "/tmp/proj_1")
	envSvc := NewTestEnvService(d)
	planSvc := NewTestPlanService(d, envSvc)
	return NewTestRunService(d, envSvc, planSvc), planSvc, "proj_1"
}

func seedEnv(t *testing.T, svc *TestRunService, envSvc *TestEnvService, projectID, name string) *model.TestEnvironment {
	t.Helper()
	env, err := svc.envSvc.Create(projectID, model.CreateTestEnvReq{
		Name: name, BaseURL: "http://localhost:3000",
	})
	if err != nil {
		t.Fatalf("seed env: %v", err)
	}
	return env
}

// ── plans ──────────────────────────────────────────────────────────────

func TestPlanLifecycle(t *testing.T) {
	runSvc, planSvc, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")

	plan, err := planSvc.Create(projID, model.CreatePlanReq{
		Title: "Login flow", Scenario: "The login page accepts valid credentials", EnvID: env.ID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if plan.Status != model.TestPlanStatusDraft {
		t.Errorf("status = %q, want draft", plan.Status)
	}
	if len(plan.SpecFileList) != 0 {
		t.Errorf("SpecFileList = %v, want empty", plan.SpecFileList)
	}

	if err := planSvc.MarkPlanning(plan.ID, "job_1"); err != nil {
		t.Fatalf("MarkPlanning: %v", err)
	}

	if err := planSvc.MarkPlanned(plan.ID, "# Plan\n\nsteps", "/ws/specs/login-flow.md", "sess_1"); err != nil {
		t.Fatalf("MarkPlanned: %v", err)
	}
	got, _ := planSvc.Get(plan.ID)
	if got.Status != model.TestPlanStatusPlanned || got.PlanMD != "# Plan\n\nsteps" || got.ClaudeSessionID != "sess_1" {
		t.Errorf("after MarkPlanned = %+v", got)
	}
	if got.LastJobID != "job_1" {
		t.Errorf("last_job_id = %q, want job_1 to survive the transition", got.LastJobID)
	}

	if err := planSvc.MarkGenerating(plan.ID, "job_2"); err != nil {
		t.Fatalf("MarkGenerating: %v", err)
	}
	if err := planSvc.MarkGenerated(plan.ID, []string{"tests/login.spec.ts"}, "sess_2"); err != nil {
		t.Fatalf("MarkGenerated: %v", err)
	}
	got, _ = planSvc.Get(plan.ID)
	if got.Status != model.TestPlanStatusGenerated {
		t.Errorf("status = %q, want generated", got.Status)
	}
	if len(got.SpecFileList) != 1 || got.SpecFileList[0] != "tests/login.spec.ts" {
		t.Errorf("SpecFileList = %v", got.SpecFileList)
	}

	// failed is retryable: MarkPlanning moves it straight back.
	if err := planSvc.MarkFailed(plan.ID, "agent crashed"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if err := planSvc.MarkPlanning(plan.ID, "job_3"); err != nil {
		t.Fatalf("retry via MarkPlanning: %v", err)
	}
	got, _ = planSvc.Get(plan.ID)
	if got.Status != model.TestPlanStatusPlanning || got.Error != "" {
		t.Errorf("retry = %q / %q, want planning / cleared error", got.Status, got.Error)
	}
}

func TestPlanCreateRejectsForeignEnv(t *testing.T) {
	runSvc, planSvc, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")
	seedProject(t, runSvc.db, "proj_2", "proj 2", "/tmp/proj_2")

	_, err := planSvc.Create("proj_2", model.CreatePlanReq{
		Title: "x", Scenario: "y", EnvID: env.ID,
	})
	if err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Errorf("err = %v, want a cross-project rejection", err)
	}
}

func TestPlanCreateDerivesTitle(t *testing.T) {
	runSvc, planSvc, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")

	plan, err := planSvc.Create(projID, model.CreatePlanReq{
		Scenario: "第一行标题\n第二行内容", EnvID: env.ID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if plan.Title != "第一行标题" {
		t.Errorf("title = %q, want the scenario's first line", plan.Title)
	}
}

// A Chinese title has no ASCII slug; it must still produce a stable,
// filename-safe path rather than an empty one.
func TestPlanSlugHandlesChineseTitles(t *testing.T) {
	if got := PlanSlug("登录流程测试"); got != "plan" {
		t.Errorf("slug = %q, want the %q fallback", got, "plan")
	}
	if got := PlanSlug("Login Flow!"); got != "login-flow" {
		t.Errorf("slug = %q, want login-flow", got)
	}
	if got := PlanSlug("a  b   c"); got != "a-b-c" {
		t.Errorf("slug = %q, want collapsed dashes", got)
	}
}

// A human edit must reach the disk, because that is where the generator
// agent reads the plan from.
func TestPlanUpdatePlanMDMirrorsToDisk(t *testing.T) {
	runSvc, planSvc, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")
	plan, _ := planSvc.Create(projID, model.CreatePlanReq{Title: "Login Flow", Scenario: "s", EnvID: env.ID})

	if err := planSvc.UpdatePlanMD(plan.ID, "## edited plan"); err != nil {
		t.Fatalf("UpdatePlanMD: %v", err)
	}
	ws, _ := envSvcWorkspace(t, runSvc, env)
	path := filepath.Join(ws, "specs", "login-flow.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("plan file not written to %s: %v", path, err)
	}
	if string(raw) != "## edited plan" {
		t.Errorf("file = %q, want the edit mirrored", raw)
	}
	got, _ := planSvc.Get(plan.ID)
	if got.PlanMD != "## edited plan" || got.PlanPath != path {
		t.Errorf("db = %q @ %q", got.PlanMD, got.PlanPath)
	}
}

func envSvcWorkspace(t *testing.T, runSvc *TestRunService, env *model.TestEnvironment) (string, error) {
	t.Helper()
	return runSvc.envSvc.EnsureWorkspace(env)
}

func TestPlanRecoverInterrupted(t *testing.T) {
	runSvc, planSvc, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")

	p1, _ := planSvc.Create(projID, model.CreatePlanReq{Title: "a", Scenario: "s", EnvID: env.ID})
	p2, _ := planSvc.Create(projID, model.CreatePlanReq{Title: "b", Scenario: "s", EnvID: env.ID})
	p3, _ := planSvc.Create(projID, model.CreatePlanReq{Title: "c", Scenario: "s", EnvID: env.ID})
	planSvc.MarkPlanning(p1.ID, "j")
	planSvc.MarkGenerating(p2.ID, "j")
	// p3 stays draft and must not be touched.

	n, err := planSvc.RecoverInterrupted()
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if n != 2 {
		t.Errorf("recovered %d, want 2", n)
	}
	for _, id := range []string{p1.ID, p2.ID} {
		got, _ := planSvc.Get(id)
		if got.Status != model.TestPlanStatusFailed || got.Error == "" {
			t.Errorf("plan %s = %q / %q, want failed with a reason", id, got.Status, got.Error)
		}
	}
	if got, _ := planSvc.Get(p3.ID); got.Status != model.TestPlanStatusDraft {
		t.Errorf("draft plan was modified: %q", got.Status)
	}
}

// ── scope resolution ───────────────────────────────────────────────────

func TestResolveScope(t *testing.T) {
	runSvc, planSvc, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")

	p1, _ := planSvc.Create(projID, model.CreatePlanReq{Title: "a", Scenario: "s", EnvID: env.ID})
	p2, _ := planSvc.Create(projID, model.CreatePlanReq{Title: "b", Scenario: "s", EnvID: env.ID})
	planSvc.MarkGenerated(p1.ID, []string{"tests/a.spec.ts"}, "s")
	planSvc.MarkGenerated(p2.ID, []string{"tests/b.spec.ts", "tests/a.spec.ts"}, "s")

	// all → every plan's files, de-duplicated.
	got, err := runSvc.ResolveScope(env.ID, model.RunScope{All: true})
	if err != nil {
		t.Fatalf("ResolveScope(all): %v", err)
	}
	if len(got) != 2 || got[0] != "tests/a.spec.ts" || got[1] != "tests/b.spec.ts" {
		t.Errorf("all = %v, want sorted+deduped [a b]", got)
	}

	// plan_ids narrows, and the same file from two plans appears once.
	got, err = runSvc.ResolveScope(env.ID, model.RunScope{PlanIDs: []string{p2.ID}})
	if err != nil {
		t.Fatalf("ResolveScope(plan_ids): %v", err)
	}
	if len(got) != 2 {
		t.Errorf("plan_ids = %v, want both files", got)
	}

	// explicit spec_files pass through untouched.
	got, err = runSvc.ResolveScope(env.ID, model.RunScope{SpecFiles: []string{"tests/custom.spec.ts"}})
	if err != nil {
		t.Fatalf("ResolveScope(spec_files): %v", err)
	}
	if len(got) != 1 || got[0] != "tests/custom.spec.ts" {
		t.Errorf("spec_files = %v", got)
	}
}

// An empty selection must error: `npx playwright test` with no files exits
// 0 and would record a meaningless "all passed" run.
func TestResolveScopeEmptyIsAnError(t *testing.T) {
	runSvc, _, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")
	if _, err := runSvc.ResolveScope(env.ID, model.RunScope{All: true}); err == nil {
		t.Error("expected an error when no specs exist")
	}
}

func TestResolveScopeRejectsForeignPlan(t *testing.T) {
	runSvc, planSvc, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")
	env2 := seedEnv(t, runSvc, nil, projID, "staging")
	plan, _ := planSvc.Create(projID, model.CreatePlanReq{Title: "x", Scenario: "s", EnvID: env2.ID})
	planSvc.MarkGenerated(plan.ID, []string{"tests/x.spec.ts"}, "s")

	if _, err := runSvc.ResolveScope(env.ID, model.RunScope{PlanIDs: []string{plan.ID}}); err == nil {
		t.Error("expected a plan from another environment to be rejected")
	}
}

// ── report ingest ──────────────────────────────────────────────────────

// writeReport materializes a report fixture and returns its path.
func writeReport(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	return p
}

func TestIngestReportParsesNestedSuites(t *testing.T) {
	runSvc, planSvc, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")
	_ = planSvc
	run, err := runSvc.Create(projID, env.ID, model.RunScope{All: true}, "job_1")
	if err != nil {
		t.Fatalf("Create run: %v", err)
	}

	runDir, _ := runSvc.RunDir(run)
	if err := os.MkdirAll(filepath.Join(runDir, "artifacts"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	report := `{
  "config": {"rootDir": "/ws"},
  "stats": {"expected": 1, "unexpected": 1, "flaky": 1, "skipped": 1},
  "suites": [{
    "title": "", "file": "tests/login.spec.ts",
    "specs": [{
      "title": "logs in",
      "tests": [{
        "projectName": "chromium", "status": "expected",
        "results": [{"status":"passed","duration":120,
                     "attachments":[{"name":"shot","path":"` + runDir + `/artifacts/shot.png","contentType":"image/png"}]}]
      }]
    }]
  },{
    "title": "", "file": "tests/cart.spec.ts",
    "suites": [{
      "title": "checkout", "file": "tests/cart.spec.ts",
      "specs": [{"title":"submits","tests":[{
        "projectName":"chromium","status":"unexpected",
        "results":[{"status":"timedOut","duration":5000,
          "error":{"message":"Test timeout of 5000ms exceeded"},
          "attachments":[
            {"name":"trace","path":"` + runDir + `/artifacts/trace.zip","contentType":"application/zip"},
            {"name":"escape","path":"/etc/passwd","contentType":"text/plain"}
          ]}]
      }]}]
    }]
  },{
    "title": "", "file": "tests/flaky.spec.ts",
    "specs": [{"title":"retries","tests":[{
      "projectName":"chromium","status":"flaky",
      "results":[{"status":"failed","duration":10},{"status":"passed","duration":20}]
    }]}]
  },{
    "title": "", "file": "tests/skip.spec.ts",
    "specs": [{"title":"pending","tests":[{
      "projectName":"chromium","status":"skipped",
      "results":[{"status":"skipped","duration":0}]
    }]}]
  }]
}`
	if err := runSvc.IngestReport(run.ID, writeReport(t, report)); err != nil {
		t.Fatalf("IngestReport: %v", err)
	}

	cases, err := runSvc.GetCases(run.ID)
	if err != nil {
		t.Fatalf("GetCases: %v", err)
	}
	if len(cases) != 4 {
		t.Fatalf("got %d cases, want 4", len(cases))
	}

	byTitle := map[string]*model.TestRunCase{}
	for _, c := range cases {
		byTitle[c.Title] = c
	}

	login := byTitle["logs in"]
	if login == nil {
		t.Fatalf("missing 'logs in'; got titles %v", titlesOf(cases))
	}
	if login.Status != model.TestCaseStatusPassed || login.File != "tests/login.spec.ts" {
		t.Errorf("login = %+v", login)
	}
	if login.DurationMs != 120 {
		t.Errorf("login duration = %d, want 120", login.DurationMs)
	}
	if len(login.AttachmentList) != 1 || login.AttachmentList[0].Path != "artifacts/shot.png" {
		t.Errorf("login attachments = %+v, want a workspace-relative path", login.AttachmentList)
	}

	// Nested describe() must produce "checkout › submits".
	nested := byTitle["checkout › submits"]
	if nested == nil {
		t.Fatalf("nested suite title missing; got %v", titlesOf(cases))
	}
	if nested.Status != model.TestCaseStatusTimedOut {
		t.Errorf("nested status = %q, want timedOut", nested.Status)
	}
	if !strings.Contains(nested.Error, "timeout") {
		t.Errorf("nested error = %q", nested.Error)
	}
	// The /etc/passwd attachment must be dropped, the in-run one kept.
	if len(nested.AttachmentList) != 1 {
		t.Errorf("attachments = %+v, want the out-of-run path dropped", nested.AttachmentList)
	}
	if nested.AttachmentList[0].Path != "artifacts/trace.zip" {
		t.Errorf("kept path = %q", nested.AttachmentList[0].Path)
	}

	// A retried test: the LAST result decides.
	flaky := byTitle["retries"]
	if flaky == nil {
		t.Fatalf("missing 'retries'; got %v", titlesOf(cases))
	}
	if flaky.Status != model.TestCaseStatusFlaky || flaky.DurationMs != 20 {
		t.Errorf("flaky = %q / %d, want flaky and the last result's duration", flaky.Status, flaky.DurationMs)
	}

	if got := byTitle["pending"]; got == nil || got.Status != model.TestCaseStatusSkipped {
		t.Errorf("skipped case = %+v", got)
	}

	// Counters are derived from the stored rows, so the ledger always matches
	// what the user can click.
	final, _ := runSvc.Get(run.ID)
	if final.Total != 4 {
		t.Errorf("total = %d, want 4", final.Total)
	}
	if final.Passed != 1 {
		t.Errorf("passed = %d, want 1", final.Passed)
	}
	if final.Failed != 1 {
		t.Errorf("failed = %d, want 1 (timedOut counts as failed)", final.Failed)
	}
	if final.Flaky != 1 {
		t.Errorf("flaky = %d, want 1", final.Flaky)
	}
	if final.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", final.Skipped)
	}
	if final.Status != model.TestRunStatusFailed {
		t.Errorf("status = %q, want failed", final.Status)
	}
	if final.FinishedAt == nil {
		t.Error("finished_at was not stamped")
	}
}

func TestIngestReportAllPassedIsGreen(t *testing.T) {
	runSvc, _, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")
	run, _ := runSvc.Create(projID, env.ID, model.RunScope{All: true}, "j")

	report := `{"config":{"rootDir":"/ws"},"suites":[{"title":"","file":"tests/a.spec.ts",
	  "specs":[{"title":"ok","tests":[{"status":"expected","results":[{"status":"passed","duration":5}]}]}]}]}`
	if err := runSvc.IngestReport(run.ID, writeReport(t, report)); err != nil {
		t.Fatalf("IngestReport: %v", err)
	}
	got, _ := runSvc.Get(run.ID)
	if got.Status != model.TestRunStatusPassed || got.Failed != 0 {
		t.Errorf("run = %q / failed %d, want passed", got.Status, got.Failed)
	}
}

// Re-ingesting must replace, not duplicate.
func TestIngestReportIsIdempotent(t *testing.T) {
	runSvc, _, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")
	run, _ := runSvc.Create(projID, env.ID, model.RunScope{All: true}, "j")

	report := `{"config":{"rootDir":"/ws"},"suites":[{"title":"","file":"tests/a.spec.ts",
	  "specs":[{"title":"ok","tests":[{"status":"expected","results":[{"status":"passed","duration":5}]}]}]}]}`
	p := writeReport(t, report)
	for i := 0; i < 3; i++ {
		if err := runSvc.IngestReport(run.ID, p); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}
	cases, _ := runSvc.GetCases(run.ID)
	if len(cases) != 1 {
		t.Errorf("got %d cases after 3 ingests, want 1", len(cases))
	}
}

func TestIngestReportMissingFile(t *testing.T) {
	runSvc, _, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")
	run, _ := runSvc.Create(projID, env.ID, model.RunScope{All: true}, "j")
	if err := runSvc.IngestReport(run.ID, filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("expected an error for a missing report")
	}
}

func TestIngestReportMalformedJSON(t *testing.T) {
	runSvc, _, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")
	run, _ := runSvc.Create(projID, env.ID, model.RunScope{All: true}, "j")
	if err := runSvc.IngestReport(run.ID, writeReport(t, "{not json")); err == nil {
		t.Error("expected an error for malformed JSON")
	}
}

// The traversal guard is the security-relevant invariant: these relative
// paths are joined onto the run dir when serving.
func TestSafeRelativePath(t *testing.T) {
	base := filepath.Join("/ws", "runs", "trun_1")

	ok := []struct{ in, want string }{
		{filepath.Join(base, "artifacts", "shot.png"), "artifacts/shot.png"},
		{base, "."},
	}
	for _, tc := range ok {
		got, allowed := safeRelativePath(tc.in, base)
		if !allowed {
			t.Errorf("safeRelativePath(%q) was rejected", tc.in)
			continue
		}
		if got != tc.want {
			t.Errorf("safeRelativePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	bad := []string{
		"/etc/passwd",
		filepath.Join(base, "..", "..", "..", "etc", "passwd"),
		filepath.Join("/ws", "runs", "trun_1-evil", "x.png"), // prefix-lookalike
		"",
	}
	for _, in := range bad {
		if got, allowed := safeRelativePath(in, base); allowed {
			t.Errorf("safeRelativePath(%q) = %q, want it REJECTED", in, got)
		}
	}
}

func TestFailingCasesExcludesSkipped(t *testing.T) {
	runSvc, _, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")
	run, _ := runSvc.Create(projID, env.ID, model.RunScope{All: true}, "j")

	report := `{"config":{"rootDir":"/ws"},"suites":[{"title":"","file":"tests/a.spec.ts","specs":[
	  {"title":"ok","tests":[{"status":"expected","results":[{"status":"passed","duration":1}]}]},
	  {"title":"bad","tests":[{"status":"unexpected","results":[{"status":"failed","duration":1,"error":{"message":"boom"}}]}]},
	  {"title":"skip","tests":[{"status":"skipped","results":[{"status":"skipped","duration":0}]}]}
	]}]}`
	if err := runSvc.IngestReport(run.ID, writeReport(t, report)); err != nil {
		t.Fatalf("IngestReport: %v", err)
	}
	failing, err := runSvc.FailingCases(run.ID)
	if err != nil {
		t.Fatalf("FailingCases: %v", err)
	}
	if len(failing) != 1 || failing[0].Title != "bad" {
		t.Errorf("failing = %v, want only the failed case", titlesOf(failing))
	}
}

// ── retention / lifecycle ──────────────────────────────────────────────

func TestTrimRetention(t *testing.T) {
	runSvc, _, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")
	if _, err := runSvc.envSvc.Update(env.ID, model.UpdateTestEnvReq{RetentionRuns: ptr(2)}); err != nil {
		t.Fatalf("set retention: %v", err)
	}

	var ids []string
	for i := 0; i < 4; i++ {
		run, err := runSvc.Create(projID, env.ID, model.RunScope{All: true}, "j")
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids = append(ids, run.ID)
		dir, _ := runSvc.RunDir(run)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "x.bin"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		// Space the timestamps so the ordering is deterministic.
		if _, err := runSvc.db.Exec(`UPDATE test_runs SET started_at = ? WHERE id = ?`,
			timeAt(i), run.ID); err != nil {
			t.Fatalf("stamp: %v", err)
		}
	}

	if err := runSvc.TrimRetention(env.ID); err != nil {
		t.Fatalf("TrimRetention: %v", err)
	}

	remaining, _ := runSvc.List(projID, env.ID)
	if len(remaining) != 2 {
		t.Fatalf("kept %d runs, want 2", len(remaining))
	}
	// The newest survive, the oldest are gone from disk.
	if remaining[0].ID != ids[3] || remaining[1].ID != ids[2] {
		t.Errorf("kept %s/%s, want the two newest %s/%s", remaining[0].ID, remaining[1].ID, ids[3], ids[2])
	}
	for _, id := range ids[:2] {
		run, err := runSvc.Get(id)
		if err != nil {
			continue // already deleted
		}
		dir, _ := runSvc.RunDir(run)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("evidence dir for trimmed run %s survived", id)
		}
	}
}

func TestRecoverInterruptedRuns(t *testing.T) {
	runSvc, _, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")
	run, _ := runSvc.Create(projID, env.ID, model.RunScope{All: true}, "j")

	n, err := runSvc.RecoverInterrupted()
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if n != 1 {
		t.Errorf("recovered %d, want 1", n)
	}
	got, _ := runSvc.Get(run.ID)
	if got.Status != model.TestRunStatusError || got.Error == "" {
		t.Errorf("run = %q / %q, want error with a reason", got.Status, got.Error)
	}
}

func TestFinishRunCanceled(t *testing.T) {
	runSvc, _, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")
	run, _ := runSvc.Create(projID, env.ID, model.RunScope{All: true}, "j")

	if err := runSvc.FinishRun(run.ID, model.TestRunStatusCanceled, "用户取消"); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	got, _ := runSvc.Get(run.ID)
	if got.Status != model.TestRunStatusCanceled || got.FinishedAt == nil {
		t.Errorf("run = %+v", got)
	}
}

// Close-loop backfills must be single-shot: a second click is refused.
func TestCloseLoopBackfillsAreIdempotent(t *testing.T) {
	runSvc, _, projID := newRunSvc(t)
	env := seedEnv(t, runSvc, nil, projID, "dev")
	run, _ := runSvc.Create(projID, env.ID, model.RunScope{All: true}, "j")

	report := `{"config":{"rootDir":"/ws"},"suites":[{"title":"","file":"tests/a.spec.ts",
	  "specs":[{"title":"bad","tests":[{"status":"unexpected","results":[{"status":"failed","duration":1}]}]}]}]}`
	if err := runSvc.IngestReport(run.ID, writeReport(t, report)); err != nil {
		t.Fatalf("IngestReport: %v", err)
	}
	cases, _ := runSvc.FailingCases(run.ID)
	id := cases[0].ID

	if err := runSvc.AttachIssueURL(id, "https://github.com/o/r/issues/1", 1); err != nil {
		t.Fatalf("AttachIssueURL: %v", err)
	}
	if err := runSvc.AttachIssueURL(id, "https://github.com/o/r/issues/2", 2); err == nil {
		t.Error("a second issue backfill must be refused")
	}
	got, _ := runSvc.GetCase(id)
	if got.IssueURL != "https://github.com/o/r/issues/1" || got.IssueNumber != 1 {
		t.Errorf("case = %+v, want the first issue to stick", got)
	}

	if err := runSvc.AttachRequirementID(id, "req_x"); err != nil {
		t.Fatalf("AttachRequirementID: %v", err)
	}
	if err := runSvc.AttachRequirementID(id, "req_y"); err == nil {
		t.Error("a second requirement backfill must be refused")
	}
}

func titlesOf(cases []*model.TestRunCase) []string {
	out := make([]string, len(cases))
	for i, c := range cases {
		out[i] = c.Title
	}
	return out
}

// ptr returns a pointer to v, for the *int update fields.
func ptr[T any](v T) *T { return &v }

// timeAt builds deterministic, increasing timestamps for retention ordering.
func timeAt(i int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC)
}
