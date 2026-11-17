package service

// code_review_auto_iterate_test.go — the persistence layer of the
// requirement-level Review's auto-iteration chain.
//
// The chain's state lives on the code_reviews row (NOT in an in-memory
// registry) precisely so that a backend restart cannot strand it. These
// tests pin that contract: the columns round-trip, the reconcile query
// finds exactly the chain tips, and the state change is visible to the next
// read.

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestCodeReviewService_AutoIterateColumnsRoundTrip pins that the chain's
// three columns survive a write/read cycle.
//
// They are appended to the canonical column list AND to the INSERT (which
// enumerates its columns explicitly) — missing either one would leave the
// column permanently at its default, and the chain would silently lose its
// configuration.
func TestCodeReviewService_AutoIterateColumnsRoundTrip(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, nil)

	rv, err := svc.Create(&model.CodeReview{
		ProjectID:         projectID,
		ReviewType:        model.ReviewTypeRequirement,
		RequirementID:     "req_chain",
		RoleKey:           "reviewer",
		Branch:            "feat/x",
		Title:             "Review · 链",
		AutoIterateRounds: 3,
		AutoIterateState:  "current",
		AutoIterateNote:   "",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(rv.ID) })

	if rv.AutoIterateRounds != 3 {
		t.Errorf("AutoIterateRounds = %d, want 3", rv.AutoIterateRounds)
	}
	if rv.AutoIterateState != "current" {
		t.Errorf("AutoIterateState = %q, want %q", rv.AutoIterateState, "current")
	}

	got, err := svc.Get(rv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AutoIterateRounds != 3 || got.AutoIterateState != "current" {
		t.Errorf("round trip mismatch: rounds=%d state=%q", got.AutoIterateRounds, got.AutoIterateState)
	}
}

// TestCodeReviewService_AutoIterateRoundsClampedToOne pins the default.
//
// A row created without an explicit budget must carry 1 (the schema's
// default), never 0 — a zero budget would mean "no rounds at all" and the
// chain would stop immediately.
func TestCodeReviewService_AutoIterateRoundsClampedToOne(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, nil)

	rv, err := svc.Create(&model.CodeReview{
		ProjectID:     projectID,
		ReviewType:    model.ReviewTypeRequirement,
		RequirementID: "req_chain",
		Branch:        "feat/x",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(rv.ID) })
	if rv.AutoIterateRounds != 1 {
		t.Errorf("AutoIterateRounds = %d, want 1", rv.AutoIterateRounds)
	}
	if rv.AutoIterateState != "" {
		t.Errorf("AutoIterateState = %q, want %q (a row that did not opt in is not part of a chain)", rv.AutoIterateState, "")
	}
}

// TestCodeReviewService_MarkAutoIterateState pins the chain's single state
// writer.
func TestCodeReviewService_MarkAutoIterateState(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, nil)

	rv, err := svc.Create(&model.CodeReview{
		ProjectID:         projectID,
		ReviewType:        model.ReviewTypeRequirement,
		RequirementID:     "req_chain",
		Branch:            "feat/x",
		AutoIterateRounds: 2,
		AutoIterateState:  "current",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(rv.ID) })

	if err := svc.MarkAutoIterateState(rv.ID, "stopped", "fix_failed"); err != nil {
		t.Fatalf("MarkAutoIterateState: %v", err)
	}
	got, err := svc.Get(rv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AutoIterateState != "stopped" || got.AutoIterateNote != "fix_failed" {
		t.Errorf("state=%q note=%q, want stopped/fix_failed", got.AutoIterateState, got.AutoIterateNote)
	}
	// The budget must survive the state change — it is the chain's
	// configuration, not part of the state machine.
	if got.AutoIterateRounds != 2 {
		t.Errorf("AutoIterateRounds = %d, want 2 (the budget must survive)", got.AutoIterateRounds)
	}
}

// TestCodeReviewService_ListAutoIterateCurrent pins the reconcile query.
//
// The reconcile tick must see EXACTLY the chain tips: the requirement-level
// rows whose state is 'current'. A project-level Review (requirement_id =
// ”) is never part of a chain, and a chain that already stopped must not
// come back.
func TestCodeReviewService_ListAutoIterateCurrent(t *testing.T) {
	d, projectID := newCodeReviewTestDB(t)
	svc := NewCodeReviewService(d, nil)

	mk := func(id, reqID, reviewType, state string) *model.CodeReview {
		t.Helper()
		rv, err := svc.Create(&model.CodeReview{
			ProjectID:         projectID,
			ReviewType:        reviewType,
			RequirementID:     reqID,
			Branch:            "feat/x",
			AutoIterateRounds: 2,
			AutoIterateState:  state,
		})
		if err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		t.Cleanup(func() { _ = svc.Delete(rv.ID) })
		return rv
	}

	tip := mk("crv_tip", "req_a", model.ReviewTypeRequirement, "current")
	mk("crv_project", "", model.ReviewTypeBranch, "current")
	mk("crv_stopped", "req_b", model.ReviewTypeRequirement, "stopped")
	mk("crv_plain", "req_c", model.ReviewTypeRequirement, "")

	rows, err := svc.ListAutoIterateCurrent()
	if err != nil {
		t.Fatalf("ListAutoIterateCurrent: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (only the requirement-level chain tip)", len(rows))
	}
	if rows[0].ID != tip.ID {
		t.Errorf("row id = %q, want %q", rows[0].ID, tip.ID)
	}
}
