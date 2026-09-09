package handler

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// requireSubTaskSvc is a small guard helper. When the service was never
// injected (legacy / standalone deployment) the sub-task endpoints must 503
// rather than 500 from a nil-pointer panic. Returns false after writing the
// 503 response; the caller returns immediately.
func (h *WizardHandler) requireSubTaskSvc(w http.ResponseWriter) bool {
	if h.subTaskSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "sub-task service not initialized")
		return false
	}
	return true
}

// resolveSubTaskSource picks the session id a sub-task should fork off. The
// primary source is the sub-task's explicit `source_session_id` (set by the
// caller — either the requirement's coding_session_id for a fresh sub-task,
// or another sub-task's session_id for an adjustment). Empty forces the
// caller to error out (no main agent session exists yet).
func subTaskSourceSID(req *model.Requirement, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if req != nil && req.CodingSessionID != "" {
		return req.CodingSessionID
	}
	if req != nil && req.DesignSessionID != "" {
		return req.DesignSessionID
	}
	return ""
}

// runSubTask is a thin adapter that delegates to the shared SubTaskRunner.
// Kept as a method (instead of inlining the call) so the existing call sites
// in StartSubTask / AdjustSubTask / dispatchChildrenSequential stay unchanged
// when the runner takes over the heavy lifting.
//
// configIDOverride mirrors the body.Model override: empty lets the runner
// resolve from the executor role's binding, non-empty pins the executor to
// a specific Claude config so a per-run model override stays coherent with
// the per-run config override the caller wants to honor.
//
// See SubTaskRunner.Run for the full lifecycle.
func (h *WizardHandler) runSubTask(
	req *model.Requirement,
	st *model.SubTask,
	job *store.Job,
	newSID string,
	sourceSID string,
	body string,
	modelOverride string,
	configIDOverride string,
	adjust bool,
) {
	if h.subTaskRunner == nil {
		log.Printf("[sub-task] runner not wired, cannot run %s", st.ID)
		job.Append(store.LogLine{Type: "error", Content: "❌ 子任务执行器未初始化"})
		job.Finish(1, store.JobError)
		return
	}
	h.subTaskRunner.Run(req, st, job, newSID, sourceSID, body, modelOverride, configIDOverride, adjust)
	// (The agent-server routing branch previously inlined here moved to
	// SubTaskRunner.Run so that every sub-task path — manual children,
	// orchestrated children, and push/PR sub-tasks — shares the same
	// req.AgentServerID plumbing.)
}

// computeSubTaskCostCents resolves the run's USD-equivalent cost in cents
// against the active claude config's per-model unit price (input / output
// per million tokens, same convention the dashboard's usage rollup uses).
// Cache creation + cache reads are billed as input. Returns 0 when there's
// no active config, the model isn't priced, or the config hasn't
// configured unit prices yet — the SubTaskPanel renders "—" rather than
// "$0.00" in that case so the user knows pricing wasn't available.
//
// Best-effort: failures are silently swallowed (the artifact / status /
// tokens are the durable record; cost is decorative). The active config
// lookup mirrors the one roleConfig uses at sub-task start time, so this
// resolves against the same source-of-truth the run actually billed.
func computeSubTaskCostCents(modelName string, tokens model.SubTaskTokens, claudeCfg *service.ClaudeConfigService) int {
	if claudeCfg == nil {
		return 0
	}
	cfg, err := claudeCfg.ActiveConfig()
	if err != nil || cfg == nil {
		return 0
	}
	for _, p := range cfg.Models {
		if p.Model != modelName {
			continue
		}
		if p.InputPrice == 0 && p.OutputPrice == 0 {
			return 0
		}
		inUSD := float64(tokens.Input+tokens.CacheCreation+tokens.CacheRead) / 1e6 * p.InputPrice
		outUSD := float64(tokens.Output) / 1e6 * p.OutputPrice
		cents := int((inUSD + outUSD) * 100)
		if cents < 0 {
			return 0
		}
		return cents
	}
	return 0
}

