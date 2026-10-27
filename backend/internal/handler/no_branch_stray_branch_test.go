package handler

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// ── the 2026-10-27 incident ────────────────────────────────────────────────
//
// req_1a42496198026380 ran in the no-branch mode ("直接在 base 分支上开发").
// The isolated worktree is a DETACHED HEAD — which looks broken to a coding
// agent — so the agent ran `git checkout -b feat/1a42496198026380`, committed
// on it, pushed it and opened a PR (#392). The platform's own push stage then
// hit the "cur != base" guard and refused:
//
//	🚀 本地 shell 推送 base 流程启动（no-branch 模式, base=main）
//	❌ 当前 HEAD 在分支 feat/1a42496198026380,cur != base=main,拒绝推送。
//
// The requirement could not reach its base branch at all. Two invariants
// carry the fix:
//
//  1. The prompt tells the agent not to create branches / push / open PRs
//     (the platform owns the delivery).
//  2. The push stage does NOT depend on which branch happens to be checked
//     out: it pushes the current HEAD onto the base branch and cleans the
//     stray branch up afterwards.

// TestPushBaseWithStrayBranchOnWorktree is the end-to-end regression guard:
// a stray branch checked out in the isolated worktree must not stop the
// push, must not create any remote branch, and must land the work on the
// base branch.
func TestPushBaseWithStrayBranchOnWorktree(t *testing.T) {
	r := newNoBranchRepo(t)

	// ── DB wiring ──────────────────────────────────────────────────────────
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	platformSvc := service.NewPlatformTokenService(d)
	projectSvc := service.NewProjectService(d, platformSvc, nil)
	reqSvc := service.NewRequirementService(d, nil)
	subTaskSvc := service.NewSubTaskService(d, nil)
	jobLogSvc := service.NewJobLogService(d)
	roleSvc := service.NewRoleService(d)
	jobs := store.NewJobStore(50)
	runner := NewSubTaskRunner(projectSvc, subTaskSvc, reqSvc, jobs, nil, roleSvc, jobLogSvc,
		nil, nil, nil, platformSvc, nil, nil, 1, nil, nil, nil)

	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, 'ready', 'main')`,
		"proj_stray", "stray proj", r.projectDir); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	row, err := reqSvc.Create(model.CreateRequirementReq{
		ProjectID:   "proj_stray",
		Title:       "no branch requirement",
		Description: "d",
	})
	if err != nil {
		t.Fatalf("create requirement: %v", err)
	}

	// The requirement develops in a detached worktree parked on "main" …
	wtPath, err := ensureDetachedWorktreeCore(r.projectDir, row.ID, "main", nil)
	if err != nil {
		t.Fatalf("ensureDetachedWorktreeCore: %v", err)
	}
	// … and the coding agent checks out a branch of its own and commits there.
	if out, err := runGitCtx(wtPath, "checkout", "-b", "feat/stray-agent-branch"); err != nil {
		t.Fatalf("agent checkout -b: %v\n%s", err, out)
	}
	r.writeFileIn(t, wtPath, "feature.txt", "agent work\n")
	r.commitAll(t, wtPath, "agent work")
	agentSHA, err := gitRun(wtPath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	if err := reqSvc.UpdateNoBranch(row.ID, true); err != nil {
		t.Fatalf("UpdateNoBranch: %v", err)
	}
	if err := reqSvc.UpdateBaseBranch(row.ID, "main"); err != nil {
		t.Fatalf("UpdateBaseBranch: %v", err)
	}
	if err := reqSvc.UpdateWorktree(row.ID, "", wtPath); err != nil {
		t.Fatalf("UpdateWorktree: %v", err)
	}
	if err := reqSvc.UpdateAutoPushFlags(row.ID, true, true, false); err != nil {
		t.Fatalf("UpdateAutoPushFlags: %v", err)
	}

	h := &WizardHandler{
		db:            d,
		projectSvc:    projectSvc,
		reqSvc:        reqSvc,
		subTaskSvc:    subTaskSvc,
		subTaskRunner: runner,
		roleSvc:       roleSvc,
		jobLogSvc:     jobLogSvc,
		jobs:          jobs,
		platformSvc:   platformSvc,
	}

	fresh, err := reqSvc.Get(row.ID)
	if err != nil {
		t.Fatalf("reload requirement: %v", err)
	}
	h.autoPushPR(fresh)

	jobID := waitForPushBaseJob(t, subTaskSvc, row.ID)
	lines, status := waitForPushBaseJobResult(t, jobs, jobID)
	if status != store.JobDone {
		t.Fatalf("push job status = %q, want %q — the push must not be refused:\n%s",
			status, store.JobDone, renderJobLog(lines))
	}

	// The refusal message must be gone for good.
	if strings.Contains(renderJobLog(lines), "拒绝推送") {
		t.Fatalf("the push was refused:\n%s", renderJobLog(lines))
	}

	// The agent's commit must have landed on the base branch.
	remoteSHA, err := gitRun(r.originBare, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse refs/heads/main: %v", err)
	}
	if remoteSHA != agentSHA {
		t.Fatalf("origin/main = %q, want the agent's commit %q", remoteSHA, agentSHA)
	}

	// And NO branch may exist on the remote besides the base branch.
	refs, err := gitRun(r.originBare, "for-each-ref", "--format=%(refname)", "refs/heads")
	if err != nil {
		t.Fatalf("for-each-ref on origin: %v", err)
	}
	got := strings.Fields(refs)
	if len(got) != 1 || got[0] != "refs/heads/main" {
		t.Fatalf("origin refs/heads = %v, want exactly [refs/heads/main] — the no-branch mode must not create branches", got)
	}

	// The worktree is back on the detached HEAD and the stray local branch
	// is gone — "不创建分支" has to mean it.
	cur, err := gitRun(wtPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse in worktree: %v", err)
	}
	if cur != "HEAD" {
		t.Fatalf("worktree HEAD = %q, want \"HEAD\" (detached) after the push", cur)
	}
	branches, err := gitRun(r.projectDir, "branch", "--list")
	if err != nil {
		t.Fatalf("branch list: %v", err)
	}
	if strings.Contains(branches, "feat/stray-agent-branch") {
		t.Fatalf("the stray local branch survived the push:\n%s", branches)
	}
}

// TestEnsureDetachedWorktreeDropsRequirementBranch pins the cleanup that runs
// when the no-branch mode takes over a worktree the design stage had already
// given a branch: the requirement's own branch must not survive the
// conversion — that is the "每个需求都会先创建一个分支" complaint.
func TestEnsureDetachedWorktreeDropsRequirementBranch(t *testing.T) {
	r := newNoBranchRepo(t)
	wtPath := WorktreePath(r.projectDir, r.reqID)

	// The design stage created a branch-shaped worktree for this requirement.
	if out, err := runGitCtx(r.projectDir, "worktree", "add", "-b", "feat/"+r.reqID, wtPath, "main"); err != nil {
		t.Fatalf("worktree add -b: %v\n%s", err, out)
	}
	// A branch that belongs to somebody else must survive.
	if out, err := runGitCtx(r.projectDir, "branch", "shared-release"); err != nil {
		t.Fatalf("branch shared-release: %v\n%s", err, out)
	}

	if _, err := ensureDetachedWorktreeCore(r.projectDir, r.reqID, "main", nil); err != nil {
		t.Fatalf("ensureDetachedWorktreeCore: %v", err)
	}

	branches, err := gitRun(r.projectDir, "branch", "--list")
	if err != nil {
		t.Fatalf("branch list: %v", err)
	}
	if strings.Contains(branches, "feat/"+r.reqID) {
		t.Fatalf("the requirement's own branch survived the no-branch conversion:\n%s", branches)
	}
	if !strings.Contains(branches, "shared-release") {
		t.Fatalf("an unrelated branch was deleted by the no-branch conversion:\n%s", branches)
	}
}

// TestRequirementOwnedBranch pins the ownership predicate that decides which
// local branches the no-branch cleanup is allowed to delete.
func TestRequirementOwnedBranch(t *testing.T) {
	cases := []struct {
		branch string
		reqID  string
		want   bool
	}{
		{"feat/req_abc123", "req_abc123", true},
		{"fix/req_abc123", "req_abc123", true},
		// The older convention that stripped the "req_" prefix.
		{"feat/abc123", "req_abc123", true},
		// Somebody else's branch / a shared branch / a release branch.
		{"feat/req_other", "req_abc123", false},
		{"main", "req_abc123", false},
		{"release/1.x", "req_abc123", false},
		{"feat/", "req_abc123", false},
		{"feat/req_abc123", "", false},
	}
	for _, tc := range cases {
		if got := requirementOwnedBranch(tc.branch, tc.reqID); got != tc.want {
			t.Fatalf("requirementOwnedBranch(%q, %q) = %v, want %v", tc.branch, tc.reqID, got, tc.want)
		}
	}
}

// TestNoBranchPromptForbidsBranchCreation pins the prompt contract: the
// coding agent must be told, BEFORE it starts, that the detached HEAD is
// normal, that it must not create branches, and that the platform pushes
// the base branch itself.
func TestNoBranchPromptForbidsBranchCreation(t *testing.T) {
	r := newNoBranchRepo(t)

	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	platformSvc := service.NewPlatformTokenService(d)
	projectSvc := service.NewProjectService(d, platformSvc, nil)
	reqSvc := service.NewRequirementService(d, nil)

	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, 'ready', 'main')`,
		"proj_prompt", "prompt proj", r.projectDir); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	row, err := reqSvc.Create(model.CreateRequirementReq{ProjectID: "proj_prompt", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create requirement: %v", err)
	}
	if err := reqSvc.UpdateNoBranch(row.ID, true); err != nil {
		t.Fatalf("UpdateNoBranch: %v", err)
	}
	if err := reqSvc.UpdateBaseBranch(row.ID, "main"); err != nil {
		t.Fatalf("UpdateBaseBranch: %v", err)
	}
	fresh, err := reqSvc.Get(row.ID)
	if err != nil {
		t.Fatalf("reload requirement: %v", err)
	}

	block := gitProjectBlock(projectSvc, platformSvc, fresh, false)
	for _, want := range []string{
		"## 不创建分支模式",
		"detached HEAD",
		"禁止创建 / 切换 / 重命名分支",
		"禁止推送与创建 PR",
		"直接推送到 base 分支",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("the no-branch prompt contract is missing %q:\n%s", want, block)
		}
	}

	// A branch-mode requirement must NOT get the contract (it really does
	// develop on a branch and really does need to push it).
	if err := reqSvc.UpdateNoBranch(row.ID, false); err != nil {
		t.Fatalf("UpdateNoBranch(false): %v", err)
	}
	fresh, err = reqSvc.Get(row.ID)
	if err != nil {
		t.Fatalf("reload requirement (2): %v", err)
	}
	if block := gitProjectBlock(projectSvc, platformSvc, fresh, false); strings.Contains(block, "## 不创建分支模式") {
		t.Fatalf("the no-branch contract leaked into a branch-mode prompt:\n%s", block)
	}

	// The push / PR sub-agent must NOT get it either — pushing is its job.
	if err := reqSvc.UpdateNoBranch(row.ID, true); err != nil {
		t.Fatalf("UpdateNoBranch(true): %v", err)
	}
	fresh, err = reqSvc.Get(row.ID)
	if err != nil {
		t.Fatalf("reload requirement (3): %v", err)
	}
	if block := gitProjectBlock(projectSvc, platformSvc, fresh, true); strings.Contains(block, "## 不创建分支模式") {
		t.Fatalf("the no-branch contract leaked into the push/PR prompt:\n%s", block)
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

// waitForPushBaseJobResult blocks until the push job reaches a terminal state
// and returns its log lines plus the final status.
func waitForPushBaseJobResult(t *testing.T, jobs *store.JobStore, jobID string) ([]store.LogLine, store.JobStatus) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := jobs.Get(jobID)
		if ok {
			lines, status, _ := job.Snapshot()
			if status == store.JobDone || status == store.JobError {
				return lines, status
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach a terminal state", jobID)
	return nil, store.JobStatus("")
}

// renderJobLog flattens a job's log lines into one string for assertions.
func renderJobLog(lines []store.LogLine) string {
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l.Content)
		sb.WriteString("\n")
	}
	return sb.String()
}
