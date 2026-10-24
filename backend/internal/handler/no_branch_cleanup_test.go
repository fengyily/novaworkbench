package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// noBranchCleanupFixture wires a MergeHandler against a throwaway DB + git
// repo so the cleanup endpoint can be driven end to end.
type noBranchCleanupFixture struct {
	repo    *noBranchRepo
	row     *model.Requirement
	handler *MergeHandler
	db      *db.DB
}

// newNoBranchCleanupFixture builds the DB + requirement row. The requirement
// is created BEFORE the worktree so the worktree path can be keyed on the
// real requirement id (that is what the drift guard compares against).
func newNoBranchCleanupFixture(t *testing.T) *noBranchCleanupFixture {
	t.Helper()
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
	return &noBranchCleanupFixture{
		repo:    r,
		row:     row,
		db:      d,
		handler: NewMergeHandler(d, projectSvc, reqSvc, nil, nil, nil, platformSvc, nil, nil, nil, nil, nil, nil, nil),
	}
}

// cleanup drives the cleanup endpoint for the fixture's requirement.
func (f *noBranchCleanupFixture) cleanup(t *testing.T, force bool) *httptest.ResponseRecorder {
	t.Helper()
	body := strings.NewReader(`{"force":` + boolJSON(force) + `}`)
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/"+f.row.ID+"/worktree/cleanup", body)
	req.SetPathValue("id", f.row.ID)
	rec := httptest.NewRecorder()
	f.handler.Cleanup(rec, req)
	return rec
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestCleanupNoBranchRemovesWorktree pins the new behaviour: a no-branch
// requirement owns a REAL isolated worktree now, so "clean up" must actually
// remove it (and clear the persisted fields) instead of short-circuiting.
//
// Pre-fix the no-branch branch returned early with "nothing to clean", which
// left the detached worktree (and its commits) on disk forever.
func TestCleanupNoBranchRemovesWorktree(t *testing.T) {
	f := newNoBranchCleanupFixture(t)

	wtPath, err := ensureDetachedWorktreeCore(f.repo.projectDir, f.row.ID, "main", nil)
	if err != nil {
		t.Fatalf("ensureDetachedWorktreeCore: %v", err)
	}
	if err := f.handler.reqSvc.UpdateNoBranch(f.row.ID, true); err != nil {
		t.Fatalf("UpdateNoBranch: %v", err)
	}
	if err := f.handler.reqSvc.UpdateWorktree(f.row.ID, "", wtPath); err != nil {
		t.Fatalf("UpdateWorktree: %v", err)
	}

	rec := f.cleanup(t, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(wtPath); err == nil {
		t.Fatalf("worktree %s still on disk after cleanup", wtPath)
	}
	got, err := f.handler.reqSvc.Get(f.row.ID)
	if err != nil {
		t.Fatalf("reload requirement: %v", err)
	}
	if got.WorktreePath != "" || got.BranchName != "" {
		t.Fatalf("persisted worktree fields not cleared: worktree_path=%q branch_name=%q", got.WorktreePath, got.BranchName)
	}
}

// TestCleanupNoBranchLegacyRowIsNoop covers the rows written by an older
// build: they developed straight in the project directory, so there is no
// worktree to remove. The endpoint must succeed without touching anything.
func TestCleanupNoBranchLegacyRowIsNoop(t *testing.T) {
	f := newNoBranchCleanupFixture(t)

	if err := f.handler.reqSvc.UpdateNoBranch(f.row.ID, true); err != nil {
		t.Fatalf("UpdateNoBranch: %v", err)
	}
	// Legacy row: no worktree_path at all.

	rec := f.cleanup(t, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Success bool `json:"success"`
		Data    struct {
			OK       bool   `json:"ok"`
			Message  string `json:"message"`
			NoBranch bool   `json:"no_branch"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !envelope.Success || !envelope.Data.OK {
		t.Fatalf("ok = false, want true: %s", rec.Body.String())
	}
	if !strings.Contains(envelope.Data.Message, "无 worktree") {
		t.Fatalf("message = %q, want it to mention that there is no worktree", envelope.Data.Message)
	}
	// The project checkout must be untouched.
	if _, err := os.Stat(f.repo.projectDir); err != nil {
		t.Fatalf("project dir disappeared: %v", err)
	}
}

// TestCleanupNoBranchRefusesUnpushedCommits is the safety guard for the
// detached worktree: a detached HEAD's COMMITS are invisible to the
// "uncommitted changes" check, so deleting the worktree would silently lose
// work that was never pushed. The endpoint must refuse (409) unless the
// caller forces it.
func TestCleanupNoBranchRefusesUnpushedCommits(t *testing.T) {
	f := newNoBranchCleanupFixture(t)

	wtPath, err := ensureDetachedWorktreeCore(f.repo.projectDir, f.row.ID, "main", nil)
	if err != nil {
		t.Fatalf("ensureDetachedWorktreeCore: %v", err)
	}
	// A commit that was never pushed to the base branch.
	f.repo.writeFileIn(t, wtPath, "feature.txt", "unshipped work\n")
	f.repo.commitAll(t, wtPath, "unshipped work")

	if err := f.handler.reqSvc.UpdateNoBranch(f.row.ID, true); err != nil {
		t.Fatalf("UpdateNoBranch: %v", err)
	}
	if err := f.handler.reqSvc.UpdateBaseBranch(f.row.ID, "main"); err != nil {
		t.Fatalf("UpdateBaseBranch: %v", err)
	}
	if err := f.handler.reqSvc.UpdateWorktree(f.row.ID, "", wtPath); err != nil {
		t.Fatalf("UpdateWorktree: %v", err)
	}

	// Without force the cleanup must refuse.
	rec := f.cleanup(t, false)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "WORKTREE_UNPUSHED") {
		t.Fatalf("expected the WORKTREE_UNPUSHED error code, got: %s", rec.Body.String())
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree must NOT be removed when the push is refused: %v", err)
	}

	// With force it goes through.
	rec = f.cleanup(t, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("forced status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(wtPath); err == nil {
		t.Fatalf("worktree %s still on disk after a forced cleanup", wtPath)
	}
}
