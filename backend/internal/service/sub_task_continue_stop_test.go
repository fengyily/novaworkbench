package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

// seedSubTaskRowForContinueStop inserts a project + requirement + a single
// sub_tasks row directly via SQL so the tests don't have to round-trip
// through NewSubTaskService.Create (which would force a fresh title derived
// from the prompt and seed the source column — neither of which the
// ContinueReset / MarkStopped assertions care about).
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
// parent_subtask_id pointing at another row, so the
// RecoverInterrupted / child-row tests can exercise the Adjust / legacy tree
// code paths without going through RedoReset / ContinueReset (which re-arm a
// row in place and therefore never create a child).
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

// TestContinueReset_ReArmsSameRow locks down the in-place continue semantics:
// pressing 继续 re-arms the SAME row (status flipped back to 'pending', the
// previous attempt's results cleared) instead of growing a child card. The
// session columns are deliberately preserved — the resumed run --resume's the
// very JSONL the previous attempt appended to, so wiping session_id here
// would strand it. The handler re-stamps the same pair through UpdateSession
// right after the reset.
func TestContinueReset_ReArmsSameRow(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_cont", reqID: "req_1", status: "stopped", sessionID: "sid_x", sourceSID: "sid_src", artifact: "原报告内容"})

	svc := NewSubTaskService(d, nil)
	got, err := svc.ContinueReset("st_cont", "")
	if err != nil {
		t.Fatalf("ContinueReset: %v", err)
	}

	if got.ID != "st_cont" {
		t.Errorf("returned id = %q, want st_cont (the clicked row, not a new child)", got.ID)
	}
	if got.Status != model.SubTaskStatusPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if got.Artifact != "" {
		t.Errorf("artifact = %q, want empty (the resumed run rewrites it)", got.Artifact)
	}
	if got.JobID != "" {
		t.Errorf("job_id = %q, want empty (handler mints a new one)", got.JobID)
	}
	if got.InputTokens != 0 || got.OutputTokens != 0 ||
		got.CacheCreationTokens != 0 || got.CacheReadTokens != 0 {
		t.Errorf("tokens not zeroed: in=%d out=%d cc=%d cr=%d",
			got.InputTokens, got.OutputTokens, got.CacheCreationTokens, got.CacheReadTokens)
	}
	// Title / prompt are the work item, not the attempt — unchanged.
	if got.Title != "t" || got.Prompt != "p" {
		t.Errorf("work item clobbered: title=%q prompt=%q", got.Title, got.Prompt)
	}
	// The session columns survive so --resume has something to point at.
	if got.SessionID != "sid_x" {
		t.Errorf("session_id = %q, want sid_x (Continue resumes this JSONL)", got.SessionID)
	}
	if got.SourceSessionID != "sid_src" {
		t.Errorf("source_session_id = %q, want sid_src", got.SourceSessionID)
	}

	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM sub_tasks WHERE requirement_id='req_1'").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("row count = %d, want 1 (in-place continue must not insert)", n)
	}
}

// TestContinueReset_AcceptsStoppedAndError pins the wider status guard: 继续
// is the user's way to pick a sub-task back up from a manual Stop as well as
// from a failure, so both source states are re-armable.
func TestContinueReset_AcceptsStoppedAndError(t *testing.T) {
	for _, status := range []string{"stopped", "error"} {
		t.Run(status, func(t *testing.T) {
			d := newTestDB(t)
			seedSubTaskRowForContinueStop(t, d, struct {
				id, reqID, status, sessionID, sourceSID, artifact string
				withBatchID bool
			}{id: "st_" + status, reqID: "req_1", status: status, sessionID: "sid_x", sourceSID: "sid_src", artifact: "报告"})

			svc := NewSubTaskService(d, nil)
			if _, err := svc.ContinueReset("st_"+status, ""); err != nil {
				t.Fatalf("ContinueReset from %q: %v", status, err)
			}
		})
	}
}

