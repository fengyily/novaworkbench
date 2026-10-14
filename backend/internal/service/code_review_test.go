package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// newCodeReviewTestDB opens whatever driver the environment points at
// (matching the convention in attachment_sync_test.go) and seeds the parent
// project row the FK needs. Skips — never fails — when no DB is reachable.
func newCodeReviewTestDB(t *testing.T) (*db.DB, string) {
	t.Helper()
	d, err := db.Init(db.LoadConfig())
	if err != nil {
		t.Skipf("db init (%s) — set NOVA_DB_DRIVER=sqlite + NOVA_DB_PATH", err)
	}
	t.Cleanup(func() { d.Close() })
	projectID := "proj_crvtest"
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		projectID, "code review test", "/tmp/code-review-test"); err != nil {
		t.Skipf("seed project: %v", err)
	}
	return d, projectID
}

func TestCodeReviewService_CreateGetUpdateDelete(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, NewCodeReviewAttachmentService(d))

	rv, err := svc.Create(&model.CodeReview{
		ProjectID:   projectID,
		ReviewType:  model.ReviewTypeBranch,
		Branch:      "feat/x",
		BaseBranch:  "main",
		Description: "把 X 接口改成分页返回",
		CreatedBy:   "usr_test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(rv.ID) })

	if rv.Status != model.ReviewStatusDraft {
		t.Fatalf("status = %q, want draft", rv.Status)
	}
	if len(rv.ID) < 4 || rv.ID[:4] != "crv_" {
		t.Fatalf("id = %q, want a crv_ prefix", rv.ID)
	}

	got, err := svc.Get(rv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Description != "把 X 接口改成分页返回" || got.Branch != "feat/x" {
		t.Fatalf("round trip mismatch: %+v", got)
	}

	updated, err := svc.Update(rv.ID, &model.CodeReview{
		ReviewType:  model.ReviewTypeBranch,
		Branch:      "feat/y",
		BaseBranch:  "develop",
		Title:       "分页改造",
		Description: "改成游标分页",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Branch != "feat/y" || updated.BaseBranch != "develop" || updated.Title != "分页改造" {
		t.Fatalf("update didn't stick: %+v", updated)
	}

	if err := svc.Delete(rv.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(rv.ID); err == nil {
		t.Fatal("expected the row to be gone after delete")
	}
}

func TestCodeReviewService_CreateValidation(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, nil)

	if _, err := svc.Create(&model.CodeReview{ProjectID: projectID, ReviewType: model.ReviewTypeBranch}); err == nil {
		t.Fatal("a branch review without a branch must be rejected")
	}
	if _, err := svc.Create(&model.CodeReview{Branch: "x"}); err == nil {
		t.Fatal("a review without a project must be rejected")
	}
	// PR reviews are allowed to omit branch validation at this layer.
	rv, err := svc.Create(&model.CodeReview{ProjectID: projectID, ReviewType: model.ReviewTypePR, PRNumber: 7})
	if err != nil {
		t.Fatalf("pr review create: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(rv.ID) })
	if rv.PRNumber != 7 || rv.ReviewType != model.ReviewTypePR {
		t.Fatalf("pr row = %+v", rv)
	}
}

// The running→finished transition is the one the stream tail drives; the
// "no edits while running" rule is what keeps a persisted report honest
// about what was actually reviewed.
func TestCodeReviewService_RunningLifecycle(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, nil)

	rv, err := svc.Create(&model.CodeReview{
		ProjectID: projectID, ReviewType: model.ReviewTypeBranch, Branch: "feat/run",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(rv.ID) })

	if err := svc.MarkRunning(rv.ID, "job_abc", "claude-opus-5"); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	running, _ := svc.Get(rv.ID)
	if running.Status != model.ReviewStatusRunning || running.JobID != "job_abc" || running.Model != "claude-opus-5" {
		t.Fatalf("running row = %+v", running)
	}
	if _, err := svc.Update(rv.ID, &model.CodeReview{Branch: "feat/other"}); err == nil {
		t.Fatal("editing a running review must be refused")
	}

	if err := svc.MarkFinished(rv.ID, ReviewOutcome{
		Status:       model.ReviewStatusDone,
		ResultMD:     "## 总体评价\n可以合入",
		FindingsJSON: `[{"severity":"P0","title":"t"}]`,
		P0:           1, P1: 2, P3: 3,
		GoalMet: "partial",
	}); err != nil {
		t.Fatalf("mark finished: %v", err)
	}
	done, _ := svc.Get(rv.ID)
	if done.Status != model.ReviewStatusDone || done.P0Count != 1 || done.P1Count != 2 || done.P3Count != 3 {
		t.Fatalf("finished row = %+v", done)
	}
	if done.GoalMet != "partial" || done.FinishedAt == nil {
		t.Fatalf("goal/finished_at not set: %+v", done)
	}

	// Re-running clears the previous result so the UI never shows stale
	// findings next to a live stream.
	if err := svc.MarkRunning(rv.ID, "job_def", "m"); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	again, _ := svc.Get(rv.ID)
	if again.ResultMD != "" || again.FindingsJSON != "" || again.P0Count != 0 || again.FinishedAt != nil {
		t.Fatalf("re-run didn't clear the previous result: %+v", again)
	}
}

func TestCodeReviewService_ListByProjectNewestFirst(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, nil)

	var ids []string
	for _, b := range []string{"feat/1", "feat/2", "feat/3"} {
		rv, err := svc.Create(&model.CodeReview{
			ProjectID: projectID, ReviewType: model.ReviewTypeBranch, Branch: b,
		})
		if err != nil {
			t.Fatalf("create %s: %v", b, err)
		}
		ids = append(ids, rv.ID)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_ = svc.Delete(id)
		}
	})

	rows, err := svc.ListByProject(projectID, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) < 3 {
		t.Fatalf("len(rows) = %d, want >= 3", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].CreatedAt.After(rows[i-1].CreatedAt) {
			t.Fatalf("rows are not newest-first at %d", i)
		}
	}
	if capped, err := svc.ListByProject(projectID, 2); err != nil || len(capped) != 2 {
		t.Fatalf("limit=2 → %d rows, err=%v", len(capped), err)
	}
}

// R10: deleting a session must reclaim the on-disk attachment bytes, not
// just the rows the FK cascade would take care of.
func TestCodeReviewService_DeleteReclaimsAttachmentFiles(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)

	root := t.TempDir()
	oldRoot := attachmentRoot
	attachmentRoot = root
	t.Cleanup(func() { attachmentRoot = oldRoot })

	attach := NewCodeReviewAttachmentService(d)
	svc := NewCodeReviewService(d, attach)

	rv, err := svc.Create(&model.CodeReview{
		ProjectID: projectID, ReviewType: model.ReviewTypeBranch, Branch: "feat/att",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	a := mustUpload(t, attach, rv.ID, "design.md", []byte("# design\n"), "设计稿")
	abs := attach.AbsolutePath(&a)
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("uploaded file should exist: %v", err)
	}
	if a.OwnerID != rv.ID {
		t.Fatalf("owner_id = %q, want %q", a.OwnerID, rv.ID)
	}

	if err := svc.Delete(rv.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(abs); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("attachment file survived the delete: %v", err)
	}
	if rows, err := attach.List(rv.ID); err != nil || len(rows) != 0 {
		t.Fatalf("attachment rows = %d, err = %v", len(rows), err)
	}
}

// R4: the two AttachmentService instances must not see each other's rows —
// that isolation is what makes "one handler, two tables" safe.
func TestAttachmentService_ReviewScopeIsolation(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)

	root := t.TempDir()
	oldRoot := attachmentRoot
	attachmentRoot = root
	t.Cleanup(func() { attachmentRoot = oldRoot })

	reqAttach := NewAttachmentService(d)
	reviewAttach := NewCodeReviewAttachmentService(d)
	crSvc := NewCodeReviewService(d, reviewAttach)

	reqID := "req_" + util.NewID("scope")[:12]
	createReqForTest(t, d, reqID)
	rv, err := crSvc.Create(&model.CodeReview{
		ProjectID: projectID, ReviewType: model.ReviewTypeBranch, Branch: "feat/scope",
	})
	if err != nil {
		t.Fatalf("create review: %v", err)
	}
	t.Cleanup(func() { _ = crSvc.Delete(rv.ID) })

	reqAtt := mustUpload(t, reqAttach, reqID, "req.log", []byte("requirement side\n"), "")
	rvAtt := mustUpload(t, reviewAttach, rv.ID, "review.log", []byte("review side\n"), "")

	reqRows, err := reqAttach.List(reqID)
	if err != nil {
		t.Fatalf("list req: %v", err)
	}
	if len(reqRows) != 1 || reqRows[0].ID != reqAtt.ID {
		t.Fatalf("requirement side sees %+v", reqRows)
	}
	rvRows, err := reviewAttach.List(rv.ID)
	if err != nil {
		t.Fatalf("list review: %v", err)
	}
	if len(rvRows) != 1 || rvRows[0].ID != rvAtt.ID {
		t.Fatalf("review side sees %+v", rvRows)
	}
	// Cross lookups must miss entirely.
	if rows, _ := reqAttach.List(rv.ID); len(rows) != 0 {
		t.Fatalf("requirement service leaked review rows: %+v", rows)
	}
	if _, err := reviewAttach.Get(reqAtt.ID); err == nil {
		t.Fatal("review service resolved a requirement attachment id")
	}

	// Manifests are per-owner too.
	m := reviewAttach.BuildManifest(rv.ID, filepath.Join(attachmentRoot, rv.ID))
	if m == "" || !strings.Contains(m, "review.log") || strings.Contains(m, "req.log") {
		t.Fatalf("review manifest = %q", m)
	}
	_ = reqAttach.Delete(reqAtt.ID)
}
