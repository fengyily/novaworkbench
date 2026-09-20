package handler

import (
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/store"
)

// planTestHandler builds the minimal WizardHandler decomposePlanIntoSteps
// touches: the LLM gateway and nothing else. Passing a nil LLMConfigProvider
// makes ExtractStepsFromPlan fail fast with "llm not configured", which is
// exactly the degradation the fallback exists for — no network, no CLI, no DB.
func planTestHandler() *WizardHandler {
	return &WizardHandler{llm: llm.New(nil, nil)}
}

func planTestJob() *store.Job {
	return store.NewJobStore(4).Create("req_plan_test")
}

// TestDecomposePlanIntoSteps_FallbackWhenLLMUnconfigured is the load-bearing
// guarantee of the split path: ticking 「拆分并自动派发」 must always produce at
// least one child agent. When the direct HTTP LLM channel is unconfigured
// (base_url / api_key blank — a very common state) step extraction cannot run,
// and stalling the batch at zero children would silently strand the
// requirement. Instead we dispatch one child carrying the whole plan.
func TestDecomposePlanIntoSteps_FallbackWhenLLMUnconfigured(t *testing.T) {
	h := planTestHandler()
	job := planTestJob()
	plan := "# 实施计划\n\n1. 加列\n2. 写 handler\n"
	req := &model.Requirement{Title: "实现编码阶段计划模式拆分子任务"}

	payload, stepsJSON := h.decomposePlanIntoSteps("req_x", plan, req, job)

	if payload == nil {
		t.Fatal("fallback must never return a nil payload")
	}
	if len(payload.Subtasks) != 1 {
		t.Fatalf("expected exactly 1 fallback child, got %d", len(payload.Subtasks))
	}
	if !strings.Contains(payload.Subtasks[0].Prompt, plan) {
		t.Fatalf("fallback child must carry the whole plan, got prompt=%q", payload.Subtasks[0].Prompt)
	}
	if payload.Subtasks[0].Title != req.Title {
		t.Fatalf("fallback child should be titled after the requirement, got %q", payload.Subtasks[0].Title)
	}
	// meta is provenance for a *parsed* step list; there is none here, so the
	// batch row must not claim otherwise.
	if stepsJSON != "" {
		t.Fatalf("fallback must record empty step JSON, got %q", stepsJSON)
	}
	// The operator has to be able to tell a degraded run from a real 1-step
	// plan, so the downgrade is announced on the job stream.
	lines, _, _ := job.Snapshot()
	found := false
	for _, l := range lines {
		if strings.Contains(l.Content, "降级") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a job log line explaining the downgrade")
	}
}

// TestDecomposePlanIntoSteps_FallbackTitleWithoutRequirement covers the legacy
// quick-start path, which has no requirement row: the child still needs a
// non-empty title because sub_tasks.title feeds the SubTaskCard header.
func TestDecomposePlanIntoSteps_FallbackTitleWithoutRequirement(t *testing.T) {
	h := planTestHandler()
	payload, _ := h.decomposePlanIntoSteps("req_x", "# 计划\n\n1. 做点什么\n", nil, planTestJob())

	if len(payload.Subtasks) != 1 {
		t.Fatalf("expected 1 fallback child, got %d", len(payload.Subtasks))
	}
	if strings.TrimSpace(payload.Subtasks[0].Title) == "" {
		t.Fatal("fallback child title must not be empty")
	}
}

// TestBuildPlanPrompt_FreshSessionCarriesDesign verifies the fresh-session
// branch hand-feeds the stored design doc. That session never joined the
// design conversation, so omitting the doc would leave the planner planning
// against the requirement title alone.
func TestBuildPlanPrompt_FreshSessionCarriesDesign(t *testing.T) {
	h := planTestHandler()
	in := &planSplitInput{
		p:       &codingRunParams{RequirementTitle: "标题", RequirementDesc: "追加说明内容"},
		workDir: "/tmp/wt",
		reqRow:  &model.Requirement{DesignDocs: "## 方案正文 XYZ"},
		// sourceSID empty = fresh session
	}
	got := h.buildPlanPrompt(in)

	if !strings.Contains(got, "## 方案正文 XYZ") {
		t.Fatal("fresh-session prompt must embed requirements.design_docs")
	}
	if !strings.Contains(got, "追加说明内容") {
		t.Fatal("prompt must carry the user's pre-coding note")
	}
	if !strings.Contains(got, "/tmp/wt") {
		t.Fatal("prompt must name the working directory")
	}
	if !strings.Contains(got, "自包含") {
		t.Fatal("prompt must state the self-contained-step rule")
	}
}

// TestBuildPlanPrompt_ForkOmitsDesign is the mirror case: when we fork the
// design session the conversation already holds the design, so re-feeding it
// would just burn context.
func TestBuildPlanPrompt_ForkOmitsDesign(t *testing.T) {
	h := planTestHandler()
	in := &planSplitInput{
		p:         &codingRunParams{RequirementTitle: "标题"},
		workDir:   "/tmp/wt",
		reqRow:    &model.Requirement{DesignDocs: "## 方案正文 XYZ"},
		sourceSID: "sid-design",
		fork:      true,
	}
	got := h.buildPlanPrompt(in)

	if strings.Contains(got, "## 方案正文 XYZ") {
		t.Fatal("forked prompt must not re-feed the design doc")
	}
	if !strings.Contains(got, "基于已完成的需求分析与技术方案") {
		t.Fatal("forked prompt should reference the inherited conversation")
	}
}

// TestBuildPlanPrompt_PrependsCompressionSummary guards the ordering rule: a
// compression summary is ground truth for the run and must lead the prompt, not
// trail it where the model would read it as a fresh instruction.
func TestBuildPlanPrompt_PrependsCompressionSummary(t *testing.T) {
	h := planTestHandler()
	in := &planSplitInput{
		p:       &codingRunParams{RequirementTitle: "标题"},
		workDir: "/tmp/wt",
		reqRow:  &model.Requirement{CodingContextSummary: "之前做了 A 和 B"},
	}
	got := h.buildPlanPrompt(in)

	if !strings.HasPrefix(got, "## 上下文压缩摘要") {
		t.Fatalf("compression summary must lead the prompt, got prefix %q", got[:min(60, len(got))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
