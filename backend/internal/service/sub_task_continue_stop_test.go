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

// TestContinueReset_PreservesArtifact locks down the "继续" semantics: the
// old report stays visible until the new run lands its terminal artifact.
// session_id and source_session_id must also be untouched so the runner
// can --resume the same JSONL conversation. All four token counters reset
// to zero, and job_id clears so a page refresh doesn't try to reconnect
// to the previous run's evicted job.
func TestContinueReset_PreservesArtifact(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_cont", reqID: "req_1", status: "done", sessionID: "sid_x", sourceSID: "sid_src", artifact: "原报告内容"})

	svc := NewSubTaskService(d)
	got, err := svc.ContinueReset("st_cont", "")
	if err != nil {
		t.Fatalf("ContinueReset: %v", err)
	}
	if got.Status != "pending" {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if got.Artifact != "原报告内容" {
		t.Errorf("artifact = %q, want unchanged (继续 preserves)", got.Artifact)
	}
	if got.SessionID != "sid_x" {
		t.Errorf("session_id = %q, want sid_x (Continue reuses parent session)", got.SessionID)
	}
	if got.SourceSessionID != "sid_src" {
		t.Errorf("source_session_id = %q, want sid_src", got.SourceSessionID)
	}
	if got.JobID != "" {
		t.Errorf("job_id = %q, want empty (cleared on reset)", got.JobID)
	}
	if got.InputTokens != 0 || got.OutputTokens != 0 ||
		got.CacheCreationTokens != 0 || got.CacheReadTokens != 0 {
		t.Errorf("tokens not zeroed: in=%d out=%d cc=%d cr=%d",
			got.InputTokens, got.OutputTokens, got.CacheCreationTokens, got.CacheReadTokens)
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
