package service

import (
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
)

// seedSubTaskRowForAdjust inserts a finished sub_tasks row that already owns
// a session_id (the precondition for "追加调整"). Mirrors
// seedSubTaskRowForRedo but with status='done' so AdjustSubTask accepts it
// (AdjustSubTask allows all non-empty statuses; the SubTaskPanel hides the
// button on running rows but the handler itself doesn't gate on status).
func seedSubTaskRowForAdjust(t *testing.T, d *db.DB, id, sessionID, sourceSID string) {
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
	if _, err := d.Exec(`INSERT INTO sub_tasks (id, requirement_id, title, prompt, status,
		session_id, source_session_id, model, source,
		created_at, updated_at, completed_at)
	VALUES (?, 'req_1', '原始子任务', 'first prompt', 'done',
		?, ?, 'claude-test', 'manual',
		?, ?, ?)`,
		id, sessionID, sourceSID, now, now, now); err != nil {
		t.Fatalf("seed sub_task: %v", err)
	}
}

// TestCreateAdjustment_InheritsParentSession locks down the user-facing
// contract behind "追加调整":
//
//   - The new row hangs under the parent via parent_subtask_id.
//   - source_session_id is exactly parent.SessionID — this is the value
//     the handler hands to SubTaskRunner.Run as the primary --resume target
//     (with --fork-session), so the adjustment inherits the parent's
//     conversation context rather than restarting from the requirement's
//     coding session. Without this column the CLI has nothing to fork off
//     and would either start fresh or hit the "源会话已失效" path.
//   - session_id is left empty on insert — the handler mints a fresh UUID
//     via UpdateSession right after CreateAdjustment returns (so the API
//     response carries the new id and the row's --session-id arg matches).
//   - Title is the "调整: <parent title>" prefix so the SubTaskPanel renders
//     the right card header.
//   - status is 'pending' so the runner picks it up.
//
// All of the above must hold for the stale-fallback chain in SubTaskRunner.Run
// (sourceSID → parentSourceSID → req.CodingSessionID) to find a meaningful
// primary source to try first.
func TestCreateAdjustment_InheritsParentSession(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForAdjust(t, d, "st_adj", "sid_parent", "sid_grandparent")

	svc := NewSubTaskService(d)
	res, err := svc.CreateAdjustment("req_1", "st_adj", "再补一行单元测试")
	if err != nil {
		t.Fatalf("CreateAdjustment: %v", err)
	}

	// 1. New row grew — the adjustment must INSERT a child row, not mutate
	//    the parent in place (same shape as RedoAsNew so the panel renders
	//    a tree of children, not a single row that flips between states).
	if res.ID == "st_adj" {
		t.Errorf("returned the parent id %q; expected a freshly minted child id", res.ID)
	}
	if res.ParentSubtaskID != "st_adj" {
		t.Errorf("returned ParentSubtaskID = %q, want st_adj", res.ParentSubtaskID)
	}
	if res.Status != "pending" {
		t.Errorf("new row status = %q, want pending", res.Status)
	}

	// 2. The core contract: source_session_id == parent.SessionID.
	//    This is what `--fork-session --resume <src>` will consume.
	if res.SourceSessionID != "sid_parent" {
		t.Errorf("new row SourceSessionID = %q, want sid_parent (parent.SessionID)", res.SourceSessionID)
	}

	// 3. session_id is left empty so the handler can stamp the freshly
	//    minted UUID via UpdateSession; the inserted row should also read
	//    back as empty before that stamp.
	if res.SessionID != "" {
		t.Errorf("new row SessionID = %q, want empty (handler mints via UpdateSession)", res.SessionID)
	}
	got, err := svc.Get(res.ID)
	if err != nil {
		t.Fatalf("Get new row: %v", err)
	}
	if got.SessionID != "" {
		t.Errorf("persisted new-row SessionID = %q, want empty", got.SessionID)
	}
	if got.SourceSessionID != "sid_parent" {
		t.Errorf("persisted SourceSessionID = %q, want sid_parent", got.SourceSessionID)
	}

	// 4. Title format matches the "调整: <parent title>" prefix.
	if got.Title != "调整: 原始子任务" {
		t.Errorf("new row Title = %q, want %q", got.Title, "调整: 原始子任务")
	}

	// 5. Parent stays untouched — same session_id, same status, same
	//    source_session_id; the panel still renders the original card
	//    alongside the new adjustment card.
	parent, err := svc.Get("st_adj")
	if err != nil {
		t.Fatalf("Get parent: %v", err)
	}
	if parent.SessionID != "sid_parent" {
		t.Errorf("parent SessionID clobbered: got %q, want sid_parent", parent.SessionID)
	}
	if parent.SourceSessionID != "sid_grandparent" {
		t.Errorf("parent SourceSessionID clobbered: got %q, want sid_grandparent", parent.SourceSessionID)
	}
	if parent.Status != "done" {
		t.Errorf("parent Status clobbered: got %q, want done", parent.Status)
	}
	if parent.ParentSubtaskID != "" {
		t.Errorf("parent.ParentSubtaskID = %q, want empty (parent itself must not be a child)", parent.ParentSubtaskID)
	}
}

// TestCreateAdjustment_EmptyParentSessionGuard locks down the precondition
// AdjustSubTask enforces in the handler (wizard_subtask.go:557-561): the
// handler short-circuits with 409 NO_SESSION when parent.SessionID is empty.
// CreateAdjustment itself does NOT enforce this (it just inserts), but the
// SQL constraint makes the resulting source_session_id the literal empty
// string — which the runner's staleSourceCandidates helper drops from the
// chain, so the first attempt would have nothing to --resume and the chain
// would fall through to parentSourceSID (also empty) → req.CodingSessionID.
//
// This test pins the row shape so a future refactor that adds an explicit
// guard at the service layer knows what the empty case looks like.
func TestCreateAdjustment_EmptyParentSessionGuard(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForAdjust(t, d, "st_adj_empty", "", "")

	svc := NewSubTaskService(d)
	res, err := svc.CreateAdjustment("req_1", "st_adj_empty", "follow-up")
	if err != nil {
		t.Fatalf("CreateAdjustment: %v", err)
	}
	if res.SourceSessionID != "" {
		t.Errorf("new row SourceSessionID = %q, want empty when parent.SessionID is empty", res.SourceSessionID)
	}
}
