package service

import (
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

// seedRunningRow inserts one orchestrated sub_tasks row in the given state.
// Used by the double-dispatch regression tests so each scenario can stage a
// known DB shape (status, batch_id_seq_run age, session_id) without going
// through Create + Finish.
//
// batch_id_seq_run is staged in UTC because that is what production writes
// (ClaimNextPending / MarkHeartbeat). It matters: on SQLite the stale-cutoff
// predicate compares this column as text, so seeding it in a different zone
// than production uses would let these tests pass against a build whose
// predicate is broken — which is exactly how the double-dispatch loop shipped
// green. See SubTaskService.ClaimNextPending's docstring.
func seedRunningRow(t *testing.T, d *db.DB, id, batchID, sessionID, jobID, status string, heartbeatAge time.Duration) {
	t.Helper()
	d.Exec("INSERT INTO projects (id, name, local_path) VALUES ('proj_dd', 'p', '/tmp/p')")
	d.Exec("INSERT INTO requirements (id, project_id, title) VALUES ('req_dd', 'proj_dd', 'req')")
	now := time.Now().Add(-heartbeatAge)
	hb := now.UTC()
	if _, err := d.Exec(`INSERT INTO sub_tasks (id, requirement_id, title, prompt, status,
		session_id, source_session_id, job_id, model, source,
		batch_id, batch_seq, batch_id_seq_run, created_at, updated_at)
		VALUES (?, 'req_dd', 't', 'p', ?, ?, 'src', ?, '', 'auto', ?, 1, ?, ?, ?)`,
		id, status, sessionID, jobID, batchID, hb, now, now); err != nil {
		t.Fatalf("seed sub_task %s: %v", id, err)
	}
}

// TestRecoverStaleRunningInBatch_DoesNotTouchFreshHeartbeat locks down the
// "live child must not be re-flipped" guarantee. A running row with a fresh
// (5s old) heartbeat is what an orchestrator child looks like in steady
// state; self-heal MUST leave it alone.
func TestRecoverStaleRunningInBatch_DoesNotTouchFreshHeartbeat(t *testing.T) {
	d := newTestDB(t)
	seedRunningRow(t, d, "st_fresh", "batch_dd", "sid-fresh", "job-fresh", model.SubTaskStatusRunning, 5*time.Second)
	seedRunningRow(t, d, "st_stale", "batch_dd", "sid-stale", "job-stale", model.SubTaskStatusRunning, 3*time.Minute)
	seedRunningRow(t, d, "st_pending", "batch_dd", "", "", model.SubTaskStatusPending, 0)

	svc := NewSubTaskService(d)
	rows, err := svc.RecoverStaleRunningInBatch("batch_dd", 2*time.Minute)
	if err != nil {
		t.Fatalf("RecoverStaleRunningInBatch: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("flipped %d rows, want exactly 1 (the stale row); rows=%+v", len(rows), rows)
	}
	if rows[0].ID != "st_stale" {
		t.Errorf("flipped row id = %q, want st_stale", rows[0].ID)
	}

	// Fresh heartbeat row stays running.
	if row, _ := svc.Get("st_fresh"); row.Status != model.SubTaskStatusRunning {
		t.Errorf("st_fresh status = %q, want running (heartbeat was fresh)", row.Status)
	}
	if row, _ := svc.Get("st_fresh"); row.SessionID != "sid-fresh" {
		t.Errorf("st_fresh session_id clobbered: got %q", row.SessionID)
	}
	if row, _ := svc.Get("st_fresh"); row.JobID != "job-fresh" {
		t.Errorf("st_fresh job_id clobbered: got %q", row.JobID)
	}

	// Stale row is now pending with heartbeat nulled + job_id cleared (per
	// the contract — this is what makes the row re-claimable like a fresh
	// pending row).
	if row, _ := svc.Get("st_stale"); row.Status != model.SubTaskStatusPending {
		t.Errorf("st_stale status = %q, want pending", row.Status)
	}
	if row, _ := svc.Get("st_stale"); row.BatchIDSeqRun != nil {
		t.Errorf("st_stale heartbeat = %v, want NULL", row.BatchIDSeqRun)
	}
	if row, _ := svc.Get("st_stale"); row.JobID != "" {
		t.Errorf("st_stale job_id = %q, want cleared", row.JobID)
	}

	// Pending rows are not touched by self-heal (they're the queue's input).
	if row, _ := svc.Get("st_pending"); row.Status != model.SubTaskStatusPending {
		t.Errorf("st_pending status = %q, want pending untouched", row.Status)
	}
}

// TestRecoverStaleRunningInBatch_ReportsAffectedRowMetadata asserts the
// observability contract: every flipped row carries child_id + session_id
// + job_id + heartbeat_age so the queue can log per-row breadcrumbs. This
// is the diagnostic that distinguishes a real orphan recover from a
// false-positive that killed a still-live goroutine.
func TestRecoverStaleRunningInBatch_ReportsAffectedRowMetadata(t *testing.T) {
	d := newTestDB(t)
	seedRunningRow(t, d, "st_a", "batch_dd", "sid-abc", "job-xyz", model.SubTaskStatusRunning, 5*time.Minute)
	seedRunningRow(t, d, "st_b", "batch_dd", "", "", model.SubTaskStatusRunning, 7*time.Minute)

	svc := NewSubTaskService(d)
	rows, err := svc.RecoverStaleRunningInBatch("batch_dd", 2*time.Minute)
	if err != nil {
		t.Fatalf("RecoverStaleRunningInBatch: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("flipped %d rows, want 2", len(rows))
	}
	byID := map[string]RecoveredStaleRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	a, ok := byID["st_a"]
	if !ok {
		t.Fatalf("st_a not in affected rows: %+v", rows)
	}
	if a.SessionID != "sid-abc" || a.JobID != "job-xyz" {
		t.Errorf("st_a metadata = %+v, want session_id=sid-abc job_id=job-xyz", a)
	}
	if a.HeartbeatAge < 4*time.Minute || a.HeartbeatAge > 6*time.Minute {
		t.Errorf("st_a heartbeat_age = %s, want ~5m", a.HeartbeatAge)
	}
	b, ok := byID["st_b"]
	if !ok {
		t.Fatalf("st_b not in affected rows: %+v", rows)
	}
	if b.SessionID != "" || b.JobID != "" {
		t.Errorf("st_b metadata = %+v, want empty session_id and job_id", b)
	}
	if b.HeartbeatAge < 6*time.Minute || b.HeartbeatAge > 8*time.Minute {
		t.Errorf("st_b heartbeat_age = %s, want ~7m", b.HeartbeatAge)
	}
}

// TestRecoverStaleRunningInBatch_EmptyWhenFresh is the no-op guard: a
// healthy batch (every running row has a fresh heartbeat) must not produce
// any log lines, because nothing was flipped.
func TestRecoverStaleRunningInBatch_EmptyWhenFresh(t *testing.T) {
	d := newTestDB(t)
	seedRunningRow(t, d, "st_a", "batch_dd", "sid", "job", model.SubTaskStatusRunning, 1*time.Second)
	seedRunningRow(t, d, "st_b", "batch_dd", "sid", "job", model.SubTaskStatusRunning, 10*time.Second)

	svc := NewSubTaskService(d)
	rows, err := svc.RecoverStaleRunningInBatch("batch_dd", 2*time.Minute)
	if err != nil {
		t.Fatalf("RecoverStaleRunningInBatch: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("flipped %d rows, want 0 (every heartbeat was inside the cutoff)", len(rows))
	}
	for _, id := range []string{"st_a", "st_b"} {
		row, _ := svc.Get(id)
		if row.Status != model.SubTaskStatusRunning {
			t.Errorf("%s status = %q, want running (heartbeat was fresh)", id, row.Status)
		}
	}
}

// TestMarkHeartbeat_SessionAware locks the session-id guard: a stale
// heartbeat goroutine whose row has been re-claimed under a new session
// MUST become a no-op. Otherwise the heartbeat would silently keep
// batch_id_seq_run fresh on a row whose real owner is the new goroutine —
// the exact precondition for an endless self-heal / re-claim / heartbeat
// loop that the orchestration race is built on.
func TestMarkHeartbeat_SessionAware(t *testing.T) {
	d := newTestDB(t)
	seedRunningRow(t, d, "st_hb", "batch_dd", "sid-original", "job-original", model.SubTaskStatusRunning, 30*time.Second)

	svc := NewSubTaskService(d)

	// Stale-goroutine heartbeat: session mismatch MUST no-op. We can't
	// directly observe the timestamp didn't move (RecoverStaleRunningInBatch
	// would re-flip the row), so instead assert the heartbeat write
	// refuses: re-read batch_id_seq_run and compare.
	hbBefore, _ := svc.Get("st_hb")
	if hbBefore.BatchIDSeqRun == nil {
		t.Fatal("test seed missing heartbeat")
	}
	hbBeforeT := *hbBefore.BatchIDSeqRun

	if err := svc.MarkHeartbeat("st_hb", "sid-DIFFERENT"); err != nil {
		t.Fatalf("MarkHeartbeat session-mismatch: %v", err)
	}

	hbAfter, _ := svc.Get("st_hb")
	if hbAfter.BatchIDSeqRun == nil {
		t.Fatal("heartbeat column vanished")
	}
	if !hbAfter.BatchIDSeqRun.Equal(hbBeforeT) {
		t.Errorf("heartbeat moved on session mismatch: before=%v after=%v", hbBeforeT, *hbAfter.BatchIDSeqRun)
	}

	// Matching session ID: heartbeat must refresh.
	if err := svc.MarkHeartbeat("st_hb", "sid-original"); err != nil {
		t.Fatalf("MarkHeartbeat session-match: %v", err)
	}
	hbRefreshed, _ := svc.Get("st_hb")
	if hbRefreshed.BatchIDSeqRun == nil {
		t.Fatal("heartbeat column vanished after legitimate refresh")
	}
	if !hbRefreshed.BatchIDSeqRun.After(hbBeforeT) {
		t.Errorf("heartbeat did NOT advance on session match: before=%v after=%v", hbBeforeT, *hbRefreshed.BatchIDSeqRun)
	}
}

// TestFinishForSession_RefusesOnSessionMismatch is the core idempotency
// guard: when a self-heal has overwritten session_id (the new goroutine's
// pre-mint) and the OLD goroutine calls Finish on its way out, the write
// must be a no-op. Without this guard the OLD goroutine silently clobbers
// the row's terminal state — the exact race in req_650ea321dcab5195.
func TestFinishForSession_RefusesOnSessionMismatch(t *testing.T) {
	d := newTestDB(t)
	seedRunningRow(t, d, "st_fin", "batch_dd", "sid-original", "job-original", model.SubTaskStatusRunning, 30*time.Second)

	svc := NewSubTaskService(d)

	// Simulate the self-heal: a re-claim overwrites session_id.
	if err := svc.UpdateSession("st_fin", "sid-RECLAIMED", "src"); err != nil {
		t.Fatalf("UpdateSession simulate re-claim: %v", err)
	}

	// The original goroutine tries to write its terminal result. With
	// expectedSessionID="sid-original" the write MUST refuse (0 rows
	// affected, no error) so the re-claimed row's state is preserved.
	tokens := model.SubTaskTokens{Input: 100, Output: 50, CacheCreation: 10, CacheRead: 5}
	if err := svc.FinishForSession("st_fin", "sid-original", model.SubTaskStatusDone,
		"artifact from old goroutine", "claude-sonnet", tokens, 1, time.Now()); err != nil {
		t.Fatalf("FinishForSession session-mismatch: %v", err)
	}

	row, _ := svc.Get("st_fin")
	if row.Status != model.SubTaskStatusRunning {
		t.Errorf("status = %q, want running (the stale Finish must NOT have written)", row.Status)
	}
	if row.Artifact != "" {
		t.Errorf("artifact = %q, want empty (stale Finish must NOT have written)", row.Artifact)
	}
	if row.SessionID != "sid-RECLAIMED" {
		t.Errorf("session_id clobbered: got %q, want sid-RECLAIMED", row.SessionID)
	}
	if row.InputTokens != 0 || row.OutputTokens != 0 {
		t.Errorf("tokens leaked through stale Finish: %+v", row)
	}

	// Now the new (matching) goroutine writes its terminal result. This
	// MUST succeed.
	if err := svc.FinishForSession("st_fin", "sid-RECLAIMED", model.SubTaskStatusDone,
		"artifact from new goroutine", "claude-sonnet", tokens, 1, time.Now()); err != nil {
		t.Fatalf("FinishForSession session-match: %v", err)
	}
	row, _ = svc.Get("st_fin")
	if row.Status != model.SubTaskStatusDone {
		t.Errorf("status = %q, want done (matching Finish must have written)", row.Status)
	}
	if row.Artifact != "artifact from new goroutine" {
		t.Errorf("artifact = %q, want new goroutine's artifact", row.Artifact)
	}
	if row.InputTokens != 100 || row.OutputTokens != 50 {
		t.Errorf("tokens did not land: %+v", row)
	}
}

// TestFinishForSession_EmptyExpectedSessionID_FallsThroughToUnconditional
// pins the legacy-compat behavior: passing "" must behave exactly like the
// original Finish (write regardless of session_id). Existing call sites
// that don't have a session id in hand keep working.
func TestFinishForSession_EmptyExpectedSessionID_FallsThroughToUnconditional(t *testing.T) {
	d := newTestDB(t)
	seedRunningRow(t, d, "st_legacy", "batch_dd", "sid-original", "job-original", model.SubTaskStatusRunning, 30*time.Second)

	svc := NewSubTaskService(d)
	tokens := model.SubTaskTokens{Input: 100, Output: 50}
	if err := svc.FinishForSession("st_legacy", "", model.SubTaskStatusDone,
		"legacy write", "claude-sonnet", tokens, 0, time.Now()); err != nil {
		t.Fatalf("FinishForSession with empty expected: %v", err)
	}
	row, _ := svc.Get("st_legacy")
	if row.Status != model.SubTaskStatusDone {
		t.Errorf("status = %q, want done (legacy path must write)", row.Status)
	}
	if row.Artifact != "legacy write" {
		t.Errorf("artifact = %q, want legacy write", row.Artifact)
	}
}