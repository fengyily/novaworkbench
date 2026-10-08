package service

import (
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

// seedBatchBase inserts the minimal project + requirement rows the
// sub_tasks fixtures rely on. INSERT OR IGNORE so it can be called
// once at the top of any test before laying down rows without
// tripping UNIQUE constraints when the True case lays down 3 rows
// under the same project/requirement.
func seedBatchBase(t *testing.T, d *db.DB, reqID string) {
	t.Helper()
	if _, err := d.Exec(
		"INSERT OR IGNORE INTO projects (id, name, local_path) VALUES ('proj_1', 'p', '/tmp/p')"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := d.Exec(
		"INSERT OR IGNORE INTO requirements (id, project_id, title) VALUES (?, 'proj_1', 'req')",
		reqID); err != nil {
		t.Fatalf("seed requirement: %v", err)
	}
}

// seedBatchSubTask inserts a single sub_tasks row that hangs under
// batchID. Caller is expected to have seeded the project/requirement
// base via seedBatchBase first — this split lets the True test lay
// down 3 rows under the same batch without re-inserting the parent
// rows. Used so the LIKE-keyword predicate inside HasCommitPushChild
// can be exercised without going through NewSubTaskService.Create
// (which derives a fresh title and would force a non-batch_id
// insertion).
func seedBatchSubTask(t *testing.T, d *db.DB, args struct {
	id, reqID, title, prompt, source, batchID string
}) {
	t.Helper()
	now := time.Now()
	stmt := `INSERT INTO sub_tasks (id, requirement_id, title, prompt, status,
		model, source, batch_id,
		input_tokens, output_tokens, created_at, updated_at)
	VALUES (?, ?, ?, ?, 'pending', '', ?, ?, 1, 2, ?, ?)`
	if _, err := d.Exec(stmt,
		args.id, args.reqID, args.title, args.prompt, args.source, args.batchID, now, now); err != nil {
		t.Fatalf("seed sub_task: %v", err)
	}
}

// TestHasCommitPushChild_True pins the happy path: when a batch contains
// a user/AI-authored row whose title or prompt contains any of the
// commit-push keywords (提交 / 推送 / push / commit / pr), the helper
// must return (true, nil). The mix also includes a push_pr row and an
// unrelated row, so the test would catch a regression that drops the
// source<>push_pr guard or widens the keyword scan to match everything.
func TestHasCommitPushChild_True(t *testing.T) {
	d := newTestDB(t)
	seedBatchBase(t, d, "req_1")
	// User/AI-authored row — must satisfy the LIKE match.
	seedBatchSubTask(t, d, struct {
		id, reqID, title, prompt, source, batchID string
	}{id: "st_user", reqID: "req_1", title: "提交推送 PR", prompt: "执行 git push 并创建 PR",
		source: model.SubTaskSourceManual, batchID: "batch_1"})
	// Auto-dispatched push_pr row — must NOT be the only satisfier.
	seedBatchSubTask(t, d, struct {
		id, reqID, title, prompt, source, batchID string
	}{id: "st_auto", reqID: "req_1", title: "auto dispatch", prompt: "auto",
		source: model.SubTaskSourcePushPR, batchID: "batch_1"})
	// Unrelated control row.
	seedBatchSubTask(t, d, struct {
		id, reqID, title, prompt, source, batchID string
	}{id: "st_unrelated", reqID: "req_1", title: "登录功能", prompt: "实现登录页",
		source: model.SubTaskSourceManual, batchID: "batch_1"})

	svc := NewSubTaskService(d)
	has, err := svc.HasCommitPushChild("batch_1")
	if err != nil {
		t.Fatalf("HasCommitPushChild: %v", err)
	}
	if !has {
		t.Errorf("HasCommitPushChild = false, want true (batch contains user-written '提交推送 PR' row)")
	}
}

// TestHasCommitPushChild_False_NoMatching pins the negative path: a
// batch with only rows whose title + prompt contain no commit-push
// keyword must return (false, nil). This is the regression guard for
// the LIKE scan being too broad (e.g. matching arbitrary single letters
// or short Chinese substrings).
func TestHasCommitPushChild_False_NoMatching(t *testing.T) {
	d := newTestDB(t)
	seedBatchBase(t, d, "req_1")
	seedBatchSubTask(t, d, struct {
		id, reqID, title, prompt, source, batchID string
	}{id: "st_login", reqID: "req_1", title: "登录功能", prompt: "实现登录页",
		source: model.SubTaskSourceManual, batchID: "batch_1"})

	svc := NewSubTaskService(d)
	has, err := svc.HasCommitPushChild("batch_1")
	if err != nil {
		t.Fatalf("HasCommitPushChild: %v", err)
	}
	if has {
		t.Errorf("HasCommitPushChild = true, want false (no row contains commit-push keywords)")
	}
}

// TestHasCommitPushChild_False_OnlyPushPR locks down the feedback-loop
// guard: when the only row in the batch is the auto-dispatched push_pr
// row itself (e.g. after autoPushPR has already claimed the batch),
// HasCommitPushChild MUST return false. If the source<>push_pr guard
// regressed, the auto-dispatch would satisfy its own bypass and the
// site-level gate would never short-circuit — producing a second
// push_pr row.
func TestHasCommitPushChild_False_OnlyPushPR(t *testing.T) {
	d := newTestDB(t)
	seedBatchBase(t, d, "req_1")
	seedBatchSubTask(t, d, struct {
		id, reqID, title, prompt, source, batchID string
	}{id: "st_auto", reqID: "req_1", title: "提交并推送 PR", prompt: "git push + create pr",
		source: model.SubTaskSourcePushPR, batchID: "batch_1"})

	svc := NewSubTaskService(d)
	has, err := svc.HasCommitPushChild("batch_1")
	if err != nil {
		t.Fatalf("HasCommitPushChild: %v", err)
	}
	if has {
		t.Errorf("HasCommitPushChild = true, want false (push_pr rows must self-exclude)")
	}
}

// TestHasCommitPushChild_EmptyBatchID locks down the short-circuit:
// passing an empty batchID must return (false, nil) without hitting
// the DB. The implementation guarantees this so callers can pass a
// possibly-empty batch.ID without an extra nil/empty guard.
func TestHasCommitPushChild_EmptyBatchID(t *testing.T) {
	d := newTestDB(t)
	// No rows seeded — the early-return on empty batchID must not
	// produce a SQL error and must not panic on a fresh DB.
	svc := NewSubTaskService(d)
	has, err := svc.HasCommitPushChild("")
	if err != nil {
		t.Fatalf("HasCommitPushChild(\"\"): %v", err)
	}
	if has {
		t.Errorf("HasCommitPushChild(\"\") = true, want false (empty batchID must short-circuit)")
	}
}
