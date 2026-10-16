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

	head, base, _, err := resolveDiffRefs(context.Background(), work, "feat/local", "main", nil)
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

	head, base, _, err := resolveDiffRefs(context.Background(), work, "feat/pushed", "main", nil)
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

	head, _, _, err := resolveDiffRefs(context.Background(), work, "feat/remote", "main", nil)
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

	if _, _, _, err := resolveDiffRefs(context.Background(), work, "feat/ghost", "main", nil); err == nil {
		t.Fatal("expected an error for a branch that exists nowhere")
	} else if !strings.Contains(err.Error(), "feat/ghost") {
		t.Fatalf("error should name the branch, got %v", err)
	}

	if _, _, _, err := resolveDiffRefs(context.Background(), work, "main", "nope", nil); err == nil {
		t.Fatal("expected an error for a missing base branch")
	}
}

// An already-qualified origin/<x> input resolves verbatim rather than
// becoming origin/origin/<x>.
func TestResolveDiffRefs_AcceptsQualifiedRef(t *testing.T) {
	work := newRefsFixture(t)
	commitOnBranch(t, work, "feat/q", "d.txt")
	gitIn(t, work, "push", "origin", "feat/q")

	head, _, _, err := resolveDiffRefs(context.Background(), work, "origin/feat/q", "main", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if head != "origin/feat/q" {
		t.Fatalf("head = %q", head)
	}
}

func TestResolveDiffRefs_BlankInputs(t *testing.T) {
	work := newRefsFixture(t)
	if _, _, _, err := resolveDiffRefs(context.Background(), work, "", "main", nil); err == nil {
		t.Fatal("blank branch should error")
	}
	if _, _, _, err := resolveDiffRefs(context.Background(), work, "main", "  ", nil); err == nil {
		t.Fatal("blank base should error")
	}
}

// TestResolveDiffRefs_FetchFailsBranchMissing：当仓库无 origin 远端（fetch
// 必失败 exit=128），且分支本地与 origin 都不存在时，错误消息含
// "git fetch origin 失败 (exit=128)" + fetchWarning 非空。
func TestResolveDiffRefs_FetchFailsBranchMissing(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "init", "--initial-branch=main")
	gitIn(t, work, "config", "user.email", "t@e")
	gitIn(t, work, "config", "user.name", "t")
	// 注意：故意不 git remote add origin，所以 fetch 会失败
	head, base, fetchWarn, err := resolveDiffRefs(context.Background(), work, "feat/ghost", "main", nil)
	if head != "" || base != "" || err == nil {
		t.Fatalf("expected empty refs + error, got head=%q base=%q err=%v fetchWarn=%q", head, base, err, fetchWarn)
	}
	if fetchWarn == "" {
		t.Fatalf("expected non-empty fetchWarning when fetch fails, got %q", fetchWarn)
	}
	if !strings.Contains(err.Error(), "git fetch origin 失败") || !strings.Contains(err.Error(), "exit=128") {
		t.Fatalf("error should describe fetch failure, got: %v", err)
	}
}

// TestResolveDiffRefs_FetchFailsBranchLocalOnly：当 fetch 失败但分支仅本地
// 存在时，仍能正常解析，且返回 fetchWarning 非空让上游日志可见。
func TestResolveDiffRefs_FetchFailsBranchLocalOnly(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "init", "--initial-branch=main")
	gitIn(t, work, "config", "user.email", "t@e")
	gitIn(t, work, "config", "user.name", "t")
	gitIn(t, work, "commit", "--allow-empty", "-m", "init")
	gitIn(t, work, "checkout", "-b", "feat/local-only")
	// 不设 origin，fetch 会失败，但分支本地存在
	head, _, warn, err := resolveDiffRefs(context.Background(), work, "feat/local-only", "main", nil)
	if err != nil {
		t.Fatalf("expected success for local-only branch, got %v", err)
	}
	if !strings.Contains(head, "feat/local-only") {
		t.Fatalf("head ref should contain branch name, got %q", head)
	}
	if warn == "" || !strings.Contains(warn, "git fetch origin 失败") {
		t.Fatalf("expected fetchWarning to mention fetch failure, got %q", warn)
	}
}

// TestResolveDiffRefs_FetchOKBranchMissing：fetch 成功但分支真的不存在
// （用 bare origin + 不存在的分支名），fetchWarning 为空，消息保持旧文案。
func TestResolveDiffRefs_FetchOKBranchMissing(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	work := filepath.Join(root, "work")
	gitIn(t, root, "init", "--bare", "--initial-branch=main", origin)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	// clone 必须在 root 跑（cwd），否则目标目录里再克隆一份
	gitIn(t, root, "clone", origin, work)
	// 现在有 origin 但没有 feat/ghost 分支
	_, _, warn, err := resolveDiffRefs(context.Background(), work, "feat/ghost", "main", nil)
	if err == nil {
		t.Fatal("expected error for genuinely missing branch")
	}
	if warn != "" {
		t.Fatalf("expected empty fetchWarning when fetch succeeds, got %q", warn)
	}
	if !strings.Contains(err.Error(), "在本地与 origin 上都不存在") {
		t.Fatalf("legacy message should be preserved on fetch-success path, got: %v", err)
	}
}
