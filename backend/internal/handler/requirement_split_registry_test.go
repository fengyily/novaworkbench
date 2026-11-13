package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/store"
)

// ── 登记处 ─────────────────────────────────────────────────────────────────

// The registry is the single source of truth for one split run: status,
// candidates, error, execution env — all keyed by job id, plus a
// requirement → job mapping that is the key to "find the previous run".
func TestSplitRuns_RegistryLifecycle(t *testing.T) {
	runs := newSplitRuns(4)

	runs.start("job_1", "req_a", "sonnet", "")
	got := runs.get("job_1")
	if got == nil {
		t.Fatal("job_1 is not registered")
	}
	if got.Status != store.JobRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
	if got.Model != "sonnet" {
		t.Errorf("model = %q, want sonnet", got.Model)
	}
	if got.Candidates != nil {
		t.Errorf("candidates = %v, want nil", got.Candidates)
	}

	runs.finish("job_1", store.JobDone, "", []prompt.SplitTask{{Title: "A", Order: 1}}, nil)
	got = runs.get("job_1")
	if got == nil || got.Status != store.JobDone {
		t.Fatalf("run = %+v, want status done", got)
	}
	if len(got.Candidates) != 1 {
		t.Errorf("candidates = %v, want 1", got.Candidates)
	}
	if got.FinishedAt.IsZero() {
		t.Error("finished_at was not set")
	}

	// The lock is released when the job ends, but the registration stays —
	// this is exactly what lets the reopened modal find the finished run.
	if latest := runs.latestFor("req_a"); latest == nil || latest.JobID != "job_1" {
		t.Errorf("latestFor(req_a) = %+v, want job_1", latest)
	}

	// get() hands out a copy — mutating it must not corrupt the registry.
	got.Candidates[0].Title = "mutated"
	if again := runs.get("job_1"); again.Candidates[0].Title != "A" {
		t.Errorf("candidates were mutated through the returned copy: %q", again.Candidates[0].Title)
	}
}

// A consumed run (its candidates were persisted) must read as absent —
// reopening the modal must land on the config stage, not on a stale table.
func TestSplitRuns_ConsumeLatest(t *testing.T) {
	runs := newSplitRuns(4)
	runs.start("job_1", "req_a", "", "")
	runs.finish("job_1", store.JobDone, "", []prompt.SplitTask{{Title: "A", Order: 1}}, nil)

	if runs.latestFor("req_a") == nil {
		t.Fatal("latestFor(req_a) = nil, want the run")
	}
	runs.consumeLatest("req_a")
	if runs.latestFor("req_a") != nil {
		t.Error("latestFor(req_a) is not nil after consumeLatest, want nil")
	}
	// The run itself is still there — only its "relevance" is gone.
	if runs.get("job_1") == nil {
		t.Error("the run was dropped from byJob")
	}
}

// Eviction may only ever drop FINISHED runs: evicting a running one would
// make the reopened modal unable to reattach to the split still in flight.
func TestSplitRuns_EvictOldestSkipsRunningRuns(t *testing.T) {
	runs := newSplitRuns(2)

	runs.start("job_running", "req_a", "", "")
	runs.start("job_done1", "req_b", "", "")
	runs.finish("job_done1", store.JobDone, "", nil, nil)
	runs.start("job_done2", "req_c", "", "")
	runs.finish("job_done2", store.JobDone, "", nil, nil)

	if runs.get("job_running") == nil {
		t.Error("the running run was evicted — it must never be")
	}
	if runs.get("job_done1") != nil {
		t.Error("the oldest finished run was not evicted")
	}
	if runs.get("job_done2") == nil {
		t.Error("the newest finished run was evicted")
	}
	// The evicted run must not stay reachable through the reqID mapping.
	if runs.latestFor("req_b") != nil {
		t.Error("latestFor(req_b) still resolves the evicted run")
	}
}

// ── GET /api/requirements/{id}/split/state ─────────────────────────────────

