package service

import (
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
)

// seedSubTaskRowForContinueStop inserts a project + requirement + a single
// sub_tasks row directly via SQL so the tests don't have to round-trip
// through NewSubTaskService.Create (which would force a fresh title derived
// from the prompt and seed the source column — neither of which the
// ContinueAsNew / MarkStopped assertions care about).
func seedSubTaskRowForContinueStop(t *testing.T, d *db.DB, args struct {
	id, reqID, status, sessionID, sourceSID, artifact string
	withBatchID bool
}) {
	t.Helper()
	if _, err := d.Exec(
		"INSERT INTO projects (id, name, local_path) VALUES ('proj_1', 'p', '/tmp/p')"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := d.Exec(
		"INSERT INTO requirements (id, project_id, title) VALUES (?, 'proj_1', 'req')",
		args.reqID); err != nil {
		t.Fatalf("seed requirement: %v", err)
	}
	batchID := ""
	if args.withBatchID {
		batchID = "batch_1"
	}
	stmt := `INSERT INTO sub_tasks (id, requirement_id, title, prompt, status,
		session_id, source_session_id, job_id, artifact, model, source, batch_id,
		input_tokens, output_tokens, created_at, updated_at)
	VALUES (?, ?, 't', 'p', ?, ?, ?, '', ?, '', 'manual', ?, 1, 2, ?, ?)`
	now := time.Now()
	if _, err := d.Exec(stmt, args.id, args.reqID, args.status, args.sessionID, args.sourceSID, args.artifact, batchID, now, now); err != nil {
		t.Fatalf("seed sub_task: %v", err)
	}
}

// seedSubTaskChildRow inserts a sub_tasks row that already has
// parent_subtask_id pointing at another row, so the new
// RecoverInterrupted / child-row tests can exercise the "tree child"
// code paths without going through RedoAsNew / ContinueAsNew.
func seedSubTaskChildRow(t *testing.T, d *db.DB, args struct {
	id, reqID, parentID, status string
}) {
	t.Helper()
	now := time.Now()
	if _, err := d.Exec(`INSERT INTO sub_tasks (id, requirement_id, parent_subtask_id, title, prompt, status,
		session_id, source_session_id, job_id, artifact, model, source, batch_id,
		input_tokens, output_tokens, created_at, updated_at)
	VALUES (?, ?, ?, 'child', 'p', ?, '', '', '', '', '', 'manual', '', 1, 2, ?, ?)`,
		args.id, args.reqID, args.parentID, args.status, now, now); err != nil {
		t.Fatalf("seed child sub_task: %v", err)
	}
}

// TestContinueAsNew_NewChildRow locks down the new tree-friendly continue
// semantics: pressing Continue MUST grow a new sub_tasks row that hangs
// under the original as a child. The parent row keeps its terminal
// status ('error' or 'stopped') and its prior artifact intact so the
// SubTaskPanel can render both cards — the stopped/errored original
// (still showing the previous report) and the fresh child — at the same
// time. The new row inherits source_session_id from the parent so the
// runner can --resume the same JSONL conversation.
func TestContinueAsNew_NewChildRow(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_cont", reqID: "req_1", status: "stopped", sessionID: "sid_x", sourceSID: "sid_src", artifact: "原报告内容"})

	svc := NewSubTaskService(d)
	got, err := svc.ContinueAsNew("st_cont", "")
	if err != nil {
		t.Fatalf("ContinueAsNew: %v", err)
	}

	// Returned struct is the NEW child row.
	if got.ID == "st_cont" {
		t.Errorf("ContinueAsNew returned the parent id %q; expected a freshly minted child id", got.ID)
	}
	if got.ParentSubtaskID != "st_cont" {
		t.Errorf("returned ParentSubtaskID = %q, want st_cont", got.ParentSubtaskID)
	}
	if got.Status != "pending" {
		t.Errorf("new row status = %q, want pending", got.Status)
	}
	if got.JobID != "" {
		t.Errorf("new row job_id = %q, want empty (runner mints a new one)", got.JobID)
	}
	if got.InputTokens != 0 || got.OutputTokens != 0 ||
		got.CacheCreationTokens != 0 || got.CacheReadTokens != 0 {
		t.Errorf("new row tokens not zeroed: in=%d out=%d cc=%d cr=%d",
			got.InputTokens, got.OutputTokens, got.CacheCreationTokens, got.CacheReadTokens)
	}

	// Title must be the continue-prefixed shape so the SubTaskPanel can
	// render a "继续: <原标题>" header.
	if got.Title != "继续: t" {
		t.Errorf("new row title = %q, want 继续: t", got.Title)
	}

	// The new child carries the parent's session id as source_session_id so
	// the runner can --resume the same conversation; the new row's own
	// session_id stays empty (runner mints a fresh one).
	if got.SourceSessionID != "sid_x" {
		t.Errorf("new row source_session_id = %q, want sid_x (inherited for --resume)", got.SourceSessionID)
	}

	// Parent row stays untouched in DB — its previous artifact is still
	// visible mid-run until the new attempt lands a terminal artifact.
	parent, err := svc.Get("st_cont")
	if err != nil {
		t.Fatalf("Get parent: %v", err)
	}
	if parent.Status != "stopped" {
		t.Errorf("parent status = %q, want stopped (ContinueAsNew must NOT mutate the parent)", parent.Status)
	}
	if parent.Artifact != "原报告内容" {
		t.Errorf("parent artifact clobbered: got %q, want 原报告内容", parent.Artifact)
	}
	if parent.JobID != "" {
		t.Errorf("parent job_id clobbered: got %q, want empty (was empty in fixture)", parent.JobID)
	}
	if parent.SessionID != "sid_x" {
		t.Errorf("parent session_id clobbered: got %q, want sid_x", parent.SessionID)
	}
	if parent.SourceSessionID != "sid_src" {
		t.Errorf("parent source_session_id clobbered: got %q, want sid_src", parent.SourceSessionID)
	}
	if parent.ParentSubtaskID != "" {
		t.Errorf("parent ParentSubtaskID = %q, want empty (parent itself must not be a child)", parent.ParentSubtaskID)
	}

	// Row count for the requirement grew by exactly one.
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM sub_tasks WHERE requirement_id='req_1'").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("row count = %d, want 2 (ContinueAsNew must insert a child)", n)
	}
}