// TestContinueReset_RejectsWrongParentStatus guards the status gate: only
// errored or stopped rows may be continued. A 'done' row has nothing left to
// continue, and a 'running' row is already owned by a live dispatch — the
// clickable action there is Stop, not Continue.
func TestContinueReset_RejectsWrongParentStatus(t *testing.T) {
	for _, status := range []string{"done", "running", "pending"} {
		t.Run(status, func(t *testing.T) {
			d := newTestDB(t)
			seedSubTaskRowForContinueStop(t, d, struct {
				id, reqID, status, sessionID, sourceSID, artifact string
				withBatchID bool
			}{id: "st_" + status, reqID: "req_1", status: status, sessionID: "sid_x", sourceSID: "sid_src", artifact: "已完成报告"})

			svc := NewSubTaskService(d, nil)
			if _, err := svc.ContinueReset("st_"+status, ""); !errors.Is(err, ErrAlreadyContinuing) {
				t.Fatalf("ContinueReset on %q: err = %v, want ErrAlreadyContinuing", status, err)
			}
			got, err := svc.Get("st_" + status)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != status {
				t.Errorf("status = %q, want %q (a rejected reset must not touch the row)", got.Status, status)
			}
			if got.Artifact != "已完成报告" {
				t.Errorf("artifact clobbered by a rejected reset: %q", got.Artifact)
			}
		})
	}
}

