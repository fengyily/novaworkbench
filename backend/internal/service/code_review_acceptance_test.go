package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestCodeReviewService_AcceptanceRowsStayOutOfTheRoundChain pins the
// read-path split between the two requirement-scoped review kinds.
//
// The requirement page renders TWO lists from the same table: the Review
// round chain (which has rounds, findings and fix sub-tasks) and the
// acceptance checks (which have a verdict and a gap list). Letting an
// acceptance row into the round chain would put a row there that the
// round-chain UI cannot render — and vice versa.
func TestCodeReviewService_AcceptanceRowsStayOutOfTheRoundChain(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, NewCodeReviewAttachmentService(d))

	reqID := "req_cr_acceptance"
	if _, err := d.Exec(
		`INSERT INTO requirements (id, project_id, title) VALUES (?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		reqID, projectID, "acceptance master"); err != nil {
		t.Skipf("seed requirement: %v", err)
	}

	rv, err := svc.Create(&model.CodeReview{
		ProjectID:     projectID,
		ReviewType:    model.ReviewTypeRequirement,
		RequirementID: reqID,
		Branch:        "feat/req_x",
	})
	if err != nil {
		t.Fatalf("create requirement review: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(rv.ID) })

	acc, err := svc.Create(&model.CodeReview{
		ProjectID:     projectID,
		ReviewType:    model.ReviewTypeAcceptance,
		RequirementID: reqID,
	})
	if err != nil {
		t.Fatalf("create acceptance check: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(acc.ID) })

	rounds, err := svc.ListByRequirement(reqID)
	if err != nil {
		t.Fatalf("ListByRequirement: %v", err)
	}
	if len(rounds) != 1 {
		t.Fatalf("ListByRequirement returned %d rows, want 1 (the acceptance row must stay out)", len(rounds))
	}
	if rounds[0].ID != rv.ID {
		t.Fatalf("ListByRequirement returned %q, want %q", rounds[0].ID, rv.ID)
	}

	checks, err := svc.ListAcceptanceChecks(reqID)
	if err != nil {
		t.Fatalf("ListAcceptanceChecks: %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("ListAcceptanceChecks returned %d rows, want 1", len(checks))
	}
	if checks[0].ID != acc.ID {
		t.Fatalf("ListAcceptanceChecks returned %q, want %q", checks[0].ID, acc.ID)
	}
}

// TestCodeReviewService_MarkFinishedWritesAcceptanceJSON pins the
// persistence of the structured acceptance result.
func TestCodeReviewService_MarkFinishedWritesAcceptanceJSON(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, NewCodeReviewAttachmentService(d))

	acc, err := svc.Create(&model.CodeReview{
		ProjectID:     projectID,
		ReviewType:    model.ReviewTypeAcceptance,
		RequirementID: "req_cr_acc_json",
	})
	if err != nil {
		t.Fatalf("create acceptance check: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(acc.ID) })

	if acc.AcceptanceJSON != "" {
		t.Fatalf("a fresh acceptance row must have an empty acceptance_json, got %q", acc.AcceptanceJSON)
	}

	const payload = `{"verdict":"partial","summary":"一半","checks":[{"criterion":"x","status":"no","evidence":"a.go:1"}]}`
	if err := svc.MarkFinished(acc.ID, ReviewOutcome{
		Status:        model.ReviewStatusDone,
		ResultMD:      "## 总体结论\n一半",
		GoalMet:       model.AcceptanceVerdictPartial,
		AcceptanceJSON: payload,
	}); err != nil {
		t.Fatalf("MarkFinished: %v", err)
	}

	got, err := svc.Get(acc.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.ReviewStatusDone {
		t.Fatalf("status = %q, want done", got.Status)
	}
	if got.GoalMet != model.AcceptanceVerdictPartial {
		t.Fatalf("goal_met = %q, want partial", got.GoalMet)
	}
	if got.AcceptanceJSON != payload {
		t.Fatalf("acceptance_json = %q, want %q", got.AcceptanceJSON, payload)
	}
	if got.ResultMD != "## 总体结论\n一半" {
		t.Fatalf("result_md = %q", got.ResultMD)
	}
}

// TestCodeReviewService_MarkRunningClearsAcceptanceJSON makes sure a
// re-run cannot show the previous verdict while the new stream is still
// going.
func TestCodeReviewService_MarkRunningClearsAcceptanceJSON(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, NewCodeReviewAttachmentService(d))

	acc, err := svc.Create(&model.CodeReview{
		ProjectID:     projectID,
		ReviewType:    model.ReviewTypeAcceptance,
		RequirementID: "req_cr_acc_running",
	})
	if err != nil {
		t.Fatalf("create acceptance check: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(acc.ID) })

	if err := svc.MarkFinished(acc.ID, ReviewOutcome{
		Status:        model.ReviewStatusDone,
		GoalMet:       model.AcceptanceVerdictNo,
		AcceptanceJSON: `{"verdict":"no"}`,
	}); err != nil {
		t.Fatalf("MarkFinished: %v", err)
	}
	if err := svc.MarkRunning(acc.ID, "job_1", "claude-x"); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	got, err := svc.Get(acc.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AcceptanceJSON != "" {
		t.Fatalf("acceptance_json = %q, want it cleared on a re-run", got.AcceptanceJSON)
	}
	if got.GoalMet != "" {
		t.Fatalf("goal_met = %q, want it cleared on a re-run", got.GoalMet)
	}
	if got.Status != model.ReviewStatusRunning {
		t.Fatalf("status = %q, want running", got.Status)
	}
}
