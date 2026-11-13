package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/store"
)

// The "互相参考" layer is judged by the model (architect view), not by the
// user — the user cannot tell which two candidates genuinely share context.
// These tests pin the plumbing that carries the model's judgement from the
// split run to the confirmation UI.

// The terminal SSE frame must carry the model's reference suggestions next to
// the candidates — the frontend pre-checks them, so losing them here means
// the user is back to staring at an empty N×N checkbox matrix.
func TestStreamSplitJob_TerminalFrameCarriesReferences(t *testing.T) {
	h, _, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")

	h.splitExec = func(job *store.Job, reqRow *model.Requirement, target splitExecTarget,
		promptText, systemPrompt, modelName, configID string, run splitRuntime, usage *usageCtx) ([]prompt.SplitTask, []prompt.SplitReference, string) {
		return []prompt.SplitTask{
			{Title: "子需求 A", Order: 1},
			{Title: "子需求 B", Order: 2},
		}, []prompt.SplitReference{{From: 1, To: 2, Reason: "2 的上报格式由 1 定义"}}, ""
	}

	status, jobID := splitStartJob(t, h, "req_m", "{}")
	if status != http.StatusOK || jobID == "" {
		t.Fatalf("start = %d %q, want 200 with a job id", status, jobID)
	}
	if jobStatus, _ := waitSplitJob(t, h, jobID); jobStatus != store.JobDone {
		t.Fatalf("job status = %q, want done", jobStatus)
	}

	frame := splitTerminalFrame(t, h, "req_m", jobID)
	result, ok := frame["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %#v, want an object", frame["result"])
	}
	refs, ok := result["references"].([]any)
	if !ok {
		t.Fatalf("result.references = %#v, want an array", result["references"])
	}
	if len(refs) != 1 {
		t.Fatalf("result.references has %d entries, want 1", len(refs))
	}
	ref, ok := refs[0].(map[string]any)
	if !ok {
		t.Fatalf("reference = %#v, want an object", refs[0])
	}
	if ref["from"] != float64(1) || ref["to"] != float64(2) {
		t.Errorf("reference = %#v, want from=1 to=2", ref)
	}
	if ref["reason"] != "2 的上报格式由 1 定义" {
		t.Errorf("reference reason = %v, want the model's reason to survive", ref["reason"])
	}
}

// The state endpoint is what a REOPENED modal reads — it must carry the
// references too, otherwise closing and reopening the modal loses the model's
// architectural judgement.
func TestGetSplitState_CarriesReferences(t *testing.T) {
	h, _, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")

	h.splitExec = func(job *store.Job, reqRow *model.Requirement, target splitExecTarget,
		promptText, systemPrompt, modelName, configID string, run splitRuntime, usage *usageCtx) ([]prompt.SplitTask, []prompt.SplitReference, string) {
		return []prompt.SplitTask{
			{Title: "子需求 A", Order: 1},
			{Title: "子需求 B", Order: 2},
		}, []prompt.SplitReference{{From: 1, To: 2, Reason: "共享协议"}}, ""
	}

	status, jobID := splitStartJob(t, h, "req_m", "{}")
	if status != http.StatusOK || jobID == "" {
		t.Fatalf("start = %d %q, want 200 with a job id", status, jobID)
	}
	if jobStatus, _ := waitSplitJob(t, h, jobID); jobStatus != store.JobDone {
		t.Fatalf("job status = %q, want done", jobStatus)
	}

	code, data, _ := splitStateBody(t, h, "req_m")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	refs, ok := data["references"].([]any)
	if !ok {
		t.Fatalf("references = %#v, want an array", data["references"])
	}
	if len(refs) != 1 {
		t.Fatalf("references has %d entries, want 1", len(refs))
	}
}

// "Never ran" must still report an EMPTY array, not null — the frontend
// iterates over it unconditionally.
func TestGetSplitState_ReferencesIsNeverNull(t *testing.T) {
	h, _, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")

	_, _, raw := splitStateBody(t, h, "req_m")
	var env struct {
		Data struct {
			References *[]prompt.SplitReference `json:"references"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("decode state body %q: %v", raw, err)
	}
	if env.Data.References == nil {
		t.Fatalf("references is null in %q — the wire format must always carry an array", raw)
	}
	if len(*env.Data.References) != 0 {
		t.Errorf("references = %v, want empty", *env.Data.References)
	}
}

// A run without references (the model judged that no two candidates share
// context) must still publish an empty array on the terminal frame.
func TestStreamSplitJob_ReferencesIsEmptyArrayWhenTheModelFoundNone(t *testing.T) {
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

	frame := splitTerminalFrame(t, h, "req_m", jobID)
	result, ok := frame["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %#v, want an object", frame["result"])
	}
	refs, ok := result["references"].([]any)
	if !ok {
		t.Fatalf("result.references = %#v, want an array (never null)", result["references"])
	}
	if len(refs) != 0 {
		t.Errorf("result.references = %v, want empty", refs)
	}
}
