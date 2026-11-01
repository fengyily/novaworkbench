package handler

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ── fixture ────────────────────────────────────────────────────────────────

// noBranchRepo is a throwaway git project plus a bare "origin" used by the
// no-branch (develop-directly-on-base) tests.
//
// The project checkout sits on `main` and the bare origin receives a pushed
// `main` — that mirrors the real layout the no-branch mode has to cope with:
// the base branch is checked out in the project directory, so a worktree can
// never check it out again.
type noBranchRepo struct {
	projectDir string
	originBare string
	reqID      string
}

// newNoBranchRepo builds the fixture and points $HOME at a temp dir so the
// canonical worktree path (worktreeRoot → ~/.novaworkbench/worktrees/...)
// materialises inside the sandbox and cleans itself up.
func newNoBranchRepo(t *testing.T) *noBranchRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on this host; skipping no-branch worktree test")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LC_ALL", "C")
	t.Setenv("LANG", "C")

	r := &noBranchRepo{
		projectDir: t.TempDir(),
		originBare: t.TempDir(),
		reqID:      "req_test_no_branch",
	}
	r.git(t, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(r.projectDir, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.git(t, "add", "README.md")
	r.git(t, "commit", "-q", "-m", "seed commit")

	// The bare origin stands in for the remote — a local path, no network.
	cmd := exec.Command("git", "init", "-q", "--bare", "-b", "main")
	cmd.Dir = r.originBare
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	r.git(t, "remote", "add", "origin", r.originBare)
	r.git(t, "push", "-q", "origin", "main")
	r.git(t, "fetch", "-q", "origin")
	return r
}

// git runs a git command in the project checkout and fails the test on error.
func (r *noBranchRepo) git(t *testing.T, args ...string) string {
	t.Helper()
	out, err := runGitCtx(r.projectDir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// gitIn runs a git command in an arbitrary directory (the worktree) and
// fails the test on error.
func (r *noBranchRepo) gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGitCtx(dir, args...)
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

// writeFile writes a file relative to the project checkout.
func (r *noBranchRepo) writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.projectDir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// commitAll stages everything in dir and commits it.
func (r *noBranchRepo) commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	r.gitIn(t, dir, "add", "-A")
	r.gitIn(t, dir, "commit", "-q", "-m", msg)
}

// ── tests ──────────────────────────────────────────────────────────────────

// TestEnsureDetachedWorktree_CreatesDetachedAtBase pins the core of the
// no-branch mode: the requirement gets an isolated worktree whose HEAD is
// DETACHED on the base line, and the project checkout is left completely
// alone (still on its own branch, with the base branch NOT hijacked).
//
// The pre-fix behaviour was the opposite: the no-branch path checked the
// project directory out onto base and developed right there.
func TestEnsureDetachedWorktree_CreatesDetachedAtBase(t *testing.T) {
	r := newNoBranchRepo(t)

	wtPath, err := ensureDetachedWorktreeCore(r.projectDir, r.reqID, "main", nil)
	if err != nil {
		t.Fatalf("ensureDetachedWorktreeCore: %v", err)
	}
	if wtPath != WorktreePath(r.projectDir, r.reqID) {
		t.Fatalf("worktree path = %q, want the canonical %q", wtPath, WorktreePath(r.projectDir, r.reqID))
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree dir missing: %v", err)
	}

	// The worktree must be on a DETACHED HEAD.
	branch, err := gitRun(wtPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse in worktree: %v", err)
	}
	if branch != "HEAD" {
		t.Fatalf("worktree HEAD = %q, want \"HEAD\" (detached)", branch)
	}

	// The worktree must sit exactly on the base commit.
	baseSHA, err := gitRun(r.projectDir, "rev-parse", "origin/main")
	if err != nil {
		t.Fatalf("rev-parse origin/main: %v", err)
	}
	wtSHA, err := gitRun(wtPath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if wtSHA != baseSHA {
		t.Fatalf("worktree HEAD sha = %q, want the base sha %q", wtSHA, baseSHA)
	}

	// The project checkout must be untouched.
	projBranch, err := gitRun(r.projectDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse in project: %v", err)
	}
	if projBranch != "main" {
		t.Fatalf("project HEAD = %q, want \"main\" — the project checkout must not be touched", projBranch)
	}

	// No branch may have been created for the requirement.
	branches, err := gitRun(r.projectDir, "branch", "--list")
	if err != nil {
		t.Fatalf("branch list: %v", err)
	}
	if strings.Contains(branches, r.reqID) {
		t.Fatalf("a branch was created for the requirement (must create none):\n%s", branches)
	}
}

// TestEnsureDetachedWorktree_ConvertsRegisteredBranchWorktree covers the
// hand-over from the design stage: the design stage may already have created
// a branch-shaped worktree (feat/<reqID>). The no-branch mode must detach
// that worktree in place — the branch stops being checked out, but the
// worktree itself is reused (no rebuild, no data loss).
func TestEnsureDetachedWorktree_ConvertsRegisteredBranchWorktree(t *testing.T) {
	r := newNoBranchRepo(t)
	wtPath := WorktreePath(r.projectDir, r.reqID)
	branch := "feat/" + r.reqID

	// Simulate the design stage's branch-shaped worktree.
	if out, err := runGitCtx(r.projectDir, "worktree", "add", "-b", branch, wtPath, "main"); err != nil {
		t.Fatalf("worktree add -b: %v\n%s", err, out)
	}

	got, err := ensureDetachedWorktreeCore(r.projectDir, r.reqID, "main", nil)
	if err != nil {
		t.Fatalf("ensureDetachedWorktreeCore: %v", err)
	}
	if got != wtPath {
		t.Fatalf("worktree path = %q, want the existing %q (must be reused, not rebuilt)", got, wtPath)
	}

	// The worktree is detached now.
	cur, err := gitRun(wtPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse in worktree: %v", err)
	}
	if cur != "HEAD" {
		t.Fatalf("worktree HEAD = %q, want \"HEAD\" (detached)", cur)
	}

	// The registration must report a detached worktree, and the feature
	// branch must not be checked out anywhere any more.
	porcelain, err := gitRun(r.projectDir, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	if !strings.Contains(porcelain, "detached") {
		t.Fatalf("expected a detached worktree in `worktree list --porcelain`:\n%s", porcelain)
	}
	if strings.Contains(porcelain, "branch refs/heads/"+branch) {
		t.Fatalf("%s is still checked out by a worktree:\n%s", branch, porcelain)
	}
}

// TestEnsureDetachedWorktree_PreservesLocalCommits is the safety guard: a
// worktree that already carries local commits must keep them. The conversion
// detaches IN PLACE and only attempts a fast-forward onto the base line —
// when the base moved on, the fast-forward fails and development continues on
// the current HEAD.
func TestEnsureDetachedWorktree_PreservesLocalCommits(t *testing.T) {
	r := newNoBranchRepo(t)

	wtPath, err := ensureDetachedWorktreeCore(r.projectDir, r.reqID, "main", nil)
	if err != nil {
		t.Fatalf("ensureDetachedWorktreeCore: %v", err)
	}

	// A local commit inside the worktree.
	r.writeFileIn(t, wtPath, "feature.txt", "local work\n")
	r.commitAll(t, wtPath, "local work")
	localSHA, err := gitRun(wtPath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	// The base branch moves on (someone else pushed) — the fast-forward onto
	// origin/main can no longer succeed.
	r.writeFile(t, "upstream.txt", "upstream\n")
	r.commitAll(t, r.projectDir, "upstream work")
	r.git(t, "push", "-q", "origin", "main")
	r.git(t, "fetch", "-q", "origin")

	// Re-running the conversion must not discard the local commit.
	wtPath2, err := ensureDetachedWorktreeCore(r.projectDir, r.reqID, "main", nil)
	if err != nil {
		t.Fatalf("ensureDetachedWorktreeCore (2nd): %v", err)
	}
	head, err := gitRun(wtPath2, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD after conversion: %v", err)
	}
	if head != localSHA {
		t.Fatalf("worktree HEAD = %q, want the local commit %q — local work must never be discarded", head, localSHA)
	}
	if _, err := gitRun(wtPath2, "cat-file", "-e", localSHA+"^{commit}"); err != nil {
		t.Fatalf("local commit %s is no longer reachable: %v", localSHA, err)
	}
}

// TestEnsureDetachedWorktree_DirtyTreeKeepsWork guards the other side of the
// same safety rule: an uncommitted working tree must survive the conversion
// untouched (no error, no content change).
func TestEnsureDetachedWorktree_DirtyTreeKeepsWork(t *testing.T) {
	r := newNoBranchRepo(t)

	wtPath, err := ensureDetachedWorktreeCore(r.projectDir, r.reqID, "main", nil)
	if err != nil {
		t.Fatalf("ensureDetachedWorktreeCore: %v", err)
	}

	// An uncommitted edit.
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wtPath2, err := ensureDetachedWorktreeCore(r.projectDir, r.reqID, "main", nil)
	if err != nil {
		t.Fatalf("ensureDetachedWorktreeCore (2nd) on a dirty tree: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(wtPath2, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if string(content) != "dirty\n" {
		t.Fatalf("README.md = %q, want %q — the dirty working tree must be preserved", content, "dirty\n")
	}
}

// TestDetachedPushToBase_CreatesNoRemoteBranch is the Q2 acceptance test at
// the git level: pushing a detached HEAD onto the base branch advances
// refs/heads/<base> and creates NOTHING else — no feature branch, no local
// branch, no remote branch.
func TestDetachedPushToBase_CreatesNoRemoteBranch(t *testing.T) {
	r := newNoBranchRepo(t)

	wtPath, err := ensureDetachedWorktreeCore(r.projectDir, r.reqID, "main", nil)
	if err != nil {
		t.Fatalf("ensureDetachedWorktreeCore: %v", err)
	}
	r.writeFileIn(t, wtPath, "feature.txt", "no branch work\n")
	r.commitAll(t, wtPath, "no branch work")
	newSHA, err := gitRun(wtPath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	// Push exactly the way the production path does.
	r.gitIn(t, wtPath, pushBaseRefspecArgs("main")...)

	// The remote must carry the base branch and nothing else.
	refs, err := gitRun(r.originBare, "for-each-ref", "--format=%(refname)", "refs/heads")
	if err != nil {
		t.Fatalf("for-each-ref on origin: %v", err)
	}
	got := strings.Fields(refs)
	if len(got) != 1 || got[0] != "refs/heads/main" {
		t.Fatalf("origin refs/heads = %v, want exactly [refs/heads/main]", got)
	}

	// And the base branch must point at the new commit.
	remoteSHA, err := gitRun(r.originBare, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse refs/heads/main: %v", err)
	}
	if remoteSHA != newSHA {
		t.Fatalf("origin/main = %q, want the new commit %q", remoteSHA, newSHA)
	}
}

// TestDetachedPushNonFastForward_ThenRebaseSucceeds covers the concurrent
// case: another requirement pushed to base in the meantime, so the plain push
// is rejected as non-fast-forward. The recovery path is fetch + rebase (never
// a force push), after which the push succeeds.
func TestDetachedPushNonFastForward_ThenRebaseSucceeds(t *testing.T) {
	r := newNoBranchRepo(t)

	wtPath, err := ensureDetachedWorktreeCore(r.projectDir, r.reqID, "main", nil)
	if err != nil {
		t.Fatalf("ensureDetachedWorktreeCore: %v", err)
	}
	r.writeFileIn(t, wtPath, "feature.txt", "local work\n")
	r.commitAll(t, wtPath, "local work")

	// Somebody else advances the base branch.
	r.writeFile(t, "upstream.txt", "upstream\n")
	r.commitAll(t, r.projectDir, "upstream work")
	r.git(t, "push", "-q", "origin", "main")

	// The plain push must be rejected (non fast-forward).
	if out, err := runGitCtx(wtPath, "push", "origin", "HEAD:refs/heads/main"); err == nil {
		t.Fatalf("expected a non-fast-forward push to fail; it succeeded:\n%s", out)
	}

	// fetch + rebase → push succeeds.
	r.gitIn(t, wtPath, "fetch", "origin", "main")
	r.gitIn(t, wtPath, "rebase", "origin/main")
	r.gitIn(t, wtPath, "push", "origin", "HEAD:refs/heads/main")

	remoteSHA, err := gitRun(r.originBare, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse refs/heads/main: %v", err)
	}
	wtSHA, err := gitRun(wtPath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if remoteSHA != wtSHA {
		t.Fatalf("origin/main = %q, want the rebased HEAD %q", remoteSHA, wtSHA)
	}
}

// TestDetachedRebaseConflict_AbortsCleanly pins the conflict path: the
// rebase fails, the recovery aborts it, and the worktree is left in a usable
// state (no rebase in progress) with the local work still reachable.
func TestDetachedRebaseConflict_AbortsCleanly(t *testing.T) {
	r := newNoBranchRepo(t)

	wtPath, err := ensureDetachedWorktreeCore(r.projectDir, r.reqID, "main", nil)
	if err != nil {
		t.Fatalf("ensureDetachedWorktreeCore: %v", err)
	}

	// A local commit that touches README.md …
	r.writeFileIn(t, wtPath, "README.md", "local change\n")
	r.commitAll(t, wtPath, "local change")
	localSHA, err := gitRun(wtPath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	// … and an upstream commit that touches the same file.
	r.writeFile(t, "README.md", "upstream change\n")
	r.commitAll(t, r.projectDir, "upstream change")
	r.git(t, "push", "-q", "origin", "main")
	r.gitIn(t, wtPath, "fetch", "origin", "main")

	// The rebase must conflict.
	if out, err := runGitCtx(wtPath, "rebase", "origin/main"); err == nil {
		t.Fatalf("expected the rebase to conflict; it succeeded:\n%s", out)
	}

	// The recovery aborts it.
	r.gitIn(t, wtPath, "rebase", "--abort")

	// No rebase state may be left behind.
	gitDir, err := gitRun(wtPath, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatalf("rev-parse --absolute-git-dir: %v", err)
	}
	for _, marker := range []string{"rebase-merge", "rebase-apply"} {
		if _, statErr := os.Stat(filepath.Join(gitDir, marker)); statErr == nil {
			t.Fatalf("%s still present after `rebase --abort` — the worktree would be unusable", marker)
		}
	}

	// The local commit must still be there.
	head, err := gitRun(wtPath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD after abort: %v", err)
	}
	if head != localSHA {
		t.Fatalf("worktree HEAD = %q, want the local commit %q", head, localSHA)
	}
}

// writeFileIn writes a file relative to an arbitrary directory (the worktree).
func (r *noBranchRepo) writeFileIn(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
