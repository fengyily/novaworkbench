package service

import (
	"errors"
	"testing"

	"database/sql"

	"github.com/novaworkbench/backend/internal/model"
)

// seedFinishedReview creates a review and drives it to a finished report, the
// state every revision operation starts from.
func seedFinishedReview(t *testing.T, svc *CodeReviewService, projectID, report string) *model.CodeReview {
	t.Helper()
	rv, err := svc.Create(&model.CodeReview{
		ProjectID:   projectID,
		ReviewType:  model.ReviewTypeBranch,
		Branch:      "feat/revisions",
		BaseBranch:  "main",
		Description: "给 Review 报告加版本历史",
		CreatedBy:   "usr_test",
	})
	if err != nil {
		t.Fatalf("create review: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(rv.ID) })

	if err := svc.MarkRunning(rv.ID, "job_1", "claude-opus-5"); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := svc.MarkFinished(rv.ID, ReviewOutcome{
		Status:         model.ReviewStatusDone,
		ResultMD:       report,
		FindingsJSON:   `[{"severity":"P0","title":"空指针"}]`,
		P0:             1,
		GoalMet:        "partial",
		GoalChecksJSON: `[{"criterion":"分页生效","status":"yes","evidence":"api.go:42"}]`,
	}); err != nil {
		t.Fatalf("mark finished: %v", err)
	}
	got, err := svc.Get(rv.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return got
}

func TestRevisionService_SnapshotAssignsSequentialNumbers(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, NewCodeReviewAttachmentService(d))
	revs := NewCodeReviewRevisionService(d)

	rv := seedFinishedReview(t, svc, projectID, "# v1 报告")

	first, err := revs.Snapshot(rv, model.RevisionTriggerInitial, 0)
	if err != nil {
		t.Fatalf("snapshot v1: %v", err)
	}
	if first.RevNo != 1 {
		t.Fatalf("first rev_no = %d, want 1", first.RevNo)
	}
	if first.ID[:4] != "crr_" {
		t.Fatalf("id = %q, want a crr_ prefix", first.ID)
	}
	// The whole point of the snapshot: the evidence table and the counts
	// travel with the report, not just its Markdown.
	if first.GoalChecksJSON == "" || first.P0Count != 1 || first.GoalMet != "partial" {
		t.Fatalf("snapshot lost graded fields: %+v", first)
	}
	// Snapshot stamps the review so "current version" and "newest revision"
	// agree without a second query.
	if rv.RevisionNo != 1 {
		t.Fatalf("review revision_no = %d, want 1", rv.RevisionNo)
	}
	stored, err := svc.Get(rv.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.RevisionNo != 1 {
		t.Fatalf("persisted revision_no = %d, want 1", stored.RevisionNo)
	}

	rv.ResultMD = "# v2 报告"
	second, err := revs.Snapshot(rv, model.RevisionTriggerRegenerate, 3)
	if err != nil {
		t.Fatalf("snapshot v2: %v", err)
	}
	if second.RevNo != 2 {
		t.Fatalf("second rev_no = %d, want 2", second.RevNo)
	}
	if second.Trigger != model.RevisionTriggerRegenerate || second.FollowupCount != 3 {
		t.Fatalf("v2 metadata = %q/%d, want regenerate/3", second.Trigger, second.FollowupCount)
	}

	list, err := revs.ListByReview(rv.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].RevNo != 1 || list[1].RevNo != 2 {
		t.Fatalf("list = %+v, want v1 then v2", list)
	}
}

// A report-less review must not produce a selectable version that renders
// nothing — Snapshot returns (nil, nil) rather than inserting an empty row.
func TestRevisionService_SnapshotSkipsEmptyReport(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, NewCodeReviewAttachmentService(d))
	revs := NewCodeReviewRevisionService(d)

	rv, err := svc.Create(&model.CodeReview{
		ProjectID: projectID, ReviewType: model.ReviewTypeBranch,
		Branch: "feat/empty", CreatedBy: "usr_test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(rv.ID) })

	rev, err := revs.Snapshot(rv, model.RevisionTriggerInitial, 0)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if rev != nil {
		t.Fatalf("snapshot = %+v, want nil for a draft with no report", rev)
	}
	list, err := revs.ListByReview(rv.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("list = %+v, want empty", list)
	}
}

// Restoring is what makes regeneration safe to try: v1 comes back in full,
// and v2 stays on the list so the user can change their mind again.
func TestRevisionService_RestoreAsCurrent(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, NewCodeReviewAttachmentService(d))
	revs := NewCodeReviewRevisionService(d)

	rv := seedFinishedReview(t, svc, projectID, "# v1 报告")
	if _, err := revs.Snapshot(rv, model.RevisionTriggerInitial, 0); err != nil {
		t.Fatalf("snapshot v1: %v", err)
	}

	// A regeneration lands a second report through the ordinary path.
	if err := svc.MarkFinished(rv.ID, ReviewOutcome{
		Status:       model.ReviewStatusDone,
		ResultMD:     "# v2 报告",
		FindingsJSON: `[]`,
		GoalMet:      "yes",
	}); err != nil {
		t.Fatalf("mark finished v2: %v", err)
	}
	v2, err := svc.Get(rv.ID)
	if err != nil {
		t.Fatalf("reload v2: %v", err)
	}
	if _, err := revs.Snapshot(v2, model.RevisionTriggerRegenerate, 2); err != nil {
		t.Fatalf("snapshot v2: %v", err)
	}

	if err := revs.RestoreAsCurrent(rv.ID, 1); err != nil {
		t.Fatalf("restore v1: %v", err)
	}
	back, err := svc.Get(rv.ID)
	if err != nil {
		t.Fatalf("reload after restore: %v", err)
	}
	if back.ResultMD != "# v1 报告" || back.P0Count != 1 || back.GoalMet != "partial" {
		t.Fatalf("restore did not bring v1 back: %+v", back)
	}
	if back.RevisionNo != 1 {
		t.Fatalf("revision_no after restore = %d, want 1", back.RevisionNo)
	}
	// Restoring selects a version, it does not delete the others.
	list, err := revs.ListByReview(rv.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list after restore has %d entries, want both versions", len(list))
	}
}

// A review that finished before this table existed has result_md but no
// revision row. EnsureSnapshot backfills it so a regeneration always has
// something to roll back to — and is a no-op once it has.
func TestRevisionService_EnsureSnapshotBackfillsLegacyRow(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, NewCodeReviewAttachmentService(d))
	revs := NewCodeReviewRevisionService(d)

	rv := seedFinishedReview(t, svc, projectID, "# 旧报告")
	// Mimic the ALTER default: a legacy row claims v1 with no revision behind it.
	if _, err := d.Exec(`UPDATE code_reviews SET revision_no = 1 WHERE id = ?`, rv.ID); err != nil {
		t.Fatalf("fake legacy row: %v", err)
	}
	rv.RevisionNo = 1

	if err := revs.EnsureSnapshot(rv, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	list, err := revs.ListByReview(rv.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ResultMD != "# 旧报告" {
		t.Fatalf("list = %+v, want the legacy report backfilled as v1", list)
	}

	// Second call must not duplicate it.
	if err := revs.EnsureSnapshot(rv, 0); err != nil {
		t.Fatalf("ensure (second): %v", err)
	}
	list, err = revs.ListByReview(rv.ID)
	if err != nil {
		t.Fatalf("list (second): %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list has %d entries after a repeat ensure, want 1", len(list))
	}
}

// MarkRegenerating is MarkRunning's non-destructive twin: the report the user
// is reading has to survive the run that is trying to replace it.
func TestCodeReviewService_MarkRegeneratingKeepsReport(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, NewCodeReviewAttachmentService(d))

	rv := seedFinishedReview(t, svc, projectID, "# 现有报告")
	if err := svc.SetClaudeSessionID(rv.ID, "sess-abc"); err != nil {
		t.Fatalf("set session: %v", err)
	}

	if err := svc.MarkRegenerating(rv.ID, "job_regen"); err != nil {
		t.Fatalf("mark regenerating: %v", err)
	}
	during, err := svc.Get(rv.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if during.Status != model.ReviewStatusRunning || during.JobID != "job_regen" {
		t.Fatalf("status/job = %q/%q, want running/job_regen", during.Status, during.JobID)
	}
	if during.ResultMD != "# 现有报告" || during.P0Count != 1 || during.FindingsJSON == "" {
		t.Fatalf("regenerating wiped the previous report: %+v", during)
	}
	// Clearing the session would silently downgrade every regeneration to a
	// cold run.
	if during.ClaudeSessionID != "sess-abc" {
		t.Fatalf("claude_session_id = %q, want it preserved", during.ClaudeSessionID)
	}

	if err := svc.MarkRegenerateFailed(rv.ID); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	after, err := svc.Get(rv.ID)
	if err != nil {
		t.Fatalf("reload after failure: %v", err)
	}
	if after.Status != model.ReviewStatusDone || after.ResultMD != "# 现有报告" {
		t.Fatalf("failed regeneration did not restore the review: %+v", after)
	}
}

// Deleting a review must take its version history with it — the CASCADE in
// the DDL never fires on SQLite, and an orphan revision keeps a full report
// alive forever.
func TestCodeReviewService_DeleteDropsRevisions(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, NewCodeReviewAttachmentService(d))
	revs := NewCodeReviewRevisionService(d)

	rv := seedFinishedReview(t, svc, projectID, "# 待删除")
	if _, err := revs.Snapshot(rv, model.RevisionTriggerInitial, 0); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := svc.Delete(rv.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, err := revs.ListByReview(rv.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("list = %+v, want no revisions after the review is deleted", list)
	}
	if _, err := revs.Get(rv.ID, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("get after delete err = %v, want sql.ErrNoRows", err)
	}
}
