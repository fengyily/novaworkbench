package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

// newQATestDB opens a disposable DB and seeds the project + requirement rows
// the QA turn table's FK needs.
func newQATestDB(t *testing.T) (*db.DB, string) {
	t.Helper()
	d := newServiceTestDB(t)
	projectID := "proj_qatest"
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		projectID, "qa test", "/tmp/qa-test"); err != nil {
		t.Skipf("seed project: %v", err)
	}
	reqID := "req_qatest"
	if _, err := d.Exec(
		`INSERT INTO requirements (id, project_id, title) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		reqID, projectID, "qa requirement"); err != nil {
		t.Skipf("seed requirement: %v", err)
	}
	return d, reqID
}

func TestRequirementQAService_CreateListMarkFinished(t *testing.T) {
	d, reqID := newQATestDB(t)
	svc := NewRequirementQAService(d)

	turn, err := svc.Create(&model.RequirementQATurn{
		RequirementID:   reqID,
		Stage:           model.QAStageDesign,
		Question:        "为什么这里用 turn 表而不是转录表？",
		JobID:           "job_1",
		SessionID:       "11111111-1111-4111-8111-111111111111",
		SourceSessionID: "22222222-2222-4222-8222-222222222222",
		CreatedBy:       "usr_test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(turn.ID) < 4 || turn.ID[:4] != "qat_" {
		t.Fatalf("id = %q, want a qat_ prefix", turn.ID)
	}
	if turn.Status != model.QAStatusRunning {
		t.Fatalf("status = %q, want running", turn.Status)
	}
	if turn.Answer != "" {
		t.Fatalf("answer = %q, want empty on create", turn.Answer)
	}
	// The pre-minted session id must be persisted up front — it is what a
	// crashed run would leave behind, and it is what the next turn resumes.
	if turn.SessionID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("session_id = %q, want the pre-minted id", turn.SessionID)
	}
	if turn.SourceSessionID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("source_session_id = %q, want the fork source snapshot", turn.SourceSessionID)
	}

	// A second turn on the same stage.
	turn2, err := svc.Create(&model.RequirementQATurn{
		RequirementID: reqID,
		Stage:         model.QAStageDesign,
		Question:      "那 fork 出来的会话 id 记在哪？",
		JobID:         "job_2",
		SessionID:     "33333333-3333-4333-8333-333333333333",
	})
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}

	list, err := svc.ListByStage(reqID, model.QAStageDesign)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len(list) = %d, want 2", len(list))
	}
	// Oldest-first: the thread reads as a conversation.
	if list[0].ID != turn.ID || list[1].ID != turn2.ID {
		t.Fatalf("list order = %s,%s want %s,%s", list[0].ID, list[1].ID, turn.ID, turn2.ID)
	}

	// The other stage must not bleed into this thread.
	coding, err := svc.ListByStage(reqID, model.QAStageCoding)
	if err != nil {
		t.Fatalf("list coding: %v", err)
	}
	if len(coding) != 0 {
		t.Fatalf("len(coding list) = %d, want 0", len(coding))
	}

	if err := svc.MarkFinished(turn.ID, model.QAStatusDone, "因为答案要由后端落库", "sonnet", turn.SessionID, turn.SourceSessionID, true); err != nil {
		t.Fatalf("mark finished: %v", err)
	}
	got, err := svc.Get(turn.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.QAStatusDone || got.Answer != "因为答案要由后端落库" {
		t.Fatalf("terminal row mismatch: %+v", got)
	}
	if !got.Resumed {
		t.Fatal("resumed = false, want true")
	}
	if got.FinishedAt == nil {
		t.Fatal("finished_at is nil after MarkFinished")
	}
	if got.Model != "sonnet" {
		t.Fatalf("model = %q, want sonnet", got.Model)
	}
}

func TestRequirementQAService_LatestTurns(t *testing.T) {
	d, reqID := newQATestDB(t)
	svc := NewRequirementQAService(d)

	if _, err := svc.LatestTurn(reqID, model.QAStageDesign); err == nil {
		t.Fatal("LatestTurn on an empty thread should return sql.ErrNoRows")
	}

	first, err := svc.Create(&model.RequirementQATurn{
		RequirementID: reqID,
		Stage:         model.QAStageCoding,
		Question:      "q1",
		SessionID:     "aaaa",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	second, err := svc.Create(&model.RequirementQATurn{
		RequirementID: reqID,
		Stage:         model.QAStageCoding,
		Question:      "q2",
		SessionID:     "bbbb",
	})
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}

	latest, err := svc.LatestTurn(reqID, model.QAStageCoding)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.ID != second.ID {
		t.Fatalf("LatestTurn = %s, want %s", latest.ID, second.ID)
	}

	// The answered turn is the one a new question resumes from.
	answered, err := svc.LatestAnsweredTurn(reqID, model.QAStageCoding)
	if err != nil {
		t.Fatalf("latest answered: %v", err)
	}
	if answered.ID != second.ID {
		t.Fatalf("LatestAnsweredTurn = %s, want %s", answered.ID, second.ID)
	}

	// An errored turn must NOT become the resume target — its session may be
	// half-written. The previous answered turn stays the resume source.
	if err := svc.MarkFinished(second.ID, model.QAStatusError, "boom", "", "bbbb", "", false); err != nil {
		t.Fatalf("mark error: %v", err)
	}
	answered, err = svc.LatestAnsweredTurn(reqID, model.QAStageCoding)
	if err != nil {
		t.Fatalf("latest answered 2: %v", err)
	}
	if answered.ID != first.ID {
		t.Fatalf("LatestAnsweredTurn after error = %s, want %s", answered.ID, first.ID)
	}
	// The concurrency gate still sees the errored turn (any status).
	latest, err = svc.LatestTurn(reqID, model.QAStageCoding)
	if err != nil {
		t.Fatalf("latest 2: %v", err)
	}
	if latest.ID != second.ID {
		t.Fatalf("LatestTurn after error = %s, want %s", latest.ID, second.ID)
	}
}

func TestRequirementQAService_MarkOrphaned(t *testing.T) {
	d, reqID := newQATestDB(t)
	svc := NewRequirementQAService(d)

	turn, err := svc.Create(&model.RequirementQATurn{
		RequirementID: reqID,
		Stage:         model.QAStageDesign,
		Question:      "q",
		JobID:         "job_gone",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.MarkOrphaned(turn.ID, ""); err != nil {
		t.Fatalf("mark orphaned: %v", err)
	}
	got, err := svc.Get(turn.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.QAStatusError {
		t.Fatalf("status = %q, want error", got.Status)
	}
	if got.Answer == "" {
		t.Fatal("answer is empty — the UI would show an empty bubble")
	}
}

func TestRequirementQAService_DeleteAndReset(t *testing.T) {
	d, reqID := newQATestDB(t)
	svc := NewRequirementQAService(d)

	a, err := svc.Create(&model.RequirementQATurn{
		RequirementID: reqID, Stage: model.QAStageDesign, Question: "q1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Create(&model.RequirementQATurn{
		RequirementID: reqID, Stage: model.QAStageCoding, Question: "q2",
	}); err != nil {
		t.Fatalf("create 2: %v", err)
	}

	if err := svc.DeleteTurn(a.ID); err != nil {
		t.Fatalf("delete turn: %v", err)
	}
	list, err := svc.ListByStage(reqID, model.QAStageDesign)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("len(list) = %d, want 0 after DeleteTurn", len(list))
	}
	// Deleting one turn of one stage must not touch the other stage.
	coding, err := svc.ListByStage(reqID, model.QAStageCoding)
	if err != nil {
		t.Fatalf("list coding: %v", err)
	}
	if len(coding) != 1 {
		t.Fatalf("len(coding list) = %d, want 1", len(coding))
	}

	if err := svc.ResetThread(reqID, model.QAStageCoding); err != nil {
		t.Fatalf("reset thread: %v", err)
	}
	coding, err = svc.ListByStage(reqID, model.QAStageCoding)
	if err != nil {
		t.Fatalf("list coding 2: %v", err)
	}
	if len(coding) != 0 {
		t.Fatalf("len(coding list) = %d, want 0 after ResetThread", len(coding))
	}

	if err := svc.DeleteByRequirement(reqID); err != nil {
		t.Fatalf("delete by requirement: %v", err)
	}
	design, err := svc.ListByStage(reqID, model.QAStageDesign)
	if err != nil {
		t.Fatalf("list design: %v", err)
	}
	if len(design) != 0 {
		t.Fatalf("len(design list) = %d, want 0 after DeleteByRequirement", len(design))
	}
}

func TestRequirementQAService_CreateRejectsEmpty(t *testing.T) {
	d, reqID := newQATestDB(t)
	svc := NewRequirementQAService(d)

	if _, err := svc.Create(nil); err == nil {
		t.Fatal("Create(nil) should fail")
	}
	if _, err := svc.Create(&model.RequirementQATurn{Stage: model.QAStageDesign, Question: "q"}); err == nil {
		t.Fatal("Create without requirement_id should fail")
	}
	if _, err := svc.Create(&model.RequirementQATurn{RequirementID: reqID, Stage: model.QAStageDesign}); err == nil {
		t.Fatal("Create without question should fail")
	}
}
