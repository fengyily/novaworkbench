package handler

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// initScopeRepo builds a tiny git repo with one commit, a dirty tracked
// file and one untracked file — the three segments a requirement-level
// Review has to cover.
func initScopeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.go")
	run("commit", "-qm", "init")

	// A tracked file with an uncommitted change.
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package main // edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A brand-new, untracked file.
	if err := os.WriteFile(filepath.Join(dir, "b_test.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestResolveReviewScope_WorktreeCoversUncommittedAndUntracked is the guard
// for the risk the design called "最致命": a 回归 Review that only looked at
// `base...head` would re-grade the PRE-FIX code, so the "fix → re-review
// until it passes" loop would never converge.
//
// The scope must therefore include the uncommitted working-tree change and
// the untracked new file.
func TestResolveReviewScope_WorktreeCoversUncommittedAndUntracked(t *testing.T) {
	dir := initScopeRepo(t)

	sc, err := resolveReviewScope(t.Context(), reviewScopeRequest{
		RepoDir: dir,
		Branch:  "main",
		Base:    "main",
	})
	if err != nil {
		t.Fatalf("resolveReviewScope: %v", err)
	}
	if sc.Mode != reviewScopeWorktree {
		t.Fatalf("Mode = %q, want %q (the tree is dirty)", sc.Mode, reviewScopeWorktree)
	}
	if !sc.Uncommitted {
		t.Fatalf("Uncommitted = false, want true (a.go was edited)")
	}
	found := false
	for _, f := range sc.Untracked {
		if f == "b_test.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Untracked = %v, want it to contain b_test.go", sc.Untracked)
	}
	block := sc.Block
	for _, want := range []string{"git diff HEAD", "b_test.go", "工作区未提交改动"} {
		if !strings.Contains(block, want) {
			t.Fatalf("scope block missing %q:\n%s", want, block)
		}
	}
}

// TestResolveReviewScope_CleanTreeDegradesToRefs pins the degraded mode: a
// clean tree with no untracked files has nothing beyond the committed range,
// and the block says so explicitly so the user is never misled.
func TestResolveReviewScope_CleanTreeDegradesToRefs(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.go")
	run("commit", "-qm", "init")

	sc, err := resolveReviewScope(t.Context(), reviewScopeRequest{
		RepoDir: dir,
		Branch:  "main",
		Base:    "main",
	})
	if err != nil {
		t.Fatalf("resolveReviewScope: %v", err)
	}
	if sc.Mode != reviewScopeRefs {
		t.Fatalf("Mode = %q, want %q (the tree is clean)", sc.Mode, reviewScopeRefs)
	}
	if !strings.Contains(sc.Block, "只覆盖已提交的改动") {
		t.Fatalf("the degraded scope must say what it does NOT cover:\n%s", sc.Block)
	}
}

// TestResolveReviewScope_MissingBranchIsAnError pins the fail-fast rule: a
// Review whose refs can't be resolved would grade an empty diff, so the
// caller must get an error instead of a process.
func TestResolveReviewScope_MissingBranchIsAnError(t *testing.T) {
	dir := initScopeRepo(t)
	if _, err := resolveReviewScope(t.Context(), reviewScopeRequest{
		RepoDir: dir,
		Branch:  "no/such/branch",
		Base:    "main",
	}); err == nil {
		t.Fatal("expected an error for an unresolvable branch")
	}
}

// TestResolveReviewWorkDir_PrefersTheExistingWorktree pins the directory
// choice: a Review must run in the directory its scope was resolved
// against, and it must never be a freshly created worktree.
func TestResolveReviewWorkDir_PrefersTheExistingWorktree(t *testing.T) {
	proj := t.TempDir()
	wt := t.TempDir()
	req := &model.Requirement{ID: "req_x", WorktreePath: wt}
	if got := resolveReviewWorkDir(req, proj); got != wt {
		t.Fatalf("resolveReviewWorkDir = %q, want the existing worktree %q", got, wt)
	}

	// A worktree path that no longer exists falls back to the checkout.
	missing := filepath.Join(t.TempDir(), "gone")
	req2 := &model.Requirement{ID: "req_y", WorktreePath: missing}
	if got := resolveReviewWorkDir(req2, proj); got != proj {
		t.Fatalf("resolveReviewWorkDir = %q, want the project checkout %q", got, proj)
	}
}

// TestPhaseEnabledForRun_ReviewIsOff pins the kind gate: a Review is a
// manual row, so without the kind check the three-section phase instruction
// would be injected into the review prompt and break the report contract.
func TestPhaseEnabledForRun_ReviewIsOff(t *testing.T) {
	if phaseEnabledForRun(model.SubTaskKindReview, model.SubTaskSourceManual) {
		t.Fatal("the phase tracker must be off for review rows")
	}
	if !phaseEnabledForRun(model.SubTaskKindCode, model.SubTaskSourceManual) {
		t.Fatal("the phase tracker must stay on for manual code rows")
	}
	if phaseEnabledForRun(model.SubTaskKindCode, model.SubTaskSourceAuto) {
		t.Fatal("the phase tracker must stay off for auto rows")
	}
}

// TestParseFindingsArray pins the shape distinction that broke the
// "turn a finding into a fix sub-task" action.
//
// code_reviews.findings_json holds the findings ARRAY (buildReviewOutcome
// re-marshals just that array out of the contract), while parseFindings
// decodes the whole CONTRACT OBJECT. Feeding the array to the contract
// parser fails — an array cannot unmarshal into a struct — so the array
// needs its own parser.
func TestParseFindingsArray(t *testing.T) {
	arr := `[{"severity":"P0","file":"a.go","line":"1","title":"未实现","detail":"d","suggestion":"s"},
	         {"severity":"P1","file":"b.go","title":"建议","detail":"d2","suggestion":"s2"},
	         {"severity":"P2","file":"c.go","title":"不该出现的档位"}]`
	findings, ok := parseFindingsArray(arr)
	if !ok {
		t.Fatal("parseFindingsArray returned ok=false for a valid findings array")
	}
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2 (the P2 bucket is dropped)", len(findings))
	}
	if findings[0].Severity != "P0" || findings[1].Severity != "P1" {
		t.Fatalf("severities = %q / %q, want P0 / P1", findings[0].Severity, findings[1].Severity)
	}
	if findings[0].Title != "未实现" {
		t.Fatalf("title = %q, want 未实现", findings[0].Title)
	}

	// The contract object is NOT an array — the parser must refuse it
	// rather than silently returning an empty list.
	if _, ok := parseFindingsArray(`{"goal_met":"yes","findings":[]}`); ok {
		t.Fatal("parseFindingsArray must refuse a contract object")
	}
	if _, ok := parseFindingsArray(""); ok {
		t.Fatal("parseFindingsArray must refuse an empty column")
	}
}
