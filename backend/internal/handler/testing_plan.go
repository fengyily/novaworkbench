package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// agentTurnTimeout bounds a planner / generator / healer turn. These agents
// open a real browser and explore the app, so 10-30 minutes is normal; the
// ceiling is what stops a wedged turn from occupying a workspace forever.
const agentTurnTimeout = 30 * time.Minute

// CreatePlan POST /api/projects/{id}/testing/plans
//
// Persists the plan, then launches the Playwright planner agent as a
// background job and returns {plan_id, job_id} immediately.
func (h *TestingHandler) CreatePlan(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")

	var req model.CreatePlanReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	req.Scenario = strings.TrimSpace(req.Scenario)
	if req.Scenario == "" {
		writeError(w, http.StatusBadRequest, "SCENARIO_REQUIRED", "请填写测试场景")
		return
	}

	env, err := h.envSvc.Get(req.EnvID)
	if errors.Is(err, service.ErrEnvNotFound) {
		writeError(w, http.StatusNotFound, "ENV_NOT_FOUND", "测试环境不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ENV_LOAD_FAILED", err.Error())
		return
	}
	if env.ProjectID != projectID {
		writeError(w, http.StatusNotFound, "ENV_NOT_FOUND", "测试环境不存在")
		return
	}
	// The agents only exist after init. Failing here — rather than letting
	// claude spin for 30 minutes against a bare workspace — is the whole
	// point of the preflight check.
	if env.InitStatus != model.TestEnvStatusReady {
		writeErrorSuggestion(w, http.StatusConflict, "ENV_NOT_READY",
			"环境尚未初始化完成，请先执行初始化",
			"点击环境旁的「初始化」按钮，等待五个步骤全部完成（绿色）后再生成计划。")
		return
	}

	workspace, err := h.envSvc.EnsureWorkspace(env)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "WORKSPACE_FAILED", err.Error())
		return
	}
	if missing := missingAgents(workspace); len(missing) > 0 {
		writeErrorSuggestion(w, http.StatusConflict, "AGENTS_MISSING",
			"环境未初始化或 Test Agents 生成失败",
			"请重新执行环境的「初始化」，以重新生成 "+strings.Join(missing, "、"))
		return
	}

	plan, err := h.planSvc.Create(projectID, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PLAN_CREATE_FAILED", err.Error())
		return
	}

	job := h.jobs.Create(plan.ID)
	if err := h.planSvc.MarkPlanning(plan.ID, job.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "PLAN_UPDATE_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"plan_id": plan.ID,
		"job_id":  job.ID,
		"status":  model.TestPlanStatusPlanning,
	})

	go h.runPlanner(job, plan, env, workspace)
}

// runPlanner drives the playwright-test-planner agent and harvests the plan
// document it writes.
func (h *TestingHandler) runPlanner(job *store.Job, plan *model.TestPlan, env *model.TestEnvironment, workspace string) {
	// Agents routinely run 10-30 minutes; the default 3-minute stall
	// watchdog inside runClaudeStream would kill a quiet-but-working turn,
	// so pass an explicit, longer one.
	outcome := h.runTestingAgent(job, agentRequest{
		Scope:   "testing-planner",
		Prompt:  plannerPrompt(plan, env, h.reqSvc),
		Env:     env,
		WorkDir: workspace,
		Model:   h.planModel(plan),
		Timeout: agentTurnTimeout,
		PlanID:  plan.ID,
	})
	if outcome.errMsg != "" {
		msg := "Planner 执行失败: " + outcome.errMsg
		_ = h.planSvc.MarkFailed(plan.ID, msg)
		job.Append(store.LogLine{Type: "error", Content: msg})
		job.Append(store.LogLine{Type: "done", Content: "生成计划失败"})
		job.Finish(1, store.JobError)
		return
	}

	// Harvest specs/<slug>.md. The agent is told to use an exact slug, but
	// fall back to any freshly-written .md so a model that invents its own
	// filename still produces a usable plan.
	path, planMD := harvestPlanFile(workspace, service.PlanSlug(plan.Title), outcome.startedBefore)
	if planMD == "" {
		msg := "Planner 未产出计划文件（specs/*.md）"
		_ = h.planSvc.MarkFailed(plan.ID, msg)
		job.Append(store.LogLine{Type: "error", Content: msg})
		job.Append(store.LogLine{Type: "done", Content: "生成计划失败"})
		job.Finish(1, store.JobError)
		return
	}

	if err := h.planSvc.MarkPlanned(plan.ID, planMD, path, outcome.sessionID); err != nil {
		job.Append(store.LogLine{Type: "error", Content: err.Error()})
		job.Finish(1, store.JobError)
		return
	}
	job.Append(store.LogLine{Type: "done", Content: "✅ 测试计划已生成"})
	job.Finish(0, store.JobDone)
	log.Printf("[testing] plan %s planned (%d chars, session %s)", plan.ID, len(planMD), outcome.sessionID)
}

