package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

// newCrossQATestDB opens a disposable DB and seeds one cross requirement
// (the turn table's FK needs a real owner row).
func newCrossQATestDB(t *testing.T) (*db.DB, string) {
	t.Helper()
	d := newServiceTestDB(t)
	xreqID := "xreq_qatest"
	if _, err := d.Exec(
		`INSERT INTO cross_requirements (id, title) VALUES (?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		xreqID, "qa cross requirement"); err != nil {
		t.Skipf("seed cross requirement: %v", err)
	}
	return d, xreqID
}

func TestCrossRequirementQAService_CreateListMarkFinished(t *testing.T) {
	d, xreqID := newCrossQATestDB(t)
	svc := NewCrossRequirementQAService(d)

	turn, err := svc.Create(&model.CrossRequirementQATurn{
		CrossRequirementID: xreqID,
		Question:           "接口超时取 3s 还是 5s？",
		JobID:              "job_1",
		SessionID:          "11111111-1111-4111-8111-111111111111",
		SourceSessionID:    "22222222-2222-4222-8222-222222222222",
		CreatedBy:          "usr_test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The turn is created already running, with the pre-minted session id
	// persisted — a crash between "spawned" and "answered" must still leave
	// a row that names the session the transcript belongs to.
	if turn.Status != model.QAStatusRunning {
		t.Fatalf("a new turn must start running, got %q", turn.Status)
	}
	if turn.SessionID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("the pre-minted session id must be persisted, got %q", turn.SessionID)
	}
	if turn.SourceSessionID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("the fork source snapshot must be persisted, got %q", turn.SourceSessionID)
	}

	if err := svc.MarkFinished(turn.ID, model.QAStatusDone, "就按 3s。", "sonnet",
		turn.SessionID, turn.SourceSessionID, true, ""); err != nil {
		t.Fatalf("mark finished: %v", err)
	}
	got, err := svc.Get(turn.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.QAStatusDone || got.Answer != "就按 3s。" {
		t.Fatalf("unexpected terminal row: %+v", got)
	}
	if !got.Resumed {
		t.Fatalf("the resumed flag must round-trip")
	}
	if got.FinishedAt == nil {
		t.Fatalf("a finished turn must carry finished_at")
	}
}

func TestCrossRequirementQAService_ListIsOldestFirst(t *testing.T) {
	d, xreqID := newCrossQATestDB(t)
	svc := NewCrossRequirementQAService(d)

	for _, q := range []string{"第一个问题", "第二个问题", "第三个问题"} {
		if _, err := svc.Create(&model.CrossRequirementQATurn{
			CrossRequirementID: xreqID,
			Question:           q,
			JobID:              "job_" + q,
			SessionID:          "s-" + q,
		}); err != nil {
			t.Fatalf("create %q: %v", q, err)
		}
	}
	rows, err := svc.List(xreqID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(rows))
	}
	if rows[0].Question != "第一个问题" || rows[2].Question != "第三个问题" {
		t.Fatalf("the thread must read oldest-first: %+v", rows)
	}
}

func TestCrossRequirementQAService_LatestAnsweredTurnSkipsErrors(t *testing.T) {
	d, xreqID := newCrossQATestDB(t)
	svc := NewCrossRequirementQAService(d)

	ok, err := svc.Create(&model.CrossRequirementQATurn{
		CrossRequirementID: xreqID, Question: "好的那个", JobID: "job_ok", SessionID: "s_ok",
	})
	if err != nil {
		t.Fatalf("create ok: %v", err)
	}
	if err := svc.MarkFinished(ok.ID, model.QAStatusDone, "结论", "sonnet", "s_ok", "src", true, ""); err != nil {
		t.Fatalf("finish ok: %v", err)
	}
	bad, err := svc.Create(&model.CrossRequirementQATurn{
		CrossRequirementID: xreqID, Question: "坏的那个", JobID: "job_bad", SessionID: "s_bad",
	})
	if err != nil {
		t.Fatalf("create bad: %v", err)
	}
	if err := svc.MarkFinished(bad.ID, model.QAStatusError, "崩了", "sonnet", "s_bad", "src", false, "cli-crash"); err != nil {
		t.Fatalf("finish bad: %v", err)
	}

	// The latest turn is the failed one…
	latest, err := svc.LatestTurn(xreqID)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.ID != bad.ID {
		t.Fatalf("LatestTurn must return the newest turn, got %s", latest.ID)
	}
	// …but the latest *answered* one skips it: resuming a failed turn's
	// half-written session would be worse than resuming the last completed one.
	answered, err := svc.LatestAnsweredTurn(xreqID)
	if err != nil {
		t.Fatalf("latest answered: %v", err)
	}
	if answered.ID != ok.ID {
		t.Fatalf("LatestAnsweredTurn must skip errored turns, got %s", answered.ID)
	}
}

func TestCrossRequirementQAService_MarkOrphanedOnlyTouchesRunning(t *testing.T) {
	d, xreqID := newCrossQATestDB(t)
	svc := NewCrossRequirementQAService(d)

	running, err := svc.Create(&model.CrossRequirementQATurn{
		CrossRequirementID: xreqID, Question: "跑到一半", JobID: "job_r", SessionID: "s_r",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	done, err := svc.Create(&model.CrossRequirementQATurn{
		CrossRequirementID: xreqID, Question: "已经答完", JobID: "job_d", SessionID: "s_d",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.MarkFinished(done.ID, model.QAStatusDone, "答案", "sonnet", "s_d", "", true, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}

	if err := svc.MarkOrphaned(running.ID, "后端已重启，请重新提问"); err != nil {
		t.Fatalf("mark orphaned: %v", err)
	}
	got, err := svc.Get(running.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.QAStatusError {
		t.Fatalf("a running turn must be flipped to error, got %q", got.Status)
	}
	kept, err := svc.Get(done.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if kept.Status != model.QAStatusDone {
		t.Fatalf("MarkOrphaned must not touch a finished turn, got %q", kept.Status)
	}
}

func TestCrossRequirementQAService_GetByJobID(t *testing.T) {
	d, xreqID := newCrossQATestDB(t)
	svc := NewCrossRequirementQAService(d)

	turn, err := svc.Create(&model.CrossRequirementQATurn{
		CrossRequirementID: xreqID, Question: "找得到我吗", JobID: "job_lookup", SessionID: "s_lookup",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := svc.GetByJobID("job_lookup")
	if err != nil {
		t.Fatalf("get by job id: %v", err)
	}
	if got.ID != turn.ID {
		t.Fatalf("GetByJobID returned the wrong turn: %s", got.ID)
	}
	if _, err := svc.GetByJobID("job_nope"); err == nil {
		t.Fatalf("an unknown job id must not resolve to a turn")
	}
}

func TestCrossRequirementQAService_ResetAndDelete(t *testing.T) {
	d, xreqID := newCrossQATestDB(t)
	svc := NewCrossRequirementQAService(d)

	a, _ := svc.Create(&model.CrossRequirementQATurn{CrossRequirementID: xreqID, Question: "A", JobID: "j_a", SessionID: "s_a"})
	b, _ := svc.Create(&model.CrossRequirementQATurn{CrossRequirementID: xreqID, Question: "B", JobID: "j_b", SessionID: "s_b"})

	if err := svc.DeleteTurn(a.ID); err != nil {
		t.Fatalf("delete turn: %v", err)
	}
	rows, _ := svc.List(xreqID)
	if len(rows) != 1 || rows[0].ID != b.ID {
		t.Fatalf("deleting one turn must leave the other: %+v", rows)
	}

	if err := svc.ResetThread(xreqID); err != nil {
		t.Fatalf("reset thread: %v", err)
	}
	rows, _ = svc.List(xreqID)
	if len(rows) != 0 {
		t.Fatalf("the thread must be empty after a reset: %+v", rows)
	}
}

func TestCrossRequirementQAService_DeleteByCrossRequirement(t *testing.T) {
	d, xreqID := newCrossQATestDB(t)
	svc := NewCrossRequirementQAService(d)

	if _, err := svc.Create(&model.CrossRequirementQATurn{
		CrossRequirementID: xreqID, Question: "会被清掉", JobID: "j_x", SessionID: "s_x",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.DeleteByCrossRequirement(xreqID); err != nil {
		t.Fatalf("delete by cross requirement: %v", err)
	}
	rows, _ := svc.List(xreqID)
	if len(rows) != 0 {
		t.Fatalf("every turn must be gone: %+v", rows)
	}
}
