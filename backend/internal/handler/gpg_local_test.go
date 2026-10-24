package handler

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestVerifyImportedGPGKey_HappyPath spins up a brand-new gpg key in a
// throwaway temp dir, exports the ASCII-armored private block, and feeds
// that block to verifyImportedGPGKey. We expect the same key id / fpr /
// uid to come back (modulo the `--quick-generate-key` UID string gpg
// normalises to `Real Name <email>`).
//
// Skipped when gpg is not on PATH — the CI box without gnupg would
// otherwise produce a permanent "no gpg" failure rather than a clean
// skip that the operator can spot at a glance.
func TestVerifyImportedGPGKey_HappyPath(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not installed on this host; skipping verifyImportedGPGKey integration test")
	}

	// Build a temporary GNUPGHOME for the setup side. We reuse the
	// buildGPGGenerateScript helper so the test exercises exactly the
	// script body we ship in production.
	setupHome := t.TempDir()
	if err := os.Chmod(setupHome, 0700); err != nil {
		t.Fatalf("chmod setupHome: %v", err)
	}
	setupScript := buildGPGGenerateScript(setupHome, "Verify Test <verify@example.com>")
	t.Logf("script:\n%s", setupScript)
	// Show what env we have for diagnostic
	for _, kv := range os.Environ() {
		if len(kv) > 4 && (kv[:4] == "GPG_" || kv[:4] == "GNUP") {
			t.Logf("INHERITED ENV: %s", kv)
		}
	}
	cmd := exec.Command("/bin/sh", "-c", setupScript)
	// Strip any inherited GNUPGHOME so the new keyring really is fresh.
	cmd.Env = append(filteredEnv("GNUPGHOME"), "GNUPGHOME="+setupHome)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// A failure to even reach a running gpg-agent is an environment
		// problem, not a regression in verifyImportedGPGKey. The common one
		// is macOS: gpg's agent socket lives under the (already long) temp
		// path, and the OS rejects it with "File name too long" before any
		// of our code runs. Treat that as a skip so the suite stays
		// meaningful on every host instead of permanently red on macOS.
		if strings.Contains(string(out), "can't connect to the gpg-agent") ||
			strings.Contains(string(out), "File name too long") ||
			strings.Contains(string(out), "未运行代理") {
			t.Skipf("gpg-agent cannot start in this environment (socket path issue): %v\n%s", err, out)
		}
		t.Fatalf("setup --gen-key failed: %v\n%s", err, out)
	}
	setupKeyID, setupFPR, setupUID := parseGPGKeyInfoFromScriptOutput(string(out))
	if setupKeyID == "" {
		t.Fatalf("setup script did not emit NOVA_GPG_KEYID; output:\n%s", out)
	}

	// Read the armored private block the setup produced.
	armored, err := os.ReadFile(setupHome + "/private.asc")
	if err != nil {
		t.Fatalf("read private.asc: %v", err)
	}
	if len(armored) == 0 {
		t.Fatalf("private.asc is empty; quick-generate-key produced no key?")
	}

	// Now feed the armored block into verifyImportedGPGKey. We expect
	// the SAME key id / fpr / uid to come back.
	gotKeyID, gotFPR, gotUID, err := verifyImportedGPGKey(string(armored), "")
	if err != nil {
		t.Fatalf("verifyImportedGPGKey returned error: %v", err)
	}
	if gotKeyID != setupKeyID {
		t.Errorf("key id round-trip mismatch: got %q, want %q", gotKeyID, setupKeyID)
	}
	if gotFPR != setupFPR {
		t.Errorf("fingerprint round-trip mismatch: got %q, want %q", gotFPR, setupFPR)
	}
	if gotUID != setupUID {
		t.Errorf("UID round-trip mismatch: got %q, want %q", gotUID, setupUID)
	}

	// Sanity: key id must be 16 hex chars; fpr must be 40 hex chars.
	if !regexp.MustCompile(`^[0-9A-F]{16}$`).MatchString(gotKeyID) {
		t.Errorf("key id does not look like 16 uppercase hex: %q", gotKeyID)
	}
	if !regexp.MustCompile(`^[0-9A-F]{40}$`).MatchString(gotFPR) {
		t.Errorf("fingerprint does not look like 40 uppercase hex: %q", gotFPR)
	}
}