// TestContinueReset_OnlyOneWinner is the concurrency contract for the resume
// path: two 继续 clicks on one card must not spawn two claude processes
// appending to the same JSONL. The guard is inside the UPDATE, so the second
// call sees ErrAlreadyContinuing (mapped to HTTP 409).
func TestContinueReset_OnlyOneWinner(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_race2", reqID: "req_1", status: "stopped", sessionID: "sid_x", sourceSID: "sid_src", artifact: "报告"})

	svc := NewSubTaskService(d, nil)
	if _, err := svc.ContinueReset("st_race2", ""); err != nil {
		t.Fatalf("first ContinueReset: %v", err)
	}
	if _, err := svc.ContinueReset("st_race2", ""); !errors.Is(err, ErrAlreadyContinuing) {
		t.Fatalf("second ContinueReset: err = %v, want ErrAlreadyContinuing", err)
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
//
// The row is seeded 'running' — the only state StopSubTask stops a row
// from. MarkStopped is status-guarded (a row that already reached a
// terminal state keeps it), so a 'done' row would be left alone; see
// TestMarkStopped_DoesNotClobberTerminalRow.
func TestMarkStopped_PrependsBanner(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_stop", reqID: "req_1", status: "running", sessionID: "sid_y", sourceSID: "sid_src", artifact: "原报告"})

	svc := NewSubTaskService(d, nil)
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

	svc := NewSubTaskService(d, nil)
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

// TestRecoverInterrupted_IncludesChildRows locks down the boot recovery for
// TREE CHILDREN (parent_subtask_id != '').
//
// The old behavior excluded children outright, with the rationale "a crashed
// child must be retried explicitly against its parent, not silently
// auto-recovered, or the tree's history gets muddled". That rationale is
// about not RE-RUNNING a child — and RecoverInterrupted never re-runs
// anything: it only flips the row to 'error' and waits for the user.
//
// The exclusion had a much worse side effect: every fix sub-task (created by
// CreateFixes with the Review's sub-task as parent) and every Review round
// sub-task carries batch_id='', so pass 2 (the batch pass) skipped them too.
// Such a row had NO recovery path at all, and 'running' is the one state the
// SubTaskPanel offers no button for (继续/重做 need 'error'/'stopped', 删除
// needs 'error') — a permanent dead end. That is exactly how
// st_b1c4395863f9581f (req_0ee0bfd8de6f5da6) got stuck: the nova process
// died mid-run, the row stayed 'running', and 继续/重做/删除 never appeared.
//
// So the child row is now marked 'error' — visible, retryable, deletable.
func TestRecoverInterrupted_IncludesChildRows(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_root", reqID: "req_1", status: "running", sessionID: "sid_r", sourceSID: "src", artifact: ""})
	seedSubTaskChildRow(t, d, struct {
		id, reqID, parentID, status string
	}{id: "st_child", reqID: "req_1", parentID: "st_root", status: "running"})

	svc := NewSubTaskService(d, nil)
	affected, err := svc.RecoverInterrupted()
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}

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
	if child.Status != "error" {
		t.Errorf("child status = %q, want error (a child left at 'running' has no UI affordance at all)", child.Status)
	}
	if child.ParentSubtaskID != "st_root" {
		t.Errorf("child ParentSubtaskID = %q, want st_root", child.ParentSubtaskID)
	}
	if !strings.Contains(child.Artifact, "服务在子任务执行期间重启") {
		t.Errorf("child artifact = %q, want the restart-recovery message", child.Artifact)
	}

	if affected != 2 {
		t.Errorf("affected = %d, want 2 (root + child)", affected)
	}
}

// TestRecoverInterrupted_LeavesDispatchlessPendingChildForOrphanSweep is the
// guard on the OTHER half of the child rule: a 'pending' child with NO
// job_id is the orphan sweep's row, not the boot recovery's.
//
// ListOrphanPending selects exactly `status='pending' AND job_id='' AND
// batch_id=''` and re-dispatches it (the 2026-11 fix for fix rows stranded
// between Create and UpdateJobID). If the boot recovery flipped those rows
// to 'error' it would silently disarm that self-heal — so it must not.
func TestRecoverInterrupted_LeavesDispatchlessPendingChildForOrphanSweep(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_root2", reqID: "req_2", status: "running", sessionID: "sid_r", sourceSID: "src", artifact: ""})
	seedSubTaskChildRow(t, d, struct {
		id, reqID, parentID, status string
	}{id: "st_orphan_child", reqID: "req_2", parentID: "st_root2", status: "pending"})

	svc := NewSubTaskService(d, nil)
	if _, err := svc.RecoverInterrupted(); err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}

	child, err := svc.Get("st_orphan_child")
	if err != nil {
		t.Fatalf("Get child: %v", err)
	}
	if child.Status != "pending" {
		t.Errorf("child status = %q, want pending (the orphan sweep must still be able to dispatch it)", child.Status)
	}
	// It must still be visible to the sweep. (The cutoff is an UPPER bound:
	// the sweep lists rows whose updated_at is OLDER than it, so a cutoff
	// in the future matches everything.)
	orphans, err := svc.ListOrphanPending(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("ListOrphanPending: %v", err)
	}
	found := false
	for _, o := range orphans {
		if o.ID == "st_orphan_child" {
			found = true
		}
	}
	if !found {
		t.Error("pending child without a job_id must stay visible to the orphan sweep")
	}
}

// TestRecoverInterrupted_LeavesStoppedAndDoneChildrenAlone pins the
// terminal states: a row the user stopped, and a row that finished, are not
// touched by the boot recovery.
func TestRecoverInterrupted_LeavesStoppedAndDoneChildrenAlone(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_root3", reqID: "req_3", status: "running", sessionID: "sid_r", sourceSID: "src", artifact: ""})
	seedSubTaskChildRow(t, d, struct {
		id, reqID, parentID, status string
	}{id: "st_stopped_child", reqID: "req_3", parentID: "st_root3", status: "stopped"})
	seedSubTaskChildRow(t, d, struct {
		id, reqID, parentID, status string
	}{id: "st_done_child", reqID: "req_3", parentID: "st_root3", status: "done"})

	svc := NewSubTaskService(d, nil)
	if _, err := svc.RecoverInterrupted(); err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}

	for id, want := range map[string]string{
		"st_stopped_child": "stopped",
		"st_done_child":    "done",
	} {
		got, err := svc.Get(id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if got.Status != want {
			t.Errorf("%s status = %q, want %q (terminal states must be left alone)", id, got.Status, want)
		}
	}
}

// TestMarkStopped_DoesNotClobberTerminalRow pins the status guard on the
// stop write: a row that already reached a terminal state keeps it. Without
// the guard a slow Stop (or the 6s stop watchdog, which may well land after
// the runner wrote the final state) would overwrite a finished run with
// "stopped" — and the artifact banner would replace the real report.
func TestMarkStopped_DoesNotClobberTerminalRow(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_done_stop", reqID: "req_4", status: "done", sessionID: "sid_y", sourceSID: "sid_src", artifact: "真正的报告"})

	svc := NewSubTaskService(d, nil)
	if err := svc.MarkStopped("st_done_stop", "原报告"); err != nil {
		t.Fatalf("MarkStopped: %v", err)
	}

	got, err := svc.Get("st_done_stop")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "done" {
		t.Errorf("status = %q, want done (a terminal row must not be rewritten as stopped)", got.Status)
	}
	if got.Artifact != "真正的报告" {
		t.Errorf("artifact = %q, want the original report untouched", got.Artifact)
	}
}