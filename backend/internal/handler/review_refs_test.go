package handler

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// resolveDiffRefs is the gate that lets a never-pushed branch be reviewed
// (R3). It touches real git, so the fixtures are real repos: a bare origin
// plus a clone, exactly like service/git_sync_test.go does.

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// newRefsFixture returns a work tree cloned from a bare origin that has one
// commit on `main`.
func newRefsFixture(t *testing.T) (work string) {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	gitIn(t, "", "init", "--bare", "--initial-branch=main", origin)

	seed := filepath.Join(root, "seed")
	gitIn(t, "", "clone", origin, seed)
	gitIn(t, seed, "config", "user.email", "test@nova")
	gitIn(t, seed, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	gitIn(t, seed, "add", "README.md")
	gitIn(t, seed, "commit", "-m", "init")
	gitIn(t, seed, "push", "origin", "HEAD:main")

	work = filepath.Join(root, "work")
	gitIn(t, "", "clone", origin, work)
	gitIn(t, work, "config", "user.email", "test@nova")
	gitIn(t, work, "config", "user.name", "Test")
	return work
}

// commitOnBranch creates <branch> off HEAD with one commit, leaving the
// repo back on main.
func commitOnBranch(t *testing.T, work, branch, file string) {
	t.Helper()
	gitIn(t, work, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(work, file), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	gitIn(t, work, "add", file)
	gitIn(t, work, "commit", "-m", "work on "+branch)
	gitIn(t, work, "checkout", "main")
}

// The headline case: a branch that exists only locally must still resolve,
// because that's the whole point of branch Review.
func TestResolveDiffRefs_LocalOnlyBranch(t *testing.T) {
	work := newRefsFixture(t)
	commitOnBranch(t, work, "feat/local", "a.txt")

	head, base, err := resolveDiffRefs(context.Background(), work, "feat/local", "main", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if head != "feat/local" {
		t.Fatalf("head = %q, want the local ref", head)
	}
	// base exists on both sides — origin wins.
	if base != "origin/main" {
		t.Fatalf("base = %q, want origin/main", base)
	}
}

// Pushed branch → origin/<x> is preferred over the local copy.
func TestResolveDiffRefs_PrefersOriginWhenPushed(t *testing.T) {
	work := newRefsFixture(t)
	commitOnBranch(t, work, "feat/pushed", "b.txt")
	gitIn(t, work, "push", "origin", "feat/pushed")

	head, base, err := resolveDiffRefs(context.Background(), work, "feat/pushed", "main", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if head != "origin/feat/pushed" {
		t.Fatalf("head = %q, want origin/feat/pushed", head)
	}
	if base != "origin/main" {
		t.Fatalf("base = %q", base)
	}
}

// Remote-only branch: delete the local copy, keep it on origin.
func TestResolveDiffRefs_RemoteOnlyBranch(t *testing.T) {
	work := newRefsFixture(t)
	commitOnBranch(t, work, "feat/remote", "c.txt")
	gitIn(t, work, "push", "origin", "feat/remote")
	gitIn(t, work, "branch", "-D", "feat/remote")

	head, _, err := resolveDiffRefs(context.Background(), work, "feat/remote", "main", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if head != "origin/feat/remote" {
		t.Fatalf("head = %q", head)
	}
}

// Neither side has it → an explicit error so the handler can 400 instead of
// letting claude run against an empty diff.
func TestResolveDiffRefs_MissingBranchErrors(t *testing.T) {
	work := newRefsFixture(t)

	if _, _, err := resolveDiffRefs(context.Background(), work, "feat/ghost", "main", nil); err == nil {
		t.Fatal("expected an error for a branch that exists nowhere")
	} else if !strings.Contains(err.Error(), "feat/ghost") {
		t.Fatalf("error should name the branch, got %v", err)
	}

	if _, _, err := resolveDiffRefs(context.Background(), work, "main", "nope", nil); err == nil {
		t.Fatal("expected an error for a missing base branch")
	}
}

// An already-qualified origin/<x> input resolves verbatim rather than
// becoming origin/origin/<x>.
func TestResolveDiffRefs_AcceptsQualifiedRef(t *testing.T) {
	work := newRefsFixture(t)
	commitOnBranch(t, work, "feat/q", "d.txt")
	gitIn(t, work, "push", "origin", "feat/q")

	head, _, err := resolveDiffRefs(context.Background(), work, "origin/feat/q", "main", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if head != "origin/feat/q" {
		t.Fatalf("head = %q", head)
	}
}

func TestResolveDiffRefs_BlankInputs(t *testing.T) {
	work := newRefsFixture(t)
	if _, _, err := resolveDiffRefs(context.Background(), work, "", "main", nil); err == nil {
		t.Fatal("blank branch should error")
	}
	if _, _, err := resolveDiffRefs(context.Background(), work, "main", "  ", nil); err == nil {
		t.Fatal("blank base should error")
	}
}