// StartSubTask handles POST /api/requirements/{id}/sub-tasks.
//
// Body: { "prompt": "...", "title": "..." }  (title optional)
//
// Response: 200 { "job_id": "...", "sub_task_id": "..." }
//
// Creates a fresh sub_task row that forks the requirement's main-agent
// session (coding_session_id, with design_session_id as fallback). The
// orchestration work (validate, persist session/job ids) happens inline via
// the shared SubTaskRunner so the pre-Run state matches what every other
// caller (push/PR, auto-orchestrate, adjust, redo) does; the actual claude
// subprocess spawn is delegated to runner.Run so the runtime stays in one
// place.
func (h *WizardHandler) StartSubTask(w http.ResponseWriter, r *http.Request) {
	if !h.requireSubTaskSvc(w) {
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
		Title  string `json:"title"`
		Model  string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "prompt 不能为空")
		return
	}
	id := r.PathValue("id")
	req, err := h.reqSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "requirement not found")
		return
	}
	sourceSID := subTaskSourceSID(req, "")
	if sourceSID == "" {
		writeError(w, http.StatusConflict, "NO_SESSION",
			"需求尚未启动 coding 或 design session，无法创建子任务")
		return
	}

	// subTaskRunner is required to spawn the child process; refuse cleanly
	// (503) instead of nil-deref if the runner wasn't wired (legacy / non-
	// distributed deployment).
	if h.subTaskRunner == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "子任务执行器未初始化")
		return
	}

	st, job, newSID, err := h.subTaskRunner.NewPendingSubTask(id, strings.TrimSpace(body.Title), body.Prompt, body.Model, sourceSID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"job_id":      job.ID,
		"sub_task_id": st.ID,
	})

	go h.runSubTask(req, st, job, newSID, sourceSID, body.Prompt, body.Model, "", false)
}

// AdjustSubTask handles POST /api/requirements/{id}/sub-tasks/{sid}/adjust.
//
// Body: { "prompt": "...", "model"?: "..." }
//
// Creates a NEW sub_task row that forks the parent sub_task's session id —
// letting the user push follow-up instructions into the same implementation
// thread without re-forking from the (much older) main-agent session. The
// prompt is the only new instruction; the rest of the context (project
// files, design docs, prior code edits) is inherited automatically.
func (h *WizardHandler) AdjustSubTask(w http.ResponseWriter, r *http.Request) {
	if !h.requireSubTaskSvc(w) {
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
		Model  string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "prompt 不能为空")
		return
	}
	id := r.PathValue("id")
	sid := r.PathValue("sid")
	req, err := h.reqSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "requirement not found")
		return
	}

	// Look up the parent to validate ownership and capture its session id
	// (the source for the adjustment's --fork-session resume).
	parent, err := h.subTaskSvc.Get(sid)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "sub-task not found")
		return
	}
	if parent.RequirementID != id {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "sub-task does not belong to this requirement")
		return
	}
	if parent.SessionID == "" {
		writeError(w, http.StatusConflict, "NO_SESSION",
			"原子任务尚未生成 session（可能仍在启动或运行），无法追加调整")
		return
	}

	// Adjusting a failed sub-task is allowed but its session file may have
	// rolled back to a state before the failure — surfaced via staleSession
	// at run time, same UX as the main adjust-coding path.
	if h.subTaskRunner == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "子任务执行器未初始化")
		return
	}
	// CreateAdjustment creates the row with source_session_id already
	// populated (= parent's session id), so we just need to fill in the
	// session id / job id / model fields via the runner helper. We don't
	// call runner.NewPendingSubTask because that creates a fresh row
	// (CreateAdjustment sets the parent-fork relationship Create wouldn't).
	st, err := h.subTaskSvc.CreateAdjustment(id, sid, body.Prompt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	newSID := util.NewUUID()
	if perr := h.subTaskSvc.UpdateSession(st.ID, newSID, parent.SessionID); perr != nil {
		log.Printf("[sub-task adjust] failed to persist session for %s: %v", st.ID, perr)
	}
	job := h.jobs.Create(id)
	if perr := h.subTaskSvc.UpdateJobID(st.ID, job.ID); perr != nil {
		log.Printf("[sub-task adjust] failed to persist job_id for %s: %v", st.ID, perr)
	}
	if body.Model != "" {
		if perr := h.subTaskSvc.UpdateModel(st.ID, body.Model); perr != nil {
			log.Printf("[sub-task adjust] failed to persist model for %s: %v", st.ID, perr)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"job_id":      job.ID,
		"sub_task_id": st.ID,
	})

	// Re-use the shared spawn helper with adjust=true. This runs the same
	// prompt prefix + system prompt as a fresh sub-task, but the
	// source_session_id is the parent's session id (not the main agent),
	// so the conversation inherits the parent's edits.
	go h.runSubTask(req, st, job, newSID, parent.SessionID, body.Prompt, body.Model, "", true)
}