// splitStateBody calls the state endpoint and returns the decoded envelope.
func splitStateBody(t *testing.T, h *WizardHandler, reqID string) (int, map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/requirements/"+reqID+"/split/state", nil)
	req.SetPathValue("id", reqID)
	rec := httptest.NewRecorder()
	h.GetSplitState(rec, req)

	var env struct {
		Data map[string]any `json:"data"`
	}
	raw := rec.Body.String()
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("decode state body %q: %v", raw, err)
	}
	return rec.Code, env.Data, raw
}

// "Never ran" is a normal state, not an error: 200 + status:none (NOT 404).
func TestGetSplitState_NoneWhenNeverRan(t *testing.T) {
	h, _, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")

	code, data, raw := splitStateBody(t, h, "req_m")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if data["status"] != "none" {
		t.Errorf("status = %v, want none", data["status"])
	}
	if data["running"] != false {
		t.Errorf("running = %v, want false", data["running"])
	}
	if !strings.Contains(raw, `"candidates":[]`) {
		t.Errorf("candidates must be an empty array, not null; body = %s", raw)
	}
}

// The state endpoint must report the live run while it is in flight, and the
// finished candidates afterwards — this is what the reopened modal reads.
func TestGetSplitState_RunningThenDone(t *testing.T) {
	h, _, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")

	release := make(chan struct{})
	h.splitExec = func(job *store.Job, reqRow *model.Requirement, target splitExecTarget,
		promptText, systemPrompt, modelName, configID string, run splitRuntime, usage *usageCtx) ([]prompt.SplitTask, []prompt.SplitReference, string) {
		<-release
		return []prompt.SplitTask{{Title: "子需求 A", Order: 1}}, nil, ""
	}

	status, jobID := splitStartJob(t, h, "req_m", "{}")
	if status != http.StatusOK || jobID == "" {
		t.Fatalf("start = %d %q, want 200 with a job id", status, jobID)
	}

	code, data, _ := splitStateBody(t, h, "req_m")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if data["status"] != "running" {
		t.Errorf("status = %v, want running", data["status"])
	}
	if data["running"] != true {
		t.Errorf("running = %v, want true", data["running"])
	}
	if data["job_id"] != jobID {
		t.Errorf("job_id = %v, want %s", data["job_id"], jobID)
	}

	close(release)
	if jobStatus, _ := waitSplitJob(t, h, jobID); jobStatus != store.JobDone {
		t.Fatalf("job status = %q, want done", jobStatus)
	}

	_, data, _ = splitStateBody(t, h, "req_m")
	if data["status"] != "done" {
		t.Errorf("status = %v, want done", data["status"])
	}
	cands, _ := data["candidates"].([]any)
	if len(cands) != 1 {
		t.Fatalf("candidates = %v, want 1", data["candidates"])
	}
}

// ── SSE 终帧契约 ───────────────────────────────────────────────────────────

// splitTerminalFrame subscribes to the split SSE and returns the decoded
// job_done frame.
func splitTerminalFrame(t *testing.T, h *WizardHandler, reqID, jobID string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/requirements/"+reqID+"/split/"+jobID+"/stream", nil)
	req.SetPathValue("id", reqID)
	req.SetPathValue("job_id", jobID)
	rec := httptest.NewRecorder()
	h.StreamSplitJob(rec, req)

	body := rec.Body.String()
	idx := strings.LastIndex(body, "data: ")
	if idx < 0 {
		t.Fatalf("no SSE frame in %q", body)
	}
	frame := strings.TrimSpace(body[idx+len("data: "):])
	var out map[string]any
	if err := json.Unmarshal([]byte(frame), &out); err != nil {
		t.Fatalf("decode terminal frame %q: %v", frame, err)
	}
	return out
}

