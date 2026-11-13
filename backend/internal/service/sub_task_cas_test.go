package service

import (
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// The sub-task status machine had two holes that combined into a permanent
// dead end (req_0ee0bfd8de6f5da6 / st_b1c4395863f9581f):
//
//  1. MarkRunning wrote `UPDATE ... WHERE id=?` with NO status guard, while
//     its caller checked for 'stopped' with a separate READ. A Stop landing
//     in that window was silently undone — the card went 已停止 → 运行中 on
//     its own.
//  2. The terminal write (finishInternal) was unguarded too, so a late
//     Finish could resurrect a stopped row as 'done'.
//
// These tests pin the compare-and-set semantics.

// TestMarkRunning_RefusesStoppedRow is the core of the fix: a row the user
// stopped must never be revived by a dispatch that was already in flight.
func TestMarkRunning_RefusesStoppedRow(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_cas_stopped", reqID: "req_cas", status: "stopped", sessionID: "sid_a", sourceSID: "src", artifact: "半截报告"})

	svc := NewSubTaskService(d, nil)
	_, won, err := svc.MarkRunning("st_cas_stopped")
	if err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if won {
		t.Error("won = true, want false (a stopped row must not be revived)")
	}

	got, err := svc.Get("st_cas_stopped")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != model.SubTaskStatusStopped {
		t.Errorf("status = %q, want stopped", got.Status)
	}
	if got.StartedAt != nil {
		t.Errorf("started_at = %v, want NULL (a lost CAS must not stamp a start)", got.StartedAt)
	}
}

// TestMarkRunning_WinsFromPending is the happy path: a dispatched row goes
// pending → running and gets its start stamp.
func TestMarkRunning_WinsFromPending(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_cas_pending", reqID: "req_cas", status: "pending", sessionID: "sid_a", sourceSID: "src", artifact: ""})

	svc := NewSubTaskService(d, nil)
	start, won, err := svc.MarkRunning("st_cas_pending")
	if err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if !won {
		t.Fatal("won = false, want true")
	}
	if start.IsZero() {
		t.Error("start time is zero; the caller feeds it into Finish for duration_seconds")
	}

	got, err := svc.Get("st_cas_pending")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != model.SubTaskStatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
	if got.StartedAt == nil {
		t.Error("started_at is nil; want the dispatched run's start stamp")
	}
}

// TestMarkRunning_KeepsFirstStartedAt pins the COALESCE invariant: a
// re-dispatch against a row that already carries a start (「继续开发」 on a
// previously finished child) must not reset the clock, or the elapsed time of
// every earlier attempt silently disappears from duration_seconds.
func TestMarkRunning_KeepsFirstStartedAt(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_cas_restart", reqID: "req_cas", status: "pending", sessionID: "sid_a", sourceSID: "src", artifact: ""})

	original := time.Now().Add(-90 * time.Minute)
	if _, err := d.Exec(`UPDATE sub_tasks SET started_at=? WHERE id=?`, original, "st_cas_restart"); err != nil {
		t.Fatalf("seed started_at: %v", err)
	}

	svc := NewSubTaskService(d, nil)
	if _, won, err := svc.MarkRunning("st_cas_restart"); err != nil || !won {
		t.Fatalf("MarkRunning: won=%v err=%v", won, err)
	}

	got, err := svc.Get("st_cas_restart")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.StartedAt == nil {
		t.Fatal("started_at is nil")
	}
	if diff := got.StartedAt.Sub(original); diff > time.Second || diff < -time.Second {
		t.Errorf("started_at moved by %v; the first-start stamp must be preserved", diff)
	}
}

// TestMarkRunning_AllowsTerminalRowTakeover documents that the CAS is
// "don't revive a row the user STOPPED", not a mutual-exclusion lock and not
// a "only pending may run" rule.
//
// The scenario it protects: attempt 1 was stopped, the user pressed 继续
// (ContinueReset → 'pending'), and attempt 1's late Finish landed before
// attempt 2's MarkRunning — putting the row in 'done'. A tight predicate
// would abort attempt 2 there and the user's 「继续」 would silently do
// nothing. The loose predicate lets the new attempt take the row over.
func TestMarkRunning_AllowsTerminalRowTakeover(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_cas_takeover", reqID: "req_cas", status: "done", sessionID: "sid_a", sourceSID: "src", artifact: "上一轮结果"})

	svc := NewSubTaskService(d, nil)
	if _, won, err := svc.MarkRunning("st_cas_takeover"); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	} else if !won {
		t.Error("won = false, want true (a finished row may be taken over by a new attempt)")
	}
}

// TestMarkStoppedWithReason_AddsReasonAndKeepsPrior pins the banner shape:
// the reason line sits between the timestamp and the prior content, and the
// prior artifact is preserved verbatim at the end.
func TestMarkStoppedWithReason_AddsReasonAndKeepsPrior(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_reason", reqID: "req_cas", status: "running", sessionID: "sid_a", sourceSID: "src", artifact: "原报告"})

	svc := NewSubTaskService(d, nil)
	if err := svc.MarkStoppedWithReason("st_reason", "原报告", "（后台任务已不存在）"); err != nil {
		t.Fatalf("MarkStoppedWithReason: %v", err)
	}

	got, err := svc.Get("st_reason")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != model.SubTaskStatusStopped {
		t.Errorf("status = %q, want stopped", got.Status)
	}
	if !strings.HasPrefix(got.Artifact, "⏹ 用户中止于 ") {
		t.Errorf("artifact does not start with the banner: %q", got.Artifact)
	}
	if !strings.Contains(got.Artifact, "（后台任务已不存在）") {
		t.Errorf("artifact is missing the reason line: %q", got.Artifact)
	}
	if !strings.Contains(got.Artifact, "\n\n原结果：\n") {
		t.Errorf("artifact missing the 原结果 separator: %q", got.Artifact)
	}
	if !strings.HasSuffix(got.Artifact, "原报告") {
		t.Errorf("artifact does not end with the prior content: %q", got.Artifact)
	}
}

// TestMarkStopped_EmptyReasonKeepsBannerUnchanged pins the "no reason"
// variant byte for byte — MarkStopped is a thin wrapper over
// MarkStoppedWithReason, and its banner is what the frontend renders.
func TestMarkStopped_EmptyReasonKeepsBannerUnchanged(t *testing.T) {
	d := newTestDB(t)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID bool
	}{id: "st_noreason", reqID: "req_cas", status: "running", sessionID: "sid_a", sourceSID: "src", artifact: "原报告"})

	svc := NewSubTaskService(d, nil)
	if err := svc.MarkStopped("st_noreason", "原报告"); err != nil {
		t.Fatalf("MarkStopped: %v", err)
	}

	got, err := svc.Get("st_noreason")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.HasPrefix(got.Artifact, "⏹ 用户中止于 ") {
		t.Errorf("artifact does not start with the banner: %q", got.Artifact)
	}
	if !strings.Contains(got.Artifact, "\n\n原结果：\n") {
		t.Errorf("artifact missing the 原结果 separator: %q", got.Artifact)
	}
	if strings.Contains(got.Artifact, "\n\n\n") {
		t.Errorf("artifact has an empty reason line: %q", got.Artifact)
	}
}
