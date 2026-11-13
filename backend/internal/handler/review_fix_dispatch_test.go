package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// TestCreateFixes_DispatchesTheFixSubTask is the regression test for the
// bug that left a requirement-level Review's fix sub-task stuck at
// "排队中 · 等待项目空闲" forever.
//
// What happened: CreateFixes INSERTed the fix row with
// SubTaskService.Create — which creates a row with status='pending' and NO
// job_id and NO batch_id — and then returned. Nothing ever dispatched it:
//
//   - the orchestration tick claims by batch_id (ClaimNextPending), and a
//     manual row has none;
//   - the manual path only runs on an explicit user action, and every such
//     action (redo / continue / adjust / delete) is gated on a terminal
//     status;
//   - the boot recovery (RecoverInterrupted) only reaches a child row when it
//     already carries a job_id — a fix row's parent IS the Review sub-task,
//     so a row stranded BEFORE the job id was stamped stays pending and is
//     this sweep's business.
//
// So the row was claimed by nobody: it showed "排队中" forever, could not be
// started, cancelled or deleted, and the documented
// "fix → re-review until P0/P1 clear" loop could never converge.
//
// The invariant this test pins: a fix sub-task MUST come out of CreateFixes
// carrying a job_id (the thing that makes a row dispatchable) and a
// session_id (the pre-minted claude session the run resumes from).
func TestCreateFixes_DispatchesTheFixSubTask(t *testing.T) {
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Skipf("db init: %v", err)
	}
	defer d.Close()

	const reqID = "req_fix_dispatch"
	const revID = "crv_fix_dispatch"

	if _, err := d.Exec(`INSERT INTO requirements (id, title, project_id) VALUES (?, ?, ?)`,
		reqID, "修复派发需求", "proj_x"); err != nil {
		t.Fatalf("seed requirement: %v", err)
	}
	findings := `[{"severity":"P1","file":"a.go","line":"10","title":"缺陷 b","detail":"登记行与代码不符","suggestion":"改掉"}]`
	if _, err := d.Exec(`INSERT INTO code_reviews
		(id, project_id, review_type, requirement_id, role_key, status, title, description, model, findings_json, p1_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		revID, "proj_x", model.ReviewTypeRequirement, reqID, "reviewer",
		model.ReviewStatusDone, "Review · 修复派发需求", "目标", "sonnet", findings, 1); err != nil {
		t.Fatalf("seed code_review: %v", err)
	}

	subTaskSvc := service.NewSubTaskService(d, nil)
	reqSvc := service.NewRequirementService(d, nil)
	reviewSvc := service.NewCodeReviewService(d, nil)
	jobs := store.NewJobStore(50)
	jobLogSvc := service.NewJobLogService(d)
	runner := NewSubTaskRunner(nil, subTaskSvc, reqSvc, jobs, nil, nil, jobLogSvc, nil, nil, nil, nil, nil, nil, 0, nil, nil, nil)

	h := NewRequirementReviewHandler(reqSvc, nil, subTaskSvc, reviewSvc, nil, nil, nil, nil, runner, jobs)

	body := strings.NewReader(`{"finding_ref":"P1-1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/"+reqID+"/reviews/"+revID+"/fixes", body)
	req.SetPathValue("id", reqID)
	req.SetPathValue("rid", revID)
	rec := httptest.NewRecorder()
	h.CreateFixes(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			SubTaskIDs []string `json:"sub_task_ids"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}
	if len(resp.Data.SubTaskIDs) != 1 {
		t.Fatalf("sub_task_ids = %v, want exactly one fix row", resp.Data.SubTaskIDs)
	}

	st, err := subTaskSvc.Get(resp.Data.SubTaskIDs[0])
	if err != nil {
		t.Fatalf("Get fix sub-task: %v", err)
	}
	// The two columns whose absence IS the bug. A pending row without a
	// job_id belongs to no dispatcher and stays "排队中" forever.
	if st.JobID == "" {
		t.Error("fix sub-task has no job_id — nothing can dispatch it; it would sit at 排队中 forever")
	}
	if st.SessionID == "" {
		t.Error("fix sub-task has no session_id — the run has no claude session to resume")
	}
	// The fix must hang off the Review sub-task so the tree stays walkable.
	// (No Review sub-task row exists in this fixture, so it attaches to the
	// requirement root — either way the parent must be recorded, never lost.)
	if st.RequirementID != reqID {
		t.Errorf("RequirementID = %q, want %q", st.RequirementID, reqID)
	}
	if st.TaskKind != model.SubTaskKindCode {
		t.Errorf("TaskKind = %q, want %q (a fix is a code sub-task)", st.TaskKind, model.SubTaskKindCode)
	}
	if st.Source != model.SubTaskSourceManual {
		t.Errorf("Source = %q, want %q", st.Source, model.SubTaskSourceManual)
	}
	if st.SessionMode != model.SubTaskSessionModeFork {
		t.Errorf("SessionMode = %q, want %q (the fixer forks the developer's session)",
			st.SessionMode, model.SubTaskSessionModeFork)
	}

	// The stronger half of the assertion: the row must actually LEAVE
	// 'pending'. A dispatched row that reaches a terminal state proves the
	// run was LAUNCHED, not merely stamped with a job id.
	//
	// This fixture has no worktree, so the runner fails fast with
	// "无法解析工作目录" and the row lands on 'error'. Waiting for the
	// terminal state (rather than just "not pending") also keeps the
	// background goroutine from outliving the test's database.
	deadline := time.Now().Add(15 * time.Second)
	for {
		st, err = subTaskSvc.Get(resp.Data.SubTaskIDs[0])
		if err != nil {
			t.Fatalf("Get fix sub-task: %v", err)
		}
		if st.Status == model.SubTaskStatusError || st.Status == model.SubTaskStatusDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fix sub-task is still %q after 15s — it was never dispatched", st.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