// RedoSubTask handles POST /api/requirements/{id}/sub-tasks/{sid}/redo.
//
// Body: { "model"?: "..." }
//
// Re-runs a FAILED sub-task with its original prompt. Unlike AdjustSubTask —
// which forks the failed sub-task's own session to inherit partial edits — a
// redo forks the parent's SOURCE session (the session it originally forked
// from), so the child re-executes the original task from a clean starting
// point. The optional model override lets the user switch models on the retry;
// empty falls back to the developer role default inside runSubTask.
func (h *WizardHandler) RedoSubTask(w http.ResponseWriter, r *http.Request) {
	if !h.requireSubTaskSvc(w) {
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	id := r.PathValue("id")
	sid := r.PathValue("sid")
	req, err := h.reqSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "requirement not found")
		return
	}

	// Look up the parent to validate ownership + capture the source session
	// the failed run originally forked from (the clean redo starting point).
	parent, err := h.subTaskSvc.Get(sid)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "sub-task not found")
		return
	}
	if parent.RequirementID != id {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "sub-task does not belong to this requirement")
		return
	}
	// Redo is scoped to failures — a done/pending/running row has nothing to
	// recover; the UI only offers the button on error cards.
	if parent.Status != model.SubTaskStatusError {
		writeError(w, http.StatusConflict, "NOT_FAILED", "该子任务未失败，无需重做")
		return
	}

	sourceSID := parent.SourceSessionID
	if sourceSID == "" {
		sourceSID = subTaskSourceSID(req, "")
	}
	if sourceSID == "" {
		writeError(w, http.StatusConflict, "NO_SESSION",
			"无法解析可复用的源会话，请重新发起 coding 后再试")
		return
	}

	st, err := h.subTaskSvc.Redo(id, sid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	newSID := util.NewUUID()
	if perr := h.subTaskSvc.UpdateSession(st.ID, newSID, sourceSID); perr != nil {
		log.Printf("[sub-task redo] failed to persist session for %s: %v", st.ID, perr)
	}
	job := h.jobs.Create(id)
	if perr := h.subTaskSvc.UpdateJobID(st.ID, job.ID); perr != nil {
		log.Printf("[sub-task redo] failed to persist job_id for %s: %v", st.ID, perr)
	}
	if body.Model != "" {
		if perr := h.subTaskSvc.UpdateModel(st.ID, body.Model); perr != nil {
			log.Printf("[sub-task redo] failed to persist model for %s: %v", st.ID, perr)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"job_id":      job.ID,
		"sub_task_id": st.ID,
	})

	// Re-use the shared spawn helper with adjust=false and the ORIGINAL prompt
	// (st.Prompt) so the child re-executes the same task from a clean fork.
	go h.runSubTask(req, st, job, newSID, sourceSID, st.Prompt, body.Model, "", false)
}

