package handler

// review_ai_parse_test.go — guards for the 「AI 解析结构化结果」 endpoint.
//
// It is the LAST of the three sources of a Review report's grading (the
// contract block, the report body's severity sections, and this LLM
// extraction). It is the only one that costs tokens, so it is explicit and
// asynchronous — and it must never rewrite the report the user read.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// aiParseHandler wires a ReviewHandler with the services the endpoint
// touches plus a JobStore (the extraction runs in a background job).
func aiParseHandler(t *testing.T, extract func(string) (string, error)) (*ReviewHandler, *service.CodeReviewService, *store.JobStore, string) {
	t.Helper()
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "aiparse.db")})
	if err != nil {
		t.Skipf("db init: %v", err)
	}
	projectID := "proj_aiparse"
	if _, err := d.Exec(`INSERT INTO projects (id, name, local_path) VALUES (?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		projectID, "ai parse test", "/tmp/aiparse-test"); err != nil {
		t.Skipf("seed project: %v", err)
	}
	svc := service.NewCodeReviewService(d, service.NewCodeReviewAttachmentService(d))
	jobs := store.NewJobStore(10)
	h := NewReviewHandler(nil, nil, nil, nil, jobs, service.NewJobLogService(d), nil, nil)
	h.SetCodeReviewSvc(svc)
	h.SetCodeReviewRevisionSvc(service.NewCodeReviewRevisionService(d))
	h.SetFindingsExtractor(extract)
	return h, svc, jobs, projectID
}

// aiParseReview seeds a finished review whose report carries NO structured
// findings — the state this endpoint exists for.
func aiParseReview(t *testing.T, svc *service.CodeReviewService, projectID, report string) *model.CodeReview {
	t.Helper()
	rv, err := svc.Create(&model.CodeReview{
		ProjectID:     projectID,
		ReviewType:    model.ReviewTypeRequirement,
		RequirementID: "req_aiparse",
		Branch:        "feat/x",
		BaseBranch:    "main",
		CreatedBy:     "usr_test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = svc.Delete(rv.ID) })
	if err := svc.MarkRunning(rv.ID, "job_1", "claude-opus-5"); err != nil {
		t.Fatalf("mark running: %v", err)
	}
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

// callAIParse starts an extraction and returns the job id.
func callAIParse(t *testing.T, h *ReviewHandler, reviewID string) (string, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/reviews/"+reviewID+"/reparse/ai", strings.NewReader("{}"))
	req.SetPathValue("rid", reviewID)
	rec := httptest.NewRecorder()
	h.ReparseReviewWithAI(rec, req)
	if rec.Code != http.StatusOK {
		return "", rec
	}
	var envelope struct {
		Data struct {
			JobID string `json:"job_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return envelope.Data.JobID, rec
}

