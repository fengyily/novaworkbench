package service

import (
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// The orphan sweep is the safety net for the bug this file exists for: a
// pending sub-task that no dispatcher owns sits at "排队中 · 等待项目空闲"
// forever, with no way to start it, cancel it, or delete it.
//
// "No dispatcher owns it" has an exact SQL meaning:
//
//	status = 'pending' AND job_id = '' AND batch_id = ''
//
// Every legitimate pending row falls into exactly one of two buckets —
// batch_id != '' (an orchestrated child, claimed by ClaimNextPending) or
// job_id != '' (a dispatched manual / Review / push_pr row, waiting on the
// per-project concurrency gate). The sweep must therefore pick up the
// intersection of "neither bucket" and nothing else.
//
// The age cutoff is measured against updated_at, NOT created_at, because
// RedoReset / ContinueReset re-arm a row IN PLACE (status='pending',
// job_id='', updated_at=now) and dispatch it a few statements later.
// Filtering on created_at would let the sweep race that window and spawn two
// claude processes on one row.

// insertPendingRow writes one pending row with an explicit job_id /
// batch_id / updated_at so the sweep's predicate can be probed directly.
func insertPendingRow(t *testing.T, svc *SubTaskService, id, jobID, batchID string, updatedAt time.Time) {
	t.Helper()
	// SubTaskService.Create stamps created_at/updated_at with time.Now();
	// for the age probe we need an arbitrary past timestamp, so the row is
	// written directly and then back-dated.
	if _, err := svc.db.Exec(`INSERT INTO sub_tasks (id, requirement_id, title, prompt, status,
		job_id, batch_id, source, session_mode, report_mode, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "req_orphan", "orphan "+id, "prompt", model.SubTaskStatusPending,
		jobID, batchID, model.SubTaskSourceManual, model.SubTaskSessionModeFork,
		model.SubTaskReportModeAuto, updatedAt, updatedAt); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

// TestListOrphanPending_SelectsOnlyUnownedRows pins the sweep's predicate.
//
// The four rows below cover every bucket:
//
//   - orphan:      pending, no job, no batch      → MUST be listed
//   - withJob:     pending, job set               → must NOT (dispatched, queued)
//   - withBatch:   pending, batch set             → must NOT (orchestrated child)
//   - notPending:  error,   no job, no batch      → must NOT (terminal)
func TestListOrphanPending_SelectsOnlyUnownedRows(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewSubTaskService(d, nil)

	old := time.Now().Add(-2 * time.Hour)
	insertPendingRow(t, svc, "st_orphan", "", "", old)
	insertPendingRow(t, svc, "st_withJob", "job_1", "", old)
	insertPendingRow(t, svc, "st_withBatch", "", "ob_1", old)
	if _, err := d.Exec(`INSERT INTO sub_tasks (id, requirement_id, title, prompt, status,
		job_id, batch_id, source, session_mode, report_mode, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"st_notPending", "req_orphan", "failed", "prompt", model.SubTaskStatusError,
		"", "", model.SubTaskSourceManual, model.SubTaskSessionModeFork,
		model.SubTaskReportModeAuto, old, old); err != nil {
		t.Fatalf("insert st_notPending: %v", err)
	}

	got, err := svc.ListOrphanPending(time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("ListOrphanPending: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("listed %d rows, want 1 (only the unowned pending row): %+v", len(got), got)
	}
	if got[0].ID != "st_orphan" {
		t.Fatalf("listed %q, want st_orphan", got[0].ID)
	}
}

// TestListOrphanPending_RespectsAgeCutoff is the guard against the sweep
// racing a row that a dispatcher has just created but not yet stamped.
//
// The window between "row INSERTed as pending" and "job_id written" is a
// couple of statements wide. A row younger than the cutoff must be left
// alone.
func TestListOrphanPending_RespectsAgeCutoff(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewSubTaskService(d, nil)

	insertPendingRow(t, svc, "st_fresh", "", "", time.Now())
	insertPendingRow(t, svc, "st_stale", "", "", time.Now().Add(-2*time.Hour))

	got, err := svc.ListOrphanPending(time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("ListOrphanPending: %v", err)
	}
	if len(got) != 1 || got[0].ID != "st_stale" {
		t.Fatalf("listed %+v, want only st_stale", got)
	}
}

// TestClaimOrphanPending_IsCompareAndSet proves the claim is atomic: the
// first caller wins and stamps the job id, the second is refused.
//
// Without the guard, two sweep ticks (or a sweep racing a Redo) would both
// dispatch the same row and spawn two claude processes on one worktree.
func TestClaimOrphanPending_IsCompareAndSet(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewSubTaskService(d, nil)

	old := time.Now().Add(-2 * time.Hour)
	insertPendingRow(t, svc, "st_orphan", "", "", old)
	cutoff := time.Now().Add(-time.Minute)

	ok, err := svc.ClaimOrphanPending("st_orphan", "job_first", "sid_first", cutoff)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !ok {
		t.Fatal("first claim was refused, want it to win")
	}
	ok, err = svc.ClaimOrphanPending("st_orphan", "job_second", "sid_second", cutoff)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if ok {
		t.Fatal("second claim succeeded, want it refused (compare-and-set)")
	}

	got, err := svc.Get("st_orphan")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.JobID != "job_first" {
		t.Fatalf("JobID = %q, want job_first", got.JobID)
	}
	// The session id is written by the SAME statement as the job id, so a
	// row can never be observed half-stamped (job id set, session id still
	// missing) — every other dispatch path stamps session_id first and
	// job_id second, so a job id implies a session id.
	if got.SessionID != "sid_first" {
		t.Fatalf("SessionID = %q, want sid_first", got.SessionID)
	}
}

// TestClaimOrphanPending_RefusesRearmedRow covers the Redo / Continue
// window: a row re-armed in place has a FRESH updated_at, so the claim's
// age predicate no longer matches and the row is left to the dispatcher
// that just re-armed it.
func TestClaimOrphanPending_RefusesRearmedRow(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewSubTaskService(d, nil)

	old := time.Now().Add(-2 * time.Hour)
	insertPendingRow(t, svc, "st_rearmed", "", "", old)
	cutoff := time.Now().Add(-time.Minute)

	// Simulate RedoReset: re-arm in place (status stays pending, job_id is
	// cleared, updated_at is bumped to now).
	if _, err := d.Exec(`UPDATE sub_tasks SET status=?, job_id='', updated_at=? WHERE id=?`,
		model.SubTaskStatusPending, time.Now(), "st_rearmed"); err != nil {
		t.Fatalf("re-arm: %v", err)
	}

	ok, err := svc.ClaimOrphanPending("st_rearmed", "job_sweep", "sid_sweep", cutoff)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if ok {
		t.Fatal("claim succeeded on a freshly re-armed row, want it refused")
	}
}