// GenerateSubTaskSummary handles POST /api/requirements/{id}/sub-tasks/summary
// — the manual summary trigger for requirements whose sub-tasks were created
// by hand (no auto batch ever ran) but are now all terminal. It synthesizes
// the same effect as the auto summary round without needing an
// orchestration_batches row to seed from:
//
//  1. Sub-task list gate: empty → 400; any pending/running → 400.
//  2. Active batch gate: an existing dispatching/summarizing batch on this
//     requirement → 409 (the queue already has a round in flight).
//  3. Create a fresh orchestration_batches row in 'summarizing' state with
//     total_children=0 (no children — they're manual and not batch-scoped).
//  4. Kick the OrchestrationQueue so the next tick fires the summary.
//
// The summary goroutine (RunOrchestratorSummary) reads children via
// subTaskSvc.List(reqID) when batch.TotalChildren==0 — but the current
// implementation pulls children only via ListByBatch(batchID), which would
// skip manual sub-tasks. To keep the manual summary meaningful we copy the
// manual children into the new batch by stamping their batch_id retroactively.
//
// Returns { batch_id, job_id: "" } so the frontend can poll batch state via
// the existing endpoints.
//
// Body: { "model"?: "..." } — currently informational; the batch inherits the
// developer's role-bound model like tryAutoOrchestrate does.
func (h *WizardHandler) GenerateSubTaskSummary(w http.ResponseWriter, r *http.Request) {
	if !h.requireSubTaskSvc(w) {
		return
	}
	if h.batchSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "批次服务未初始化")
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	id := r.PathValue("id")
	req, err := h.reqSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "requirement not found")
		return
	}

	// Gate 1: requirement must have at least one sub-task.
	existing, lerr := h.subTaskSvc.List(id)
	if lerr != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", lerr.Error())
		return
	}
	if len(existing) == 0 {
		writeError(w, http.StatusBadRequest, "no_subtasks", "无子任务可汇总")
		return
	}
	// Gate 2: refuse to summarize while a child is still in flight; the
	// summary reads final artifacts only.
	for _, st := range existing {
		if st.Status == model.SubTaskStatusPending || st.Status == model.SubTaskStatusRunning {
			writeError(w, http.StatusBadRequest, "subtasks_in_flight", "请等待子任务执行完成后再汇总")
			return
		}
	}
	// Gate 3: an active batch means the queue already has a round in flight.
	if active, gerr := h.batchSvc.GetActiveByRequirement(id); gerr == nil && active != nil {
		writeError(w, http.StatusConflict, "orchestration_in_progress", "已有正在进行的批次，请等待完成后重试")
		return
	}

	// Resolve the same runtime params tryAutoOrchestrate uses: developer role's
	// model + claude config, plus the requirement's worktree path so the
	// summary agent edits the right tree.
	_, modelName, claudeConfigID := h.roleConfig("developer")
	if body.Model != "" {
		modelName = body.Model
	}
	workDir := ""
	if proj, perr := h.projectSvc.Get(req.ProjectID); perr == nil {
		workDir = proj.LocalPath
	}
	if req.WorktreePath != "" {
		if _, statErr := os.Stat(req.WorktreePath); statErr == nil {
			workDir = req.WorktreePath
		}
	}
	if workDir == "" {
		writeError(w, http.StatusBadRequest, "no_workdir", "无法解析工作目录，请先完成 start-coding")
		return
	}
	// Pull the orchestrator session id (the coding session main agent forked).
	orchestratorSID := req.CodingSessionID
	if orchestratorSID == "" {
		orchestratorSID = req.DesignSessionID
	}
	if orchestratorSID == "" {
		writeError(w, http.StatusBadRequest, "no_session", "需求尚未启动 coding 或 design session，无法生成汇总")
		return
	}

	// Create the batch row directly (not via TryAutoOrchestrate, which would
	// also try to parse payload.Subtasks — we have no payload here, we want
	// to summarize the EXISTING manual sub-tasks). status='summarizing'
	// signals the queue to run a summary round without re-dispatching any
	// children.
	batch, berr := h.batchSvc.Create(id, orchestratorSID, modelName, workDir, claudeConfigID, 0)
	if berr != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", berr.Error())
		return
	}
	// Manually flip to summarizing since Create() defaults to dispatching.
	if serr := h.batchSvc.MarkSummarizing(batch.ID); serr != nil {
		log.Printf("[summary] %s: mark summarizing: %v", batch.ID, serr)
	}

	// Wake the queue so the next tick fires RunOrchestratorSummary. The
	// summary goroutine pulls children via subTaskSvc.List(id) — we want
	// manual children (batch_id='') too, so stamp them onto this batch
	// first. Each manual child gets batch_seq = its position + 1 so the
	// summary prompt reads them in user-defined order.
	for i, st := range existing {
		if st.BatchID != "" {
			continue
		}
		if perr := h.subTaskSvc.SetBatchID(st.ID, batch.ID, i+1); perr != nil {
			log.Printf("[summary] %s: stamp child %s: %v", batch.ID, st.ID, perr)
		}
	}
	// Bump total_children so CountTerminalByBatch gating stays consistent.
	_ = h.batchSvc.UpdateTotalChildren(batch.ID, len(existing))

	if h.orchQueue != nil {
		h.orchQueue.Kick()
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"job_id":   "",
		"batch_id": batch.ID,
	})
}

