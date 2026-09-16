package service

import (
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
)

// seedSubTaskRowForRedo is the redo-specific helper: it inserts a parent
// sub_tasks row with status='error' (the only state RedoSubTask allows
// the redo from) and a pre-existing session_id / source_session_id /
// job_id / artifact so we can prove RedoAsNew leaves the parent row
// untouched and produces a fresh child row that links back via
// parent_subtask_id.
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
// TestRedoAsNew_InsertsChildRow to prove RedoAsNew DOES grow a new row
// (replaces the legacy "原地重做 must not grow rows" invariant — the
// Tree feature explicitly wants every action to land a child row).
func countSubTasks(t *testing.T, d *db.DB) int {
	t.Helper()
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM sub_tasks WHERE requirement_id='req_1'").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestRedoAsNew_InsertsChildRow locks down the new tree-friendly redo
// semantics: pressing Redo MUST grow a new sub_tasks row that hangs
// under the original as a child. The parent row keeps its terminal
// status ('error') and all its durable columns (session_id, artifact,
// tokens, completed_at) untouched so the SubTaskPanel can render both
// cards — the failed original and the fresh child — at the same time.
func TestRedoAsNew_InsertsChildRow(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForRedo(t, d, "st_redo", "error", "sid_a", "sid_src", "job_old", "旧报告")
	if got := countSubTasks(t, d); got != 1 {
		t.Fatalf("precondition: row count = %d, want 1", got)
	}

	svc := NewSubTaskService(d)
	res, err := svc.RedoAsNew("st_redo", "")
	if err != nil {
		t.Fatalf("RedoAsNew: %v", err)
	}
	if got := countSubTasks(t, d); got != 2 {
		t.Errorf("RedoAsNew did not grow a row: count = %d, want 2 (tree redo must insert a child)", got)
	}

	// Returned struct is the NEW child row, not the parent.
	if res.ID == "st_redo" {
		t.Errorf("RedoAsNew returned the parent id %q; expected a freshly minted child id", res.ID)
	}
	if res.ParentSubtaskID != "st_redo" {
		t.Errorf("returned ParentSubtaskID = %q, want st_redo", res.ParentSubtaskID)
	}
	if res.Status != "pending" {
		t.Errorf("new row status = %q, want pending", res.Status)
	}
	if res.Artifact != "" {
		t.Errorf("new row artifact = %q, want empty (fresh run)", res.Artifact)
	}
	if res.JobID != "" {
		t.Errorf("new row job_id = %q, want empty (runner mints a new one)", res.JobID)
	}
	if res.InputTokens != 0 || res.OutputTokens != 0 ||
		res.CacheCreationTokens != 0 || res.CacheReadTokens != 0 ||
		res.CostCents != 0 || res.DurationSeconds != 0 {
		t.Errorf("new row metrics not zeroed: in=%d out=%d cc=%d cr=%d cost=%d dur=%d",
			res.InputTokens, res.OutputTokens, res.CacheCreationTokens, res.CacheReadTokens, res.CostCents, res.DurationSeconds)
	}

	// Title must be the redo-prefixed shape so the SubTaskPanel can render
	// a "重做: <原标题>" header. capTitle truncates at 80 runes.
	if got := res.Title; got != "重做: t" {
		t.Errorf("new row title = %q, want %q", got, "重做: t")
	}

	// Parent row stays untouched in DB.
	parent, err := svc.Get("st_redo")
	if err != nil {
		t.Fatalf("Get parent: %v", err)
	}
	if parent.Status != "error" {
		t.Errorf("parent status = %q, want error (RedoAsNew must NOT mutate the parent)", parent.Status)
	}
	if parent.Artifact != "旧报告" {
		t.Errorf("parent artifact clobbered: got %q, want 旧报告", parent.Artifact)
	}
	if parent.JobID != "job_old" {
		t.Errorf("parent job_id clobbered: got %q, want job_old", parent.JobID)
	}
	if parent.InputTokens != 10 || parent.OutputTokens != 20 ||
		parent.CacheCreationTokens != 1 || parent.CacheReadTokens != 2 ||
		parent.CostCents != 30 || parent.DurationSeconds != 40 {
		t.Errorf("parent token counters clobbered: in=%d out=%d cc=%d cr=%d cost=%d dur=%d",
			parent.InputTokens, parent.OutputTokens, parent.CacheCreationTokens, parent.CacheReadTokens, parent.CostCents, parent.DurationSeconds)
	}
	if parent.SessionID != "sid_a" {
		t.Errorf("parent session_id clobbered: got %q, want sid_a", parent.SessionID)
	}
	if parent.SourceSessionID != "sid_src" {
		t.Errorf("parent source_session_id clobbered: got %q, want sid_src", parent.SourceSessionID)
	}
	if parent.ParentSubtaskID != "" {
		t.Errorf("parent ParentSubtaskID = %q, want empty (parent itself must not be a child)", parent.ParentSubtaskID)
	}
}

// TestRedoAsNew_ModelOverride asserts the optional model switch: when the
// user picks a different model from the picker before pressing Redo,
// the new child row's model column is set to the override so the
// SubTaskCard shows the new badge immediately and the runner uses the
// new value. The parent row's model is unchanged.
func TestRedoAsNew_ModelOverride(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForRedo(t, d, "st_redo_m", "error", "sid_b", "sid_src", "job_old", "旧报告")

	svc := NewSubTaskService(d)
	res, err := svc.RedoAsNew("st_redo_m", "claude-test")
	if err != nil {
		t.Fatalf("RedoAsNew: %v", err)
	}
	if res.Model != "claude-test" {
		t.Errorf("new row model = %q, want claude-test", res.Model)
	}
	if res.ParentSubtaskID != "st_redo_m" {
		t.Errorf("new row ParentSubtaskID = %q, want st_redo_m", res.ParentSubtaskID)
	}
	// Confirm persistence — re-read from DB to make sure the INSERT
	// actually committed (not just that the returned struct was
	// populated).
	got, err := svc.Get(res.ID)
	if err != nil {
		t.Fatalf("Get new row: %v", err)
	}
	if got.Model != "claude-test" {
		t.Errorf("persisted new-row model = %q, want claude-test", got.Model)
	}
	if got.ParentSubtaskID != "st_redo_m" {
		t.Errorf("persisted ParentSubtaskID = %q, want st_redo_m", got.ParentSubtaskID)
	}
	// Parent keeps its original model.
	parent, err := svc.Get("st_redo_m")
	if err != nil {
		t.Fatalf("Get parent: %v", err)
	}
	if parent.Model != "claude-old" {
		t.Errorf("parent model clobbered: got %q, want claude-old", parent.Model)
	}
}