// TestVerifyImportedGPGKey_CorruptedBlock ensures that an armored block
// with junk in the body (simulating a paste mishap) makes
// verifyImportedGPGKey return a non-nil error rather than silently
// parsing junk.
func TestVerifyImportedGPGKey_CorruptedBlock(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not installed on this host; skipping verifyImportedGPGKey corruption test")
	}

	// Feed a fake-but-recognisable armored block with the BEGIN/END
	// markers but junk body. gpg's --import should refuse.
	bogus := "-----BEGIN PGP PRIVATE KEY BLOCK-----\n" +
		"this is not a real base64 pgp packet\n" +
		"=\n" +
		"-----END PGP PRIVATE KEY BLOCK-----\n"
	_, _, _, err := verifyImportedGPGKey(bogus, "")
	if err == nil {
		t.Fatalf("verifyImportedGPGKey accepted a bogus armored block")
	}
}

// filteredEnv returns os.Environ() with every entry whose KEY matches
// dropKey removed (case-sensitive). Used by integration tests to scrub
// an inherited GNUPGHOME / GPG_AGENT_INFO that would otherwise steer
// gpg at the developer's interactive keyring.
func filteredEnv(dropKey string) []string {
	out := make([]string, 0, 32)
	prefix := dropKey + "="
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// gpgWorktreeConfigGet reads one key at one scope of a worktree, via the
// same argv git would use. A non-nil error means the key is absent at that
// scope — which is the assertion most of these tests make.
func gpgWorktreeConfigGet(t *testing.T, wt, scope, key string) (string, error) {
	t.Helper()
	out, err := exec.Command("git", "-C", wt, "config", scope, "--get", key).Output()
	return strings.TrimSpace(string(out)), err
}

// gpgWriteStateFile lays down one of the teardown state files the provision
// script would have written. An empty value means "the key had no value at
// this scope before we wrote it", which the script expresses by NOT creating
// the .value file — so we mirror that by not creating it either.
func gpgWriteStateFile(t *testing.T, stateDir, key, scope, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(stateDir, key+".scope"), []byte(scope+"\n"), 0o600); err != nil {
		t.Fatalf("write %s.scope: %v", key, err)
	}
	if value == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(stateDir, key+".value"), []byte(value), 0o600); err != nil {
		t.Fatalf("write %s.value: %v", key, err)
	}
}

// newGPGWorktreeFixture returns a real LINKED worktree. That shape matters:
// it is the only one where `git config --worktree` lands in
// .git/worktrees/<id>/config.worktree instead of being rejected, so a plain
// checkout would exercise a different file and could pass even while the
// restore wrote to the wrong scope.
func newGPGWorktreeFixture(t *testing.T) (work, wt string) {
	t.Helper()
	work = newRefsFixture(t)
	wt = filepath.Join(filepath.Dir(work), "wt")
	gitIn(t, work, "config", "extensions.worktreeConfig", "true")
	gitIn(t, work, "worktree", "add", "-b", "feat/x", wt)
	return work, wt
}