// ListSubTasks handles GET /api/requirements/{id}/sub-tasks.
// Returns the sub-tasks ordered oldest-first so the panel renders them in
// firing order. Validates the requirement exists first so a typo returns
// 404 rather than an empty array (which would silently look like "no
// sub-tasks yet").
func (h *WizardHandler) ListSubTasks(w http.ResponseWriter, r *http.Request) {
	if !h.requireSubTaskSvc(w) {
		return
	}
	id := r.PathValue("id")
	if _, err := h.reqSvc.Get(id); err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "requirement not found")
		return
	}
	items, err := h.subTaskSvc.List(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// GetSubTask handles GET /api/requirements/{id}/sub-tasks/{sid}.
// Verifies the row's requirement_id matches the URL {id} so a forged URL
// can't be used to fetch a sub-task that belongs to another requirement —
// the front-end only knows ids from List, but a tampering curl call should
// still hit this guard.
func (h *WizardHandler) GetSubTask(w http.ResponseWriter, r *http.Request) {
	if !h.requireSubTaskSvc(w) {
		return
	}
	id := r.PathValue("id")
	sid := r.PathValue("sid")
	st, err := h.subTaskSvc.Get(sid)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "sub-task not found")
		return
	}
	if st.RequirementID != id {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "sub-task does not belong to this requirement")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// buildSubTaskArtifact composes the final Markdown report persisted into
// sub_tasks.artifact. The header carries the title / prompt / model /
// timestamps so the report reads standalone — useful when the SubTaskPanel
// downloads it or the user opens it from disk later. body is the claude
// output verbatim (already trimmed upstream); error strings start with "❌"
// so the panel can render them in the error style without further
// inspection.
func buildSubTaskArtifact(st *model.SubTask, model, body string, finishedAt time.Time) string {
	var b strings.Builder
	b.WriteString("# 子任务: ")
	b.WriteString(st.Title)
	b.WriteString("\n\n")
	b.WriteString("**提示词**: ")
	b.WriteString(strings.TrimSpace(st.Prompt))
	b.WriteString("\n\n")
	b.WriteString("**完成时间**: ")
	b.WriteString(finishedAt.Format("2006-01-02 15:04:05"))
	b.WriteString("  **模型**: ")
	b.WriteString(model)
	b.WriteString("\n\n---\n\n")
	b.WriteString(strings.TrimSpace(body))
	return b.String()
}

// truncateForLog shortens prompt for the "📝 提示词:" log line so a multi-KB
// prompt doesn't spam the coding panel. Uses the same char-budget as the
// SubTaskPanel card title (240) so the panel preview matches the panel.
func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
