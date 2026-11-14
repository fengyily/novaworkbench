package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// UpdateParsedFindings back-fills the structured columns of a finished
// review WITHOUT touching the run's bookkeeping.
//
// The point of the whole exercise: a report whose contract block was
// mis-parsed has a perfectly good result_md and empty grading columns.
// Re-parsing fills the grading in — but it must NOT pretend a new run
// happened, so status / job_id / finished_at / revision_no stay exactly as
// the run left them.
func TestUpdateParsedFindings_KeepsRunBookkeeping(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, NewCodeReviewAttachmentService(d))

	rv, err := svc.Create(&model.CodeReview{
		ProjectID:  projectID,
		ReviewType: model.ReviewTypeBranch,
		Branch:     "feat/x",
		BaseBranch: "main",
		CreatedBy:  "usr_test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(rv.ID) })

	if err := svc.MarkRunning(rv.ID, "job_reparse", "claude-opus-5"); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := svc.MarkFinished(rv.ID, ReviewOutcome{
		Status:   model.ReviewStatusDone,
		ResultMD: "## 总体评价\n\n正文",
	}); err != nil {
		t.Fatalf("mark finished: %v", err)
	}
	before, err := svc.Get(rv.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	err = svc.UpdateParsedFindings(rv.ID, ReviewOutcome{
		ResultMD:       "## 总体评价\n\n正文",
		FindingsJSON:   `[{"severity":"P0","title":"空指针"}]`,
		P0:             1,
		GoalMet:        "partial",
		GoalChecksJSON: `[{"criterion":"分页生效","status":"yes","evidence":"api.go:42"}]`,
	})
	if err != nil {
		t.Fatalf("update parsed findings: %v", err)
	}

	after, err := svc.Get(rv.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.FindingsJSON == "" || after.P0Count != 1 || after.GoalMet != "partial" || after.GoalChecksJSON == "" {
		t.Fatalf("structured columns not filled: %+v", after)
	}
	// The run's own bookkeeping must be untouched — a re-parse is not a run.
	if after.Status != before.Status {
		t.Errorf("status = %q, want %q (unchanged)", after.Status, before.Status)
	}
	if after.JobID != before.JobID {
		t.Errorf("job_id = %q, want %q (unchanged)", after.JobID, before.JobID)
	}
	if after.RevisionNo != before.RevisionNo {
		t.Errorf("revision_no = %d, want %d (unchanged)", after.RevisionNo, before.RevisionNo)
	}
	if after.FinishedAt == nil || before.FinishedAt == nil {
		t.Fatalf("finished_at went missing: before=%v after=%v", before.FinishedAt, after.FinishedAt)
	}
	if !after.FinishedAt.Equal(*before.FinishedAt) {
		t.Errorf("finished_at = %v, want %v (unchanged)", after.FinishedAt, before.FinishedAt)
	}
}

// UpdateCurrent corrects the FROZEN version in place.
//
// A re-parse produces no new report, so the version history must not grow:
// the version the review already serves is rewritten with the corrected
// grading, keeping the same rev_no.
func TestUpdateCurrent_RewritesTheSameVersion(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, NewCodeReviewAttachmentService(d))
	revs := NewCodeReviewRevisionService(d)

	rv := seedFinishedReview(t, svc, projectID, "# v1 报告")
	if _, err := revs.Snapshot(rv, model.RevisionTriggerInitial, 0); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// The re-parse recovered a grading the first run failed to persist.
	rv.ResultMD = "# v1 报告"
	rv.FindingsJSON = `[{"severity":"P1","title":"竞态"}]`
	rv.P0Count, rv.P1Count, rv.P3Count = 0, 1, 0
	rv.GoalMet = "yes"
	rv.GoalChecksJSON = `[{"criterion":"分页生效","status":"yes","evidence":"api.go:42"}]`
	if err := revs.UpdateCurrent(rv); err != nil {
		t.Fatalf("update current: %v", err)
	}

	rows, err := revs.ListByReview(rv.ID)
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(revisions) = %d, want 1 (a re-parse must not add a version)", len(rows))
	}
	if rows[0].P1Count != 1 || rows[0].GoalMet != "yes" {
		t.Fatalf("version not corrected: %+v", rows[0])
	}
	if rows[0].P0Count != 0 {
		t.Errorf("p0_count = %d, want 0", rows[0].P0Count)
	}
}