// StartGenerate POST /api/testing/plans/{plan_id}/generate
//
// Launches the playwright-test-generator agent against an existing plan.
func (h *TestingHandler) StartGenerate(w http.ResponseWriter, r *http.Request) {
	planID := r.PathValue("plan_id")

	plan, err := h.planSvc.Get(planID)
	if errors.Is(err, service.ErrPlanNotFound) {
		writeError(w, http.StatusNotFound, "PLAN_NOT_FOUND", "测试计划不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PLAN_LOAD_FAILED", err.Error())
		return
	}
	if plan.Status != model.TestPlanStatusPlanned {
		writeError(w, http.StatusConflict, "PLAN_NOT_READY", "请先生成测试计划")
		return
	}

	env, err := h.envSvc.Get(plan.EnvID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ENV_LOAD_FAILED", err.Error())
		return
	}
	workspace, err := h.envSvc.EnsureWorkspace(env)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "WORKSPACE_FAILED", err.Error())
		return
	}

	job := h.jobs.Create(plan.ID)
	if err := h.planSvc.MarkGenerating(plan.ID, job.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "PLAN_UPDATE_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"plan_id": planID,
		"job_id":  job.ID,
		"status":  model.TestPlanStatusGenerating,
	})

	go h.runGenerator(job, plan, env, workspace)
}

// runGenerator drives the playwright-test-generator agent and harvests the
// spec files it writes.
func (h *TestingHandler) runGenerator(job *store.Job, plan *model.TestPlan, env *model.TestEnvironment, workspace string) {
	outcome := h.runTestingAgent(job, agentRequest{
		Scope:   "testing-generator",
		Prompt:  generatorPrompt(plan, env),
		Env:     env,
		WorkDir: workspace,
		Model:   h.planModel(plan),
		Timeout: agentTurnTimeout,
		PlanID:  plan.ID,
	})
	if outcome.errMsg != "" {
		msg := "Generator 执行失败: " + outcome.errMsg
		_ = h.planSvc.MarkFailed(plan.ID, msg)
		job.Append(store.LogLine{Type: "error", Content: msg})
		job.Append(store.LogLine{Type: "done", Content: "生成测试代码失败"})
		job.Finish(1, store.JobError)
		return
	}

	// Only spec files the generator actually created — the seed test is
	// excluded so re-generating a plan never claims the seed as its output.
	files := harvestSpecFiles(workspace, outcome.startedBefore)
	if len(files) == 0 {
		msg := "Generator 未产出测试文件（tests/*.spec.ts）"
		_ = h.planSvc.MarkFailed(plan.ID, msg)
		job.Append(store.LogLine{Type: "error", Content: msg})
		job.Append(store.LogLine{Type: "done", Content: "生成测试代码失败"})
		job.Finish(1, store.JobError)
		return
	}

	if err := h.planSvc.MarkGenerated(plan.ID, files, outcome.sessionID); err != nil {
		job.Append(store.LogLine{Type: "error", Content: err.Error()})
		job.Finish(1, store.JobError)
		return
	}
	job.Append(store.LogLine{Type: "done", Content: fmt.Sprintf("✅ 已生成 %d 个测试文件", len(files))})
	job.Finish(0, store.JobDone)
	log.Printf("[testing] plan %s generated %v", plan.ID, files)
}

// UpdatePlan PUT /api/testing/plans/{plan_id}
//
// Applies a human edit to the plan document. The service mirrors it to
// specs/<slug>.md so the generator reads the edit rather than the original.
func (h *TestingHandler) UpdatePlan(w http.ResponseWriter, r *http.Request) {
	planID := r.PathValue("plan_id")

	plan, err := h.planSvc.Get(planID)
	if errors.Is(err, service.ErrPlanNotFound) {
		writeError(w, http.StatusNotFound, "PLAN_NOT_FOUND", "测试计划不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PLAN_LOAD_FAILED", err.Error())
		return
	}

	var req model.UpdatePlanReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	if req.PlanMD == "" {
		writeError(w, http.StatusBadRequest, "PLAN_MD_REQUIRED", "计划内容不能为空")
		return
	}

	// An optional title change renames the plan and moves the file with it.
	if strings.TrimSpace(req.Title) != "" && strings.TrimSpace(req.Title) != plan.Title {
		if err := h.planSvc.UpdateTitle(planID, req.Title); err != nil {
			writeError(w, http.StatusInternalServerError, "PLAN_UPDATE_FAILED", err.Error())
			return
		}
	}
	if err := h.planSvc.UpdatePlanMD(planID, req.PlanMD); err != nil {
		writeError(w, http.StatusInternalServerError, "PLAN_UPDATE_FAILED", err.Error())
		return
	}
	updated, _ := h.planSvc.Get(planID)
	writeJSON(w, http.StatusOK, updated)
}