// The terminal frame must carry the candidates as result.tasks — an ARRAY
// inside an object.
//
// This is the regression guard for the shape mismatch that made the split UI
// unusable: the backend used to put a bare array into `result` while the
// frontend read `result.tasks`, so the candidates were never found and every
// run ended in "拆分结果已失效".
func TestStreamSplitJob_TerminalFrameCarriesTasks(t *testing.T) {
	h, _, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")

	h.splitExec = func(job *store.Job, reqRow *model.Requirement, target splitExecTarget,
		promptText, systemPrompt, modelName, configID string, run splitRuntime, usage *usageCtx) ([]prompt.SplitTask, []prompt.SplitReference, string) {
		return []prompt.SplitTask{
			{Title: "子需求 A", Order: 1},
			{Title: "子需求 B", Order: 2, DependsOn: []int{1}},
		}, nil, ""
	}

	status, jobID := splitStartJob(t, h, "req_m", "{}")
	if status != http.StatusOK || jobID == "" {
		t.Fatalf("start = %d %q, want 200 with a job id", status, jobID)
	}
	if jobStatus, _ := waitSplitJob(t, h, jobID); jobStatus != store.JobDone {
		t.Fatalf("job status = %q, want done", jobStatus)
	}

	frame := splitTerminalFrame(t, h, "req_m", jobID)
	if frame["type"] != "job_done" {
		t.Fatalf("type = %v, want job_done", frame["type"])
	}
	if frame["status"] != "done" {
		t.Errorf("status = %v, want done", frame["status"])
	}
	result, ok := frame["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %#v, want an object {tasks:[...]}", frame["result"])
	}
	tasks, ok := result["tasks"].([]any)
	if !ok {
		t.Fatalf("result.tasks = %#v, want an array", result["tasks"])
	}
	if len(tasks) != 2 {
		t.Fatalf("result.tasks has %d entries, want 2", len(tasks))
	}
	first, _ := tasks[0].(map[string]any)
	if _, has := first["depends_on"]; !has {
		t.Errorf("the candidate has no depends_on field: %v", first)
	}
}

// A failed split must carry the backend's user-visible reason on the terminal
// frame — the frontend used to show a bare "拆分失败：" with no cause.
func TestStreamSplitJob_TerminalFrameCarriesErrorMessage(t *testing.T) {
	h, _, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")

	h.splitExec = func(job *store.Job, reqRow *model.Requirement, target splitExecTarget,
		promptText, systemPrompt, modelName, configID string, run splitRuntime, usage *usageCtx) ([]prompt.SplitTask, []prompt.SplitReference, string) {
		return nil, nil, "远端失败"
	}

	status, jobID := splitStartJob(t, h, "req_m", "{}")
	if status != http.StatusOK || jobID == "" {
		t.Fatalf("start = %d %q, want 200 with a job id", status, jobID)
	}
	if jobStatus, _ := waitSplitJob(t, h, jobID); jobStatus != store.JobError {
		t.Fatalf("job status = %q, want error", jobStatus)
	}

	frame := splitTerminalFrame(t, h, "req_m", jobID)
	if frame["status"] != "error" {
		t.Errorf("status = %v, want error", frame["status"])
	}
	if frame["error_message"] != "远端失败" {
		t.Errorf("error_message = %v, want 远端失败", frame["error_message"])
	}
	// The state endpoint must serve the same reason after the modal is reopened.
	_, data, _ := splitStateBody(t, h, "req_m")
	if data["error_message"] != "远端失败" {
		t.Errorf("state error_message = %v, want 远端失败", data["error_message"])
	}
}

