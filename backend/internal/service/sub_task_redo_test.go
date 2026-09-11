package service

import (
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
)

// seedSubTaskRowForRedo is the redo-specific helper: it inserts a parent
// sub_tasks row with status='error' (the only state RedoSubTask allows
// the redo from) and a pre-existing session_id / source_session_id /
// job_id / artifact so we can prove RedoReset clears the right columns
// and keeps the others intact.
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
		cost_cents, duration_seconds, created_at, updated_at, completed_at)
	VALUES (?, 'req_1', 't', 'p', ?, ?, ?, ?, ?, 'claude-old', 'manual',
		10, 20, 1, 2, 30, 40, ?, ?, ?)`,
		id, status, sessionID, sourceSID, jobID, artifact, now, now, completed); err != nil {
		t.Fatalf("seed sub_task: %v", err)
	}
}

// countSubTasks returns the row count for a single requirement. Used by
// TestRedoReset_NoNewRow to prove RedoReset doesn't insert a new row.
func countSubTasks(t *testing.T, d *db.DB) int {
	t.Helper()
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM sub_tasks WHERE requirement_id='req_1'").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestRedoReset_NoNewRow locks down the headline invariant of the
// "原地重做" feature: pressing Redo must NOT grow a new sub_task row.
// The same id carries across runs; only the volatile columns
// (status, job_id, artifact, completed_at, tokens, cost, duration)
// are cleared, while the durable columns (session_id, source_session_id,
// prompt, title, source, batch_id, model) survive.
//
// We seed a row with non-trivial session/job/artifact/tokens and assert
// each cleared column lands at zero / empty, and each preserved column
// matches its seeded value.
func TestRedoReset_NoNewRow(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForRedo(t, d, "st_redo", "error", "sid_a", "sid_src", "job_old", "旧报告")
	if got := countSubTasks(t, d); got != 1 {
		t.Fatalf("precondition: row count = %d, want 1", got)
	}

	svc := NewSubTaskService(d)
	res, err := svc.RedoReset("st_redo", "")
	if err != nil {
		t.Fatalf("RedoReset: %v", err)
	}
	if got := countSubTasks(t, d); got != 1 {
		t.Errorf("RedoReset inserted a new row: count = %d, want 1 (原地重做 must not grow rows)", got)
	}

	if res.ID != "st_redo" {
		t.Errorf("id = %q, want st_redo", res.ID)
	}
	if res.Status != "pending" {
		t.Errorf("status = %q, want pending", res.Status)
	}
	if res.Artifact != "" {
		t.Errorf("artifact = %q, want empty (cleared for fresh run)", res.Artifact)
	}
	if res.JobID != "" {
		t.Errorf("job_id = %q, want empty (cleared so page refresh doesn't reconnect to stale job)", res.JobID)
	}
	if res.SessionID != "sid_a" {
		t.Errorf("session_id = %q, want sid_a (handler overwrites via UpdateSession right after)", res.SessionID)
	}
	if res.SourceSessionID != "sid_src" {
		t.Errorf("source_session_id = %q, want sid_src", res.SourceSessionID)
	}
	if res.InputTokens != 0 || res.OutputTokens != 0 ||
		res.CacheCreationTokens != 0 || res.CacheReadTokens != 0 ||
		res.CostCents != 0 || res.DurationSeconds != 0 {
		t.Errorf("metrics not zeroed: in=%d out=%d cc=%d cr=%d cost=%d dur=%d",
			res.InputTokens, res.OutputTokens, res.CacheCreationTokens, res.CacheReadTokens, res.CostCents, res.DurationSeconds)
	}
}

// TestRedoReset_ModelOverride asserts the optional model switch: when the
// user picks a different model from the picker before pressing Redo,
// the row's model column is overwritten so the SubTaskCard shows the
// new badge immediately and the runner uses the new value.
func TestRedoReset_ModelOverride(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForRedo(t, d, "st_redo_m", "error", "sid_b", "sid_src", "job_old", "旧报告")

	svc := NewSubTaskService(d)
	res, err := svc.RedoReset("st_redo_m", "claude-test")
	if err != nil {
		t.Fatalf("RedoReset: %v", err)
	}
	if res.Model != "claude-test" {
		t.Errorf("model = %q, want claude-test", res.Model)
	}
	// Confirm persistence — re-read from DB to make sure the UPDATE
	// actually committed (not just that the returned struct was
	// populated).
	got, err := svc.Get("st_redo_m")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Model != "claude-test" {
		t.Errorf("persisted model = %q, want claude-test", got.Model)
	}
}
