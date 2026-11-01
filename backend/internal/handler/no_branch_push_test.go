package handler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// TestPushBaseRefspecArgs pins the push shape used by the no-branch mode.
//
// The refspec form (`HEAD:refs/heads/<base>`) is the ONLY shape: it advances
// the base branch without creating anything, and it must NOT carry `-u` —
// a detached HEAD cannot have an upstream and git refuses with "cannot set up
// tracking information".
//
// It is also the only shape that survives the coding agent checking out a
// branch of its own inside the isolated worktree (req_1a42496198026380,
// 2026-10-27): `push origin <base>` would push the LOCAL base branch instead
// of the agent's commits, while `HEAD:refs/heads/<base>` pushes whatever HEAD
// points at.
func TestPushBaseRefspecArgs(t *testing.T) {
	got := pushBaseRefspecArgs("main")
	want := []string{"push", "origin", "HEAD:refs/heads/main"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %v, want %v", got, want)
		}
	}
	for _, a := range got {
		if a == "-u" {
			t.Fatalf("the no-branch push must not set an upstream (-u): %v", got)
		}
	}

	// A different base is honored verbatim.
	other := pushBaseRefspecArgs("develop")
	wantOther := []string{"push", "origin", "HEAD:refs/heads/develop"}
	for i := range wantOther {
		if other[i] != wantOther[i] {
			t.Fatalf("args = %v, want %v", other, wantOther)
		}
	}
}

