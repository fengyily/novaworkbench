package service

import (
	"errors"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

// seedSubTaskRowForRedo is the redo-specific helper: it inserts a parent
// sub_tasks row with status='error' (the only state RedoReset allows the
// redo from) and a pre-existing session_id / source_session_id / job_id /
// artifact / token counters, so we can prove the reset re-arms THAT row
// instead of growing a sibling.
func seedSubTaskRowForRedo(t *testing.T, d *db.DB, id, status, sessionID, sourceSID, jobID, artifact string) {
	t.Helper()
	if _, err := d.Exec(
		"INSERT INTO projects (id, name, local_path) VALUES ('proj_1', 'p', '/tmp/p')"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := d.Exec(
		"INSERT INTO requirements (id, project_id, title) VALUES ('req_1', 'proj_1', 'req')"); err != nil {
		t.Fatalf("seed requirement: %v", err)
	}
	now := time.Now()
	completed := now.Add(-time.Minute)
	if _, err := d.Exec(`INSERT INTO sub_tasks (id, requirement_id, title, prompt, status,
		session_id, source_session_id, job_id, artifact, model, source,
		input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
		cost_cents, duration_seconds, created_at, updated_at, completed_at, started_at)
	VALUES (?, 'req_1', 't', 'p', ?, ?, ?, ?, ?, 'claude-old', 'manual',
		10, 20, 1, 2, 30, 40, ?, ?, ?, ?)`,
		id, status, sessionID, sourceSID, jobID, artifact, now, now, completed, now.Add(-2*time.Minute)); err != nil {
		t.Fatalf("seed sub_task: %v", err)
	}
}

// countSubTasks returns the row count for a single requirement. The invariant
// it guards is the whole point of the in-place reset: 重做 must not grow the
// table, so the count is identical before and after.
func countSubTasks(t *testing.T, d *db.DB) int {
	t.Helper()
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM sub_tasks WHERE requirement_id='req_1'").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestRedoReset_ReArmsSameRow locks down the in-place redo semantics: pressing
// 重做 flips the SAME row back to 'pending' and clears the previous attempt's
// results. No new row is inserted (count stays 1), so the SubTaskPanel keeps
// exactly one card per sub-task and the card's SSE subscription — keyed on
// (sub_task_id, job_id) — follows the single row through the whole retry.
func TestRedoReset_ReArmsSameRow(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForRedo(t, d, "st_redo", "error", "sid_a", "sid_src", "job_old", "旧报告")
	if got := countSubTasks(t, d); got != 1 {
		t.Fatalf("precondition: row count = %d, want 1", got)
	}
	// A stale thinking row from the failed attempt must not survive the reset.
	if _, err := d.Exec(`INSERT INTO sub_task_thinking (sub_task_id, content, updated_at)
		VALUES ('st_redo', '上一次的思考', ?)`, time.Now()); err != nil {
		t.Fatalf("seed thinking: %v", err)
	}

	svc := NewSubTaskService(d, nil)
	res, err := svc.RedoReset("st_redo", "")
	if err != nil {
		t.Fatalf("RedoReset: %v", err)
	}

	if got := countSubTasks(t, d); got != 1 {
		t.Errorf("RedoReset grew the table: count = %d, want 1 (in-place reset must not insert)", got)
	}
	if res.ID != "st_redo" {
		t.Errorf("returned id = %q, want st_redo (the clicked row, not a new child)", res.ID)
	}
	if res.Status != model.SubTaskStatusPending {
		t.Errorf("status = %q, want pending", res.Status)
	}

	// Everything the previous attempt produced is cleared.
	if res.Artifact != "" {
		t.Errorf("artifact = %q, want empty (fresh run)", res.Artifact)
	}
	if res.JobID != "" {
		t.Errorf("job_id = %q, want empty (handler mints a new one)", res.JobID)
	}
	if res.InputTokens != 0 || res.OutputTokens != 0 ||
		res.CacheCreationTokens != 0 || res.CacheReadTokens != 0 ||
		res.CostCents != 0 || res.DurationSeconds != 0 {
		t.Errorf("metrics not zeroed: in=%d out=%d cc=%d cr=%d cost=%d dur=%d",
			res.InputTokens, res.OutputTokens, res.CacheCreationTokens, res.CacheReadTokens, res.CostCents, res.DurationSeconds)
	}
	if res.CompletedAt != nil || res.StartedAt != nil {
		t.Errorf("completed_at / started_at = %v / %v, want both nil", res.CompletedAt, res.StartedAt)
	}

	// Everything describing the WORK ITEM survives — this is the same task,
	// not a copy of it.
	if res.Title != "t" || res.Prompt != "p" {
		t.Errorf("work item clobbered: title=%q prompt=%q", res.Title, res.Prompt)
	}
	if res.SessionID != "sid_a" || res.SourceSessionID != "sid_src" {
		t.Errorf("session columns clobbered: session_id=%q source_session_id=%q", res.SessionID, res.SourceSessionID)
	}
	if res.ParentSubtaskID != "" {
		t.Errorf("ParentSubtaskID = %q, want empty (the reset must not re-parent the row)", res.ParentSubtaskID)
	}
	if res.Model != "claude-old" {
		t.Errorf("model = %q, want claude-old (an empty override must not blank it)", res.Model)
	}

	// Stale thinking is dropped so the card doesn't show the failed attempt's
	// reasoning next to the new run.
	thinking, err := svc.GetThinking("st_redo")
	if err != nil {
		t.Fatalf("GetThinking: %v", err)
	}
	if thinking != "" {
		t.Errorf("thinking survived the reset: %q", thinking)
	}
}

// TestRedoReset_ClearsPhaseFragments covers the three-phase tracker columns
// (理解 / 实施 / 小结). Leaving them behind would render the previous attempt's
// fragments under the new run's report.
func TestRedoReset_ClearsPhaseFragments(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForRedo(t, d, "st_phase", "error", "sid_a", "sid_src", "job_old", "旧报告")
	if _, err := d.Exec(`UPDATE sub_tasks SET
		phase_understanding='u', phase_implementation='i', phase_summary='s', phase_emitted=1
		WHERE id='st_phase'`); err != nil {
		t.Fatalf("seed phases: %v", err)
	}

	svc := NewSubTaskService(d, nil)
	res, err := svc.RedoReset("st_phase", "")
	if err != nil {
		t.Fatalf("RedoReset: %v", err)
	}
	if res.PhaseUnderstanding != "" || res.PhaseImplementation != "" || res.PhaseSummary != "" {
		t.Errorf("phase fragments survived: u=%q i=%q s=%q",
			res.PhaseUnderstanding, res.PhaseImplementation, res.PhaseSummary)
	}
	if res.PhaseEmitted {
		t.Error("phase_emitted = true, want false")
	}
}

// TestRedoReset_ModelOverride asserts the optional model switch: picking a
// different model in the card's picker rewrites the row's model column, while
// an empty pick keeps whatever the previous attempt ran on.
func TestRedoReset_ModelOverride(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForRedo(t, d, "st_redo_m", "error", "sid_b", "sid_src", "job_old", "旧报告")

	svc := NewSubTaskService(d, nil)
	res, err := svc.RedoReset("st_redo_m", "claude-test")
	if err != nil {
		t.Fatalf("RedoReset: %v", err)
	}
	if res.Model != "claude-test" {
		t.Errorf("model = %q, want claude-test", res.Model)
	}
	got, err := svc.Get("st_redo_m")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Model != "claude-test" {
		t.Errorf("persisted model = %q, want claude-test", got.Model)
	}
	if countSubTasks(t, d) != 1 {
		t.Errorf("model override inserted a row; count = %d, want 1", countSubTasks(t, d))
	}
}

// TestRedoReset_RejectsWrongStatus locks the status guard: only a FAILED row is
// redoable. 'done' has nothing to recover; 'stopped' is the user's own decision
// and stays Continue-able; 'running' / 'pending' are already owned by a live
// dispatch.
func TestRedoReset_RejectsWrongStatus(t *testing.T) {
	for _, status := range []string{"done", "stopped", "running", "pending"} {
		t.Run(status, func(t *testing.T) {
			d := newTestDB(t)
			seedSubTaskRowForRedo(t, d, "st_"+status, status, "sid_a", "sid_src", "job_old", "旧报告")

			svc := NewSubTaskService(d, nil)
			if _, err := svc.RedoReset("st_"+status, ""); !errors.Is(err, ErrAlreadyRedoing) {
				t.Fatalf("RedoReset on %q: err = %v, want ErrAlreadyRedoing", status, err)
			}
			// The rejected reset must leave the row exactly as it was.
			got, err := svc.Get("st_" + status)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != status {
				t.Errorf("status = %q, want %q (a rejected reset must not touch the row)", got.Status, status)
			}
			if got.Artifact != "旧报告" {
				t.Errorf("artifact clobbered by a rejected reset: %q", got.Artifact)
			}
		})
	}
}

// TestRedoReset_OnlyOneWinner is the concurrency contract: two clicks on the
// same card (double-click, or a retried request) must not start two runs. The
// guard lives in the UPDATE's WHERE clause, so exactly one call flips the row
// and every later one gets ErrAlreadyRedoing — which the handler maps to 409.
func TestRedoReset_OnlyOneWinner(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForRedo(t, d, "st_race", "error", "sid_a", "sid_src", "job_old", "旧报告")

	svc := NewSubTaskService(d, nil)
	if _, err := svc.RedoReset("st_race", ""); err != nil {
		t.Fatalf("first RedoReset: %v", err)
	}
	if _, err := svc.RedoReset("st_race", ""); !errors.Is(err, ErrAlreadyRedoing) {
		t.Fatalf("second RedoReset: err = %v, want ErrAlreadyRedoing", err)
	}
	if got := countSubTasks(t, d); got != 1 {
		t.Errorf("row count = %d, want 1", got)
	}
}

// TestRedoReset_DetachesFromBatch guards the anti-double-dispatch invariant.
// A manually re-run row must not stay claimable by OrchestrationQueue —
// ClaimNextPending selects on (batch_id, status='pending'), and the manual run
// sits at 'pending' for as long as it queues behind the project's concurrency
// cap. Leaving batch_id behind would let the 10s batch tick claim it and spawn
// a second claude process on the same row and the same worktree. The batch's
// own total_children is decremented separately by the caller
// (OrchestrationBatchService.ReleaseDispatchingChild).
func TestRedoReset_DetachesFromBatch(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForRedo(t, d, "st_batch", "error", "sid_a", "sid_src", "job_old", "旧报告")
	if _, err := d.Exec(`UPDATE sub_tasks
		SET batch_id='ob_1', batch_seq=2, batch_id_seq_run='2026-09-18 03:32:43'
		WHERE id='st_batch'`); err != nil {
		t.Fatalf("stamp batch: %v", err)
	}

	svc := NewSubTaskService(d, nil)
	res, err := svc.RedoReset("st_batch", "")
	if err != nil {
		t.Fatalf("RedoReset: %v", err)
	}
	if res.BatchID != "" || res.BatchSeq != 0 {
		t.Errorf("row still attached to the batch: batch_id=%q batch_seq=%d", res.BatchID, res.BatchSeq)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM sub_tasks WHERE id='st_batch' AND batch_id=''`).Scan(&n); err != nil {
		t.Fatalf("verify detach: %v", err)
	}
	if n != 1 {
		t.Errorf("persisted batch_id = still set (or row missing); want '' for exactly one row")
	}
}

// TestReleaseDispatchingChild_Decrement covers the batch-side half of the
// detach: a still-dispatching batch expects one fewer child. A batch that has
// already moved on keeps the count it actually summarized.
func TestReleaseDispatchingChild_Decrement(t *testing.T) {
	d := newTestDB(t)
	if _, err := d.Exec(
		"INSERT INTO projects (id, name, local_path) VALUES ('proj_1', 'p', '/tmp/p')"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := d.Exec(
		"INSERT INTO requirements (id, project_id, title) VALUES ('req_1', 'proj_1', 'req')"); err != nil {
		t.Fatalf("seed requirement: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO orchestration_batches
		(id, requirement_id, orchestrator_session_id, model, status, total_children, created_at, updated_at)
		VALUES ('ob_1', 'req_1', 'sid_o', 'claude-old', ?, 3, ?, ?)`,
		model.BatchDispatching, time.Now(), time.Now()); err != nil {
		t.Fatalf("seed batch: %v", err)
	}
	svc := NewOrchestrationBatchService(d)

	if err := svc.ReleaseDispatchingChild("ob_1"); err != nil {
		t.Fatalf("ReleaseDispatchingChild: %v", err)
	}
	var total int
	if err := d.QueryRow(`SELECT total_children FROM orchestration_batches WHERE id='ob_1'`).Scan(&total); err != nil {
		t.Fatalf("read total: %v", err)
	}
	if total != 2 {
		t.Errorf("total_children = %d, want 2", total)
	}

	// A batch that already flipped to summarizing keeps its recorded total.
	if _, err := d.Exec(`UPDATE orchestration_batches SET status=?, total_children=3 WHERE id='ob_1'`,
		model.BatchSummarizing); err != nil {
		t.Fatalf("flip batch: %v", err)
	}
	if err := svc.ReleaseDispatchingChild("ob_1"); err != nil {
		t.Fatalf("ReleaseDispatchingChild on summarizing batch: %v", err)
	}
	if err := d.QueryRow(`SELECT total_children FROM orchestration_batches WHERE id='ob_1'`).Scan(&total); err != nil {
		t.Fatalf("read total: %v", err)
	}
	if total != 3 {
		t.Errorf("total_children = %d, want 3 (a finished batch must keep the count it summarized)", total)
	}

	// The counter never goes negative, no matter how many times it's released.
	if _, err := d.Exec(`UPDATE orchestration_batches SET status=?, total_children=0 WHERE id='ob_1'`,
		model.BatchDispatching); err != nil {
		t.Fatalf("reset total: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := svc.ReleaseDispatchingChild("ob_1"); err != nil {
			t.Fatalf("ReleaseDispatchingChild #%d: %v", i+1, err)
		}
	}
	if err := d.QueryRow(`SELECT total_children FROM orchestration_batches WHERE id='ob_1'`).Scan(&total); err != nil {
		t.Fatalf("read total: %v", err)
	}
	if total != 0 {
		t.Errorf("total_children = %d, want 0 (never negative)", total)
	}
}
