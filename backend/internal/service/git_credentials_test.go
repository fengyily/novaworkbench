package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

// gitCredTestDB opens a fresh sqlite DB with the full schema applied and
// seeds a single project row + an optional platform token. Pass token=nil to
// insert the project without any platform_token_id bound (covers TestBuildEnv_NoToken
// and TestBuildEnv_ProjectLookupFails, where the project_id itself is the
// missing piece).
func gitCredTestDB(t *testing.T, projectID, remoteURL, platformTokenID string) *db.DB {
	t.Helper()
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path, remote_url, default_branch)
		 VALUES (?, 'Test', ?, ?, 'main')`,
		projectID, t.TempDir(), remoteURL,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	// Stamp platform_token_id on the project so the JOIN can find a token row.
	// Empty string is allowed — TestBuildEnv_NoToken asserts the empty-id branch.
	if _, err := d.Exec(`UPDATE projects SET platform_token_id = ? WHERE id = ?`, platformTokenID, projectID); err != nil {
		t.Fatalf("stamp platform_token_id: %v", err)
	}
	return d
}

// seedToken inserts a platform_tokens row and returns its id. gitUserName may
// be empty (covers both GitLab branches).
func seedToken(t *testing.T, d *db.DB, id, platform, token, gitUserName string) {
	t.Helper()
	if _, err := d.Exec(
		`INSERT INTO platform_tokens (id, name, platform, token, git_user_name, git_user_email)
		 VALUES (?, 'seed', ?, ?, ?, '')`,
		id, platform, token, gitUserName,
	); err != nil {
		t.Fatalf("seed platform_token %s: %v", id, err)
	}
}

// envHasPrefix returns true if env contains a "KEY=VALUE" pair with VALUE
// starting with the given prefix. Used to assert GIT_ASKPASS / GIT_TERMINAL_PROMPT
// are present without hard-coding the temp file name.
func envHasPrefix(env []string, key, valuePrefix string) bool {
	prefix := key + "=" + valuePrefix
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// envExact returns the value of "KEY=..." in env, or "" if absent.
func envExact(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix)
		}
	}
	return ""
}

// assertNoResidualAskpass fails the test if any nova-askpass-*.sh script is
// left behind in the OS temp dir. Returns a func() suitable for t.Cleanup
// so a panic in a test case still surfaces the leak.
func assertNoResidualAskpass(t *testing.T) func() {
	t.Helper()
	return func() {
		matches, err := filepath.Glob(filepath.Join(os.TempDir(), "nova-askpass-*.sh"))
		if err != nil {
			t.Errorf("glob: %v", err)
			return
		}
		if len(matches) > 0 {
			t.Errorf("residual askpass scripts after test: %v", matches)
			for _, m := range matches {
				_ = os.Remove(m)
			}
		}
	}
}

// --------------------------------------------------------------------
// 1. GitHub HTTPS — token as username, empty password
// --------------------------------------------------------------------
func TestBuildEnv_HTTPSGitHub(t *testing.T) {
	t.Cleanup(assertNoResidualAskpass(t))
	d := gitCredTestDB(t, "proj_gh", "https://github.com/owner/repo.git", "tok_gh")
	seedToken(t, d, "tok_gh", "github", "ghp-secret-token-abc123", "")

	env, cleanup, err := NewGitCredentialService(d).BuildEnv(context.Background(), "proj_gh")
	t.Cleanup(cleanup)
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}
	if len(env) == 0 {
		t.Fatal("expected askpass env, got empty slice")
	}
	askpass := envExact(env, "GIT_ASKPASS")
	if askpass == "" {
		t.Fatalf("GIT_ASKPASS missing from env: %v", env)
	}
	if !envHasPrefix(env, "GIT_ASKPASS", filepath.Join(os.TempDir(), "nova-askpass-")) {
		t.Errorf("GIT_ASKPASS not under os.TempDir(): %s", askpass)
	}
	if envExact(env, "GIT_TERMINAL_PROMPT") != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT != 0: %v", env)
	}
	// GitHub: askpass script emits the token (which is also the username for
	// basic-auth). Run the script and assert stdout equals the raw token.
	out, runErr := execAskpass(t, askpass)
	if runErr != nil {
		t.Fatalf("askpass exec: %v", runErr)
	}
	if out != "ghp-secret-token-abc123" {
		t.Errorf("askpass output = %q, want %q", out, "ghp-secret-token-abc123")
	}
}

// --------------------------------------------------------------------
// 2. GitLab HTTPS with explicit GitUserName
// --------------------------------------------------------------------
func TestBuildEnv_HTTPSGitLab_WithGitUser(t *testing.T) {
	t.Cleanup(assertNoResidualAskpass(t))
	d := gitCredTestDB(t, "proj_gl_user", "https://gitlab.example.com/group/proj.git", "tok_gl_user")
	seedToken(t, d, "tok_gl_user", "gitlab", "glpat-deadbeef", "alice")

	env, cleanup, err := NewGitCredentialService(d).BuildEnv(context.Background(), "proj_gl_user")
	t.Cleanup(cleanup)
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}
	askpass := envExact(env, "GIT_ASKPASS")
	if askpass == "" {
		t.Fatalf("GIT_ASKPASS missing: %v", env)
	}
	// GitLab with git_user_name: askpass stdout = the token (password); the
	// URL/remote already supplies the username separately.
	out, runErr := execAskpass(t, askpass)
	if runErr != nil {
		t.Fatalf("askpass exec: %v", runErr)
	}
	if out != "glpat-deadbeef" {
		t.Errorf("askpass output = %q, want %q", out, "glpat-deadbeef")
	}
}

// --------------------------------------------------------------------
// 3. GitLab HTTPS without GitUserName — falls back to "oauth2"
// --------------------------------------------------------------------
func TestBuildEnv_HTTPSGitLab_NoGitUser(t *testing.T) {
	t.Cleanup(assertNoResidualAskpass(t))
	d := gitCredTestDB(t, "proj_gl_oauth", "https://gitlab.example.com/group/proj.git", "tok_gl_oauth")
	seedToken(t, d, "tok_gl_oauth", "gitlab", "glpat-xyz", "")

	env, cleanup, err := NewGitCredentialService(d).BuildEnv(context.Background(), "proj_gl_oauth")
	t.Cleanup(cleanup)
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}
	askpass := envExact(env, "GIT_ASKPASS")
	if askpass == "" {
		t.Fatalf("GIT_ASKPASS missing: %v", env)
	}
	out, runErr := execAskpass(t, askpass)
	if runErr != nil {
		t.Fatalf("askpass exec: %v", runErr)
	}
	// Without git_user_name the askpass answer is still just the token; the
	// "oauth2" username choice only changes the basic-auth userinfo split in
	// the remote URL — the askpass script stdout remains the token. We assert
	// it here for parity with the WithGitUser case.
	if out != "glpat-xyz" {
		t.Errorf("askpass output = %q, want %q", out, "glpat-xyz")
	}
}

// --------------------------------------------------------------------
// 4. SSH remote — skip askpass, return empty env + no-op cleanup
// --------------------------------------------------------------------
func TestBuildEnv_SSHRemote(t *testing.T) {
	t.Cleanup(assertNoResidualAskpass(t))
	d := gitCredTestDB(t, "proj_ssh", "git@github.com:owner/repo.git", "tok_ssh")
	seedToken(t, d, "tok_ssh", "github", "ghp-ssh-remote-token", "")

	called := false
	env, cleanup, err := NewGitCredentialService(d).BuildEnv(context.Background(), "proj_ssh")
	// Wrap cleanup so we can assert it's a no-op (no file to remove).
	wrapped := func() {
		called = true
		cleanup()
	}
	t.Cleanup(wrapped)
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}
	if len(env) != 0 {
		t.Errorf("SSH remote should not inject env, got: %v", env)
	}
	if envExact(env, "GIT_ASKPASS") != "" {
		t.Errorf("GIT_ASKPASS should be empty for SSH, got: %s", envExact(env, "GIT_ASKPASS"))
	}
	// Calling the cleanup is safe even though there's nothing to clean.
	wrapped()
	if !called {
		t.Error("cleanup not invoked")
	}
	// And no temp file should have been created at all.
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "nova-askpass-*.sh"))
	if len(matches) > 0 {
		t.Errorf("SSH path created askpass files: %v", matches)
	}
}

// --------------------------------------------------------------------
// 5. Project has no platform token bound — return empty env, no error
// --------------------------------------------------------------------
func TestBuildEnv_NoToken(t *testing.T) {
	t.Cleanup(assertNoResidualAskpass(t))
	// platformTokenID is "" — the project's platform_token_id column is
	// never set, so the JOIN yields NULL and lookupProjectToken returns
	// (project, nil, nil).
	d := gitCredTestDB(t, "proj_no_tok", "https://github.com/owner/repo.git", "")

	env, cleanup, err := NewGitCredentialService(d).BuildEnv(context.Background(), "proj_no_tok")
	t.Cleanup(cleanup)
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}
	if len(env) != 0 {
		t.Errorf("no-token project should yield empty env, got: %v", env)
	}
}

// --------------------------------------------------------------------
// 6. Project does not exist — return empty env, no panic, no error
// --------------------------------------------------------------------
func TestBuildEnv_ProjectLookupFails(t *testing.T) {
	t.Cleanup(assertNoResidualAskpass(t))
	// Seed an unrelated project so the DB is non-empty; we then look up a
	// different ID to hit the sql.ErrNoRows path.
	d := gitCredTestDB(t, "proj_real", "https://github.com/owner/repo.git", "")
	seedToken(t, d, "tok_real", "github", "ghp-real", "")

	env, cleanup, err := NewGitCredentialService(d).BuildEnv(context.Background(), "proj_missing")
	t.Cleanup(cleanup)
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}
	if len(env) != 0 {
		t.Errorf("missing project should yield empty env, got: %v", env)
	}
	// Also exercise the empty / whitespace projectID short-circuits.
	for _, pid := range []string{"", "   "} {
		env2, cleanup2, err2 := NewGitCredentialService(d).BuildEnv(context.Background(), pid)
		t.Cleanup(cleanup2)
		if err2 != nil {
			t.Errorf("BuildEnv(%q): %v", pid, err2)
		}
		if len(env2) != 0 {
			t.Errorf("BuildEnv(%q) should yield empty env, got: %v", pid, env2)
		}
	}
	// And a nil requirement through the convenience wrapper.
	env3, cleanup3, err3 := NewGitCredentialService(d).BuildEnvForRequirement(context.Background(), nil)
	t.Cleanup(cleanup3)
	if err3 != nil {
		t.Errorf("BuildEnvForRequirement(nil): %v", err3)
	}
	if len(env3) != 0 {
		t.Errorf("BuildEnvForRequirement(nil) should yield empty env, got: %v", env3)
	}
	// And BuildEnvForRequirement with a real requirement (covers the
	// reqRow.ProjectID → BuildEnv path).
	req := &model.Requirement{ProjectID: "proj_missing"}
	env4, cleanup4, err4 := NewGitCredentialService(d).BuildEnvForRequirement(context.Background(), req)
	t.Cleanup(cleanup4)
	if err4 != nil {
		t.Errorf("BuildEnvForRequirement: %v", err4)
	}
	if len(env4) != 0 {
		t.Errorf("BuildEnvForRequirement missing project should yield empty env, got: %v", env4)
	}
}

// --------------------------------------------------------------------
// 7. Cleanup removes the script
// --------------------------------------------------------------------
func TestBuildEnv_AskpassScriptCleanup(t *testing.T) {
	t.Cleanup(assertNoResidualAskpass(t))
	d := gitCredTestDB(t, "proj_clean", "https://github.com/owner/repo.git", "tok_clean")
	seedToken(t, d, "tok_clean", "github", "ghp-cleanup-test", "")

	env, cleanup, err := NewGitCredentialService(d).BuildEnv(context.Background(), "proj_clean")
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}
	if len(env) == 0 {
		t.Fatal("expected askpass env")
	}
	scriptPath := envExact(env, "GIT_ASKPASS")
	if scriptPath == "" {
		t.Fatal("GIT_ASKPASS missing")
	}
	// Script exists before cleanup.
	if _, statErr := os.Stat(scriptPath); statErr != nil {
		t.Fatalf("askpass script missing before cleanup: %v", statErr)
	}
	// Cleanup removes it.
	cleanup()
	if _, statErr := os.Stat(scriptPath); !os.IsNotExist(statErr) {
		t.Errorf("askpass script not removed after cleanup: stat err = %v", statErr)
	}
	// Cleanup is idempotent — second call must not panic or surface a real error.
	cleanup()
}

// --------------------------------------------------------------------
// 8. Token with shell metacharacters is escaped correctly
// --------------------------------------------------------------------
func TestBuildEnv_TokenWithSpecialChars(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh-based askpass not portable to windows")
	}
	t.Cleanup(assertNoResidualAskpass(t))
	dangerous := "tok'$WITH`BACK\\SLASH\"and-quote"
	d := gitCredTestDB(t, "proj_meta", "https://gitlab.example.com/group/proj.git", "tok_meta")
	seedToken(t, d, "tok_meta", "gitlab", dangerous, "")

	env, cleanup, err := NewGitCredentialService(d).BuildEnv(context.Background(), "proj_meta")
	t.Cleanup(cleanup)
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}
	scriptPath := envExact(env, "GIT_ASKPASS")
	if scriptPath == "" {
		t.Fatal("GIT_ASKPASS missing")
	}
	// Run the script and assert the raw token survives intact. A break in
	// the single-quote escaping would either truncate the token, run
	// $WITH / `BACK` as command substitution, or fail to run at all.
	out, runErr := execAskpass(t, scriptPath)
	if runErr != nil {
		t.Fatalf("askpass exec: %v (stdout=%q)", runErr, out)
	}
	if out != dangerous {
		t.Errorf("askpass output = %q, want %q (escaping broken?)", out, dangerous)
	}
}

// execAskpass runs the askpass script via `sh <path>` and returns its stdout.
// We don't exec the file directly so a missing +x bit (test environment
// quirk on some hosts) doesn't mask an escaping regression.
func execAskpass(t *testing.T, scriptPath string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", scriptPath)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