// TestContinueAsNew_RejectsWrongParentStatus guards the new status gate:
// only errored or stopped parents may be continued. A 'done' or
// 'pending' parent has no reason to spawn a child — the user shouldn't
// double up on a finished run or race against a still-running one.
func TestContinueAsNew_RejectsWrongParentStatus(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_done", reqID: "req_1", status: "done", sessionID: "sid_x", sourceSID: "sid_src", artifact: "已完成报告"})

	svc := NewSubTaskService(d)
	if _, err := svc.ContinueAsNew("st_done", ""); err == nil {
		t.Fatal("ContinueAsNew on a 'done' parent succeeded; want error")
	}

	// 'running' must also be rejected (the existing runner owns it).
	if _, err := d.Exec(`UPDATE sub_tasks SET status='running' WHERE id='st_done'`); err != nil {
		t.Fatalf("flip status: %v", err)
	}
	if _, err := svc.ContinueAsNew("st_done", ""); err == nil {
		t.Fatal("ContinueAsNew on a 'running' parent succeeded; want error")
	}
}

// TestMarkStopped_PrependsBanner confirms the user-facing banner format
// the StopSubTask handler relies on for the SubTaskPanel's "⏹ 用户中止…"
// rendering. We anchor on three invariants:
//
//   - the artifact starts with the "⏹ 用户中止于" prefix so a regex test
//     in the frontend can pick it up,
//   - the "\n\n原结果：\n" separator splits banner from prior content,
//   - the prior artifact is preserved verbatim at the end so the user
//     can still scroll back through whatever was on screen at stop time.
func TestMarkStopped_PrependsBanner(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_stop", reqID: "req_1", status: "done", sessionID: "sid_y", sourceSID: "sid_src", artifact: "原报告"})

	svc := NewSubTaskService(d)
	if err := svc.MarkStopped("st_stop", "原报告"); err != nil {
		t.Fatalf("MarkStopped: %v", err)
	}

	got, err := svc.Get("st_stop")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "stopped" {
		t.Errorf("status = %q, want stopped", got.Status)
	}
	if !strings.HasPrefix(got.Artifact, "⏹ 用户中止于 ") {
		t.Errorf("artifact does not start with banner: %q", got.Artifact)
	}
	if !strings.Contains(got.Artifact, "\n\n原结果：\n") {
		t.Errorf("artifact missing 原结果 separator: %q", got.Artifact)
	}
	if !strings.HasSuffix(got.Artifact, "原报告") {
		t.Errorf("artifact does not end with prior content: %q", got.Artifact)
	}
	if got.CompletedAt == nil {
		t.Error("completed_at is nil; want a timestamp from MarkStopped")
	}
}

