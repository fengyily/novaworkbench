package service

import (
	"errors"
	"testing"

	"database/sql"

	"github.com/novaworkbench/backend/internal/model"
)

// newFollowupFixture seeds a review row for the thread to hang off, reusing
// the code-review harness (which skips when no DB is reachable).
func newFollowupFixture(t *testing.T) (*CodeReviewService, *CodeReviewFollowupService, *model.CodeReview) {
	t.Helper()
	d, projectID := newCodeReviewTestDB(t)
	reviewSvc := NewCodeReviewService(d, NewCodeReviewAttachmentService(d))
	rv, err := reviewSvc.Create(&model.CodeReview{
		ProjectID:  projectID,
		ReviewType: model.ReviewTypeBranch,
		Branch:     "feat/followup",
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("create review: %v", err)
	}
	t.Cleanup(func() { _ = reviewSvc.Delete(rv.ID) })
	return reviewSvc, NewCodeReviewFollowupService(d), rv
}

func TestFollowupService_CreateListFinish(t *testing.T) {
	_, svc, rv := newFollowupFixture(t)

	fu, err := svc.Create(&model.CodeReviewFollowup{
		ReviewID:  rv.ID,
		Question:  "P0 第二条为什么会导致数据损坏？",
		JobID:     "job_1",
		CreatedBy: "usr_test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if fu.Status != model.FollowupStatusRunning {
		t.Fatalf("status = %q, want running", fu.Status)
	}
	if fu.ID[:4] != "crf_" {
		t.Fatalf("id = %q, want a crf_ prefix", fu.ID)
	}
	if fu.Resumed {
		t.Fatal("a fresh row must not claim it resumed anything")
	}

	// A blank question is the one input that makes the whole turn pointless.
	if _, err := svc.Create(&model.CodeReviewFollowup{ReviewID: rv.ID, Question: "  "}); err == nil {
		t.Fatal("expected an error for a blank question")
	}

	if err := svc.MarkFinished(fu.ID, model.FollowupStatusDone, "因为写入路径没有事务边界…", "claude-opus-5", true); err != nil {
		t.Fatalf("mark finished: %v", err)
	}
	got, err := svc.Get(fu.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.FollowupStatusDone || got.Answer == "" || !got.Resumed {
		t.Fatalf("finished row = %+v, want done/answered/resumed", got)
	}
	if got.FinishedAt == nil {
		t.Fatal("finished_at not stamped")
	}

	rows, err := svc.ListByReview(rv.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != fu.ID {
		t.Fatalf("list = %+v, want exactly the one turn", rows)
	}
}

// Deleting a review has to take its thread with it. SQLite never enables
// PRAGMA foreign_keys, so this would silently leak rows if CodeReviewService
// .Delete relied on the FK cascade alone.
func TestCodeReviewDelete_RemovesFollowups(t *testing.T) {
	reviewSvc, svc, rv := newFollowupFixture(t)

	fu, err := svc.Create(&model.CodeReviewFollowup{ReviewID: rv.ID, Question: "这条 P1 的影响范围？"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := reviewSvc.Delete(rv.ID); err != nil {
		t.Fatalf("delete review: %v", err)
	}
	if _, err := svc.Get(fu.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("follow-up survived its review: err = %v", err)
	}
}

// The review's claude session id is what a follow-up resumes; a re-run must
// clear it so the next 追问 can't resume a conversation that no longer
// matches the report on screen.
func TestCodeReviewService_SessionIDLifecycle(t *testing.T) {
	reviewSvc, _, rv := newFollowupFixture(t)

	if err := reviewSvc.SetClaudeSessionID(rv.ID, "sess-abc"); err != nil {
		t.Fatalf("set session id: %v", err)
	}
	got, err := reviewSvc.Get(rv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ClaudeSessionID != "sess-abc" {
		t.Fatalf("claude_session_id = %q, want sess-abc", got.ClaudeSessionID)
	}

	// An empty id is a no-op rather than an erase: callers hand us whatever
	// the stream gave them, and a stream that never emitted one must not
	// wipe a good id from an earlier run.
	if err := reviewSvc.SetClaudeSessionID(rv.ID, ""); err != nil {
		t.Fatalf("set empty session id: %v", err)
	}
	if got, _ = reviewSvc.Get(rv.ID); got.ClaudeSessionID != "sess-abc" {
		t.Fatalf("claude_session_id = %q after a blank set, want it untouched", got.ClaudeSessionID)
	}

	if err := reviewSvc.MarkRunning(rv.ID, "job_2", "claude-opus-5"); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if got, _ = reviewSvc.Get(rv.ID); got.ClaudeSessionID != "" {
		t.Fatalf("claude_session_id = %q after a re-run, want it cleared", got.ClaudeSessionID)
	}
}