// waitJob waits for the background job to reach a terminal state.
func waitJob(t *testing.T, jobs *store.JobStore, jobID string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		job, ok := jobs.Get(jobID)
		if !ok {
			t.Fatalf("job %s vanished", jobID)
		}
		_, status, _ := job.Snapshot()
		if status != store.JobRunning {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish in time", jobID)
}

// The happy path: the extractor returns a contract-shaped JSON, and the
// findings land in the structured columns — WITHOUT the report text being
// touched.
func TestReparseReviewWithAI_FillsTheFindings(t *testing.T) {
	report := "## 总体评价\n\n有一处必须修复的问题。\n\n## P0 — 必须修复\n\n**空指针** — `api/x.go:12`，这里没有判空。\n"
	h, svc, jobs, projectID := aiParseHandler(t, func(string) (string, error) {
		return `{"goal_met":"no","findings":[{"severity":"P0","file":"api/x.go","line":"12","title":"空指针","detail":"没有判空","suggestion":"补判空"},{"severity":"P1","file":"api/y.go","line":"3","title":"竞态","detail":"并发写","suggestion":"加锁"}]}`, nil
	})
	rv := aiParseReview(t, svc, projectID, report)

	jobID, rec := callAIParse(t, h, rv.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if jobID == "" {
		t.Fatal("no job id returned")
	}
	waitJob(t, jobs, jobID)

	after, err := svc.Get(rv.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.P0Count != 1 || after.P1Count != 1 {
		t.Fatalf("counts = (%d,%d,%d), want (1,1,0)", after.P0Count, after.P1Count, after.P3Count)
	}
	if after.FindingsSource != reviewFindingsSourceLLM {
		t.Fatalf("findings_source = %q, want %q", after.FindingsSource, reviewFindingsSourceLLM)
	}
	// The report text is what the user read — the extraction must not
	// rewrite it.
	if after.ResultMD != report {
		t.Fatalf("result_md changed:\n got %q\nwant %q", after.ResultMD, report)
	}
	// The run's bookkeeping is untouched: no new report was produced.
	if after.JobID != rv.JobID || after.Status != rv.Status {
		t.Fatalf("run bookkeeping changed: %+v", after)
	}
}

// A model that answers with garbage must not write anything to the row —
// and the job must end in the error state so the UI can say so.
func TestReparseReviewWithAI_GarbageDoesNotWrite(t *testing.T) {
	report := "## 总体评价\n\n看起来没问题。\n"
	h, svc, jobs, projectID := aiParseHandler(t, func(string) (string, error) {
		return "这不是 JSON，只是一段散文。", nil
	})
	rv := aiParseReview(t, svc, projectID, report)

	jobID, rec := callAIParse(t, h, rv.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	waitJob(t, jobs, jobID)

	job, _ := jobs.Get(jobID)
	_, status, exitCode := job.Snapshot()
	if status != store.JobError {
		t.Fatalf("job status = %s, want error", status)
	}
	if exitCode == 0 {
		t.Error("a failed extraction must not exit 0")
	}

	after, err := svc.Get(rv.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.FindingsJSON != "" || after.P0Count != 0 {
		t.Fatalf("a failed extraction must not write findings: %+v", after)
	}
}

// An extractor that ERRORS must also leave the row untouched.
func TestReparseReviewWithAI_ErrorLeavesTheRowAlone(t *testing.T) {
	report := "## 总体评价\n\n看起来没问题。\n"
	h, svc, jobs, projectID := aiParseHandler(t, func(string) (string, error) {
		return "", errFake
	})
	rv := aiParseReview(t, svc, projectID, report)

	jobID, rec := callAIParse(t, h, rv.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	waitJob(t, jobs, jobID)

	after, err := svc.Get(rv.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.FindingsJSON != "" {
		t.Fatalf("a failed extraction must not write findings: %+v", after)
	}
}

// The concurrency gate: a second extraction for the same review is refused
// while the first one is running — two extractions would race to write the
// same row and cost two model calls.
func TestReparseReviewWithAI_ConcurrentRunRefused(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	report := "## 总体评价\n\n看起来没问题。\n"
	h, svc, jobs, projectID := aiParseHandler(t, func(string) (string, error) {
		once.Do(func() { close(entered) })
		<-release
		return `{"findings":[{"severity":"P1","title":"x"}]}`, nil
	})
	rv := aiParseReview(t, svc, projectID, report)

	jobID, rec := callAIParse(t, h, rv.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	<-entered

	// The second call must be refused while the first is in flight.
	_, rec2 := callAIParse(t, h, rv.ID)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("second call status = %d, want 409; body = %s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), aiParseInProgress) {
		t.Fatalf("body = %s, want %s", rec2.Body.String(), aiParseInProgress)
	}

	close(release)
	waitJob(t, jobs, jobID)

	// The gate must be released once the run finished, so the user can run
	// it again.
	jobID2, rec3 := callAIParse(t, h, rv.ID)
	if rec3.Code != http.StatusOK {
		t.Fatalf("third call status = %d, want 200; body = %s", rec3.Code, rec3.Body.String())
	}
	waitJob(t, jobs, jobID2)
}

// A running review is refused: its report is not final yet.
func TestReparseReviewWithAI_RefusesRunning(t *testing.T) {
	h, svc, _, projectID := aiParseHandler(t, func(string) (string, error) {
		return `{"findings":[]}`, nil
	})
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

	_, rec := callAIParse(t, h, rv.ID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

// errFake is the error the extractor returns in the failure case.
var errFake = &fakeError{}

type fakeError struct{}

func (e *fakeError) Error() string { return "boom" }
