package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestClaimNextPending_FirstInBatch_NoPredecessor locks the seq=1
// exemption: batch_seq=1 has no predecessor and is always claimable
// regardless of any other state in the batch. This guards the
// "first in batch" boundary case so a regression that hardens the
// predicate too far (e.g. adding a "must be unique" join that
// double-filters seq=1) shows up here instead of as a silent
// deadlock on the first child of every orchestrated batch.
func TestClaimNextPending_FirstInBatch_NoPredecessor(t *testing.T) {
	d := newTestDB(t)
	// A "messy" batch: another sibling is already running. With seq=1
	// that should not matter — seq=1 has no predecessor.
	seedPendingChild(t, d, "st_first_1", "batch_first", 1)
	seedPendingChild(t, d, "st_first_2", "batch_first", 2)
	if _, err := d.Exec(`UPDATE sub_tasks SET status=? WHERE id='st_first_2'`,
		model.SubTaskStatusRunning); err != nil {
		t.Fatalf("seed st_first_2 running: %v", err)
	}

	svc := NewSubTaskService(d)

	// seq=1 must be claimable regardless of any sibling state.
	first, ok, err := svc.ClaimNextPending("batch_first")
	if err != nil || !ok {
		t.Fatalf("ClaimNextPending seq=1: ok=%v err=%v", ok, err)
	}
	if first.ID != "st_first_1" {
		t.Fatalf("seq=1 claim id = %q, want st_first_1", first.ID)
	}
	if first.Status != model.SubTaskStatusRunning {
		t.Errorf("seq=1 status = %q, want running", first.Status)
	}
}

// TestClaimNextPending_OnlySeq1Running_NoImpactOnLaterClaims is the
// edge case that protects a single-child batch: when a batch has only
// seq=1, the NOT EXISTS predicate has nothing to check and seq=1 stays
// claimable in steady state. A regression that flipped the predicate
// to "any row with a non-terminal sibling is rejected" would deadlock
// here.
func TestClaimNextPending_OnlySeq1Running_NoImpactOnLaterClaims(t *testing.T) {
	d := newTestDB(t)
	seedPendingChild(t, d, "st_only_1", "batch_only", 1)

	svc := NewSubTaskService(d)

	first, ok, err := svc.ClaimNextPending("batch_only")
	if err != nil || !ok || first.ID != "st_only_1" {
		t.Fatalf("first claim ok=%v err=%v id=%v", ok, err, first)
	}

	// A repeat claim must come back empty (nothing pending left) and
	// MUST NOT panic, deadlock, or return the row we already own.
	second, ok, err := svc.ClaimNextPending("batch_only")
	if err != nil {
		t.Fatalf("repeat claim: %v", err)
	}
	if ok {
		t.Fatalf("repeat claim returned ok=true id=%q, want ok=false (only one row in batch, "+
			"already claimed)", second.ID)
	}
}

// TestClaimNextPending_MultipleBatchesIsolated locks the batch
// isolation invariant: the NOT EXISTS predicate is scoped by
// batch_id, so a running predecessor in batch A must NOT block a
// claim in batch B. This is what lets the OrchestrationQueue process
// multiple orchestrated batches per tick (e.g. two projects each
// running one) without the strict-in-order rule leaking across
// batches.
func TestClaimNextPending_MultipleBatchesIsolated(t *testing.T) {
	d := newTestDB(t)
	seedPendingChild(t, d, "st_iso_a1", "batch_iso_a", 1)
	seedPendingChild(t, d, "st_iso_a2", "batch_iso_a", 2)
	seedPendingChild(t, d, "st_iso_b1", "batch_iso_b", 1)
	seedPendingChild(t, d, "st_iso_b2", "batch_iso_b", 2)

	svc := NewSubTaskService(d)

	// Batch A: claim seq 1.
	firstA, ok, err := svc.ClaimNextPending("batch_iso_a")
	if err != nil || !ok || firstA.ID != "st_iso_a1" {
		t.Fatalf("batch A seq 1 claim ok=%v err=%v id=%v", ok, err, firstA)
	}

	// Batch B: its seq 1 has no predecessor and is independent of
	// batch A's running row.
	firstB, ok, err := svc.ClaimNextPending("batch_iso_b")
	if err != nil || !ok || firstB.ID != "st_iso_b1" {
		t.Fatalf("batch B seq 1 claim ok=%v err=%v id=%v", ok, err, firstB)
	}

	// Both batches' seq 2 must be blocked by THEIR OWN seq 1, not
	// each other's — that's the isolation guarantee.
	_, ok, err = svc.ClaimNextPending("batch_iso_a")
	if err != nil {
		t.Fatalf("batch A seq 2 (blocked): %v", err)
	}
	if ok {
		t.Fatal("batch A seq 2 returned ok=true while seq 1 still running: " +
			"in-batch isolation broken")
	}
	_, ok, err = svc.ClaimNextPending("batch_iso_b")
	if err != nil {
		t.Fatalf("batch B seq 2 (blocked): %v", err)
	}
	if ok {
		t.Fatal("batch B seq 2 returned ok=true while seq 1 still running: " +
			"in-batch isolation broken")
	}
}
