package handler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// testEnvFixture builds the minimal environment the workspace writers read.
type testEnvFixture struct {
	seed     string
	browsers string
}

func (f *testEnvFixture) env() *model.TestEnvironment {
	return &model.TestEnvironment{
		ID:         "tenv_test",
		ProjectID:  "proj_test",
		Name:       "dev",
		BaseURL:    "http://localhost:3000",
		SeedTest:   f.seed,
		Browsers:   f.browsers,
		InitStatus: model.TestEnvStatusReady,
	}
}

// The harvest step is what turns an agent's filesystem output into a stored
// plan / spec list. Its two failure modes are silently producing an EMPTY
// plan (the agent worked but we missed the file) and silently claiming a
// STALE file (we picked up a leftover from a previous run) — so these tests
// pin both directions.

func touchSpec(t *testing.T, ws, rel string, age time.Duration) string {
	t.Helper()
	p := filepath.Join(ws, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte("content of "+rel), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	// Backdate so "written before the turn" vs "written during the turn" is
	// unambiguous regardless of filesystem timestamp granularity.
	stamp := time.Now().Add(-age)
	if err := os.Chtimes(p, stamp, stamp); err != nil {
		t.Fatalf("chtimes %s: %v", p, err)
	}
	return p
}

func TestHarvestPlanFilePrefersExactSlug(t *testing.T) {
	ws := t.TempDir()
	now := time.Now()
	touchSpec(t, ws, "specs/login-flow.md", 0)
	touchSpec(t, ws, "specs/other-thing.md", 0)

	path, md := harvestPlanFile(ws, "login-flow", now.Add(-time.Minute))
	if !strings.HasSuffix(path, filepath.Join("specs", "login-flow.md")) {
		t.Errorf("path = %q, want the exact slug", path)
	}
	if !strings.Contains(md, "login-flow") {
		t.Errorf("md = %q", md)
	}
}

// When the agent names its own file, the fallback must still find it — but
// only among files written during THIS turn.
func TestHarvestPlanFileFallsBackToFreshFiles(t *testing.T) {
	ws := t.TempDir()
	now := time.Now()
	touchSpec(t, ws, "specs/agent-chose-this.md", 0)

	path, md := harvestPlanFile(ws, "expected-slug", now.Add(-time.Minute))
	if !strings.HasSuffix(path, filepath.Join("specs", "agent-chose-this.md")) {
		t.Errorf("path = %q, want the fallback to the fresh file", path)
	}
	if md == "" {
		t.Error("fallback returned empty content")
	}
}

func TestHarvestPlanFileIgnoresStaleFiles(t *testing.T) {
	ws := t.TempDir()
	now := time.Now()
	// Written an hour ago — before the turn started.
	touchSpec(t, ws, "specs/old-plan.md", time.Hour)

	if _, md := harvestPlanFile(ws, "old-plan", now.Add(-time.Minute)); md != "" {
		t.Errorf("harvested a stale file (%q); the exact-slug branch must verify freshness too", md)
	}
}

func TestHarvestPlanFileEmptyWorkspace(t *testing.T) {
	if path, md := harvestPlanFile(t.TempDir(), "anything", time.Now()); path != "" || md != "" {
		t.Errorf("= %q / %q, want empty", path, md)
	}
}

func TestHarvestSpecFilesExcludesSeed(t *testing.T) {
	ws := t.TempDir()
	now := time.Now()
	touchSpec(t, ws, "tests/seed.spec.ts", 0)
	touchSpec(t, ws, "tests/login.spec.ts", 0)
	touchSpec(t, ws, "tests/cart.spec.ts", 0)

	got := harvestSpecFiles(ws, now.Add(-time.Minute))
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 files (seed excluded)", got)
	}
	for _, f := range got {
		if strings.HasSuffix(f, "seed.spec.ts") {
			t.Errorf("seed was harvested as a generated test: %v", got)
		}
	}
	// Paths must be workspace-relative and slash-separated so they are
	// portable across the DB, the API and the playwright invocation.
	for _, f := range got {
		if filepath.IsAbs(f) || strings.Contains(f, `\`) {
			t.Errorf("path %q is not workspace-relative posix form", f)
		}
	}
}

func TestHarvestSpecFilesIgnoresStale(t *testing.T) {
	ws := t.TempDir()
	now := time.Now()
	touchSpec(t, ws, "tests/from-last-time.spec.ts", time.Hour)

	if got := harvestSpecFiles(ws, now.Add(-time.Minute)); len(got) != 0 {
		t.Errorf("got %v, want no stale files", got)
	}
}

// A generator that rewrites only some files must not be credited with the
// ones it left untouched.
func TestHarvestSpecFilesPartialRewrite(t *testing.T) {
	ws := t.TempDir()
	now := time.Now()
	touchSpec(t, ws, "tests/touched.spec.ts", 0)
	touchSpec(t, ws, "tests/untouched.spec.ts", time.Hour)

	got := harvestSpecFiles(ws, now.Add(-time.Minute))
	if len(got) != 1 || !strings.HasSuffix(got[0], "touched.spec.ts") {
		t.Errorf("got %v, want only the rewritten file", got)
	}
}

func TestHarvestSpecFilesEmptyWorkspace(t *testing.T) {
	if got := harvestSpecFiles(t.TempDir(), time.Now()); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestMissingAgents(t *testing.T) {
	ws := t.TempDir()
	if got := missingAgents(ws); len(got) != 3 {
		t.Errorf("got %v, want all three missing on a bare workspace", got)
	}

	dir := filepath.Join(ws, ".claude", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range playwrightAgentFiles {
		if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if got := missingAgents(ws); len(got) != 0 {
		t.Errorf("got %v, want none missing once all agents exist", got)
	}

	// One missing is reported individually so the error names it.
	if err := os.Remove(filepath.Join(dir, "playwright-test-healer.md")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	got := missingAgents(ws)
	if len(got) != 1 || !strings.Contains(got[0], "healer") {
		t.Errorf("got %v, want just the healer", got)
	}
}

// enableProjectMCPServers is the difference between the agents having
// browser tools and silently burning their timeout with none.
func TestEnableProjectMCPServers(t *testing.T) {
	ws := t.TempDir()
	if err := enableProjectMCPServers(ws); err != nil {
		t.Fatalf("enableProjectMCPServers: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(ws, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("settings.json not written: %v", err)
	}
	if !strings.Contains(string(raw), `"enableAllProjectMcpServers": true`) {
		t.Errorf("settings.json = %s, want the MCP flag enabled", raw)
	}
}

// The merge, not the overwrite, matters: init-agents may already have
// written settings that must survive.
func TestEnableProjectMCPServersMerges(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	existing := `{"permissions":{"allow":["Bash(npm test)"]},"other":true}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(existing), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := enableProjectMCPServers(ws); err != nil {
		t.Fatalf("enableProjectMCPServers: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	got := string(raw)
	if !strings.Contains(got, "enableAllProjectMcpServers") {
		t.Errorf("flag missing: %s", got)
	}
	if !strings.Contains(got, "Bash(npm test)") || !strings.Contains(got, `"other"`) {
		t.Errorf("existing settings were clobbered: %s", got)
	}
}

// A malformed settings file must not wedge init — the agents would be
// blocked with no MCP and no way forward.
func TestEnableProjectMCPServersRecoversFromBadJSON(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := enableProjectMCPServers(ws); err != nil {
		t.Fatalf("a malformed settings.json must not fail init: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if !strings.Contains(string(raw), "enableAllProjectMcpServers") {
		t.Errorf("settings.json = %s", raw)
	}
}

func TestWriteWorkspaceFilesUsesConfiguredSeed(t *testing.T) {
	ws := t.TempDir()
	env := &testEnvFixture{seed: "test('custom', async () => {});", browsers: "chromium"}
	if err := writeWorkspaceFiles(ws, env.env()); err != nil {
		t.Fatalf("writeWorkspaceFiles: %v", err)
	}

	cfg, err := os.ReadFile(filepath.Join(ws, "playwright.config.ts"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	// baseURL and the report path must be runtime-injected so editing an
	// environment's base_url never needs a re-init.
	for _, want := range []string{"NOVA_BASE_URL", "PLAYWRIGHT_JSON_OUTPUT_NAME", "'on'", "retain-on-failure"} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("playwright.config.ts missing %q:\n%s", want, cfg)
		}
	}

	seed, err := os.ReadFile(filepath.Join(ws, "tests", "seed.spec.ts"))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if string(seed) != env.seed {
		t.Errorf("seed = %q, want the configured one", seed)
	}
}

func TestWriteWorkspaceFilesEmptySeedGetsDefault(t *testing.T) {
	ws := t.TempDir()
	if err := writeWorkspaceFiles(ws, (&testEnvFixture{}).env()); err != nil {
		t.Fatalf("writeWorkspaceFiles: %v", err)
	}
	seed, err := os.ReadFile(filepath.Join(ws, "tests", "seed.spec.ts"))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !strings.Contains(string(seed), "seed") {
		t.Errorf("default seed = %q", seed)
	}
}

// Re-running init overwrites cleanly — that is what makes init idempotent
// and doubles as the Playwright upgrade path.
func TestWriteWorkspaceFilesIsIdempotent(t *testing.T) {
	ws := t.TempDir()
	env := (&testEnvFixture{seed: "first"}).env()
	if err := writeWorkspaceFiles(ws, env); err != nil {
		t.Fatalf("first: %v", err)
	}
	env2 := (&testEnvFixture{seed: "second"}).env()
	if err := writeWorkspaceFiles(ws, env2); err != nil {
		t.Fatalf("second: %v", err)
	}
	seed, _ := os.ReadFile(filepath.Join(ws, "tests", "seed.spec.ts"))
	if string(seed) != "second" {
		t.Errorf("seed = %q, want the second write to win", seed)
	}
}
