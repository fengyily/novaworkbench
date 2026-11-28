package handler

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// 跨项目需求的「工作目录集合」：先同步基础分支，再以跨项目需求 ID 建隔离
// worktree。
//
// 这些断言守的是三件事：
//
//  1. **方案读到的是最新基线** —— 上游推进之后重建工作区，worktree 的 HEAD
//     必须跟着走；
//  2. **用户的检出不受影响** —— 不建分支、不快进、HEAD 不动；
//  3. **拉不到基线时不产出方案** —— fetch 失败是硬错误，降级只发生在「根本没有
//     上游可同步」的仓库上。

// ── fixture ────────────────────────────────────────────────────────────────

// crossWorkspaceRepo is a throwaway git project plus a bare "origin".
type crossWorkspaceRepo struct {
	projectDir string
	originBare string
}

// newCrossWorkspaceRepo builds the fixture and points $HOME at a temp dir so
// the canonical worktree path (~/.novaworkbench/worktrees/...) materialises
// inside the sandbox.
func newCrossWorkspaceRepo(t *testing.T) *crossWorkspaceRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on this host; skipping cross workspace test")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LC_ALL", "C")
	t.Setenv("LANG", "C")

	r := &crossWorkspaceRepo{
		projectDir: t.TempDir(),
		originBare: t.TempDir(),
	}
	r.git(t, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(r.projectDir, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.git(t, "add", "README.md")
	r.git(t, "commit", "-q", "-m", "seed commit")

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

func (r *crossWorkspaceRepo) git(t *testing.T, args ...string) string {
	t.Helper()
	out, err := runGitCtx(r.projectDir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (r *crossWorkspaceRepo) gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGitCtx(dir, args...)
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

// newCrossWorkspaceHandler wires a handler whose project service is the REAL
// one — the workspace builder has to run actual git commands, so a mock would
// prove nothing.
func newCrossWorkspaceHandler(t *testing.T, projectDir string) (*CrossRequirementHandler, *db.DB, string) {
	t.Helper()
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	projSvc := service.NewProjectService(d, nil, nil)
	reqSvc := service.NewRequirementService(d, nil)
	crossSvc := service.NewCrossRequirementService(d)
	worktreeH := NewWorktreeHandler(projSvc, nil)
	h := NewCrossRequirementHandler(crossSvc, reqSvc, projSvc, nil, store.NewJobStore(16),
		nil, nil, nil, worktreeH, nil, nil)

	projID := util.NewID("proj")
	now := time.Now()
	if _, err := d.Exec(
		`INSERT INTO projects (id,name,local_path,status,default_branch,added_at,updated_at) VALUES (?,?,?, 'active', 'main', ?, ?)`,
		projID, "api-svc", projectDir, now, now); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return h, d, projID
}

// seedCrossRequirementWithProject creates a cross requirement over one
// project and returns its id.
func seedCrossRequirementWithProject(t *testing.T, h *CrossRequirementHandler, projID string) string {
	t.Helper()
	x, err := h.svc.Create(model.CreateCrossRequirementReq{
		Title:      "跨仓库需求",
		ProjectIDs: []string{projID},
	})
	if err != nil {
		t.Fatalf("create cross requirement: %v", err)
	}
	return x.ID
}

// ── tests ──────────────────────────────────────────────────────────────────

// TestCrossWorkspace_PinsBaselineInDetachedWorktree — 整体方案读的那份代码
// 必须停在锁定的基线上，而且是以 detached HEAD 的形式（不给用户的仓库建任何
// 分支）。
func TestCrossWorkspace_PinsBaselineInDetachedWorktree(t *testing.T) {
	r := newCrossWorkspaceRepo(t)
	h, _, projID := newCrossWorkspaceHandler(t, r.projectDir)
	xreqID := seedCrossRequirementWithProject(t, h, projID)

	ws, err := h.buildCrossWorkspace(context.Background(), xreqID, nil)
	if err != nil {
		t.Fatalf("build workspace: %v", err)
	}
	if len(ws.Projects) != 1 {
		t.Fatalf("workspace projects = %d, want 1", len(ws.Projects))
	}
	p := ws.Projects[0]
	if p.Degraded {
		t.Fatalf("project degraded unexpectedly: %s", p.DegradedReason)
	}
	if p.BaseSHA == "" {
		t.Fatal("baseline SHA is empty — the fetch did not lock a commit")
	}
	if p.WorktreePath == "" {
		t.Fatal("worktree path is empty")
	}

	// The worktree lives under the canonical root, keyed by the CROSS
	// REQUIREMENT id (not the project's, not a requirement's).
	wantPath := WorktreePath(r.projectDir, xreqID)
	if p.WorktreePath != wantPath {
		t.Fatalf("worktree path = %q, want %q", p.WorktreePath, wantPath)
	}
	if !strings.Contains(p.WorktreePath, xreqID) {
		t.Fatalf("worktree path %q does not carry the cross requirement id %q", p.WorktreePath, xreqID)
	}

	// Detached HEAD at the locked commit.
	if branch := r.gitIn(t, p.WorktreePath, "rev-parse", "--abbrev-ref", "HEAD"); branch != "HEAD" {
		t.Fatalf("worktree HEAD branch = %q, want detached HEAD", branch)
	}
	if head := strings.TrimSpace(r.gitIn(t, p.WorktreePath, "rev-parse", "HEAD")); head != p.BaseSHA {
		t.Fatalf("worktree HEAD = %q, want the locked baseline %q", head, p.BaseSHA)
	}

	// The user's checkout is untouched: same HEAD, same branch, no new branch.
	if head := strings.TrimSpace(r.git(t, "rev-parse", "HEAD")); head != p.BaseSHA {
		t.Fatalf("project checkout HEAD = %q, want the untouched baseline %q", head, p.BaseSHA)
	}
	if branch := r.git(t, "rev-parse", "--abbrev-ref", "HEAD"); branch != "main" {
		t.Fatalf("project checkout branch = %q, want main (unchanged)", branch)
	}
	if branches := r.git(t, "branch", "--list"); strings.Contains(branches, xreqID) {
		t.Fatalf("a branch was created in the user's repo for the cross requirement:\n%s", branches)
	}
}

// TestCrossWorkspace_FollowsUpstreamBaseline — 上游推进之后重建工作区，方案
// 必须读到新的提交。这是「先同步最新基础分支的内容」这条要求的正面断言。
func TestCrossWorkspace_FollowsUpstreamBaseline(t *testing.T) {
	r := newCrossWorkspaceRepo(t)
	h, _, projID := newCrossWorkspaceHandler(t, r.projectDir)
	xreqID := seedCrossRequirementWithProject(t, h, projID)

	ws, err := h.buildCrossWorkspace(context.Background(), xreqID, nil)
	if err != nil {
		t.Fatalf("build workspace: %v", err)
	}
	firstSHA := ws.Projects[0].BaseSHA

	// Upstream moves on.
	if err := os.WriteFile(filepath.Join(r.projectDir, "next.md"), []byte("next\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.git(t, "add", "next.md")
	r.git(t, "commit", "-q", "-m", "upstream commit")
	r.git(t, "push", "-q", "origin", "main")

	ws2, err := h.buildCrossWorkspace(context.Background(), xreqID, nil)
	if err != nil {
		t.Fatalf("rebuild workspace: %v", err)
	}
	secondSHA := ws2.Projects[0].BaseSHA
	if secondSHA == firstSHA {
		t.Fatalf("baseline did not move after an upstream commit (%s)", firstSHA)
	}
	head := strings.TrimSpace(r.gitIn(t, ws2.Projects[0].WorktreePath, "rev-parse", "HEAD"))
	if head != secondSHA {
		t.Fatalf("worktree HEAD = %q, want the new baseline %q", head, secondSHA)
	}
}

// TestCrossWorkspace_FetchFailureIsHardError — 拉不到上游基线时必须硬失败，
// 不产出方案。降级只留给「根本没有上游可同步」的仓库。
func TestCrossWorkspace_FetchFailureIsHardError(t *testing.T) {
	r := newCrossWorkspaceRepo(t)
	h, _, projID := newCrossWorkspaceHandler(t, r.projectDir)
	xreqID := seedCrossRequirementWithProject(t, h, projID)

	// Break the remote: origin now points at a directory that does not exist.
	r.git(t, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))

	_, err := h.buildCrossWorkspace(context.Background(), xreqID, nil)
	if err == nil {
		t.Fatal("build workspace succeeded although the baseline could not be fetched")
	}
	if !strings.Contains(err.Error(), "同步项目") {
		t.Fatalf("error = %v, want it to name the failing project", err)
	}
}

// TestCrossWorkspace_NonGitProjectDegrades — 不是 git 仓库的项目没有上游可
// 同步，降级成读共享检出，并且**不**阻断整个方案。
func TestCrossWorkspace_NonGitProjectDegrades(t *testing.T) {
	plain := t.TempDir()
	h, _, projID := newCrossWorkspaceHandler(t, plain)
	xreqID := seedCrossRequirementWithProject(t, h, projID)

	ws, err := h.buildCrossWorkspace(context.Background(), xreqID, nil)
	if err != nil {
		t.Fatalf("build workspace: %v", err)
	}
	p := ws.Projects[0]
	if !p.Degraded {
		t.Fatal("a non-git project must degrade")
	}
	if p.BaseSHA != "" {
		t.Fatalf("degraded project has a baseline SHA %q, want empty", p.BaseSHA)
	}
	if p.ReadPath() != plain {
		t.Fatalf("read path = %q, want the shared checkout %q", p.ReadPath(), plain)
	}
}

// TestCrossWorkspace_UpstreamLacksBranchIsHardError — 上游确实没有这条分支
// 是配置错误，必须点名项目与分支，而不是静默按别的基线写方案。
func TestCrossWorkspace_UpstreamLacksBranchIsHardError(t *testing.T) {
	r := newCrossWorkspaceRepo(t)
	h, _, projID := newCrossWorkspaceHandler(t, r.projectDir)
	xreqID := seedCrossRequirementWithProject(t, h, projID)

	if err := h.svc.SetProjects(xreqID, []model.CrossRequirementProjectInput{
		{ProjectID: projID, BaseBranch: "release/2026q1"},
	}); err != nil {
		t.Fatalf("set projects: %v", err)
	}

	_, err := h.buildCrossWorkspace(context.Background(), xreqID, nil)
	if err == nil {
		t.Fatal("build workspace succeeded although the upstream has no such branch")
	}
	if !strings.Contains(err.Error(), "release/2026q1") {
		t.Fatalf("error = %v, want it to name the missing branch", err)
	}
}

// TestCrossWorkspaceDir_IsKeyedByCrossRequirement — 作业工作目录由跨项目需求
// ID 决定，并且跨作业稳定存在（临时目录跑完就没了，重连时会找不到 cwd）。
func TestCrossWorkspaceDir_IsKeyedByCrossRequirement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	dir, err := crossWorkspaceDir("xreq_abcdef0123456789", crossRunDesign)
	if err != nil {
		t.Fatalf("crossWorkspaceDir: %v", err)
	}
	if !strings.Contains(dir, "xreq_abcdef0123456789") {
		t.Fatalf("workspace dir %q does not carry the cross requirement id", dir)
	}
	if st, sErr := os.Stat(dir); sErr != nil || !st.IsDir() {
		t.Fatalf("workspace dir %q was not created", dir)
	}

	// Stable across calls — a re-run reuses the same directory.
	again, err := crossWorkspaceDir("xreq_abcdef0123456789", crossRunDesign)
	if err != nil {
		t.Fatalf("crossWorkspaceDir (2nd): %v", err)
	}
	if again != dir {
		t.Fatalf("workspace dir changed between runs: %q then %q", dir, again)
	}

	// The design and the split get separate directories.
	splitDir, err := crossWorkspaceDir("xreq_abcdef0123456789", crossRunSplit)
	if err != nil {
		t.Fatalf("crossWorkspaceDir (split): %v", err)
	}
	if splitDir == dir {
		t.Fatal("design and split must not share a working directory")
	}

	removeCrossWorkspaceDir("xreq_abcdef0123456789")
	if _, sErr := os.Stat(dir); sErr == nil {
		t.Fatal("workspace dir survived the cleanup")
	}
}

// TestCrossWorkspaceBases_RoundTrip — 基线台账落库后能读回来。
func TestCrossWorkspaceBases_RoundTrip(t *testing.T) {
	r := newCrossWorkspaceRepo(t)
	h, _, projID := newCrossWorkspaceHandler(t, r.projectDir)
	xreqID := seedCrossRequirementWithProject(t, h, projID)

	ws, err := h.buildCrossWorkspace(context.Background(), xreqID, nil)
	if err != nil {
		t.Fatalf("build workspace: %v", err)
	}
	if err := h.svc.UpsertBases(xreqID, crossWorkspaceBases(ws)); err != nil {
		t.Fatalf("upsert bases: %v", err)
	}
	bases, err := h.svc.ListBases(xreqID)
	if err != nil {
		t.Fatalf("list bases: %v", err)
	}
	if len(bases) != 1 {
		t.Fatalf("bases = %d, want 1", len(bases))
	}
	if bases[0].ProjectID != projID {
		t.Fatalf("base project = %q, want %q", bases[0].ProjectID, projID)
	}
	if bases[0].BaseSHA != ws.Projects[0].BaseSHA {
		t.Fatalf("base SHA = %q, want %q", bases[0].BaseSHA, ws.Projects[0].BaseSHA)
	}
	if bases[0].WorktreePath != ws.Projects[0].WorktreePath {
		t.Fatalf("base worktree = %q, want %q", bases[0].WorktreePath, ws.Projects[0].WorktreePath)
	}
}
