package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/store"
)

// 跨项目需求作业层的状态可发现性 + 归属校验。
//
// 「关掉页面再回来还能接回正在跑的方案作业」依赖两件事：
//
//  1. GET .../design/state 能报出最近一次作业的状态与 job id（锚点）；
//  2. SSE 端点只肯把自己那个跨项目需求的作业吐出来（归属校验）。
//
// 这两个都是纯 HTTP 层的契约，不依赖 claude CLI，所以可以在这里直接测。

// callDesignState issues GET /api/cross-requirements/{id}/design/state.
func callDesignState(t *testing.T, h *CrossRequirementHandler, xreqID string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/cross-requirements/"+xreqID+"/design/state", nil)
	req.SetPathValue("id", xreqID)
	rec := httptest.NewRecorder()
	h.DesignState(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// createCrossRequirement creates a cross requirement over the throwaway DB.
func createCrossRequirement(t *testing.T, h *CrossRequirementHandler, title string, projectIDs ...string) string {
	t.Helper()
	code, env := callCross(t, h.Create, http.MethodPost, "/api/cross-requirements",
		model.CreateCrossRequirementReq{Title: title, ProjectIDs: projectIDs})
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %v", code, env)
	}
	data, _ := env["data"].(map[string]any)
	id, _ := data["id"].(string)
	if id == "" {
		t.Fatalf("create returned no id: %v", env)
	}
	return id
}

// 没跑过整体方案 → 200 + status=none（不是错误）。
func TestCrossRequirementHandler_DesignStateNone(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	code, env := callCross(t, h.Create, http.MethodPost, "/api/cross-requirements",
		model.CreateCrossRequirementReq{Title: "还没跑方案", ProjectIDs: []string{p1}})
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %v", code, env)
	}
	data, _ := env["data"].(map[string]any)
	id, _ := data["id"].(string)

	code, env = callDesignState(t, h, id)
	if code != http.StatusOK {
		t.Fatalf("design state status = %d, body = %v", code, env)
	}
	state, _ := env["data"].(map[string]any)
	if state["status"] != "none" {
		t.Fatalf("status = %v, want none", state["status"])
	}
	if state["running"] != false {
		t.Fatalf("running = %v, want false", state["running"])
	}
	if state["job_id"] != "" {
		t.Fatalf("job_id = %v, want empty", state["job_id"])
	}
}

// 作业在跑 → status=running + job id（前端靠它接回那条流）。
func TestCrossRequirementHandler_DesignStateRunning(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	id := createCrossRequirement(t, h, "方案生成中", p1)

	job := h.jobs.Create(id)
	job.SetType("cross_design")
	h.runs.start(job.ID, id, crossRunDesign, "", "claude-opus")

	code, env := callDesignState(t, h, id)
	if code != http.StatusOK {
		t.Fatalf("design state status = %d, body = %v", code, env)
	}
	state, _ := env["data"].(map[string]any)
	if state["status"] != "running" {
		t.Fatalf("status = %v, want running", state["status"])
	}
	if state["running"] != true {
		t.Fatalf("running = %v, want true", state["running"])
	}
	if state["job_id"] != job.ID {
		t.Fatalf("job_id = %v, want %s", state["job_id"], job.ID)
	}
	// The reported id must be the one the JobStore holds — the page re-attaches
	// the SSE stream with it.
	if _, ok := h.jobs.Get(job.ID); !ok {
		t.Fatalf("job %s is not in the store", job.ID)
	}
}

// 作业成功结束 → status=done + 产物 + finished_at。
func TestCrossRequirementHandler_DesignStateDone(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	id := createCrossRequirement(t, h, "方案已完成", p1)

	job := h.jobs.Create(id)
	job.SetType("cross_design")
	h.runs.start(job.ID, id, crossRunDesign, "", "claude-opus")
	h.runs.setResult(job.ID, "# 整体方案\n\n正文", nil, "")
	h.runs.finish(job.ID, store.JobDone, "")

	code, env := callDesignState(t, h, id)
	if code != http.StatusOK {
		t.Fatalf("design state status = %d, body = %v", code, env)
	}
	state, _ := env["data"].(map[string]any)
	if state["status"] != "done" {
		t.Fatalf("status = %v, want done", state["status"])
	}
	if state["running"] != false {
		t.Fatalf("running = %v, want false", state["running"])
	}
	if design, _ := state["design"].(string); design == "" {
		t.Fatalf("design = %v, want the produced markdown", state["design"])
	}
	if finished, _ := state["finished_at"].(float64); finished <= 0 {
		t.Fatalf("finished_at = %v, want > 0", state["finished_at"])
	}
}

// 作业失败 → status=error + 失败原因。
func TestCrossRequirementHandler_DesignStateError(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	id := createCrossRequirement(t, h, "方案失败", p1)

	job := h.jobs.Create(id)
	job.SetType("cross_design")
	h.runs.start(job.ID, id, crossRunDesign, "", "claude-opus")
	h.runs.finish(job.ID, store.JobError, "未产出整体方案，请重试")

	code, env := callDesignState(t, h, id)
	if code != http.StatusOK {
		t.Fatalf("design state status = %d, body = %v", code, env)
	}
	state, _ := env["data"].(map[string]any)
	if state["status"] != "error" {
		t.Fatalf("status = %v, want error", state["status"])
	}
	if msg, _ := state["error_message"].(string); msg == "" {
		t.Fatalf("error_message = %v, want a reason", state["error_message"])
	}
}

// SSE 端点只肯吐出自己那个跨项目需求的作业。
func TestCrossRequirementHandler_StreamJobOwnership(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	owner := createCrossRequirement(t, h, "作业归属", p1)
	other := createCrossRequirement(t, h, "另一个需求", p1)

	job := h.jobs.Create(owner)
	job.SetType("cross_design")
	// Finish it: the SSE pump only returns once the job reaches a terminal
	// state, so a still-running job would block the test forever.
	job.Append(store.LogLine{Type: "phase", Content: "🧭 正在生成跨项目整体方案…"})
	job.Finish(0, store.JobDone)

	// The owning requirement may stream it.
	req := httptest.NewRequest(http.MethodGet, "/api/cross-requirements/"+owner+"/design/"+job.ID+"/stream", nil)
	req.SetPathValue("id", owner)
	req.SetPathValue("job_id", job.ID)
	rec := httptest.NewRecorder()
	h.StreamDesignJob(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner stream status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" {
		t.Fatalf("owner stream content type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rec.Body.String(), "job_done") {
		t.Fatalf("owner stream body = %q, want a job_done frame", rec.Body.String())
	}

	// Another cross requirement may not.
	req2 := httptest.NewRequest(http.MethodGet, "/api/cross-requirements/"+other+"/design/"+job.ID+"/stream", nil)
	req2.SetPathValue("id", other)
	req2.SetPathValue("job_id", job.ID)
	rec2 := httptest.NewRecorder()
	h.StreamDesignJob(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("foreign stream status = %d, want 404", rec2.Code)
	}
}