// After the candidates are persisted the run is consumed: reopening the modal
// must land on the config stage, not on the batch that was already created.
func TestConfirmSplit_ConsumesTheRun(t *testing.T) {
	h, _, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")

	h.splitExec = func(job *store.Job, reqRow *model.Requirement, target splitExecTarget,
		promptText, systemPrompt, modelName, configID string, run splitRuntime, usage *usageCtx) ([]prompt.SplitTask, []prompt.SplitReference, string) {
		return []prompt.SplitTask{{Title: "子需求 A", Order: 1}}, nil, ""
	}

	status, jobID := splitStartJob(t, h, "req_m", "{}")
	if status != http.StatusOK || jobID == "" {
		t.Fatalf("start = %d %q, want 200 with a job id", status, jobID)
	}
	if jobStatus, _ := waitSplitJob(t, h, jobID); jobStatus != store.JobDone {
		t.Fatalf("job status = %q, want done", jobStatus)
	}
	if h.splitState().runs.latestFor("req_m") == nil {
		t.Fatal("the finished run is not reachable")
	}

	// Confirm the candidates. The stack-bottom preparation needs a project
	// path that exists, so point the project at a temp git repo.
	confirm := httptest.NewRequest(http.MethodPost, "/api/requirements/req_m/split/confirm",
		strings.NewReader(`{"tasks":[{"title":"子需求 A","description":"d","scope":[],"acceptance":[],"boundary":"","order":1,"depends_on":[]}]}`))
	confirm.SetPathValue("id", "req_m")
	rec := httptest.NewRecorder()
	h.ConfirmSplit(rec, confirm)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if h.splitState().runs.latestFor("req_m") != nil {
		t.Error("the run is still reachable after the candidates were persisted")
	}
	_, data, _ := splitStateBody(t, h, "req_m")
	if data["status"] != "none" {
		t.Errorf("state status = %v, want none", data["status"])
	}
}

// A second split for a requirement that is already running must be refused
// with SPLIT_IN_PROGRESS — and the running run must stay the "latest" one, so
// the reopened modal still finds it.
func TestSplit_ConcurrentRefusalKeepsTheRunningRun(t *testing.T) {
	h, _, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")

	release := make(chan struct{})
	defer close(release)
	h.splitExec = func(job *store.Job, reqRow *model.Requirement, target splitExecTarget,
		promptText, systemPrompt, modelName, configID string, run splitRuntime, usage *usageCtx) ([]prompt.SplitTask, []prompt.SplitReference, string) {
		<-release
		return []prompt.SplitTask{{Title: "子需求 A", Order: 1}}, nil, ""
	}

	status, jobID := splitStartJob(t, h, "req_m", "{}")
	if status != http.StatusOK || jobID == "" {
		t.Fatalf("start = %d %q, want 200 with a job id", status, jobID)
	}

	code, errCode := splitErrorCode(t, h, "req_m")
	if code != http.StatusConflict || errCode != "SPLIT_IN_PROGRESS" {
		t.Fatalf("second split = %d %q, want 409 SPLIT_IN_PROGRESS", code, errCode)
	}

	// The refused request must not have hijacked the "latest run" mapping.
	latest := h.splitState().runs.latestFor("req_m")
	if latest == nil || latest.JobID != jobID {
		t.Fatalf("latest run = %+v, want %s", latest, jobID)
	}
	if latest.Status != store.JobRunning {
		t.Errorf("latest status = %q, want running", latest.Status)
	}
}

// The state endpoint must never blow up on a requirement that was never
// split, and it must answer with the same shape every time.
func TestGetSplitState_ShapeIsStable(t *testing.T) {
	h, _, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")

	_, data, _ := splitStateBody(t, h, "req_m")
	for _, key := range []string{"status", "job_id", "running", "error_message",
		"candidates", "model", "exec_env", "agent_server_id", "created_at", "finished_at"} {
		if _, ok := data[key]; !ok {
			t.Errorf("the state payload is missing %q; payload = %v", key, data)
		}
	}
}

// Guard against a timestamp regression: a run that has not finished yet must
// report finished_at = 0, not the huge negative number that the zero time's
// UnixMilli() would produce.
func TestGetSplitState_ZeroTimestamps(t *testing.T) {
	h, _, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")

	// Register a run that is still in flight (no finish call).
	h.splitState().runs.start("job_running", "req_m", "sonnet", "")

	_, data, _ := splitStateBody(t, h, "req_m")
	if data["finished_at"] != float64(0) {
		t.Errorf("finished_at = %v, want 0", data["finished_at"])
	}
	if data["created_at"] == float64(0) {
		t.Errorf("created_at = %v, want a real timestamp", data["created_at"])
	}
}
