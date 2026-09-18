package handler

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureWorktreeFrom_StaleBranchRecovery covers the regression introduced
// by req_b646601dbc5e7ac3's scheduled task: a persisted worktree existed
// (registered in the project's .git/worktrees/), but the target branch
// refs/heads/feat/<reqID> was missing locally because the ~/.novaworkbench
// /worktrees/ tree was carried over from another host clone. Pre-fix
// ensureWorktreeFrom issued `git checkout feat/<id>` and bubbled up
// "路径规格 'feat/<id>' 未匹配任何 Git 已知文件"; post-fix it must drop the
// stale registration and recreate the worktree off origin/<base>.
//
// The test wires the same flow EnsureWorktreeLogged drives — syncBaseBranch
// + worktree add — by redirecting $HOME so worktreeRoot(...) materializes
// inside the test sandbox, then asserts the recovery path runs.
func TestEnsureWorktreeFrom_StaleBranchRecovery(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on this host; skipping worktree recovery test")
	}

	// Redirect $HOME so worktreeRoot(...) lands inside t.TempDir() and the
	// test cleans itself up. os.UserHomeDir() on Unix reads $HOME first.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	// ensureWorktreeFrom's strategy order greps for the English string
	// "already exists" to detect the "branch already created locally"
	// fall-through case; force C locale so that match holds regardless of
	// the test host's default UI language.
	t.Setenv("LC_ALL", "C")
	t.Setenv("LANG", "C")

	// 1. Build a throwaway git repo with a base branch ("main") and one
	//    commit we can branch off.
	projectDir := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		full := append([]string{"-C", projectDir}, args...)
		cmd := exec.Command("git", full...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %s: %v\nstdout=%s\nstderr=%s",
				strings.Join(args, " "), err, stdout.String(), stderr.String())
		}
		return strings.TrimSpace(stdout.String())
	}
	runGit("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(projectDir, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "README.md")
	runGit("commit", "-q", "-m", "seed commit")

	// 2. Add a remote so syncBaseBranch has something to fetch from, then
	//    create a stub bare repo on the local filesystem as "origin" so the
	//    fetch succeeds in a sandboxed test (no network).
	originBare := t.TempDir()
	runGitBare := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = originBare
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("git (bare) %s: %v\nstderr=%s",
				strings.Join(args, " "), err, stderr.String())
		}
	}
	runGitBare("init", "-q", "--bare", "-b", "main")
	runGit("remote", "add", "origin", originBare)
	runGit("push", "-q", "origin", "main")
	runGit("fetch", "-q", "origin")

	const reqID = "req_test_stale_branch_recovery"
	branch := "feat/" + reqID

	// 3. First call: register the worktree normally. This simulates a
	//    previous run on another host having created feat/<id>.
	wtPath, err := EnsureWorktreeLogged(projectDir, reqID, branch, "main", nil)
	if err != nil {
		t.Fatalf("first EnsureWorktreeLogged failed: %v", err)
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree %s not on disk after first call: %v", wtPath, err)
	}

	// 4. Simulate the host-switch loss: drop the local refs/heads/<branch>
	//    so the worktree's HEAD no longer resolves, AND detach the
	//    worktree's HEAD so currentBranch(wtPath) returns something
	//    other than feat/<id>. Crucially we KEEP the
	//    .git/worktrees/<id>/ registration so worktreeRegistered stays
	//    true — that's the precondition that makes the pre-fix code emit
	//    the "checkout ... pathspec did not match" error.
	cmd := exec.Command("git", "-C", wtPath, "checkout", "-q", "--detach")
	if err := cmd.Run(); err != nil {
		t.Fatalf("detach wt head: %v", err)
	}
	// Delete refs/heads/<branch> from the project so the recovery
	// detection can fire. We must do this from a non-worktree checkout
	// (the project dir itself) since the worktree is no longer on
	// feat/<id>.
	runGit("branch", "-D", branch)
	if _, err := runGitCtx(projectDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		t.Fatalf("branch %s still exists after delete", branch)
	}

	// Verify the .git/worktrees/<id>/ registration still says wtPath is
	// registered — that's the precondition the recovery path relies on.
	out, err := runGitCtx(projectDir, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	if !strings.Contains(out, wtPath) {
		t.Fatalf("expected worktree %s in registration before recovery, got:\n%s", wtPath, out)
	}

	// 5. Now call EnsureWorktreeLogged again. Pre-fix this would error
	//    out with "checkout feat/<reqID> in worktree: 路径规格未匹配".
	//    Post-fix it must drop the stale registration, recreate the
	//    worktree off origin/main, and return a valid wtPath.
	//
	// First, sanity-check the failure preconditions so we know what
	// ensureWorktreeFrom has to work with:
	//   - git checkout feat/<id> should fail (branch doesn't exist),
	//   - git rev-parse refs/heads/feat/<id> should fail (same reason).
	if _, err := runGitCtx(wtPath, "checkout", branch); err == nil {
		t.Fatalf("expected git checkout %s to fail (branch should be deleted); it succeeded", branch)
	}
	if _, err := runGitCtx(projectDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		t.Fatalf("expected refs/heads/%s to be absent", branch)
	}

	var logLines []string
	wtPath2, err := EnsureWorktreeLogged(projectDir, reqID, branch, "main", func(s string) {
		logLines = append(logLines, s)
	})
	if err != nil {
		t.Fatalf("recovery EnsureWorktreeLogged failed: %v\nlog: %v", err, logLines)
	}
	if wtPath2 != wtPath {
		t.Fatalf("expected wtPath to be reused (%q), got %q", wtPath, wtPath2)
	}
	if _, err := os.Stat(wtPath2); err != nil {
		t.Fatalf("recovered worktree dir missing: %v", err)
	}
	// Inside the worktree, the branch should now be checked out.
	gotBranch, err := gitRun(wtPath2, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse in recovered worktree: %v", err)
	}
	if gotBranch != branch {
		t.Fatalf("worktree branch = %q, want %q", gotBranch, branch)
	}
	// The recovery log line must surface so the SSE panel tells the user
	// why a fresh worktree was created.
	foundRecoveryHint := false
	for _, line := range logLines {
		if strings.Contains(line, "在本地不存在") && strings.Contains(line, "重建 worktree") {
			foundRecoveryHint = true
			break
		}
	}
	if !foundRecoveryHint {
		t.Fatalf("expected recovery hint in log lines, got %v", logLines)
	}
}

