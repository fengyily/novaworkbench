package handler

// review_reparse_test.go — guards for the 「重新解析结构化结果」 endpoint.
//
// The endpoint exists because of a real production report: the model wrote
// `"line": 664` where the contract asked for a string, the strict decoder
// failed the WHOLE contract, and the report lost its grading (and with it
// the ability to generate fix sub-tasks) while its prose stayed perfectly
// readable.
//
// The recovery must NOT re-run the model: the user keeps the report they
// already read.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// reparseReviewHandler wires a ReviewHandler with just the two services the
// endpoint touches.
func reparseReviewHandler(t *testing.T) (*ReviewHandler, *service.CodeReviewService, string) {
	t.Helper()
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "reparse.db")})
	if err != nil {
		t.Skipf("db init: %v", err)
	}
	projectID := "proj_reparse"
	if _, err := d.Exec(`INSERT INTO projects (id, name, local_path) VALUES (?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		projectID, "reparse test", "/tmp/reparse-test"); err != nil {
		t.Skipf("seed project: %v", err)
	}
	svc := service.NewCodeReviewService(d, service.NewCodeReviewAttachmentService(d))
	h := NewReviewHandler(nil, nil, nil, nil, nil, nil, nil, nil)
	h.SetCodeReviewSvc(svc)
	h.SetCodeReviewRevisionSvc(service.NewCodeReviewRevisionService(d))
	return h, svc, projectID
}

// seedDegradedReview creates a finished review whose result_md still carries
// the contract block — exactly the shape a mis-parsed report is left in.
func seedDegradedReview(t *testing.T, svc *service.CodeReviewService, projectID, report string) *model.CodeReview {
	t.Helper()
	rv, err := svc.Create(&model.CodeReview{
		ProjectID:  projectID,
		ReviewType: model.ReviewTypeRequirement,
		RequirementID: "req_reparse",
		Branch:     "feat/x",
		BaseBranch: "main",
		CreatedBy:  "usr_test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(rv.ID) })
	if err := svc.MarkRunning(rv.ID, "job_1", "claude-opus-5"); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	// MarkFinished with an EMPTY FindingsJSON is the degraded state: the
	// report text is whole, the grading is missing.
	if err := svc.MarkFinished(rv.ID, service.ReviewOutcome{
		Status:   model.ReviewStatusDone,
		ResultMD: report,
	}); err != nil {
		t.Fatalf("mark finished: %v", err)
	}
	got, err := svc.Get(rv.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return got
}

func callReparse(t *testing.T, h *ReviewHandler, reviewID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/reviews/"+reviewID+"/reparse", strings.NewReader("{}"))
	req.SetPathValue("rid", reviewID)
	rec := httptest.NewRecorder()
	h.ReparseReview(rec, req)
	return rec
}

// The production case, end to end: a report whose contract carries a
// numeric `line` is re-parsed and comes back GRADED — without the model
// running again.
func TestReparseReview_RecoversTheGrading(t *testing.T) {
	h, svc, projectID := reparseReviewHandler(t)

	report := "## 总体评价\n\n建议修复 P0 后合入。\n\n" +
		"```json\n{\"goal_met\":\"partial\"," +
		"\"goal_checks\":[{\"criterion\":\"分页生效\",\"status\":\"yes\",\"evidence\":\"api.go:42\"}]," +
		"\"findings\":[" +
		"{\"severity\":\"P0\",\"file\":\"a.go\",\"line\":664,\"title\":\"空指针\",\"detail\":\"d\",\"suggestion\":\"s\"}," +
		"{\"severity\":\"P1\",\"file\":\"b.go\",\"line\":\"12-20\",\"title\":\"竞态\",\"detail\":\"d\",\"suggestion\":\"s\"}," +
		"{\"severity\":\"P3\",\"file\":\"c.go\",\"line\":1,\"title\":\"补注释\",\"detail\":\"d\",\"suggestion\":\"s\"}]}\n```"

	rv := seedDegradedReview(t, svc, projectID, report)
	if rv.FindingsJSON != "" {
		t.Fatalf("precondition: the degraded row should have no findings, got %q", rv.FindingsJSON)
	}

	rec := callReparse(t, h, rv.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var envelope struct {
		Success bool `json:"success"`
		Data    struct {
			P0      int    `json:"p0"`
			P1      int    `json:"p1"`
			P3      int    `json:"p3"`
			GoalMet string `json:"goal_met"`
			Count   int    `json:"findings"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	payload := envelope.Data
	if payload.P0 != 1 || payload.P1 != 1 || payload.P3 != 1 {
		t.Fatalf("counts = (%d,%d,%d), want (1,1,1)", payload.P0, payload.P1, payload.P3)
	}
	if payload.GoalMet != "partial" {
		t.Fatalf("goal_met = %q, want partial", payload.GoalMet)
	}
	if payload.Count != 3 {
		t.Fatalf("findings = %d, want 3", payload.Count)
	}

	after, err := svc.Get(rv.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.FindingsJSON == "" {
		t.Fatal("findings_json was not back-filled")
	}
	if after.P0Count != 1 || after.P1Count != 1 || after.P3Count != 1 {
		t.Fatalf("counts not back-filled: %+v", after)
	}
	if after.GoalChecksJSON == "" {
		t.Error("goal_checks_json was not back-filled")
	}
	// The report body is the same one the user read — minus the contract
	// block, which is what a normally parsed report looks like.
	if !strings.Contains(after.ResultMD, "总体评价") {
		t.Fatalf("result_md lost the report: %q", after.ResultMD)
	}
	if strings.Contains(after.ResultMD, "goal_met") {
		t.Fatalf("contract block was not stripped: %q", after.ResultMD)
	}
	// The run's bookkeeping is untouched — no new run happened.
	if after.JobID != rv.JobID || after.Status != rv.Status {
		t.Fatalf("run bookkeeping changed: %+v", after)
	}
}

// A report with NO contract block cannot be recovered — the endpoint says so
// with a code of its own (CONTRACT_UNAVAILABLE), distinct from
// FINDINGS_UNAVAILABLE ("there is a contract, we just can't read it").
func TestReparseReview_NoContractBlock(t *testing.T) {
	h, svc, projectID := reparseReviewHandler(t)
	rv := seedDegradedReview(t, svc, projectID, "## 总体评价\n\n没有契约块的报告。")

	rec := callReparse(t, h, rv.ID)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "CONTRACT_UNAVAILABLE") {
		t.Fatalf("body = %s, want CONTRACT_UNAVAILABLE", rec.Body.String())
	}
}

// A running review is refused: its report is not final yet, and the two
// writers would race.
func TestReparseReview_RefusesRunning(t *testing.T) {
	h, svc, projectID := reparseReviewHandler(t)
	rv, err := svc.Create(&model.CodeReview{
		ProjectID: projectID,
		ReviewType: model.ReviewTypeRequirement,
		Branch:     "feat/x",
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(rv.ID) })
	if err := svc.MarkRunning(rv.ID, "job_running", "claude-opus-5"); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	rec := callReparse(t, h, rv.ID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}