// TestRecoverInterrupted_PreservesExistingArtifact locks down the case
// where the backend restarted mid-run but the user had already managed
// to halt the sub-task (status=stopped, banner already in artifact) or
// the row otherwise carries durable content. The new CASE-based UPDATE
// must leave the artifact alone and only stamp the recovery message
// when the column is empty.
//
// We seed two rows: one stopped (must remain stopped — manual branch
// WHERE filters it out), and one running with existing artifact (must
// be flipped to error but keep its artifact).
func TestRecoverInterrupted_PreservesExistingArtifact(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_keep", reqID: "req_1", status: "running", sessionID: "sid_z", sourceSID: "sid_src", artifact: "用户已写到一半的报告"})

	svc := NewSubTaskService(d)
	affected, err := svc.RecoverInterrupted()
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if affected < 1 {
		t.Errorf("expected ≥1 row affected, got %d", affected)
	}

	got, err := svc.Get("st_keep")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "error" {
		t.Errorf("status = %q, want error", got.Status)
	}
	if got.Artifact != "用户已写到一半的报告" {
		t.Errorf("artifact clobbered by recovery: got %q", got.Artifact)
	}
}

// TestRecoverInterrupted_ExcludesChildRows locks down the new safety
// guard added for the Tree feature: a child row (parent_subtask_id != '')
// that errored on backend crash must NOT be silently flipped to 'error'
// by RecoverInterrupted, because that would muddle the tree — the
// parent's "what failed" card is supposed to remain the visible source
// of truth. The user must explicitly trigger Redo / Continue on the
// parent to retry the child.
//
// We seed two rows under the same requirement: a top-level 'running' row
// (must be recovered to 'error') and a child row with the same 'running'
// status pointing at a phantom parent (must stay 'running' — i.e. NOT
// recovered, NOT flipped).
func TestRecoverInterrupted_ExcludesChildRows(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_root", reqID: "req_1", status: "running", sessionID: "sid_r", sourceSID: "src", artifact: ""})
	seedSubTaskChildRow(t, d, struct {
		id, reqID, parentID, status string
	}{id: "st_child", reqID: "req_1", parentID: "st_root", status: "running"})

	svc := NewSubTaskService(d)
	affected, err := svc.RecoverInterrupted()
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}

	// Only the root row should have been flipped. Anything else (the
	// child) being affected means the parent_subtask_id guard regressed.
	root, err := svc.Get("st_root")
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	if root.Status != "error" {
		t.Errorf("root status = %q, want error (top-level orphans must still be recovered)", root.Status)
	}

	child, err := svc.Get("st_child")
	if err != nil {
		t.Fatalf("Get child: %v", err)
	}
	if child.Status != "running" {
		t.Errorf("child status = %q, want running (child rows must NOT be auto-recovered; tree history must stay explicit)", child.Status)
	}
	if child.ParentSubtaskID != "st_root" {
		t.Errorf("child ParentSubtaskID = %q, want st_root", child.ParentSubtaskID)
	}

	// affected == 1 (just st_root). If a future refactor accidentally
	// re-removes the parent_subtask_id guard, affected would jump to 2
	// and this assertion fires.
	if affected != 1 {
		t.Errorf("affected = %d, want 1 (only the top-level root must be touched)", affected)
	}
}