// TestResolveBaseBranch is the regression guard for the "push to the BASE
// branch" requirement.
//
// The base branch is the baseline the user picked at the design / coding
// stage — it lives on requirements.base_branch. The project's
// default_branch is only the last fallback of the chain. The auto-push path
// used to read proj.DefaultBranch as the ONLY source, so a requirement whose
// base was "develop" got its code pushed onto the project main branch.
func TestResolveBaseBranch(t *testing.T) {
	cases := []struct {
		name string
		row  *model.Requirement
		proj *model.Project
		want string
	}{
		{
			name: "requirement base wins over the project default branch",
			row:  &model.Requirement{BaseBranch: "develop"},
			proj: &model.Project{DefaultBranch: "main"},
			want: "develop",
		},
		{
			name: "project default branch is the first fallback",
			row:  &model.Requirement{BaseBranch: ""},
			proj: &model.Project{DefaultBranch: "main"},
			want: "main",
		},
		{
			name: "main is the last fallback",
			row:  &model.Requirement{BaseBranch: ""},
			proj: &model.Project{DefaultBranch: ""},
			want: "main",
		},
		{
			name: "nil row falls back to the project",
			row:  nil,
			proj: &model.Project{DefaultBranch: "trunk"},
			want: "trunk",
		},
		{
			name: "nil project falls back to main",
			row:  &model.Requirement{BaseBranch: "release/1.x"},
			proj: nil,
			want: "release/1.x",
		},
		{
			name: "whitespace-only values are ignored",
			row:  &model.Requirement{BaseBranch: "  "},
			proj: &model.Project{DefaultBranch: "main"},
			want: "main",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveBaseBranch(tc.row, tc.proj); got != tc.want {
				t.Fatalf("resolveBaseBranch = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveNoBranchWorkDir pins the working-directory resolution for the
// no-branch mode: the requirement's isolated worktree when it is there and
// matches, the project checkout otherwise (legacy rows, drifted paths,
// missing directories).
func TestResolveNoBranchWorkDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	projectPath := t.TempDir()
	reqID := "req_test_no_branch_dir"
	wtPath := WorktreePath(projectPath, reqID)

	// Legacy row (developed straight in the project directory).
	if got := resolveNoBranchWorkDir(&model.Requirement{ID: reqID}, projectPath); got != projectPath {
		t.Fatalf("legacy row: got %q, want the project dir %q", got, projectPath)
	}

	// A canonical worktree that exists on disk.
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveNoBranchWorkDir(&model.Requirement{ID: reqID, WorktreePath: wtPath}, projectPath); got != wtPath {
		t.Fatalf("existing worktree: got %q, want %q", got, wtPath)
	}

	// A drifted path (another requirement's directory).
	drifted := filepath.Join(projectPath, "someone-elses-worktree")
	if got := resolveNoBranchWorkDir(&model.Requirement{ID: reqID, WorktreePath: drifted}, projectPath); got != projectPath {
		t.Fatalf("drifted path: got %q, want the project dir %q", got, projectPath)
	}

	// The canonical path, but the directory is gone.
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatal(err)
	}
	if got := resolveNoBranchWorkDir(&model.Requirement{ID: reqID, WorktreePath: wtPath}, projectPath); got != projectPath {
		t.Fatalf("missing worktree: got %q, want the project dir %q", got, projectPath)
	}

	// No row at all.
	if got := resolveNoBranchWorkDir(nil, projectPath); got != projectPath {
		t.Fatalf("nil row: got %q, want the project dir %q", got, projectPath)
	}
}

// TestNoBranchWinsOverBranchName is the regression guard for the defect where
// the LaunchSpec "develop immediately" path sent branch_name="feat/req_xxx"
// AND no_branch=true at the same time, and the branch silently won — so the
// no_branch intent was dropped and the requirement got a feature branch.
func TestNoBranchWinsOverBranchName(t *testing.T) {
	if got := resolveCodingWorktreeMode(true, "feat/req_xxx", "req_xxx"); got != codingModeDetached {
		t.Fatalf("no_branch + branch_name: got %q, want %q", got, codingModeDetached)
	}
	if got := resolveCodingWorktreeMode(false, "feat/req_xxx", "req_xxx"); got != codingModeBranch {
		t.Fatalf("branch mode: got %q, want %q", got, codingModeBranch)
	}
	if got := resolveCodingWorktreeMode(true, "", "req_xxx"); got != codingModeDetached {
		t.Fatalf("no_branch without a branch name: got %q, want %q", got, codingModeDetached)
	}
	// No requirement id → no worktree anchor → the legacy in-place path.
	if got := resolveCodingWorktreeMode(true, "feat/req_xxx", ""); got != codingModeInPlace {
		t.Fatalf("no requirement id: got %q, want %q", got, codingModeInPlace)
	}
	if got := resolveCodingWorktreeMode(false, "", "req_xxx"); got != codingModeInPlace {
		t.Fatalf("no branch and no no_branch: got %q, want %q", got, codingModeInPlace)
	}
}

// TestAutoPushPRUsesRequirementBase is the end-to-end regression guard for
// the base semantics: a no-branch requirement whose base is "develop" — while
// the PROJECT's default branch is "main" — must have its commits land on
// "develop" and leave "main" untouched.
//
// It drives the real autoPushPR entry point with a real git remote, so the
// assertion is about the state of the remote, not about an internal variable.
func TestAutoPushPRUsesRequirementBase(t *testing.T) {
	r := newNoBranchRepo(t)

	// The project default branch stays "main"; the requirement's base is
	// "develop". This is the exact shape that used to push onto "main".
	r.git(t, "checkout", "-q", "-b", "develop")
	r.git(t, "push", "-q", "origin", "develop")
	r.git(t, "checkout", "-q", "main")
	r.git(t, "fetch", "-q", "origin")
	mainBefore, err := gitRun(r.originBare, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse origin main: %v", err)
	}

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

	// The project row: default branch "main", checkout at the project dir.
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, 'ready', 'main')`,
		"proj_no_branch", "no-branch proj", r.projectDir); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	row, err := reqSvc.Create(model.CreateRequirementReq{
		ProjectID:   "proj_no_branch",
		Title:       "no branch requirement",
		Description: "d",
	})
	if err != nil {
		t.Fatalf("create requirement: %v", err)
	}

	// The requirement develops in a detached worktree parked on "develop".
	// The worktree path MUST be keyed on the real requirement id — that is
	// what the drift guard compares against.
	wtPath, err := ensureDetachedWorktreeCore(r.projectDir, row.ID, "develop", nil)
	if err != nil {
		t.Fatalf("ensureDetachedWorktreeCore: %v", err)
	}
	r.writeFileIn(t, wtPath, "feature.txt", "work\n")
	r.commitAll(t, wtPath, "work on develop")
	wantSHA, err := gitRun(wtPath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	if err := reqSvc.UpdateNoBranch(row.ID, true); err != nil {
		t.Fatalf("UpdateNoBranch: %v", err)
	}
	if err := reqSvc.UpdateBaseBranch(row.ID, "develop"); err != nil {
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

	// Wait for the dispatched shell job to finish.
	jobID := waitForPushBaseJob(t, subTaskSvc, row.ID)
	waitForJobDone(t, jobs, jobID)

	// The requirement's base must have moved …
	developSHA, err := gitRun(r.originBare, "rev-parse", "refs/heads/develop")
	if err != nil {
		t.Fatalf("rev-parse origin develop: %v", err)
	}
	if developSHA != wantSHA {
		t.Fatalf("origin/develop = %q, want the pushed commit %q — the push must target the requirement's base branch", developSHA, wantSHA)
	}
	// … and the project's default branch must NOT have moved.
	mainAfter, err := gitRun(r.originBare, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse origin main: %v", err)
	}
	if mainAfter != mainBefore {
		t.Fatalf("origin/main moved from %q to %q — the push must not target the project's default branch", mainBefore, mainAfter)
	}
}

// TestCodingBaseWriteBackAlignsPushTarget is the regression guard for the
// base drift between the coding stage and the push stage.
//
// The design stage stamps the base it worked against. When the user switches
// the base on the "start coding" form, the coding stage resolves the new
// base for THAT run and writes it back onto the requirement row — so the
// push stage, which reads the row, necessarily pushes onto the same branch
// the code was written against. Without the write-back the code would be
// developed on top of A but pushed onto B.
func TestCodingBaseWriteBackAlignsPushTarget(t *testing.T) {
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	reqSvc := service.NewRequirementService(d, nil)
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, 'ready', 'main')`,
		"proj_1", "p", "/tmp/p"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	row, err := reqSvc.Create(model.CreateRequirementReq{ProjectID: "proj_1", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	proj := &model.Project{DefaultBranch: "main"}

	// The design stage stamped "develop".
	if err := reqSvc.UpdateBaseBranch(row.ID, "develop"); err != nil {
		t.Fatalf("UpdateBaseBranch(develop): %v", err)
	}
	stored, err := reqSvc.Get(row.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := resolveBaseBranch(stored, proj); got != "develop" {
		t.Fatalf("resolveBaseBranch = %q, want %q", got, "develop")
	}

	// The user switches the base to "main" on the coding form; the coding
	// stage writes the base it actually used back onto the row.
	if err := reqSvc.UpdateBaseBranch(row.ID, "main"); err != nil {
		t.Fatalf("UpdateBaseBranch(main): %v", err)
	}
	stored, err = reqSvc.Get(row.ID)
	if err != nil {
		t.Fatalf("get (2): %v", err)
	}
	if stored.BaseBranch != "main" {
		t.Fatalf("stored base_branch = %q, want %q", stored.BaseBranch, "main")
	}
	// The push stage now resolves the SAME base the coding stage used.
	if got := resolveBaseBranch(stored, proj); got != "main" {
		t.Fatalf("resolveBaseBranch = %q, want %q — the push target must follow the coding stage's base", got, "main")
	}
}

// waitForPushBaseJob polls the requirement's sub-tasks for the push_base one
// and returns its job id.
func waitForPushBaseJob(t *testing.T, subTaskSvc *service.SubTaskService, reqID string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		list, err := subTaskSvc.List(reqID)
		if err == nil {
			for _, st := range list {
				if st.Source == model.SubTaskSourcePushBase && st.JobID != "" {
					return st.JobID
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no push_base sub-task with a job id appeared for %s", reqID)
	return ""
}

// waitForJobDone blocks until the job reaches a terminal state and returns
// its log lines.
func waitForJobDone(t *testing.T, jobs *store.JobStore, jobID string) []store.LogLine {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := jobs.Get(jobID)
		if ok {
			lines, status, _ := job.Snapshot()
			if status == store.JobDone || status == store.JobError {
				return lines
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach a terminal state", jobID)
	return nil
}

// assertJobLogContains fails when the job log misses the wanted fragment.
func assertJobLogContains(t *testing.T, lines []store.LogLine, want string) {
	t.Helper()
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l.Content)
		sb.WriteString("\n")
	}
	if !strings.Contains(sb.String(), want) {
		t.Fatalf("job log does not contain %q:\n%s", want, sb.String())
	}
}
