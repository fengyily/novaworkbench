package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestCreateReviewTask_RoundTripsNewColumns is the guard for the risk the
// design called out: the new sub_tasks columns (task_kind / review_id /
// review_round) exist in the schema but the SELECT column lists are spread
// over five call sites in this file. A missed one reads back as a zero
// value, and the requirement page's Review card would render a review with
// no round number and no report pointer.
//
// The test therefore does a full CreateReviewTask → Get round trip and
// asserts all three columns survive.
func TestCreateReviewTask_RoundTripsNewColumns(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewSubTaskService(d, nil)

	reqID := "req_review_rt"
	if _, err := d.Exec(
		`INSERT INTO requirements (id, title) VALUES (?, ?) ON CONFLICT(id) DO NOTHING`,
		reqID, "review round-trip"); err != nil {
		t.Skipf("seed requirement: %v", err)
	}

	st, err := svc.CreateReviewTask(reqID, "Review 第 1 轮", "prompt", "sonnet",
		"sid_parent", "", model.SubTaskSessionModeWithContext, "", "crv_rt", 1)
	if err != nil {
		t.Fatalf("CreateReviewTask: %v", err)
	}
	if st.TaskKind != model.SubTaskKindReview {
		t.Fatalf("TaskKind = %q, want %q", st.TaskKind, model.SubTaskKindReview)
	}
	if st.ReviewID != "crv_rt" {
		t.Fatalf("ReviewID = %q, want crv_rt", st.ReviewID)
	}
	if st.ReviewRound != 1 {
		t.Fatalf("ReviewRound = %d, want 1", st.ReviewRound)
	}

	got, err := svc.Get(st.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TaskKind != model.SubTaskKindReview {
		t.Fatalf("Get TaskKind = %q, want %q", got.TaskKind, model.SubTaskKindReview)
	}
	if got.ReviewID != "crv_rt" {
		t.Fatalf("Get ReviewID = %q, want crv_rt", got.ReviewID)
	}
	if got.ReviewRound != 1 {
		t.Fatalf("Get ReviewRound = %d, want 1", got.ReviewRound)
	}
	if got.ParentSubtaskID != "" {
		t.Fatalf("Get ParentSubtaskID = %q, want empty (root round)", got.ParentSubtaskID)
	}

	// ByReviewID is the reverse pointer the requirement page uses to link a
	// review row back to its card.
	found, err := svc.ByReviewID("crv_rt")
	if err != nil {
		t.Fatalf("ByReviewID: %v", err)
	}
	if found == nil || found.ID != st.ID {
		t.Fatalf("ByReviewID = %#v, want the row we just created", found)
	}
	missing, err := svc.ByReviewID("crv_does_not_exist")
	if err != nil {
		t.Fatalf("ByReviewID(missing): %v", err)
	}
	if missing != nil {
		t.Fatalf("ByReviewID(missing) = %#v, want nil", missing)
	}
}

// TestCreateReviewTask_ChildRound pins the round chain: a 回归 Review hangs
// off the previous round's sub-task, and the round number increments.
func TestCreateReviewTask_ChildRound(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewSubTaskService(d, nil)

	reqID := "req_review_chain"
	if _, err := d.Exec(
		`INSERT INTO requirements (id, title) VALUES (?, ?) ON CONFLICT(id) DO NOTHING`,
		reqID, "review chain"); err != nil {
		t.Skipf("seed requirement: %v", err)
	}
	first, err := svc.CreateReviewTask(reqID, "第 1 轮", "p", "sonnet", "", "",
		model.SubTaskSessionModeWithContext, "", "crv_1", 1)
	if err != nil {
		t.Fatalf("create round 1: %v", err)
	}
	second, err := svc.CreateReviewTask(reqID, "第 2 轮", "p", "sonnet", "", "",
		model.SubTaskSessionModeWithContext, first.ID, "crv_2", 2)
	if err != nil {
		t.Fatalf("create round 2: %v", err)
	}
	if second.ParentSubtaskID != first.ID {
		t.Fatalf("round 2 parent = %q, want %q", second.ParentSubtaskID, first.ID)
	}
	if second.ReviewRound != 2 {
		t.Fatalf("round 2 number = %d, want 2", second.ReviewRound)
	}
}

// TestCreate_LegacyRowsAreCodeKind guards the backwards-compatibility
// promise: every pre-existing sub-task row must read back as task_kind
// 'code', so the Review card branch never swallows an ordinary sub-task.
func TestCreate_LegacyRowsAreCodeKind(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewSubTaskService(d, nil)

	reqID := "req_code_kind"
	if _, err := d.Exec(
		`INSERT INTO requirements (id, title) VALUES (?, ?) ON CONFLICT(id) DO NOTHING`,
		reqID, "code kind"); err != nil {
		t.Skipf("seed requirement: %v", err)
	}
	st, err := svc.Create(reqID, "普通子任务", "做点事", "sonnet", "", "", 0, "",
		model.SubTaskSessionModeFork, "", model.SubTaskSourceManual, model.SubTaskReportModeAuto)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := svc.Get(st.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TaskKind != model.SubTaskKindCode {
		t.Fatalf("TaskKind = %q, want %q", got.TaskKind, model.SubTaskKindCode)
	}
	if got.ReviewID != "" || got.ReviewRound != 0 {
		t.Fatalf("review columns should stay empty for code rows, got %q / %d", got.ReviewID, got.ReviewRound)
	}
}