// TestEnsureWorktreeFrom_DirtyCheckoutStillFails pins the non-recovery side of
// the fix: when the branch DOES exist locally but the worktree's working
// tree differs in a way that blocks `git checkout`, the function must still
// return the underlying checkout error (not silently drop the worktree).
// This guards against an over-eager fallback that would discard user work.
//
// We set up the dirty conflict by branching feat/<id> off an OLDER commit
// (the seed commit) and advancing main to a NEWER commit; the worktree sits
// on the NEWER commit so `git checkout feat/<id>` has to overwrite README.md.
func TestEnsureWorktreeFrom_DirtyCheckoutStillFails(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on this host; skipping dirty-checkout test")
	}
	t.Setenv("HOME", t.TempDir())
	// ensureWorktreeFrom's strategy order greps for the English string
	// "already exists" to detect the "branch already created locally"
	// fall-through case; force C locale so that match holds regardless of
	// the test host's default UI language.
	t.Setenv("LC_ALL", "C")
	t.Setenv("LANG", "C")

	projectDir := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		full := append([]string{"-C", projectDir}, args...)
		cmd := exec.Command("git", full...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %s: %v\nstderr=%s", strings.Join(args, " "), err, stderr.String())
		}
		return strings.TrimSpace(stdout.String())
	}
	runGit("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(projectDir, "README.md"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "README.md")
	runGit("commit", "-q", "-m", "v1")

	// Capture the seed commit so feat/<id> can branch off it (one commit
	// behind main).
	seedSHA := runGit("rev-parse", "HEAD")
	const reqID = "req_test_dirty_checkout"
	branch := "feat/" + reqID

	// Advance main so the worktree (which we'll register off main HEAD)
	// sits on a different commit than feat/<id>.
	if err := os.WriteFile(filepath.Join(projectDir, "README.md"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "README.md")
	runGit("commit", "-q", "-m", "v2")

	// Pre-create feat/<id> at the seed (v1) commit so the recovery path's
	// rev-parse check sees the branch as present locally.
	runGit("branch", branch, seedSHA)

	wtPath, err := EnsureWorktreeLogged(projectDir, reqID, branch, "main", nil)
	if err != nil {
		t.Fatalf("seed EnsureWorktreeLogged failed: %v", err)
	}

	// Sanity: the worktree should now be on feat/<id>. If not, the test
	// setup is broken.
	gotRev, err := gitRun(wtPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse in wt: %v", err)
	}
	if gotRev != branch {
		t.Fatalf("worktree current branch = %q, want %q", gotRev, branch)
	}

	// Detach HEAD at main HEAD (one commit ahead of feat/<id>) so the
	// working tree picks up v2's README.md. The subsequent dirty edit
	// then differs from BOTH branches' file content (v1 on feat/<id>, v2
	// on the detached HEAD), so `git checkout feat/<id>` MUST fail with
	// "your local changes would be overwritten by checkout" — not a
	// silent no-op when source and target agree on the file content.
	cmd := exec.Command("git", "-C", wtPath, "checkout", "-q", "--detach", "main")
	if err := cmd.Run(); err != nil {
		t.Fatalf("detach wt head at main: %v", err)
	}
	// Edit README.md in the worktree so checkout feat/<id> (v1) has to
	// overwrite the working tree's current "v2" → dirty collision.
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Now EnsureWorktreeLogged should:
	//   1. see worktreeRegistered=true,
	//   2. currentBranch=HEAD (detached) != branch,
	//   3. attempt `git checkout feat/<id>` which has to overwrite README
	//      (v2 → v1) and would clobber a dirty edit → fail,
	//   4. detect refs/heads/feat/<id> STILL exists locally (we created
	//      it explicitly) → return the wrapped checkout error WITHOUT
	//      removing the worktree.
	_, err = EnsureWorktreeLogged(projectDir, reqID, branch, "main", nil)
	if err == nil {
		t.Fatalf("expected non-nil error from dirty checkout; got nil (wtPath=%s)", wtPath)
	}
	if !strings.Contains(err.Error(), "checkout") || !strings.Contains(err.Error(), "worktree") {
		t.Fatalf("expected wrapped checkout error, got %v", err)
	}
	// The branch exists locally (we created it explicitly) so the
	// recovery path must NOT have removed the worktree.
	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Fatalf("worktree must NOT be removed when branch exists locally; stat=%v", statErr)
	}
}

// runGitCtx runs `git <args>` in the given dir and returns trimmed stdout.
// Errors are returned rather than fataling so tests can probe for
// "expected-to-fail" calls like `git rev-parse --verify` on a missing ref.
func runGitCtx(dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stdout.String()), err
	}
	return strings.TrimSpace(stdout.String()), nil
}