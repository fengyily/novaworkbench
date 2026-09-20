package service

import (
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// TestClaimNextPending_StrictSerialSequence locks down the strict-in-order
// rule that breaks the "subtasks ran out of order / in parallel" symptom
// (req_7d7b4d429ef7f264). A 3-row orchestrated batch behaves as a strict
// pipeline at the SQL layer: a non-seq-1 row is claimable ONLY when every
// predecessor has reached a terminal status. The pair (Go semaphore +
// SQL predicate) keeps the two layers mutually reinforcing: the queue's
// per-batch semaphore prevents two concurrent goroutines from racing on
// the same batch, and the SQL predicate protects any future caller that
// bypasses the queue from promoting an out-of-order row.
//
// Three checkpoints:
//
//  1. With seq 1 still running, the candidate SELECT for seq 2 must
//     return nothing — the NOT EXISTS predicate on the running
//     predecessor forces ok=false.
//  2. Marking seq 1 'done' (terminal success) unblocks seq 2; seq 2 is
//     now claimable.
//  3. Marking seq 2 'error' (terminal failure) unblocks seq 3 — the
//     'error' state counts as a terminal predecessor, so a single
//     failure does not halt the rest of the pipeline.
func TestClaimNextPending_StrictSerialSequence(t *testing.T) {
	d := newTestDB(t)
	seedPendingChild(t, d, "st_strict_1", "batch_strict", 1)
	seedPendingChild(t, d, "st_strict_2", "batch_strict", 2)
	seedPendingChild(t, d, "st_strict_3", "batch_strict", 3)

	svc := NewSubTaskService(d)

	// Step 1: claim seq 1 — it's the only eligible row (batch_seq=1
	// has no predecessor).
	first, ok, err := svc.ClaimNextPending("batch_strict")
	if err != nil || !ok {
		t.Fatalf("first ClaimNextPending: ok=%v err=%v", ok, err)
	}
	if first.ID != "st_strict_1" {
		t.Fatalf("first claim id = %q, want st_strict_1", first.ID)
	}

	// Step 2: with seq 1 still running, seq 2 must NOT be claimable.
	// ok=false signals the queue to release its per-batch slot and
	// retry on the next tick — the UI then renders "排队中" until
	// seq 1 settles.
	_, ok, err = svc.ClaimNextPending("batch_strict")
	if err != nil {
		t.Fatalf("second ClaimNextPending (seq 1 running): %v", err)
	}
	if ok {
		t.Fatal("second claim returned ok=true while seq 1 still running: " +
			"strict-in-order dispatch MUST refuse to claim seq 2")
	}

	// Step 3a: flip seq 1 to 'done' — a happy-path terminal status.
	if _, err := d.Exec(`UPDATE sub_tasks SET status=? WHERE id='st_strict_1'`,
		model.SubTaskStatusDone); err != nil {
		t.Fatalf("flip st_strict_1 to done: %v", err)
	}

	// Step 3b: seq 2 is now claimable.
	second, ok, err := svc.ClaimNextPending("batch_strict")
	if err != nil || !ok {
		t.Fatalf("third ClaimNextPending (seq 1 done): ok=%v err=%v", ok, err)
	}
	if second.ID != "st_strict_2" {
		t.Fatalf("third claim id = %q, want st_strict_2", second.ID)
	}

	// Step 4: same gate against seq 2 — with it still running, seq 3
	// must stay unclaimable.
	_, ok, err = svc.ClaimNextPending("batch_strict")
	if err != nil {
		t.Fatalf("fourth ClaimNextPending (seq 2 running): %v", err)
	}
	if ok {
		t.Fatal("fourth claim returned ok=true while seq 2 still running: " +
			"strict-in-order dispatch MUST refuse to claim seq 3")
	}

	// Step 5: 'error' is also a terminal status — a failed predecessor
	// does not halt the pipeline, it just unblocks whatever comes next.
	if _, err := d.Exec(`UPDATE sub_tasks SET status=? WHERE id='st_strict_2'`,
		model.SubTaskStatusError); err != nil {
		t.Fatalf("flip st_strict_2 to error: %v", err)
	}

	// Step 6: seq 3 is now claimable.
	third, ok, err := svc.ClaimNextPending("batch_strict")
	if err != nil || !ok {
		t.Fatalf("fifth ClaimNextPending (seq 2 errored): ok=%v err=%v", ok, err)
	}
	if third.ID != "st_strict_3" {
		t.Fatalf("fifth claim id = %q, want st_strict_3", third.ID)
	}

	// Final state: every row is in a terminal/running state, nothing
	// remains claimable for this batch.
	for _, id := range []string{"st_strict_1", "st_strict_2", "st_strict_3"} {
		row, err := svc.Get(id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		switch id {
		case "st_strict_1":
			if row.Status != model.SubTaskStatusDone {
				t.Errorf("%s status = %q, want done", id, row.Status)
			}
		case "st_strict_2":
			if row.Status != model.SubTaskStatusError {
				t.Errorf("%s status = %q, want error", id, row.Status)
			}
		case "st_strict_3":
			if row.Status != model.SubTaskStatusRunning {
				t.Errorf("%s status = %q, want running (just claimed)", id, row.Status)
			}
		}
	}
}

// TestClaimNextPending_StoppedPredecessorUnblocksNext pins the third
// terminal status. The design doc names "done / error / stopped" as the
// three terminal predicates — a user-halted predecessor must NOT strand
// the rest of the batch. (Manual /stop is allowed even on running
// orchestrated children; if the user stops seq 1, seq 2 should still
// run.)
func TestClaimNextPending_StoppedPredecessorUnblocksNext(t *testing.T) {
	d := newTestDB(t)
	seedPendingChild(t, d, "st_stop_1", "batch_stop", 1)
	seedPendingChild(t, d, "st_stop_2", "batch_stop", 2)

	svc := NewSubTaskService(d)

	first, ok, err := svc.ClaimNextPending("batch_stop")
	if err != nil || !ok || first.ID != "st_stop_1" {
		t.Fatalf("first claim ok=%v err=%v id=%v", ok, err, first)
	}

	// Mark seq 1 'stopped' via the public Finish path so the row carries
	// the same shape it would in production (status + completed_at).
	tokens := model.SubTaskTokens{}
	if err := svc.Finish(first.ID, model.SubTaskStatusStopped, "", "", tokens, 0, time.Now()); err != nil {
		t.Fatalf("Finish seq 1 stopped: %v", err)
	}

	second, ok, err := svc.ClaimNextPending("batch_stop")
	if err != nil || !ok {
		t.Fatalf("second ClaimNextPending after stopped predecessor: ok=%v err=%v", ok, err)
	}
	if second.ID != "st_stop_2" {
		t.Fatalf("second claim id = %q, want st_stop_2", second.ID)
	}
}

// TestClaimNextPending_RejectsClaimWhenRunningPredecessor is the
// negative-control test that locks the SQL predicate to its intended
// shape: a non-seq-1 row whose predecessor is RUNNING (not just
// non-terminal) is the most dangerous case — that's exactly the
// "out-of-order" symptom we're guarding against. The claim must come
// back empty so the queue doesn't promote seq 2 while seq 1 is still
// in flight.
func TestClaimNextPending_RejectsClaimWhenRunningPredecessor(t *testing.T) {
	d := newTestDB(t)
	seedPendingChild(t, d, "st_run_1", "batch_run", 1)
	seedPendingChild(t, d, "st_run_2", "batch_run", 2)

	svc := NewSubTaskService(d)

	first, ok, err := svc.ClaimNextPending("batch_run")
	if err != nil || !ok || first.ID != "st_run_1" {
		t.Fatalf("first claim ok=%v err=%v id=%v", ok, err, first)
	}

	// seq 1 is now running (no terminal flip). A second claim MUST
	// refuse — this is the symptom that previously shipped as
	// "sub-tasks ran out of order" and "sub-tasks ran in parallel".
	_, ok, err = svc.ClaimNextPending("batch_run")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if ok {
		t.Fatal("second claim returned ok=true while seq 1 is running: " +
			"out-of-order dispatch symptom not guarded against")
	}

	// st_run_2 itself stays pending (not running) so a downstream
	// caller inspecting the row sees "not yet claimed", matching the
	// queue's mental model.
	row, _ := svc.Get("st_run_2")
	if row.Status != model.SubTaskStatusPending {
		t.Errorf("st_run_2 status = %q, want pending (claim refused, row untouched)", row.Status)
	}
}
