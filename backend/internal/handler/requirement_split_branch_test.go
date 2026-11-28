package handler

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// The stack-bottom preparation is the one part of the split that touches
// git, so it gets real-repository tests: a fake git would not exercise the
// "never lose a commit" guarantee, which is the whole point of the
// fast-forward-only algorithm.
//
// These tests need a `git` binary; environments without one skip them.

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

// stackGit runs a git command in dir and returns trimmed stdout.
func stackGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newStackTestHandler wires a WizardHandler with a real project + platform
// service over the given repository path, so the git paths are reachable.
func newStackTestHandler(t *testing.T, repoPath, reqID string) (*WizardHandler, *service.RequirementService, *model.Project, *model.Requirement) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if _, err := d.Exec(`INSERT INTO projects (id,name,local_path,status,default_branch) VALUES ('proj_x','X',?,'ready','main')`, repoPath); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO requirements (id,project_id,title,status,kind,design_docs,dev_delegated,branch_name)
		 VALUES (?, 'proj_x', '需求', 'designed', 'requirement', '# 方案', 0, ?)`,
		reqID, "feat/"+reqID); err != nil {
		t.Fatalf("insert requirement: %v", err)
	}

	reqSvc := service.NewRequirementService(d, nil)
	plat := service.NewPlatformTokenService(d)
	projSvc := service.NewProjectService(d, plat, nil)
	h := &WizardHandler{
		reqSvc:      reqSvc,
		projectSvc:  projSvc,
		platformSvc: plat,
		jobs:        store.NewJobStore(4),
	}
	row, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("get requirement: %v", err)
	}
	proj, err := projSvc.Get("proj_x")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	return h, reqSvc, proj, row
}

// initStackRepo creates a repo with main + a "featureA" branch on top.
func initStackRepo(t *testing.T) string {
	t.Helper()
	requireGit(t)
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	stackGit(t, repo, "init", "-b", "main")
	stackGit(t, repo, "config", "user.email", "t@example.com")
	stackGit(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stackGit(t, repo, "add", ".")
	stackGit(t, repo, "commit", "-m", "init")
	stackGit(t, repo, "checkout", "-b", "featureA")
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	stackGit(t, repo, "add", ".")
	stackGit(t, repo, "commit", "-m", "featureA")
	stackGit(t, repo, "checkout", "main")
	return repo
}

// The stack bottom is created from the branch the user picked.
func TestPrepareSplitStackBase_CreatesBranchFromSource(t *testing.T) {
	repo := initStackRepo(t)
	h, reqSvc, proj, row := newStackTestHandler(t, repo, "req_x")

	branch, pushed, warnings, err := h.prepareSplitStackBase(row, proj, "featureA")
	if err != nil {
		t.Fatalf("prepareSplitStackBase: %v", err)
	}
	if branch != "feat/req_x" {
		t.Errorf("branch = %q, want feat/req_x", branch)
	}
	// No origin in this repo → nothing to push, and the user is told so.
	if pushed {
		t.Error("pushed = true, want false (the repo has no origin)")
	}
	if len(warnings) == 0 {
		t.Error("expected a warning about the missing origin")
	}
	// The branch must sit exactly on the source branch's commit.
	want := stackGit(t, repo, "rev-parse", "featureA")
	if got := stackGit(t, repo, "rev-parse", "feat/req_x"); got != want {
		t.Errorf("feat/req_x at %s, want %s", got, want)
	}
	// The master row now carries the branch (children resolve their base
	// through the parent's branch_name) and the picked source.
	after, err := reqSvc.Get("req_x")
	if err != nil {
		t.Fatalf("get master: %v", err)
	}
	if after.BranchName != "feat/req_x" {
		t.Errorf("master branch_name = %q, want feat/req_x", after.BranchName)
	}
	if after.BaseBranch != "featureA" {
		t.Errorf("master base_branch = %q, want featureA", after.BaseBranch)
	}
	if after.BaseBranchSource != model.BaseSourceExplicit {
		t.Errorf("master base_branch_source = %q, want %q", after.BaseBranchSource, model.BaseSourceExplicit)
	}
}

// Re-running the split with a DIFFERENT source must never throw away the
// commits the master's branch already carries — a child's PR may already
// have landed on it.
func TestPrepareSplitStackBase_NeverLosesCommits(t *testing.T) {
	repo := initStackRepo(t)
	h, reqSvc, proj, row := newStackTestHandler(t, repo, "req_x")

	if _, _, _, err := h.prepareSplitStackBase(row, proj, "featureA"); err != nil {
		t.Fatalf("prepareSplitStackBase: %v", err)
	}
	after, _ := reqSvc.Get("req_x")

	// Now pick "main" — featureA's commit is NOT in main, so the branch
	// must stay where it is and the user must be warned.
	branch, _, warnings, err := h.prepareSplitStackBase(after, proj, "main")
	if err != nil {
		t.Fatalf("prepareSplitStackBase (2nd): %v", err)
	}
	if branch != "feat/req_x" {
		t.Errorf("branch = %q, want feat/req_x", branch)
	}
	want := stackGit(t, repo, "rev-parse", "featureA")
	if got := stackGit(t, repo, "rev-parse", "feat/req_x"); got != want {
		t.Errorf("feat/req_x moved to %s, want %s — a commit was lost", got, want)
	}
	if len(warnings) == 0 {
		t.Error("expected a warning that the branch was not rebuilt")
	}
}

// The stack bottom must reach origin — a stacked PR's base has to exist
// remotely or the children's PRs cannot be created.
func TestPrepareSplitStackBase_PushesToOrigin(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v\n%s", err, out)
	}
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	stackGit(t, repo, "init", "-b", "main")
	stackGit(t, repo, "config", "user.email", "t@example.com")
	stackGit(t, repo, "config", "user.name", "t")
	stackGit(t, repo, "remote", "add", "origin", origin)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stackGit(t, repo, "add", ".")
	stackGit(t, repo, "commit", "-m", "init")
	stackGit(t, repo, "push", "-u", "origin", "main")

	h, _, proj, row := newStackTestHandler(t, repo, "req_x")
	branch, pushed, warnings, err := h.prepareSplitStackBase(row, proj, "main")
	if err != nil {
		t.Fatalf("prepareSplitStackBase: %v", err)
	}
	if branch != "feat/req_x" {
		t.Errorf("branch = %q, want feat/req_x", branch)
	}
	if !pushed {
		t.Errorf("pushed = false, want true (warnings: %v)", warnings)
	}
	if out := stackGit(t, origin, "branch", "--list", "feat/req_x"); out == "" {
		t.Error("origin has no feat/req_x branch")
	}
}

// The normal case: the design stage already created the worktree with the
// master's branch checked out. The branch must then be fast-forwarded IN
// PLACE — `git branch -f` would be refused by git (the branch is checked
// out), and a merge in the wrong directory would poison whatever branch
// the user happens to be on.
func TestPrepareSplitStackBase_FastForwardsInsideTheWorktree(t *testing.T) {
	repo := initStackRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	stackGit(t, repo, "worktree", "add", "-b", "feat/req_x", wt, "main")

	h, _, proj, row := newStackTestHandler(t, repo, "req_x")
	// Point the requirement at the worktree, like the design stage does.
	row.WorktreePath = wt

	branch, _, _, err := h.prepareSplitStackBase(row, proj, "featureA")
	if err != nil {
		t.Fatalf("prepareSplitStackBase: %v", err)
	}
	if branch != "feat/req_x" {
		t.Errorf("branch = %q, want feat/req_x", branch)
	}
	// The branch must now sit on featureA's commit...
	want := stackGit(t, repo, "rev-parse", "featureA")
	if got := stackGit(t, repo, "rev-parse", "feat/req_x"); got != want {
		t.Errorf("feat/req_x at %s, want %s", got, want)
	}
	// ...the worktree must still be ON that branch (no other branch got
	// merged into) ...
	if cur := stackGit(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); cur != "feat/req_x" {
		t.Errorf("worktree HEAD = %q, want feat/req_x", cur)
	}
	// ...and its working tree must reflect the fast-forward.
	if _, err := os.Stat(filepath.Join(wt, "b.txt")); err != nil {
		t.Errorf("worktree b.txt missing — the working tree was not fast-forwarded: %v", err)
	}
}

// Without a git-capable environment the preparation degrades to "no stack":
// no branch is created, nothing is pushed, and the base is left for the
// service layer to derive.
func TestPrepareSplitStackBase_DegradesWithoutGit(t *testing.T) {
	repo := initStackRepo(t)
	h, _, _, row := newStackTestHandler(t, repo, "req_m")

	// A project row whose local_path is not a git repository.
	notARepo := filepath.Join(t.TempDir(), "plain")
	if err := os.MkdirAll(notARepo, 0o755); err != nil {
		t.Fatal(err)
	}

	branch, pushed, warnings, err := h.prepareSplitStackBase(row,
		&model.Project{LocalPath: notARepo, DefaultBranch: "main"}, "main")
	if err != nil {
		t.Fatalf("prepareSplitStackBase: %v", err)
	}
	if branch != "" {
		t.Errorf("branch = %q, want empty (degraded)", branch)
	}
	if pushed {
		t.Error("pushed = true, want false (degraded)")
	}
	if len(warnings) == 0 {
		t.Error("expected a warning that the project is not a git repository")
	}
}

// splitRemoteDetail decides what the user actually reads when origin does
// not answer. Three properties matter:
//
//  1. git's REASON survives — the exec suffix ("exit status 128") does not,
//     because it repeats what the reader can already see.
//  2. The credentials embedded in a remote URL never leave the process.
//  3. A git that was killed before it said anything (the timeout path) still
//     produces a sentence, not an empty one.
func TestSplitRemoteDetail(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		want    string
		notWant string
	}{
		{
			name: "git's stderr survives, the exec suffix does not",
			err:  errors.New("fatal: unable to access 'https://github.com/x/y.git/': Failed to connect to 127.0.0.1 port 7993: Couldn't connect to server: exit status 128"),
			want: "Failed to connect to 127.0.0.1 port 7993",
			// "exit status 128" is noise for a human.
			notWant: "exit status",
		},
		{
			name:    "a token in the remote URL is redacted",
			err:     errors.New("fatal: unable to access 'https://ghp_secret1234567890:@github.com/x/y.git/': failed: exit status 128"),
			want:    "https://<redacted>@github.com/x/y.git/",
			notWant: "ghp_secret1234567890",
		},
		{
			name: "a killed git still says something",
			err:  errors.New(": signal: killed"),
			want: "远端无响应",
		},
		{
			name: "multi-line transport errors are collapsed into one line",
			err:  errors.New("fatal: unable to access 'https://github.com/x/y.git/':\n\tFailed to connect: exit status 128"),
			want: "fatal: unable to access",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitRemoteDetail(tc.err)
			if !strings.Contains(got, tc.want) {
				t.Errorf("splitRemoteDetail = %q, want it to contain %q", got, tc.want)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Errorf("splitRemoteDetail = %q, want it NOT to contain %q", got, tc.notWant)
			}
			if strings.Contains(got, "\n") {
				t.Errorf("splitRemoteDetail = %q, want a single line", got)
			}
		})
	}

	if got := splitRemoteDetail(nil); got != "" {
		t.Errorf("splitRemoteDetail(nil) = %q, want an empty string", got)
	}
}

// The truncation must cut by RUNE, not by byte: git speaks the user's
// locale, so the detail can be Chinese, and a byte cut would split a
// character in half and show mojibake.
func TestSplitRemoteDetail_TruncatesWithoutBreakingCharacters(t *testing.T) {
	long := strings.Repeat("远端不可达", 200)
	got := splitRemoteDetail(errors.New(long))
	if !strings.HasSuffix(got, "…") {
		t.Errorf("splitRemoteDetail did not mark the truncation: %q", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Errorf("splitRemoteDetail produced a broken character: %q", got)
	}
	// Everything before the ellipsis must be whole runes.
	body := strings.TrimSuffix(got, "…")
	for _, r := range body {
		if r == '�' {
			t.Errorf("splitRemoteDetail produced a broken character: %q", got)
			break
		}
	}
}
