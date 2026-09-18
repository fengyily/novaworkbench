package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

// seedPendingChild inserts one orchestrated child in 'pending' at the given
// batch_seq. Unlike seedRunningRow it stages no heartbeat and no session id —
// that is exactly the shape ClaimNextPending is supposed to promote, so these
// tests exercise the claim through its production entry point rather than
// staging a half-claimed row by hand.
func seedPendingChild(t *testing.T, d *db.DB, id, batchID string, seq int) {
	t.Helper()
	d.Exec("INSERT INTO projects (id, name, local_path) VALUES ('proj_ci', 'p', '/tmp/p')")
	d.Exec("INSERT INTO requirements (id, project_id, title) VALUES ('req_ci', 'proj_ci', 'req')")
	if _, err := d.Exec(`INSERT INTO sub_tasks (id, requirement_id, title, prompt, status,
		session_id, source_session_id, job_id, model, source, batch_id, batch_seq)
		VALUES (?, 'req_ci', ?, 'p', ?, '', 'src', '', '', 'auto', ?, ?)`,
		id, id, model.SubTaskStatusPending, batchID, seq); err != nil {
		t.Fatalf("seed sub_task %s: %v", id, err)
	}
}

// TestClaimNextPending_ReturnsTheRowItClaimed is the regression test for the
// orchestration double-dispatch loop that survived the heartbeat-timezone fix.
//
// ClaimNextPending promotes the lowest-seq *pending* row, then re-selected the
// lowest-seq *running* row to return. Those are the same row only while at
// most one child runs at a time. At subtask.concurrency=2 the second claim
// promoted seq 2 but handed seq 1 — the row already executing — back to the
// dispatcher, with two consequences seen together in production:
//
//   - The caller (ExecuteOrchestratedChild) re-ran the ALREADY-RUNNING child:
//     it minted a fresh child session id over seq 1's session_id, so the live
//     goroutine's FinishForSession stopped matching ("finish bypassed:
//     session_id mismatch — likely re-claimed mid-run") and a second claude
//     process worked the same prompt.
//   - Nobody ever dispatched seq 2. Its row sat at status='running' with the
//     claim-time heartbeat and no session_id / job_id, so nothing refreshed
//     the heartbeat; selfHealStaleRunning flipped it back to pending at
//     exactly staleAfter ("session_id= job_id=<empty> heartbeat_age=2m0.001s")
//     and the next tick fed the loop again.
func TestClaimNextPending_ReturnsTheRowItClaimed(t *testing.T) {
	d := newTestDB(t)
	seedPendingChild(t, d, "st_seq1", "batch_ci", 1)
	seedPendingChild(t, d, "st_seq2", "batch_ci", 2)
	seedPendingChild(t, d, "st_seq3", "batch_ci", 3)

	svc := NewSubTaskService(d)

	first, ok, err := svc.ClaimNextPending("batch_ci")
	if err != nil || !ok {
		t.Fatalf("first ClaimNextPending: ok=%v err=%v", ok, err)
	}
	if first.ID != "st_seq1" {
		t.Fatalf("first claim returned %q, want st_seq1 (lowest batch_seq)", first.ID)
	}

	// st_seq1 stays running — its child goroutine is still executing, which
	// is the whole point of a per-project concurrency above 1.
	second, ok, err := svc.ClaimNextPending("batch_ci")
	if err != nil || !ok {
		t.Fatalf("second ClaimNextPending: ok=%v err=%v", ok, err)
	}
	if second.ID == first.ID {
		t.Fatalf("second claim re-returned the already-running row %q: the dispatcher would "+
			"re-launch it under a new session id while its first claude process is still live", second.ID)
	}
	if second.ID != "st_seq2" {
		t.Fatalf("second claim returned %q, want st_seq2", second.ID)
	}

	// Every row the claim promoted must be a row the caller was handed;
	// otherwise it runs with a frozen heartbeat until self-heal trips.
	for _, id := range []string{"st_seq1", "st_seq2"} {
		row, err := svc.Get(id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if row.Status != model.SubTaskStatusRunning {
			t.Errorf("%s status = %q, want running", id, row.Status)
		}
	}
	row3, err := svc.Get("st_seq3")
	if err != nil {
		t.Fatalf("Get st_seq3: %v", err)
	}
	if row3.Status != model.SubTaskStatusPending {
		t.Errorf("st_seq3 status = %q, want pending (only two claims were made)", row3.Status)
	}
}

// TestClaimNextPending_SkipsRunningRowsAfterSelfHeal covers the ordering twist
// the bug depended on: a self-healed row returns to 'pending' at a LOWER
// batch_seq than rows already running, so the claim's promote-side and
// return-side disagree in both directions. The row handed back must always be
// the one this call flipped.
func TestClaimNextPending_SkipsRunningRowsAfterSelfHeal(t *testing.T) {
	d := newTestDB(t)
	seedPendingChild(t, d, "st_low", "batch_ci", 1)
	seedPendingChild(t, d, "st_high", "batch_ci", 9)

	svc := NewSubTaskService(d)

	// st_high is already running (claimed on an earlier tick, then st_low was
	// self-healed back to pending behind it).
	if _, err := d.Exec(`UPDATE sub_tasks SET status=? WHERE id='st_high'`, model.SubTaskStatusRunning); err != nil {
		t.Fatalf("stage running row: %v", err)
	}

	claimed, ok, err := svc.ClaimNextPending("batch_ci")
	if err != nil || !ok {
		t.Fatalf("ClaimNextPending: ok=%v err=%v", ok, err)
	}
	if claimed.ID != "st_low" {
		t.Fatalf("claim returned %q, want st_low (the row it promoted); returning the running "+
			"st_high would re-dispatch a live child", claimed.ID)
	}
	if claimed.Status != model.SubTaskStatusRunning {
		t.Errorf("claimed row status = %q, want running (the row must be read back post-promote)", claimed.Status)
	}
	if claimed.BatchIDSeqRun == nil {
		t.Error("claimed row heartbeat is NULL, want the claim-time stamp")
	}
}