// GetPlan GET /api/testing/plans/{plan_id}
func (h *TestingHandler) GetPlan(w http.ResponseWriter, r *http.Request) {
	plan, err := h.planSvc.Get(r.PathValue("plan_id"))
	if errors.Is(err, service.ErrPlanNotFound) {
		writeError(w, http.StatusNotFound, "PLAN_NOT_FOUND", "测试计划不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PLAN_LOAD_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// ListPlans GET /api/projects/{id}/testing/plans?env_id=
func (h *TestingHandler) ListPlans(w http.ResponseWriter, r *http.Request) {
	plans, err := h.planSvc.List(r.PathValue("id"), r.URL.Query().Get("env_id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PLAN_LIST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plans)
}

// DeletePlan DELETE /api/testing/plans/{plan_id}
func (h *TestingHandler) DeletePlan(w http.ResponseWriter, r *http.Request) {
	if err := h.planSvc.Delete(r.PathValue("plan_id")); err != nil &&
		!errors.Is(err, service.ErrPlanNotFound) {
		writeError(w, http.StatusInternalServerError, "PLAN_DELETE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ── agent execution ────────────────────────────────────────────────────

// agentRequest describes one Playwright agent turn.
type agentRequest struct {
	Scope   string
	Prompt  string
	Env     *model.TestEnvironment
	WorkDir string
	Model   string
	Timeout time.Duration
	PlanID  string
}

// agentOutcome is the subset of claudeStreamOutcome the testing handlers
// need, plus the harvest watermark for this turn.
//
// startedBefore is returned rather than stashed on the handler so that two
// concurrent agent turns cannot clobber each other's watermark — the harvest
// step compares file mtimes against it, and a shared field would let one
// turn claim or discard the other's files.
type agentOutcome struct {
	sessionID string
	errMsg    string
	// startedBefore is the instant taken immediately before the agent was
	// launched; only files newer than it count as this turn's output.
	startedBefore time.Time
}

// runTestingAgent invokes one Playwright agent turn against the test
// workspace, streaming progress into job.
//
// CWD is the workspace, NOT the user's project: that is what makes the CLI
// discover .claude/agents/playwright-test-*.md and the project-scoped
// .mcp.json holding the playwright-test server.
func (h *TestingHandler) runTestingAgent(job *store.Job, req agentRequest) agentOutcome {
	ctx, cancel := context.WithTimeout(context.Background(), req.Timeout)
	defer cancel()

	// Secrets are registered BEFORE the subprocess starts so nothing leaks
	// into the replayed job log.
	job.SetSecrets(h.testingSecretValues(req.Env))

	opts := llm.StreamOpts{
		Prompt:  req.Prompt,
		WorkDir: req.WorkDir,
		Model:   req.Model,
		// --dangerously-skip-permissions: the agents must drive a real
		// browser and write spec files, so the read-only plan mode is wrong.
		PermissionMode: "",
		HardTimeout:    req.Timeout,
	}
	cmd := h.llm.StreamCmd(ctx, opts)
	job.SetCmd(cmd, cancel)

	// Take the harvest watermark BEFORE the turn starts, so we only claim
	// files this turn actually wrote and never a leftover from an earlier run.
	startedBefore := time.Now()

	uctx := &usageCtx{
		Rec:       h.usageSvc,
		ProjectID: req.Env.ProjectID,
		JobID:     job.ID,
		Step:      req.Scope,
		Model:     req.Model,
		Meta:      req.Scope,
		Summary:   req.Scope,
	}
	out := runClaudeStream(jobSink{job}, cmd, req.Scope, uctx, nil, nil, 0, agentTurnTimeout)
	return agentOutcome{sessionID: out.sessionID, errMsg: out.errMsg, startedBefore: startedBefore}
}

// testingSecretValues collects the strings that must be masked in the job
// log. Env var VALUES are the sensitive part (a token pasted into a header
// gets echoed by the agent's debug output); the base URL is included
// because it often embeds credentials for local dev servers.
func (h *TestingHandler) testingSecretValues(env *model.TestEnvironment) []string {
	if env == nil {
		return nil
	}
	var out []string
	if env.BaseURL != "" {
		out = append(out, env.BaseURL)
	}
	if kv, err := h.envSvc.EnvVarsForRun(env); err == nil {
		for _, v := range kv {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

// planModel picks the model for a turn: the environment's default_model, or
// the CLI default when unset. Mirrors how the review toolbar falls back.
func (h *TestingHandler) planModel(plan *model.TestPlan) string {
	if plan == nil {
		return ""
	}
	if env, err := h.envSvc.Get(plan.EnvID); err == nil && env.DefaultModel != "" {
		return cliModelArg(env.DefaultModel)
	}
	return ""
}

// ── harvesting ─────────────────────────────────────────────────────────

// harvestPlanFile finds and reads the plan markdown the agent wrote.
//
// t0 is the watermark taken before the turn. The agent is told to use an
// exact slug, but a model that names the file itself must still be handled —
// so any specs/*.md modified after t0 is accepted as a fallback.
//
// BOTH branches enforce freshness. Without the check on the exact-slug path,
// a re-plan whose agent failed silently would harvest the PREVIOUS run's
// file and report success — the plan would look fine while nothing was
// regenerated.
func harvestPlanFile(workspace, slug string, t0 time.Time) (string, string) {
	readFresh := func(path string) (string, string, bool) {
		info, err := os.Stat(path)
		if err != nil || !info.ModTime().After(t0) {
			return "", "", false
		}
		raw, err := os.ReadFile(path)
		if err != nil || len(raw) == 0 {
			return "", "", false
		}
		return path, string(raw), true
	}

	if path, md, ok := readFresh(filepath.Join(workspace, "specs", slug+".md")); ok {
		return path, md
	}

	matches, _ := filepath.Glob(filepath.Join(workspace, "specs", "*.md"))
	for _, m := range matches {
		if path, md, ok := readFresh(m); ok {
			return path, md
		}
	}
	return "", ""
}

// harvestSpecFiles lists the tests/*.spec.ts files written after t0, minus
// the seed test. Paths are returned workspace-relative and slash-separated
// so they are portable across the DB and the run invocation.
func harvestSpecFiles(workspace string, t0 time.Time) []string {
	matches, _ := filepath.Glob(filepath.Join(workspace, "tests", "*.spec.ts"))
	var out []string
	for _, m := range matches {
		base := filepath.Base(m)
		// The seed is the exploration fixture, not a generated test.
		if base == "seed.spec.ts" {
			continue
		}
		if info, err := os.Stat(m); err != nil || !info.ModTime().After(t0) {
			continue
		}
		rel, err := filepath.Rel(workspace, m)
		if err != nil {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

// ── prompts ────────────────────────────────────────────────────────────

// plannerPrompt tells the planner agent what to build. The PRD block is
// optional: when the plan is linked to a requirement, the agent reads its
// description as product context and produces a more faithful plan.
func plannerPrompt(plan *model.TestPlan, env *model.TestEnvironment, svc *service.RequirementService) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Use the playwright-test-planner subagent (see .claude/agents/playwright-test-planner.md) "+
		"to create a test plan for the following scenario.\n\nScenario:\n%s\n\n", plan.Scenario)

	if plan.RequirementID != "" && svc != nil {
		if req, err := svc.Get(plan.RequirementID); err == nil && req != nil {
			if desc := strings.TrimSpace(req.Description); desc != "" {
				// Bounded so a long requirement cannot crowd the scenario out
				// of the prompt.
				if len(desc) > 4000 {
					desc = desc[:4000] + "\n…（已截断）"
				}
				fmt.Fprintf(&b, "Product context (PRD of the linked requirement %q):\n%s\n\n",
					req.Title, desc)
			}
		}
	}

	fmt.Fprintf(&b, "The application under test is at %s.\n", env.BaseURL)
	b.WriteString("Use tests/seed.spec.ts as the seed test — run it first to explore the live application.\n")
	fmt.Fprintf(&b, "Save the finished plan to specs/%s.md. Use exactly that filename.\n", service.PlanSlug(plan.Title))
	b.WriteString("Reply once the plan file is written. Do not generate tests yet and do not modify the seed test.\n")
	return b.String()
}

// generatorPrompt tells the generator agent to turn the plan into spec files
// and verify each one against the live app.
func generatorPrompt(plan *model.TestPlan, env *model.TestEnvironment) string {
	var b strings.Builder
	b.WriteString("Use the playwright-test-generator subagent (see .claude/agents/playwright-test-generator.md).\n\n")
	fmt.Fprintf(&b, "Read the test plan at specs/%s.md and produce tests/*.spec.ts files that implement it.\n",
		service.PlanSlug(plan.Title))
	fmt.Fprintf(&b, "The application under test is at %s.\n", env.BaseURL)
	b.WriteString("Verify each generated file against the live application using the playwright-test MCP tools before declaring it done.\n")
	b.WriteString("Do NOT modify tests/seed.spec.ts — it is the exploration fixture and must stay as-is.\n")
	return b.String()
}
