package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestCodeReviewService_RequirementScopedRoundChain covers the requirement
// page's read path: a requirement-level review carries requirement_id +
// role_key, and ListByRequirement returns exactly those rows (project-level
// reviews stay out of the list).
func TestCodeReviewService_RequirementScopedRoundChain(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, NewCodeReviewAttachmentService(d))

	reqID := "req_cr_chain"
	if _, err := d.Exec(
		`INSERT INTO requirements (id, title) VALUES (?, ?) ON CONFLICT(id) DO NOTHING`,
		reqID, "requirement review"); err != nil {
		t.Skipf("seed requirement: %v", err)
	}

	// A project-level review — must NOT show up in the requirement's list.
	projRv, err := svc.Create(&model.CodeReview{
		ProjectID:  projectID,
		ReviewType: model.ReviewTypeBranch,
		Branch:     "feat/x",
	})
	if err != nil {
		t.Fatalf("create project review: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(projRv.ID) })

	rv, err := svc.Create(&model.CodeReview{
		ProjectID:     projectID,
		ReviewType:    model.ReviewTypeRequirement,
		RequirementID: reqID,
		RoleKey:       "reviewer",
		Branch:        "feat/req_x",
		BaseBranch:    "main",
		Description:   "把 X 改成分页",
	})
	if err != nil {
		t.Fatalf("create requirement review: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(rv.ID) })

	if rv.RequirementID != reqID {
		t.Fatalf("RequirementID = %q, want %q", rv.RequirementID, reqID)
	}
	if rv.RoleKey != "reviewer" {
		t.Fatalf("RoleKey = %q, want reviewer", rv.RoleKey)
	}

	rows, err := svc.ListByRequirement(reqID)
	if err != nil {
		t.Fatalf("ListByRequirement: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListByRequirement returned %d rows, want 1", len(rows))
	}
	if rows[0].ID != rv.ID {
		t.Fatalf("ListByRequirement returned %q, want %q", rows[0].ID, rv.ID)
	}

	// A requirement-level review does NOT need a branch (the "不创建分支"
	// mode develops on the base branch), so the branch requirement must be
	// relaxed for that review type.
	if _, err := svc.Create(&model.CodeReview{
		ProjectID:     projectID,
		ReviewType:    model.ReviewTypeRequirement,
		RequirementID: reqID,
	}); err != nil {
		t.Fatalf("branch-less requirement review must be allowed: %v", err)
	}
}

// TestReviewVerdict pins the single source of truth for "did this round
// pass": P0 and P1 block, P3 does not, and a running / errored round is
// neither.
func TestReviewVerdict(t *testing.T) {
	cases := []struct {
		name   string
		status string
		p0, p1 int
		want   string
	}{
		{"clean", model.ReviewStatusDone, 0, 0, "pass"},
		{"p3 only", model.ReviewStatusDone, 0, 0, "pass"},
		{"p0 blocks", model.ReviewStatusDone, 1, 0, "fail"},
		{"p1 blocks", model.ReviewStatusDone, 0, 2, "fail"},
		{"running", model.ReviewStatusRunning, 0, 0, "pending"},
		{"draft", model.ReviewStatusDraft, 0, 0, "pending"},
		{"error", model.ReviewStatusError, 0, 0, "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReviewVerdict(tc.status, tc.p0, tc.p1); got != tc.want {
				t.Fatalf("ReviewVerdict(%q, %d, %d) = %q, want %q", tc.status, tc.p0, tc.p1, got, tc.want)
			}
		})
	}
}

// TestCodeReviewService_MarkReviewFailed pins the failure path: a Review
// whose run died must leave the 'running' state, or the requirement page
// would show a spinner that never resolves.
func TestCodeReviewService_MarkReviewFailed(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, NewCodeReviewAttachmentService(d))

	rv, err := svc.Create(&model.CodeReview{
		ProjectID:  projectID,
		ReviewType: model.ReviewTypeRequirement,
		Branch:     "feat/x",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(rv.ID) })

	if err := svc.MarkRunning(rv.ID, "job_1", "sonnet"); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := svc.MarkReviewFailed(rv.ID, "❌ boom"); err != nil {
		t.Fatalf("MarkReviewFailed: %v", err)
	}
	got, err := svc.Get(rv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != model.ReviewStatusError {
		t.Fatalf("status = %q, want error", got.Status)
	}
	if got.ResultMD != "❌ boom" {
		t.Fatalf("result_md = %q, want the error message", got.ResultMD)
	}
	if got.Verdict != "error" {
		t.Fatalf("verdict = %q, want error", got.Verdict)
	}
}

// TestCodeReviewService_RecoverInterruptedRunning pins the boot-time
// recovery: a requirement-level Review whose run died with the backend must
// not stay 'running' forever, or the requirement page shows a spinner for a
// round that no longer exists.
//
// Project-level rows are deliberately untouched — their session can simply
// be re-run from the history list, and flipping them would lose the draft
// state.
func TestCodeReviewService_RecoverInterruptedRunning(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, NewCodeReviewAttachmentService(d))

	reqRv, err := svc.Create(&model.CodeReview{
		ProjectID:     projectID,
		ReviewType:    model.ReviewTypeRequirement,
		RequirementID: "req_recover",
		Branch:        "feat/x",
	})
	if err != nil {
		t.Fatalf("create requirement review: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(reqRv.ID) })

	projRv, err := svc.Create(&model.CodeReview{
		ProjectID:  projectID,
		ReviewType: model.ReviewTypeBranch,
		Branch:     "feat/y",
	})
	if err != nil {
		t.Fatalf("create project review: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(projRv.ID) })

	for _, id := range []string{reqRv.ID, projRv.ID} {
		if err := svc.MarkRunning(id, "job_x", "sonnet"); err != nil {
			t.Fatalf("MarkRunning(%s): %v", id, err)
		}
	}

	n, err := svc.RecoverInterruptedRunning()
	if err != nil {
		t.Fatalf("RecoverInterruptedRunning: %v", err)
	}
	if n != 1 {
		t.Fatalf("recovered %d rows, want 1 (the requirement-scoped one)", n)
	}

	got, err := svc.Get(reqRv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != model.ReviewStatusError {
		t.Fatalf("requirement review status = %q, want error", got.Status)
	}
	if got.Verdict != "error" {
		t.Fatalf("verdict = %q, want error", got.Verdict)
	}

	stillRunning, err := svc.Get(projRv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stillRunning.Status != model.ReviewStatusRunning {
		t.Fatalf("project review status = %q, want it left running", stillRunning.Status)
	}
}
