package service

import (
	"sync"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

// seedBatchChild inserts one auto-orchestrated sub_tasks row (plus the parent
// project / requirement on first call) with the given status and retry_count,
// so the re-arm assertions can set up an exact batch shape without going
// through Create + Finish.
func seedBatchChild(t *testing.T, d *db.DB, id, batchID, status string, seq, retryCount int) {
	t.Helper()
	d.Exec("INSERT INTO projects (id, name, local_path) VALUES ('proj_r', 'p', '/tmp/p')")
	d.Exec("INSERT INTO requirements (id, project_id, title) VALUES ('req_r', 'proj_r', 'req')")
	now := time.Now()
	if _, err := d.Exec(`INSERT INTO sub_tasks (id, requirement_id, title, prompt, status,
		session_id, source_session_id, job_id, artifact, model, source,
		batch_id, batch_seq, batch_id_seq_run, retry_count, created_at, updated_at)
		VALUES (?, 'req_r', 't', 'p', ?, 'sid', 'src', 'job_old', '旧产物', '', 'auto', ?, ?, ?, ?, ?, ?)`,
		id, status, batchID, seq, now, retryCount, now, now); err != nil {
		t.Fatalf("seed sub_task %s: %v", id, err)
	}
}

// TestReArmErroredForRetry_RespectsCapAndStatus is the core of "失败自动重做":
// only errored children under the given batch are re-armed, each one at most
// retryMax times, and the reset must leave the row in the same shape a manual
// 重做 would (clean artifact / job_id / heartbeat) so the next tick can claim
// it like any other pending row.
func TestReArmErroredForRetry_RespectsCapAndStatus(t *testing.T) {
	d := newTestDB(t)
	seedBatchChild(t, d, "st_err", "batch_1", model.SubTaskStatusError, 1, 0)
	seedBatchChild(t, d, "st_exhausted", "batch_1", model.SubTaskStatusError, 2, 2)
	seedBatchChild(t, d, "st_done", "batch_1", model.SubTaskStatusDone, 3, 0)
	seedBatchChild(t, d, "st_stopped", "batch_1", model.SubTaskStatusStopped, 4, 0)
	seedBatchChild(t, d, "st_other_batch", "batch_2", model.SubTaskStatusError, 1, 0)

	svc := NewSubTaskService(d)
	n, err := svc.ReArmErroredForRetry("batch_1", 2)
	if err != nil {
		t.Fatalf("ReArmErroredForRetry: %v", err)
	}
	// Only st_err qualifies: st_exhausted is already at the cap, st_done and
	// st_stopped aren't failures, st_other_batch belongs elsewhere.
	if n != 1 {
		t.Fatalf("re-armed %d rows, want 1", n)
	}

	got, err := svc.Get("st_err")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != model.SubTaskStatusPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if got.RetryCount != 1 {
		t.Errorf("retry_count = %d, want 1", got.RetryCount)
	}
	if got.JobID != "" || got.Artifact != "" || got.CompletedAt != nil {
		t.Errorf("row not cleaned for re-run: job_id=%q artifact=%q completed_at=%v",
			got.JobID, got.Artifact, got.CompletedAt)
	}
	// A stale heartbeat would make RecoverInterrupted treat the re-armed row
	// as an orphan on the next boot.
	if got.BatchIDSeqRun != nil {
		t.Errorf("batch_id_seq_run = %v, want NULL", got.BatchIDSeqRun)
	}

	for _, id := range []string{"st_exhausted", "st_other_batch"} {
		row, err := svc.Get(id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if row.Status != model.SubTaskStatusError {
			t.Errorf("%s status = %q, want error (untouched)", id, row.Status)
		}
	}
	if row, _ := svc.Get("st_stopped"); row.Status != model.SubTaskStatusStopped {
		t.Errorf("stopped child was re-armed (status=%q); a user-stopped row must stay stopped", row.Status)
	}

	// Second pass takes st_err to the cap; a third must be a no-op, which is
	// what stops a deterministically failing child from looping forever.
	if n, err := svc.ReArmErroredForRetry("batch_1", 2); err != nil || n != 0 {
		t.Fatalf("second pass re-armed %d rows (err=%v), want 0 — st_err is pending, not error", n, err)
	}
}

// TestReArmErroredForRetry_DisabledCap covers the default-off configuration
// path: retryMax <= 0 must never touch a row.
func TestReArmErroredForRetry_DisabledCap(t *testing.T) {
	d := newTestDB(t)
	seedBatchChild(t, d, "st_err", "batch_1", model.SubTaskStatusError, 1, 0)

	svc := NewSubTaskService(d)
	n, err := svc.ReArmErroredForRetry("batch_1", 0)
	if err != nil {
		t.Fatalf("ReArmErroredForRetry: %v", err)
	}
	if n != 0 {
		t.Fatalf("re-armed %d rows with retryMax=0, want 0", n)
	}
	if row, _ := svc.Get("st_err"); row.Status != model.SubTaskStatusError {
		t.Errorf("status = %q, want error untouched", row.Status)
	}
}

// TestSubTaskConfig_DefaultsAndRoundTrip locks the defaults that reproduce the
// pre-feature behaviour (serial per project, no automatic redo) and the
// clamping applied on write.
func TestSubTaskConfig_DefaultsAndRoundTrip(t *testing.T) {
	d := newTestDB(t)
	svc := NewSettingService(d)

	conc, autoRetry, retryMax, err := svc.SubTaskConfig()
	if err != nil {
		t.Fatalf("SubTaskConfig: %v", err)
	}
	if conc != 1 || autoRetry || retryMax != 1 {
		t.Fatalf("defaults = (%d, %v, %d), want (1, false, 1)", conc, autoRetry, retryMax)
	}

	if err := svc.SetSubTaskConfig(3, true, 2); err != nil {
		t.Fatalf("SetSubTaskConfig: %v", err)
	}
	conc, autoRetry, retryMax, err = svc.SubTaskConfig()
	if err != nil {
		t.Fatalf("SubTaskConfig after write: %v", err)
	}
	if conc != 3 || !autoRetry || retryMax != 2 {
		t.Fatalf("round trip = (%d, %v, %d), want (3, true, 2)", conc, autoRetry, retryMax)
	}

	// A zero/negative concurrency would wedge every dispatch — clamp to 1.
	if err := svc.SetSubTaskConfig(0, false, -5); err != nil {
		t.Fatalf("SetSubTaskConfig clamp: %v", err)
	}
	conc, autoRetry, retryMax, _ = svc.SubTaskConfig()
	if conc != 1 || autoRetry || retryMax != 0 {
		t.Fatalf("clamped = (%d, %v, %d), want (1, false, 0)", conc, autoRetry, retryMax)
	}
}

// TestProjectLimiter_PerProjectIsolation is the requirement's "从项目的角度排队"
// in one assertion: one project's slots never block a different project's.
func TestProjectLimiter_PerProjectIsolation(t *testing.T) {
	l := NewProjectLimiter(2)

	if !l.TryAcquire("proj_a") || !l.TryAcquire("proj_a") {
		t.Fatal("first two acquires for proj_a should succeed")
	}
	if l.TryAcquire("proj_a") {
		t.Fatal("third acquire for proj_a should fail (cap 2)")
	}
	if !l.TryAcquire("proj_b") {
		t.Fatal("proj_b must not be blocked by proj_a's full queue")
	}

	l.Release("proj_a")
	if !l.TryAcquire("proj_a") {
		t.Fatal("slot freed by Release should be re-acquirable")
	}

	// Raising the cap at runtime frees capacity without a restart; lowering it
	// never evicts what's already running, it only refuses new admissions.
	l.SetMax(3)
	if !l.TryAcquire("proj_a") {
		t.Fatal("raising max should admit one more")
	}
	l.SetMax(1)
	if l.TryAcquire("proj_a") {
		t.Fatal("lowering max must refuse new admissions while over the new cap")
	}
	if got := l.Active("proj_a"); got != 3 {
		t.Fatalf("active(proj_a) = %d, want 3 (in-flight work is never evicted)", got)
	}
}

// TestProjectLimiter_ReleaseClampAndConcurrency guards the two ways a counting
// bug would silently grant extra slots: an over-release going negative, and a
// racy acquire/release pair.
func TestProjectLimiter_ReleaseClampAndConcurrency(t *testing.T) {
	l := NewProjectLimiter(1)
	l.Release("never_acquired")
	l.Release("never_acquired")
	if !l.TryAcquire("never_acquired") {
		t.Fatal("acquire after spurious releases should succeed")
	}
	if l.TryAcquire("never_acquired") {
		t.Fatal("over-release must not have pushed the counter negative")
	}
	l.Release("never_acquired")
	if got := l.Active("never_acquired"); got != 0 {
		t.Fatalf("active = %d, want 0", got)
	}

	// 100 goroutines contending on a cap of 4: the number of successful
	// acquires that are never released must never exceed the cap.
	l = NewProjectLimiter(4)
	var wg sync.WaitGroup
	var mu sync.Mutex
	held := 0
	peak := 0
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !l.TryAcquire("proj_x") {
				return
			}
			mu.Lock()
			held++
			if held > peak {
				peak = held
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			held--
			mu.Unlock()
			l.Release("proj_x")
		}()
	}
	wg.Wait()
	if peak > 4 {
		t.Fatalf("peak concurrent holders = %d, want <= 4", peak)
	}
	if got := l.Active("proj_x"); got != 0 {
		t.Fatalf("active after drain = %d, want 0", got)
	}
}