// TestRestoreLocalGPGConfig is the teardown's contract: put back the values
// the provision script overwrote, unset the ones that had no value before,
// and — the guarantee that matters most — leave strictly alone any key the
// script never recorded a scope for. A wrong unset throws away the user's
// own git configuration, which is worse than the litter being cleaned up.
func TestRestoreLocalGPGConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("git config --worktree semantics under test are POSIX-only")
	}
	_, wt := newGPGWorktreeFixture(t)
	stateDir := t.TempDir()

	// What the provision script WOULD have done...
	gitIn(t, wt, "config", "--worktree", "commit.gpgsign", "true")
	gitIn(t, wt, "config", "--worktree", "user.name", "Nova Dev")
	gitIn(t, wt, "config", "--worktree", "gpg.program", "/tmp/gone/git-gpg-wrapper")
	gitIn(t, wt, "config", "--worktree", "user.email", "untouched@example.com")
	// ...including one write that fell back to --local, which lands in the
	// SHARED .git/config rather than the per-worktree file.
	gitIn(t, wt, "config", "--local", "tag.gpgsign", "true")

	// ...and the state it recorded alongside.
	gpgWriteStateFile(t, stateDir, "commit.gpgsign", "worktree", "false") // had a prior value
	gpgWriteStateFile(t, stateDir, "user.name", "worktree", "")           // had none
	gpgWriteStateFile(t, stateDir, "gpg.program", "worktree", "")         // had none
	gpgWriteStateFile(t, stateDir, "tag.gpgsign", "local", "")            // had none at --local
	// user.email gets no state file at all: the script never recorded a
	// scope for it, so it must survive untouched.

	restoreLocalGPGConfig(wt, stateDir)

	if v, err := gpgWorktreeConfigGet(t, wt, "--worktree", "commit.gpgsign"); err != nil || v != "false" {
		t.Errorf("commit.gpgsign had prior value \"false\": want it restored, got %q (err=%v)", v, err)
	}
	for _, key := range []string{"user.name", "gpg.program"} {
		if v, err := gpgWorktreeConfigGet(t, wt, "--worktree", key); err == nil {
			t.Errorf("%s had no prior value: want it unset, still present as %q", key, v)
		}
	}
	if v, err := gpgWorktreeConfigGet(t, wt, "--local", "tag.gpgsign"); err == nil {
		t.Errorf("tag.gpgsign had no prior value at --local: want it unset, still present as %q", v)
	}
	if v, err := gpgWorktreeConfigGet(t, wt, "--worktree", "user.email"); err != nil || v != "untouched@example.com" {
		t.Errorf("user.email has no .scope file and must be left alone; got %q (err=%v)", v, err)
	}

	// Idempotent: the cleanup closure is guarded by sync.Once, but a second
	// direct call must not undo the restore either.
	restoreLocalGPGConfig(wt, stateDir)
	if v, err := gpgWorktreeConfigGet(t, wt, "--worktree", "commit.gpgsign"); err != nil || v != "false" {
		t.Errorf("second restore must be idempotent; commit.gpgsign=%q (err=%v)", v, err)
	}
}

// TestRestoreLocalGPGConfig_ValueIsDataNotCode feeds the restorer a prior
// value full of shell metacharacters. The value travels from a file into a
// single argv element — never through a shell — so it must come back byte
// for byte, and the `$(...)` inside it must NOT have executed. Re-quoting a
// captured value into shell syntax at teardown time is exactly the injection
// this design avoids, and this is the test that would catch a regression.
func TestRestoreLocalGPGConfig_ValueIsDataNotCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("git config --worktree semantics under test are POSIX-only")
	}
	_, wt := newGPGWorktreeFixture(t)
	stateDir := t.TempDir()

	canary := filepath.Join(t.TempDir(), "pwned")
	nasty := "$(touch " + canary + ") `id` \"double\" 'single'"

	gitIn(t, wt, "config", "--worktree", "user.name", "placeholder")
	gpgWriteStateFile(t, stateDir, "user.name", "worktree", nasty)

	restoreLocalGPGConfig(wt, stateDir)

	if _, err := os.Stat(canary); err == nil {
		t.Fatalf("captured value was executed as shell code (canary %s exists)", canary)
	}
	got, err := gpgWorktreeConfigGet(t, wt, "--worktree", "user.name")
	if err != nil {
		t.Fatalf("user.name should have been restored, got error: %v", err)
	}
	if got != nasty {
		t.Errorf("captured value must round-trip verbatim\n want: %q\n  got: %q", nasty, got)
	}
}

// TestRestoreLocalGPGConfig_MissingStateDir proves the teardown degrades to
// a no-op when capture never ran (provision failed before the first write,
// or the home was already cleaned) — it must not error, and must not go
// poking at git config on a guess.
func TestRestoreLocalGPGConfig_MissingStateDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("git config --worktree semantics under test are POSIX-only")
	}
	_, wt := newGPGWorktreeFixture(t)
	gitIn(t, wt, "config", "--worktree", "commit.gpgsign", "true")

	restoreLocalGPGConfig(wt, filepath.Join(t.TempDir(), "does-not-exist"))

	if v, err := gpgWorktreeConfigGet(t, wt, "--worktree", "commit.gpgsign"); err != nil || v != "true" {
		t.Errorf("no state dir means nothing to undo; commit.gpgsign=%q (err=%v)", v, err)
	}
}
