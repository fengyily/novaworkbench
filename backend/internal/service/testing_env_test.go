package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/secret"
)

func newEnvSvc(t *testing.T) (*TestEnvService, string) {
	t.Helper()
	// secret.Init generates a throwaway master key under a temp HOME, so the
	// encryption round-trip exercises the real AES-GCM path.
	t.Setenv("HOME", t.TempDir())
	_ = secret.Init()
	d := newTestDB(t)
	seedProject(t, d, "proj_1", "proj 1", "/tmp/proj_1")
	return NewTestEnvService(d), "proj_1"
}

func TestTestEnvCreateAndGet(t *testing.T) {
	svc, projID := newEnvSvc(t)

	env, err := svc.Create(projID, model.CreateTestEnvReq{
		Name:     "  dev  ",
		BaseURL:  "http://localhost:5173",
		SeedTest: "test('seed', async ({ page }) => { await page.goto('/'); });",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if env.Name != "dev" {
		t.Errorf("name = %q, want the trimmed %q", env.Name, "dev")
	}
	if env.InitStatus != model.TestEnvStatusPending {
		t.Errorf("init_status = %q, want pending", env.InitStatus)
	}
	if env.Browsers != "chromium" {
		t.Errorf("browsers = %q, want the chromium default", env.Browsers)
	}
	if env.RetentionRuns != 20 {
		t.Errorf("retention_runs = %d, want the 20 default", env.RetentionRuns)
	}
	if !strings.HasPrefix(env.ID, "tenv_") {
		t.Errorf("id = %q, want a tenv_ prefix", env.ID)
	}

	got, err := svc.Get(env.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.BaseURL != env.BaseURL || got.SeedTest != env.SeedTest {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestTestEnvCreateValidates(t *testing.T) {
	svc, projID := newEnvSvc(t)
	cases := []struct {
		name string
		req  model.CreateTestEnvReq
	}{
		{"blank name", model.CreateTestEnvReq{BaseURL: "http://x.com"}},
		{"blank base_url", model.CreateTestEnvReq{Name: "dev"}},
		{"non-http scheme", model.CreateTestEnvReq{Name: "dev", BaseURL: "file:///etc/passwd"}},
		{"quote injection", model.CreateTestEnvReq{Name: "dev", BaseURL: `http://x.com/'; rm -rf /`}},
	}
	for _, tc := range cases {
		if _, err := svc.Create(projID, tc.req); err == nil {
			t.Errorf("%s: expected a validation error", tc.name)
		}
	}
}

func TestTestEnvDuplicateNameInSameProject(t *testing.T) {
	svc, projID := newEnvSvc(t)
	if _, err := svc.Create(projID, model.CreateTestEnvReq{Name: "dev", BaseURL: "http://a.com"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := svc.Create(projID, model.CreateTestEnvReq{Name: "dev", BaseURL: "http://b.com"})
	if !errors.Is(err, ErrEnvNameTaken) {
		t.Errorf("err = %v, want ErrEnvNameTaken", err)
	}
}

func TestTestEnvSameNameAcrossProjectsAllowed(t *testing.T) {
	svc, projID := newEnvSvc(t)
	if _, err := svc.Create(projID, model.CreateTestEnvReq{Name: "dev", BaseURL: "http://a.com"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	seedProject(t, svc.db, "proj_2", "proj 2", "/tmp/proj_2")
	if _, err := svc.Create("proj_2", model.CreateTestEnvReq{Name: "dev", BaseURL: "http://b.com"}); err != nil {
		t.Errorf("same name in another project must be allowed: %v", err)
	}
}

func TestTestEnvUpdatePartial(t *testing.T) {
	svc, projID := newEnvSvc(t)
	env, err := svc.Create(projID, model.CreateTestEnvReq{Name: "dev", BaseURL: "http://a.com", DefaultModel: "m1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	newURL := "http://b.com"
	browsers := "chromium,firefox"
	retention := 5
	updated, err := svc.Update(env.ID, model.UpdateTestEnvReq{BaseURL: &newURL, Browsers: &browsers, RetentionRuns: &retention})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.BaseURL != newURL || updated.Browsers != browsers || updated.RetentionRuns != 5 {
		t.Errorf("patch not applied: %+v", updated)
	}
	// Untouched fields survive a partial update.
	if updated.Name != "dev" || updated.DefaultModel != "m1" {
		t.Errorf("partial update clobbered other fields: %+v", updated)
	}
	// base_url is a runtime injection, so changing it must NOT require a
	// re-init — status has to stay 'ready'.
	if updated.InitStatus != model.TestEnvStatusPending {
		t.Errorf("init_status = %q, want it untouched", updated.InitStatus)
	}
}

func TestTestEnvUpdateRejectsBadValues(t *testing.T) {
	svc, projID := newEnvSvc(t)
	env, _ := svc.Create(projID, model.CreateTestEnvReq{Name: "dev", BaseURL: "http://a.com"})

	bad := "javascript:alert(1)"
	if _, err := svc.Update(env.ID, model.UpdateTestEnvReq{BaseURL: &bad}); err == nil {
		t.Error("expected a validation error for a javascript: base_url")
	}
	zero := 0
	if _, err := svc.Update(env.ID, model.UpdateTestEnvReq{RetentionRuns: &zero}); err == nil {
		t.Error("expected a validation error for retention_runs = 0")
	}
	huge := 100000
	if _, err := svc.Update(env.ID, model.UpdateTestEnvReq{RetentionRuns: &huge}); err == nil {
		t.Error("expected a validation error for an absurd retention_runs")
	}
	blank := "   "
	if _, err := svc.Update(env.ID, model.UpdateTestEnvReq{Name: &blank}); err == nil {
		t.Error("expected a validation error for a blank name")
	}
}

func TestTestEnvNormalizeBrowsers(t *testing.T) {
	cases := map[string]string{
		"chromium":             "chromium",
		"chromium,firefox":     "chromium,firefox",
		" Chromium , FIREFOX ": "chromium,firefox",
		"":                     "",
		"chromium,bogus":       "chromium",
		"bogus":                "",
	}
	for in, want := range cases {
		if got := normalizeBrowsers(in); got != want {
			t.Errorf("normalizeBrowsers(%q) = %q, want %q", in, got, want)
		}
	}
}

// The init state machine is the only concurrency guard on the workspace, so
// the busy transition has to actually be atomic.
func TestTestEnvInitStateMachine(t *testing.T) {
	svc, projID := newEnvSvc(t)
	env, _ := svc.Create(projID, model.CreateTestEnvReq{Name: "dev", BaseURL: "http://a.com"})

	// ClaimInit flips pending → initializing.
	got, err := svc.ClaimInit(env.ID)
	if err != nil {
		t.Fatalf("ClaimInit: %v", err)
	}
	if got.InitStatus != model.TestEnvStatusInitializing {
		t.Fatalf("status = %q, want initializing", got.InitStatus)
	}

	// A second claim must be refused — this is the double-click guard.
	if _, err := svc.ClaimInit(env.ID); !errors.Is(err, ErrEnvBusy) {
		t.Errorf("second ClaimInit err = %v, want ErrEnvBusy", err)
	}

	// MarkReady clears any stale error and records the version.
	if err := svc.MarkFailed(env.ID, "intermediate failure"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if err := svc.MarkReady(env.ID, "Version 1.56.1"); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	after, _ := svc.Get(env.ID)
	if after.InitStatus != model.TestEnvStatusReady || after.InitError != "" {
		t.Errorf("after MarkReady = %q / err %q, want ready / empty", after.InitStatus, after.InitError)
	}
	if after.PlaywrightVersion != "Version 1.56.1" {
		t.Errorf("playwright_version = %q", after.PlaywrightVersion)
	}

	// A ready environment may be re-initialized (Playwright upgrade path).
	if _, err := svc.ClaimInit(env.ID); err != nil {
		t.Errorf("re-init of a ready env must be allowed: %v", err)
	}
}

func TestTestEnvMarkFailedTruncates(t *testing.T) {
	svc, projID := newEnvSvc(t)
	env, _ := svc.Create(projID, model.CreateTestEnvReq{Name: "dev", BaseURL: "http://a.com"})

	huge := strings.Repeat("npm ERR! ", 5000)
	if err := svc.MarkFailed(env.ID, huge); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	got, _ := svc.Get(env.ID)
	if len(got.InitError) > 2100 {
		t.Errorf("init_error is %d bytes, want it truncated", len(got.InitError))
	}
	if got.InitStatus != model.TestEnvStatusFailed {
		t.Errorf("status = %q, want failed", got.InitStatus)
	}
}

func TestTestEnvReleaseInit(t *testing.T) {
	svc, projID := newEnvSvc(t)
	env, _ := svc.Create(projID, model.CreateTestEnvReq{Name: "dev", BaseURL: "http://a.com"})
	if _, err := svc.ClaimInit(env.ID); err != nil {
		t.Fatalf("ClaimInit: %v", err)
	}
	if err := svc.ReleaseInit(env.ID); err != nil {
		t.Fatalf("ReleaseInit: %v", err)
	}
	got, _ := svc.Get(env.ID)
	if got.InitStatus != model.TestEnvStatusPending {
		t.Errorf("status = %q, want pending after release", got.InitStatus)
	}
}

// Jobs are in-memory only, so a row still 'initializing' after a restart is
// an orphan. Leaving it would make every later init return 409 forever.
func TestTestEnvRecoverInterrupted(t *testing.T) {
	svc, projID := newEnvSvc(t)
	stuck, _ := svc.Create(projID, model.CreateTestEnvReq{Name: "stuck", BaseURL: "http://a.com"})
	if _, err := svc.ClaimInit(stuck.ID); err != nil {
		t.Fatalf("ClaimInit: %v", err)
	}
	clean, _ := svc.Create(projID, model.CreateTestEnvReq{Name: "clean", BaseURL: "http://b.com"})

	n, err := svc.RecoverInterrupted()
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if n != 1 {
		t.Errorf("recovered %d rows, want 1", n)
	}
	got, _ := svc.Get(stuck.ID)
	if got.InitStatus != model.TestEnvStatusFailed || got.InitError == "" {
		t.Errorf("stuck env = %q / %q, want failed with a reason", got.InitStatus, got.InitError)
	}
	untouched, _ := svc.Get(clean.ID)
	if untouched.InitStatus != model.TestEnvStatusPending {
		t.Errorf("a pending env must not be touched, got %q", untouched.InitStatus)
	}
}

func TestTestEnvVarsRoundTrip(t *testing.T) {
	svc, projID := newEnvSvc(t)
	kv := map[string]string{"API_TOKEN": "s3cr3t", "REGION": "eu"}
	env, err := svc.Create(projID, model.CreateTestEnvReq{
		Name: "dev", BaseURL: "http://a.com", EnvVars: kv,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !env.EnvVarsSet {
		t.Error("EnvVarsSet = false after storing vars")
	}

	// The public accessor must never expose the plaintext.
	fetched, err := svc.Get(env.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if strings.Contains(fetched.EnvVarsEnc, "s3cr3t") {
		t.Error("env_vars_enc contains the plaintext — it is not encrypted at rest")
	}

	back, err := svc.EnvVarsForRun(fetched)
	if err != nil {
		t.Fatalf("EnvVarsForRun: %v", err)
	}
	if back["API_TOKEN"] != "s3cr3t" || back["REGION"] != "eu" {
		t.Errorf("decrypted vars = %v, want %v", back, kv)
	}
}

func TestTestEnvNoVarsIsNotAnError(t *testing.T) {
	svc, projID := newEnvSvc(t)
	env, _ := svc.Create(projID, model.CreateTestEnvReq{Name: "dev", BaseURL: "http://a.com"})
	if env.EnvVarsSet {
		t.Error("EnvVarsSet = true with no vars stored")
	}
	vars, err := svc.EnvVarsForRun(env)
	if err != nil {
		t.Fatalf("EnvVarsForRun on an empty env: %v", err)
	}
	if len(vars) != 0 {
		t.Errorf("vars = %v, want empty", vars)
	}
}

// Sending env_vars again REPLACES the map, so removing one is expressible.
func TestTestEnvUpdateEnvVarsReplaces(t *testing.T) {
	svc, projID := newEnvSvc(t)
	env, _ := svc.Create(projID, model.CreateTestEnvReq{
		Name: "dev", BaseURL: "http://a.com",
		EnvVars: map[string]string{"A": "1", "B": "2"},
	})

	replacement := map[string]string{"A": "9"}
	if err := svc.SetEnvVarsForRun(env.ID, replacement); err != nil {
		t.Fatalf("SetEnvVarsForRun: %v", err)
	}
	fetched, _ := svc.Get(env.ID)
	vars, err := svc.EnvVarsForRun(fetched)
	if err != nil {
		t.Fatalf("EnvVarsForRun: %v", err)
	}
	if _, stillThere := vars["B"]; stillThere {
		t.Error("B survived a replacement — the map was merged instead of replaced")
	}
	if vars["A"] != "9" {
		t.Errorf("A = %q, want 9", vars["A"])
	}

	// Clearing with an empty map turns EnvVarsSet back off.
	if err := svc.SetEnvVarsForRun(env.ID, map[string]string{}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	fetched, _ = svc.Get(env.ID)
	if fetched.EnvVarsSet {
		t.Error("EnvVarsSet = true after clearing")
	}
}

func TestTestEnvOwnsProject(t *testing.T) {
	svc, projID := newEnvSvc(t)
	env, _ := svc.Create(projID, model.CreateTestEnvReq{Name: "dev", BaseURL: "http://a.com"})

	ok, err := svc.OwnsProject(env.ID, projID)
	if err != nil || !ok {
		t.Errorf("OwnsProject(self) = %v, %v; want true, nil", ok, err)
	}
	ok, err = svc.OwnsProject(env.ID, "proj_other")
	if err != nil || ok {
		t.Errorf("OwnsProject(other) = %v, %v; want false, nil", ok, err)
	}
}

func TestTestEnvGetMissing(t *testing.T) {
	svc, _ := newEnvSvc(t)
	if _, err := svc.Get("tenv_nope"); !errors.Is(err, ErrEnvNotFound) {
		t.Errorf("err = %v, want ErrEnvNotFound", err)
	}
}

func TestTestEnvListScopedToProject(t *testing.T) {
	svc, projID := newEnvSvc(t)
	for _, n := range []string{"a", "b"} {
		if _, err := svc.Create(projID, model.CreateTestEnvReq{Name: n, BaseURL: "http://x.com"}); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	seedProject(t, svc.db, "proj_2", "proj 2", "/tmp/proj_2")
	if _, err := svc.Create("proj_2", model.CreateTestEnvReq{Name: "c", BaseURL: "http://x.com"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	list, err := svc.List(projID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("List returned %d rows, want 2 (scoped to the project)", len(list))
	}
}

func TestTestEnvDeleteAndPurgeWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_ = secret.Init()
	svc, projID := newEnvSvc(t)

	env, _ := svc.Create(projID, model.CreateTestEnvReq{Name: "dev", BaseURL: "http://a.com"})
	ws, err := svc.EnsureWorkspace(env)
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Without purge the workspace survives.
	if err := svc.Delete(env.ID, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(ws); err != nil {
		t.Errorf("workspace removed without purge: %v", err)
	}

	// With purge it goes.
	env2, _ := svc.Create(projID, model.CreateTestEnvReq{Name: "dev2", BaseURL: "http://a.com"})
	ws2, err := svc.EnsureWorkspace(env2)
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws2, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := svc.Delete(env2.ID, true); err != nil {
		t.Fatalf("Delete(purge): %v", err)
	}
	if _, err := os.Stat(ws2); !os.IsNotExist(err) {
		t.Errorf("workspace survived purge: %v", err)
	}
}

func TestTestEnvStatusReportsDiskUsage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_ = secret.Init()
	svc, projID := newEnvSvc(t)
	env, _ := svc.Create(projID, model.CreateTestEnvReq{Name: "dev", BaseURL: "http://a.com"})

	ws, _ := svc.EnsureWorkspace(env)
	if err := os.MkdirAll(filepath.Join(ws, "runs", "trun_1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "runs", "trun_1", "trace.zip"), make([]byte, 4096), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	st, err := svc.Status(env.ID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.DiskUsageBytes < 4096 {
		t.Errorf("disk usage = %d, want >= 4096", st.DiskUsageBytes)
	}
	if st.Workspace != ws {
		t.Errorf("workspace = %q, want %q", st.Workspace, ws)
	}
	if st.Env == nil || st.Env.ID != env.ID {
		t.Errorf("status did not embed the environment: %+v", st.Env)
	}
}

// An override must win over the default location, since that is the whole
// point of the escape hatch.
func TestTestEnvWorkspaceOverrideWins(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_ = secret.Init()
	svc, projID := newEnvSvc(t)

	override := filepath.Join(t.TempDir(), "in-repo", "e2e")
	env, err := svc.Create(projID, model.CreateTestEnvReq{
		Name: "dev", BaseURL: "http://a.com", WorkspaceOverride: override,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ws, err := svc.EnsureWorkspace(env)
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	if ws != filepath.Clean(override) {
		t.Errorf("workspace = %q, want the override %q", ws, override)
	}
	if _, err := os.Stat(ws); err != nil {
		t.Errorf("override workspace not created: %v", err)
	}
}
