// wizard_subtask.go: extracted from wizard.go as part of the refactoring.

package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

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

// ReOrchestrate handles POST /api/requirements/{id}/re-orchestrate — the
// manual "🔄 重新拆分" escape hatch for when StartCoding's auto-orchestration
// produced no children (main agent ignored every decomposition channel) or
// the user simply wants a fresh split. It resumes the requirement's coding
// session with the SAME decomposition trigger prompt StartCoding sends
// (including the Write-tool subtasks.json instruction), then feeds the turn
// through the exact same parse+dispatch pipeline as tryAutoOrchestrate
// (resolveSubtasksPayload → dispatchChildrenSequential → summaryKickoff).
//
// Returns { job_id } immediately; the job carries the main agent's live
// stream so the frontend can show progress via the usual job SSE endpoint.
// 409 when a child is still running — re-splitting mid-batch would fork the
// session concurrently and double-dispatch work.
//
// Body: { "model"?: "..." }
func (h *WizardHandler) ReOrchestrate(w http.ResponseWriter, r *http.Request) {
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
	req, err := h.reqSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "requirement not found")
		return
	}
	if req.Kind == "idea" {
		writeError(w, http.StatusConflict, "IDEA", "「想法」类需求不支持任务拆分，请先转为需求")
		return
	}
	// Double-fire guard: refuse to re-split while an orchestration batch is
	// still running for this requirement. ReOrchestrate and the auto path
	// (tryAutoOrchestrate) would otherwise both create competing batches —
	// the queue only services one at a time, and the loser leaks children.
	if h.batchSvc != nil {
		if existing, gerr := h.batchSvc.GetActiveByRequirement(id); gerr == nil && existing != nil {
			writeError(w, http.StatusConflict, "orchestration_in_progress", "正在自动编排中，请等待完成后重试")
			return
		}
	}
	// Refuse to re-split while any child is alive — the children fork the
	// coding session, and a concurrent decomposition turn would race them.
	if existing, lerr := h.subTaskSvc.List(id); lerr == nil {
		for _, st := range existing {
			if st.Status == model.SubTaskStatusRunning || st.Status == model.SubTaskStatusPending {
				writeError(w, http.StatusConflict, "BUSY", "有子任务正在执行，请等待完成后再重新拆分")
				return
			}
		}
	}

	job := h.jobs.Create(id)
	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})

	go func() {
		// Best-effort persistence: backend restart mid-run won't lose the log.
		defer func() {
			lines, status, exitCode := job.Snapshot()
			if perr := h.jobLogSvc.Save(job.ID, id, string(status), exitCode, job.StartedAt, job.FinishedAt, lines, job.Model); perr != nil {
				log.Printf("[re-orchestrate] failed to persist job log %s: %v", job.ID, perr)
			}
		}()

		// Workdir: prefer the requirement's isolated worktree (mirrors
		// runSubTask so both paths agree on where the code lives).
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
			job.Append(store.LogLine{Type: "error", Content: "❌ 无法解析工作目录"})
			job.Finish(1, store.JobError)
			return
		}

		systemPrompt, modelName, claudeConfigID := h.roleConfig("developer")
		if body.Model != "" {
			modelName = body.Model
		}
		job.SetModel(modelName)

		// Session threading: resume the existing coding session so the main
		// agent re-decomposes with full context. No coding session yet →
		// fresh session carrying the requirement title + description.
		sessionID := req.CodingSessionID
		resume := sessionID != ""
		var prompt string
		if resume {
			prompt = developerDecomposePrompt(req.Title,
				"上次未能成功完成任务拆分。请基于已有的需求与上下文，重新立即完成**任务拆分**：\n", workDir)
		} else {
			sessionID = util.NewUUID()
			if perr := h.reqSvc.UpdateCodingSession(id, sessionID); perr != nil {
				log.Printf("[re-orchestrate] failed to persist coding session for %s: %v", id, perr)
			}
			prompt = developerDecomposePrompt(req.Title,
				"请先读取项目中的相关文件理解现有代码结构与需求上下文，然后立即完成**任务拆分**：\n", workDir)
			if d := strings.TrimSpace(req.Description); d != "" {
				prompt += "\n\n需求描述：\n" + d
			}
		}

		job.Append(store.LogLine{Type: "phase", Content: "🔄 主 Agent 重新拆分任务中…"})
		cmd, cancel := h.llm.GenerateCode(llm.StreamOpts{
			Prompt:         prompt,
			WorkDir:        workDir,
			SystemPrompt:   systemPrompt,
			Model:          cliModelArg(modelName),
			ClaudeConfigID: claudeConfigID,
			SessionID:      sessionID,
			Resume:         resume,
		})
		defer cancel()
		usage := h.usageCtxFor("re_orchestrate", id, req.ProjectID, job.ID, modelName, "", "")
		out := runClaudeStream(jobSink{job}, cmd, "re-orchestrate", usage)

		switch {
		case out.staleSession:
			// Coding session file is gone — clear it so the next attempt
			// takes the fresh-session path instead of failing again.
			job.Append(store.LogLine{Type: "error", Content: "❌ 原开发会话已失效，请重新点击「重新拆分」（下次将使用全新会话）"})
			if perr := h.reqSvc.UpdateCodingSession(id, ""); perr != nil {
				log.Printf("[re-orchestrate] failed to clear coding session for %s: %v", id, perr)
			}
			job.Finish(1, store.JobError)
			return
		case out.errMsg != "":
			job.Append(store.LogLine{Type: "error", Content: "❌ " + out.errMsg})
			job.Finish(1, store.JobError)
			return
		case out.finalResult == "" && out.subTasksJSON == "":
			job.Append(store.LogLine{Type: "error", Content: "❌ Claude 未返回结果，请重试"})
			job.Finish(1, store.JobError)
			return
		}
		if out.finalResult != "" {
			job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(out.finalResult)})
		}

		// Same parse chain as the auto path. NOTE: unlike tryAutoOrchestrate
		// the manual path does NOT fall back to a single whole-requirement
		// child — the user explicitly asked for a SPLIT, so a total parse
		// failure is reported, not silently converted.
		payload := h.resolveManualReSplit(id, out.finalResult, out.subTasksJSON)
		// Consume-then-clean: never leave subtasks.json in the worktree.
		if rerr := os.Remove(subTasksFilePath(workDir)); rerr != nil && !os.IsNotExist(rerr) {
			log.Printf("[re-orchestrate] %s: failed to remove %s: %v", id, subTasksFilePath(workDir), rerr)
		}
		if payload == nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ 主 Agent 仍未输出可解析的任务拆分。可在下方手动创建子任务，或调整提示词后重试。"})
			job.Finish(1, store.JobError)
			return
		}
		job.Append(store.LogLine{Type: "message", Content: fmt.Sprintf("📋 拆分完成：%d 个子任务，开始串行派发…", len(payload.Subtasks))})
		job.Append(store.LogLine{Type: "done", Content: "✅ 重新拆分完成，子任务派发中"})
		job.Finish(0, store.JobDone)

		// Dispatch: hand off to the same restart-safe path as the auto
		// orchestrate. tryAutoOrchestrate re-parses the payload, re-checks
		// the active-batch gate (a no-op since we just checked above), then
		// commits N sub_tasks + 1 orchestration_batches in a single Tx and
		// kicks the queue. Running it here keeps the manual and auto paths
		// on one code path so behavior stays consistent.
		// We pass empty finalResult + capturedJSON so tryAutoOrchestrate's
		// parser falls through to resolveManualReSplit's logic — but since
		// the parse channels have already been exhausted above (the
		// payload check), it will return the single fallback child only if
		// the input is empty. To preserve the manual path's "no fallback"
		// behavior we instead drive the commit directly.
		h.commitOrchestrationBatch(id, sessionID, payload, workDir, modelName, claudeConfigID)
	}()
}

// commitOrchestrationBatch is the manual re-split's commit primitive: it
// inserts N sub_tasks (status=pending, batch_id, batch_seq=1..N) and 1
// orchestration_batches row in a single Tx, then kicks the queue. Shared
// shape with tryAutoOrchestrate's commit block, but takes a pre-parsed
// payload so the manual path's "no fallback child" contract holds.
//
// Errors are logged + swallowed; the user-facing job has already finished
// by the time we reach here, so a tx failure surfaces as "no children
// appeared" on the next UI refresh (the user can manually retry).
func (h *WizardHandler) commitOrchestrationBatch(
	reqID, orchestratorSID string,
	payload *orchestratorPayload,
	workDir, modelName, claudeConfigID string,
) {
	if h.subTaskSvc == nil || h.batchSvc == nil || payload == nil || len(payload.Subtasks) == 0 {
		log.Printf("[re-orchestrate] %s: missing deps or empty payload; skip commit", reqID)
		return
	}
	tx, terr := h.db.Begin()
	if terr != nil {
		log.Printf("[re-orchestrate] %s: begin tx: %v", reqID, terr)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	obID, berr := h.batchSvc.CreateWithTx(tx, reqID, orchestratorSID, modelName, workDir, claudeConfigID, len(payload.Subtasks))
	if berr != nil {
		log.Printf("[re-orchestrate] %s: create batch: %v", reqID, berr)
		return
	}
	for i, t := range payload.Subtasks {
		if _, cerr := h.subTaskSvc.CreateWithBatchTx(tx, reqID, t.Title, t.Prompt, modelName, orchestratorSID, obID, i+1); cerr != nil {
			log.Printf("[re-orchestrate] %s: create child %d (%s): %v", reqID, i+1, t.Title, cerr)
			return
		}
	}
	if cerr := tx.Commit(); cerr != nil {
		log.Printf("[re-orchestrate] %s: commit: %v", reqID, cerr)
		return
	}
	committed = true
	log.Printf("[re-orchestrate] %s: committed batch %s with %d children", reqID, obID, len(payload.Subtasks))
	if h.orchQueue != nil {
		h.orchQueue.Kick()
	}
}

// resolveManualReSplit is the manual re-split's parse chain: identical to
// resolveSubtasksPayload MINUS the whole-requirement fallback child — a user
// who clicked 重新拆分 asked for a decomposition, so a total parse failure
// must surface as an error instead of silently executing everything as one
// task.
func (h *WizardHandler) resolveManualReSplit(reqID, finalResult, capturedJSON string) *orchestratorPayload {
	if p := decodeSubtasksPayload(capturedJSON); p != nil {
		log.Printf("[re-orchestrate] %s: using Write-captured subtasks.json (%d subtasks)", reqID, len(p.Subtasks))
		return p
	}
	if strings.TrimSpace(finalResult) == "" {
		return nil
	}
	if p, ok := extractSubtasksPayload(finalResult); ok {
		return p
	}
	if p := extractSubtasksFromMarkdown(finalResult); p != nil {
		return p
	}
	return h.extractSubtasksWithLLM(reqID, finalResult)
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

// orchestrationBatchResponse is the wire-shape the frontend's
// OrchestrationBatch interface expects. Defined inline (not on the model)
// because model.OrchestrationBatch intentionally has no JSON tags — it's
// shared with the wizard-internal services that read fields directly, and
// adding tags would change the persistence-side serialization too. The
// front-end only needs a small subset (status, summary_status, counters,
// timestamps), so a focused projection is safer than a wide tag-soup.
type orchestrationBatchResponse struct {
	ID            string     `json:"id"`
	RequirementID string     `json:"requirement_id"`
	Status        string     `json:"status"`
	SummaryStatus string     `json:"summary_status"`
	TotalChildren int        `json:"total_children"`
	Model         string     `json:"model"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}

// GetOrchestrationBatch handles GET /api/requirements/{id}/orchestration/batch.
//
// Returns the most recently created orchestration_batches row for the
// requirement (any status: dispatching / summarizing / completed /
// errored) — the SubTaskPanel uses this to drive the summary-CTA banner
// and the page-level orchestration status hint. When no batch has ever
// been committed for the requirement we return 200 with a `null` payload
// so the front-end's existing `.catch(() => null)` fallback isn't needed
// and a missing batch is treated as a normal terminal state instead of
// an error.
//
// Note: this intentionally differs from `batchSvc.GetActiveByRequirement`
// (which only returns dispatching / summarizing). The front-end wants to
// see a finished batch as "✅ 已完成" rather than have it vanish the moment
// the status flips to 'completed' or 'errored'.
func (h *WizardHandler) GetOrchestrationBatch(w http.ResponseWriter, r *http.Request) {
	if !h.requireSubTaskSvc(w) {
		return
	}
	id := r.PathValue("id")
	if _, err := h.reqSvc.Get(id); err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "requirement not found")
		return
	}
	batch, err := h.batchSvc.GetLatestByRequirement(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if batch == nil {
		writeJSON(w, http.StatusOK, (*orchestrationBatchResponse)(nil))
		return
	}
	writeJSON(w, http.StatusOK, orchestrationBatchResponse{
		ID:            batch.ID,
		RequirementID: batch.RequirementID,
		Status:        batch.Status,
		SummaryStatus: batch.SummaryStatus,
		TotalChildren: batch.TotalChildren,
		Model:         batch.Model,
		CreatedAt:     batch.CreatedAt,
		UpdatedAt:     batch.UpdatedAt,
		CompletedAt:   batch.CompletedAt,
	})
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

// codingPlanSectionRe finds the main agent's "任务分解" (task breakdown)
// section inside a freeform claude response. The developer role's system
// prompt instructs the main agent to emit a Markdown section between two
// sentinel markers so we can locate it without resorting to full Markdown
// AST parsing.
//
// Two layouts are accepted (both intentional — the agent picks whichever
// reads better given the requirements):
//
//  1. Sentinel-wrapped (preferred for stable parsing):
//     <!-- CODING_PLAN_START -->
//     ... plan content ...
//     <!-- CODING_PLAN_END -->
//
//  2. Heading-based fallback (when the agent forgets the sentinels but
//     follows the "## 任务分解" instruction in the role prompt):
//     ## 任务分解
//     ... plan content ...
//     ## 其他章节
//
// Returns the trimmed inner Markdown (sentinels stripped), or "" when no
// plan section was found. The returned string is the value persisted into
// requirements.coding_plan; the SubTaskPanel renders it directly.
var (
	codingPlanStart = regexp.MustCompile(`(?s)<!--\s*CODING_PLAN_START\s*-->`)
	codingPlanEnd   = regexp.MustCompile(`(?s)<!--\s*CODING_PLAN_END\s*-->`)
	codingPlanHead  = regexp.MustCompile(`(?m)^#{1,3}\s*任务分解\s*$`)
)
func extractCodingPlan(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	// Layout 1: sentinel-wrapped.
	startIdx := codingPlanStart.FindStringIndex(text)
	endIdx := codingPlanEnd.FindStringIndex(text)
	if startIdx != nil && endIdx != nil && endIdx[0] > startIdx[1] {
		return strings.TrimSpace(text[startIdx[1]:endIdx[0]])
	}
	// Layout 2: heading-based. Take from the heading line through the next
	// heading of equal or higher level (## or #), or end-of-text.
	loc := codingPlanHead.FindStringIndex(text)
	if loc == nil {
		return ""
	}
	after := text[loc[1]:]
	// Find the next heading at level 1 or 2 (## or #). Level 3+ (###) is
	// sub-content of the plan and stays inside the captured block.
	nextHead := regexp.MustCompile(`(?m)^#{1,2}\s+\S`).FindStringIndex(after)
	if nextHead == nil {
		return strings.TrimSpace(text[loc[0]:])
	}
	return strings.TrimSpace(text[loc[0] : loc[1]+nextHead[0]])
}

// ----------------------------------------------------------------------------
// Auto-orchestrate (主 Agent 自动编排子任务)
//
// 工作流:
//   1. 用户在 developer-chat（或直接 POST /orchestrate）说"开始执行"
//   2. wizard handler 像普通 developer-chat 一样跑主Agent一轮（system prompt
//      已被改写，要求输出 Markdown 表格 + JSON 子任务列表 + [SUBTASKS_READY]）
//   3. finalResult 中包含 [SUBTASKS_READY] 时，解析 JSON，自动创建 sub_tasks 行
//      并**串行**调度每个子Agent（沿用 StartSubTask 的 fork 主会话逻辑）
//   4. 所有子任务结束后，自动 fork 主Agent的 session（--resume <main_sid>），
//      把每个子任务的 artifact 作为上下文注入，触发主Agent生成汇总报告
//   5. 汇总报告写入 requirements.coding_plan，前端 SubTaskPanel 顶部展示
//
// 设计取舍:
//   - 串行而非并行：避免子任务改同一文件（worktree 已隔离但语义上仍冲突）
//   - 不阻塞原始 SSE：编排启动后立即返回，主Agent 输出 + 子任务进度通过各自
//     job_id 推给前端（不通过本编排 SSE 直接流），汇总报告则由前端轮询 Get
//   - 与手动 StartSubTask 共用 sub_tasks 表：自动批次与手动创建的子任务在
//     UI 上表现一致（状态、artifact、token 计量都一样）
// ----------------------------------------------------------------------------

// orchestratedSubtask is the JSON shape the main agent emits inside a code
// fence when the user asks for "开始执行". Parsed in orchestrator; non-nil
// prompt required (the orchestrator rejects empty prompts rather than
// spawning a useless child agent).
type orchestratedSubtask struct {
	Title  string `json:"title"`
	Prompt string `json:"prompt"`
}

// orchestratorPayload is the parsed JSON the main agent emits. The handler
// reads this after extracting the ```json block from finalResult.
type orchestratorPayload struct {
	Subtasks []orchestratedSubtask `json:"subtasks"`
}

// SUBTASKS_READY is the sentinel token the main agent must end its
// auto-orchestrate output with. Any text after the sentinel (until the next
// newline / EOF) is ignored; the sentinel itself is stripped from the saved
// chat history so the user-facing rendering never leaks "[SUBTASKS_READY]".
const SUBTASKS_READY = "[SUBTASKS_READY]"

// developerDecomposePrompt builds the -p prompt that triggers the developer
// role's task-decomposition + [SUBTASKS_READY] emission. Used by BOTH the
// fresh-session (sourceSID=="") and fork/resume (sourceSID!="") StartCoding
// branches so auto-orchestration fires regardless of whether the requirement
// went through the analyst/architect stages — the original bug was that only
// the fork branch carried this trigger, so skip-design / "直接开发" rows got a
// bare "## title\n\n desc" and the agent did the work itself (no sentinel →
// no children dispatched). leadIn is the single line that differs: the fresh
// path has no prior design to reference, so it asks the agent to read files
// first; the fork path references the already-completed analysis + design.
// Everything else (Markdown table → JSON block → [SUBTASKS_READY] sentinel,
// "don't write code yourself", "don't ask for confirmation") is shared
// verbatim so the two paths can never drift out of sync.
// subTasksFileRelPath is the well-known path (relative to the coding work
// dir) the developer main agent writes its sub-task decomposition to via the
// Write tool. The backend captures the Write tool_use input from the
// stream-json events (claudeStreamOutcome.subTasksJSON) — a structured
// channel that can't be broken by embedded code fences or prose, unlike the
// legacy "```json block + [SUBTASKS_READY] sentinel" text convention, which
// stays as the fallback.
const subTasksFileRelPath = ".novaworkbench/subtasks.json"

// subTasksFilePath returns the absolute path the decompose prompt tells the
// main agent to Write to. workDir is the coding worktree (or project dir).
func subTasksFilePath(workDir string) string {
	return filepath.Join(workDir, subTasksFileRelPath)
}

func developerDecomposePrompt(title, leadIn, workDir string) string {
	return fmt.Sprintf(
		"现在切换到「开发者」角色。用户已点击「开始开发」，正式进入执行实现阶段（需求：%s）。\n"+
			leadIn+
			"1. 输出一份 Markdown 任务分解表，让用户直观看到拆分结果；\n"+
			"2. **然后必须调用 Write 工具**，把拆分结果以 JSON 写入文件 %s ，格式：\n"+
			"   {\"subtasks\":[{\"title\":\"...\",\"prompt\":\"...\"}]}，每个子任务的 prompt 必须包含足够上下文（涉及文件、做什么改动、产物形式）；\n"+
			"3. 最后在回复中单独一行输出 [SUBTASKS_READY] 哨兵。\n"+
			"注意：第 2 步的 Write 文件是后端调度子Agent 的主要依据，务必调用 Write 工具完成，不要只在回复里贴 JSON。\n"+
			"要求：不要直接编写项目代码（由子Agent完成）；不要输出『等待确认』——直接给出拆分结果，后端检测到拆分文件后会自动串行调度子Agent执行并在全部完成后交回主Agent汇总。",
		title, subTasksFilePath(workDir))
}

// agentDirectPrompt builds the -p prompt for the "agent" role on Agent-Server
// execution. It deliberately omits the "开始开发/进入执行实现阶段" trigger that
// the developer role keys its sub-task orchestration emission on — the agent
// persona implements the requirement directly on the remote server in a single
// session (no sub-task orchestration, no sentinel). leadIn mirrors
// developerDecomposePrompt's wording so the two paths share intent.
//
// NB: the literal sentinel string is intentionally NOT mentioned anywhere in
// this prompt — past experience shows models occasionally honor an explicit
// "do not emit X" instruction by emitting X anyway (req_9d24ef181a5ad5c4).
// Routing this prompt through the wizard is what guarantees no sentinel: the
// orchestrator is short-circuited (see StartCoding) and the -p message
// contains no trigger phrase.
func agentDirectPrompt(title, leadIn, workDir string) string {
	return fmt.Sprintf(
		"现在切换到「Agent 开发者」角色，正在执行需求（需求：%s，工作目录：%s）。\n"+
			leadIn+
			"直接使用 Read / Edit / Write / Bash 工具完成代码实现、构建与基础验证，并在结束时进行 git commit。\n"+
			"完成后在最终回复里简要说明：做了什么、关键文件、验证方式。",
		title, workDir)
}

// rewritePersonaWorkDir rewrites the workDir path inside the persona
// header of a coding prompt. The agentDirectPrompt / developerDecomposePrompt
// builders emit a header of the form:
//
//	现在切换到「Agent 开发者」角色，正在执行需求（需求：<title>，工作目录：<workDir>）
//
// When the prompt is sent to the Agent Server, the local workDir is wrong —
// the remote cwd is /tmp/nova-agent/<projectID>/<reqID> — so the agent on
// the remote host would otherwise see a path that doesn't exist on its
// filesystem. This helper finds the "工作目录：" label in the persona
// header and replaces from there through the next "）" full-width closing
// paren with the new path, leaving the requirement title and any other
// tokens between "（" and the label untouched.
//
// Returns the prompt unchanged when the label isn't present (e.g. a
// pre-built prompt without the persona header) or when localWorkDir is
// empty (no-op). Only the FIRST match is rewritten — the persona header
// is emitted exactly once per prompt, and looping would risk false
// positives on file content that happens to mention the label.
func extractSubtasksPayload(text string) (*orchestratorPayload, bool) {
	// Also accept a truncated sentinel "[SUBTASKS_READY" (missing the closing
	// "]") — the model occasionally drops the last character when the stream
	// ends right at the sentinel boundary.
	hasSentinel := strings.Contains(text, SUBTASKS_READY) ||
		strings.Contains(text, "[SUBTASKS_READY")
	if !hasSentinel {
		return nil, false
	}
	body := text
	// Strip the sentinel itself so it doesn't bleed into the user-visible
	// chat rendering (front-end would otherwise render "[SUBTASKS_READY]" as
	// raw text after a successful orchestration).
	body = strings.ReplaceAll(body, SUBTASKS_READY, "")
	body = strings.ReplaceAll(body, "[SUBTASKS_READY", "") // truncated form

	// Prefer the ```json fence. Do NOT cut at the first "```" after the
	// fence: a subtask prompt may itself embed a code fence (e.g. a
	// "\n```css\n…\n```\n" snippet inside a JSON string value), and the
	// naive first-fence cut truncates the payload mid-JSON — the parse then
	// fails and the caller silently degrades to the Markdown table heuristic
	// (req_9d24ef181a5ad5c4). extractJSON brace-matches string-aware, so
	// fences inside quoted strings are skipped and the trailing fence is
	// ignored.
	var candidates []string
	if fenceStart := strings.Index(body, "```json"); fenceStart >= 0 {
		candidates = append(candidates, extractJSON(body[fenceStart+len("```json"):]))
	}
	// Fallback: take the first {...} JSON block in the whole response so
	// agents that omit the fence (or accidentally include prose around the
	// JSON) still parse correctly.
	candidates = append(candidates, extractJSON(body))
	for _, raw := range candidates {
		if p := decodeSubtasksPayload(raw); p != nil {
			return p, true
		}
	}
	log.Printf("[orchestrate] JSON parse failed: no usable subtasks payload in %d candidate(s)", len(candidates))
	return nil, false
}

// decodeSubtasksPayload unmarshals one candidate JSON blob and normalizes it.
// Returns nil on any parse/validation failure so the caller can try the next
// candidate. Kept separate from extractSubtasksPayload so the fence and
// brace-match candidates share one strict decode path.
func decodeSubtasksPayload(raw string) *orchestratorPayload {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var p orchestratorPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil
	}
	return normalizePayload(&p)
}

// extractSubtasksFromMarkdown is a permissive fallback used when the main
// agent emits a Markdown task breakdown table but forgets to wrap the
// matching JSON in a code fence (or forgets the [SUBTASKS_READY] sentinel).
// The UX promise is "主Agent 拆分任务 → 后端自动派发", so we degrade
// gracefully instead of leaving the user staring at a "未派发" panel.
//
// Heuristic (matches what the developer role prompt asks the agent to write):
//  1. Locate the "## 任务分解" / "## 子任务" / "## 子任务清单" / "## 任务清单"
//     heading (case-insensitive, trimmed).
//  2. From the heading line onward, grab consecutive list items:
//     - "- " or "* " or numbered "1. " markdown items
//     - "**N. 标题**：提示词" — the agent's compressed form, separated by "：" / ":"
//     - "| 列 | 列 |" table rows starting from the 2nd data row
//  3. Skip blank lines; require at least 2 items to consider it a real plan
//     (one-liner instructions are usually prose, not a decomposition).
//
// Returns nil when nothing usable is found; caller logs + skips dispatch.
func extractSubtasksFromMarkdown(text string) *orchestratorPayload {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	// 1. Find the breakdown heading.
	headingRe := regexp.MustCompile(`(?m)^#{1,3}\s*(任务分解表?|子任务|子任务清单|任务清单|任务拆分|子Agent\s*协作|子Agent协作)\s*$`)
	loc := headingRe.FindStringIndex(text)
	if loc == nil {
		return nil
	}
	after := text[loc[1]:]
	// Stop scanning at the next sibling heading (level 1 or 2). Level 3+ is
	// sub-content of the plan and stays inside.
	if next := regexp.MustCompile(`(?m)^#{1,2}\s+\S`).FindStringIndex(after); next != nil {
		after = after[:next[0]]
	}

	// Trailing prose lines like "等待用户确认" / "完成后..." / "其他内容略。"
	// aren't part of the task list — stop the scan when the line is plain
	// prose with no list/table markers. We still keep lines whose content
	// looks like a "标题：xxx" entry (heuristic: has a colon before the first
	// non-space char of length >= 2, OR starts with a list marker, OR is a
	// table row).
	type entry struct {
		title  string
		prompt string
	}
	var entries []entry
	lines := strings.Split(after, "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}

		// Skip obvious non-entries (table dividers, code fences).
		if regexp.MustCompile(`^[-=|:\s]+$`).MatchString(line) {
			continue
		}
		if strings.HasPrefix(line, "```") {
			continue
		}

		// Skip plain prose lines without list / table markers — these are
		// the agent's closing remark, not a task entry. The check is
		// strict: the line must start with one of the recognized markers.
		isListItem := false
		for _, prefix := range []string{"- ", "* ", "• ", "‣ ", "| "} {
			if strings.HasPrefix(line, prefix) {
				isListItem = true
				break
			}
		}
		isNumbered := regexp.MustCompile(`^\d+\.\s+\S`).MatchString(line)
		if !isListItem && !isNumbered {
			// Plain prose → stop scanning, the plan section is over.
			break
		}

		// Strip common markdown list / numbering prefixes.
		stripped := line
		for _, prefix := range []string{"- ", "* ", "• ", "‣ "} {
			if strings.HasPrefix(stripped, prefix) {
				stripped = strings.TrimSpace(stripped[len(prefix):])
				break
			}
		}
		// Numbered prefix: "1. ", "2. ", … (single trailing dot).
		if m := regexp.MustCompile(`^\d+\.\s+`).FindStringIndex(stripped); m != nil {
			stripped = strings.TrimSpace(stripped[m[1]:])
		}

		// Strip ALL ** (markdown bold) — pairs of asterisks can sit
		// anywhere in the line ("**修复登录 bug**" → "修复登录 bug").
		stripped = stripAllBold(stripped)

		// Table row: split into cells so "| 实现缓存层 | 在 …" →
		// title="实现缓存层", prompt="在 …". We handle this BEFORE the
		// colon/separator step so a table row never falls through to the
		// default "whole line is the prompt" branch.
		isTableRow := strings.HasPrefix(stripped, "|")
		if isTableRow {
			cells := splitTableRowCells(stripped)
			if len(cells) >= 2 {
				// Lead index column: tables shaped "| # | 子任务 | 涉及文件 | …"
				// put the real title in the SECOND cell. Without this shift the
				// header row becomes a bogus child (title="#", prompt="子任务 | …")
				// and every data row gets a numbered title ("1", "2", …) — this
				// is exactly what dispatched the garbage sub-task in
				// req_9d24ef181a5ad5c4.
				if isTableIndexCell(cells[0]) {
					cells = cells[1:]
				}
				tableTitle := cells[0]
				tablePrompt := strings.Join(cells[1:], " | ")
				if tablePrompt == "" {
					// Single-content-column row after the shift — keep the
					// title as the prompt so normalizePayload doesn't drop it.
					tablePrompt = tableTitle
				}
				// Filter the header row (its title cell is one of the marker
				// words) and any other obvious non-entries.
				if isTableHeaderCell(tableTitle) {
					continue
				}
				entries = append(entries, entry{title: tableTitle, prompt: tablePrompt})
				continue
			}
		}

		// Two shapes (non-table rows):
		//   "标题：提示词" / "标题: 提示词"
		//   "标题 — 提示词" / "标题 - 提示词"
		var title, prompt string
		for _, sep := range []string{"：", ":", "—", " — ", " - "} {
			if idx := strings.Index(stripped, sep); idx > 0 && idx < len(stripped)-1 {
				title = strings.TrimSpace(stripped[:idx])
				prompt = strings.TrimSpace(stripped[idx+len(sep):])
				break
			}
		}
		if title == "" {
			// Whole line is the title/prompt; treat first 40 chars as title
			// and the whole thing as the prompt so the panel renders
			// something usable.
			prompt = stripped
			title = truncateForLog(stripped, 40)
		}
		// Filter: skip table headers like "子任务" / "提示词" / "描述" that
		// the agent sometimes leaves in row 1.
		lowerTitle := strings.ToLower(title)
		if title == "" || strings.HasPrefix(lowerTitle, "子任务") || strings.HasPrefix(lowerTitle, "任务") || strings.HasPrefix(lowerTitle, "提示词") || strings.HasPrefix(lowerTitle, "说明") || strings.HasPrefix(lowerTitle, "步骤") {
			continue
		}
		entries = append(entries, entry{title: title, prompt: prompt})
	}
	if len(entries) < 2 {
		// A single bullet is usually prose ("- 等等") — don't auto-dispatch
		// on it; the user can still copy-paste into the manual creator.
		return nil
	}
	p := &orchestratorPayload{Subtasks: make([]orchestratedSubtask, 0, len(entries))}
	for _, e := range entries {
		p.Subtasks = append(p.Subtasks, orchestratedSubtask{Title: e.title, Prompt: e.prompt})
	}
	return normalizePayload(p) // normalizePayload may still return nil if everything cleaned out
}

// splitTableRowCells turns a Markdown table row into its trimmed cells:
// "| a | b |" → ["a", "b"]. Empty cells are kept so column positions stay
// aligned (the index-shift heuristic in the caller depends on position).
func splitTableRowCells(row string) []string {
	row = strings.TrimSpace(strings.Trim(row, "|"))
	parts := strings.Split(row, "|")
	cells := make([]string, 0, len(parts))
	for _, c := range parts {
		cells = append(cells, strings.TrimSpace(c))
	}
	return cells
}

// isTableIndexCell reports whether a leading table cell is just a row index
// ("#", "1", "42", "序号", "编号") rather than real content — the cue that the
// actual task title lives in the NEXT column.
func isTableIndexCell(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if s == "#" || s == "序号" || s == "编号" {
		return true
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isTableHeaderCell reports whether a table title cell is really a header
// label (子任务 / 提示词 / 涉及文件 / …) that must not become a sub-task.
// Broader than the inline list-item filter because breakdown tables carry
// more column kinds (涉及文件 / 关键改动 / 产物).
func isTableHeaderCell(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	if lower == "" {
		return true
	}
	for _, marker := range []string{
		"子任务", "任务", "提示词", "说明", "步骤",
		"标题", "描述", "涉及文件", "关键改动", "产物",
		"序号", "编号", "#",
	} {
		if strings.HasPrefix(lower, marker) {
			return true
		}
	}
	return false
}

// stripAllBold removes every pair of ** asterisks from s, including
// mid-string occurrences (so "**修复登录 bug** — 在 auth/login.go" →
// "修复登录 bug — 在 auth/login.go"). The Markdown `*single*` form is left
// alone — only the bold double-asterisk variant collides with our
// title/prompt separator heuristics.
func stripAllBold(s string) string {
	for strings.Contains(s, "**") {
		s = strings.ReplaceAll(s, "**", "")
	}
	return s
}

// normalizePayload drops empty / malformed entries (prompt required) and
// auto-fills a missing title from the first 40 chars of the prompt. Returns
// nil when no usable subtask survived — caller treats that as "no plan".
func normalizePayload(p *orchestratorPayload) *orchestratorPayload {
	if p == nil {
		return nil
	}
	cleaned := p.Subtasks[:0]
	for _, s := range p.Subtasks {
		if strings.TrimSpace(s.Prompt) == "" {
			continue
		}
		if s.Title == "" {
			s.Title = truncateForLog(s.Prompt, 40)
		}
		cleaned = append(cleaned, s)
	}
	p.Subtasks = cleaned
	if len(p.Subtasks) == 0 {
		return nil
	}
	return p
}

// tryAutoOrchestrate is the auto-dispatch path called by StartCoding right
// after the main agent turn finishes. It resolves the main agent's
// decomposition via resolveSubtasksPayload (Write-captured JSON → sentinel
// text → markdown table → LLM extractor → single fallback child) and
// commits N sub_tasks + 1 orchestration_batches inside a single transaction,
// then returns immediately — dispatch is no longer this function's job.
//
// The new flow:
//
//  1. Resolve the payload (unchanged parse chain).
//  2. Insert N sub_tasks (status=pending, batch_id, batch_seq=1..N) and 1
//     orchestration_batches row in a single Tx. Either everything commits or
//     everything rolls back, so a mid-Tx failure never leaves an orphan
//     batch with zero children.
//  3. Kick the OrchestrationQueue so the first child doesn't wait the full
//     tick interval. The queue then drives sub-task dispatch in batch_seq
//     order and triggers the summary round when every child has reached a
//     terminal state.
//
// Restart-safety is handled by OrchestrationQueue.tick() (which calls
// ClaimNextPending / CountTerminalByBatch / MarkSummarizing) and
// OrchestrationBatchService.Recover() on boot — see those for details. This
// function does not own any goroutine after the Tx commits.
//
// Frontend progress remains observable through:
//   - /api/requirements/{id}/sub-tasks                 → live status of each child
//   - /api/wizard/jobs/{child_job_id}/stream           → live tool calls of each child
//   - /api/requirements/{id}                           → requirements.coding_plan surfaces the summary
//   - /api/requirements/{id}/orchestration/batch       → batch status (live)
func (h *WizardHandler) tryAutoOrchestrate(
	reqID string,
	orchestratorSID string,
	finalResult string,
	capturedJSON string,
	req *model.Requirement,
	workDir, modelName, claudeConfigID string,
) {
	if h.subTaskSvc == nil || h.batchSvc == nil {
		return
	}
	if req == nil {
		// Defensive: shouldn't happen (StartCoding fetched it before
		// dispatching this goroutine), but skip cleanly if so.
		log.Printf("[auto-orchestrate] %s: requirement row missing, skipping dispatch", reqID)
		return
	}
	// Double-fire guard: refuse to start a new batch while one is already
	// dispatching or summarizing for this requirement. The user clicking
	// StartCoding twice (or the manual re-orchestrate path racing this) would
	// otherwise create two competing batches — the second one would silently
	// leak children that the first batch's summary ignores.
	if existing, gerr := h.batchSvc.GetActiveByRequirement(reqID); gerr == nil && existing != nil {
		log.Printf("[auto-orchestrate] %s: active batch %s already in %s — skip", reqID, existing.ID, existing.Status)
		return
	}

	payload := h.resolveSubtasksPayload(reqID, finalResult, capturedJSON, req)
	// The subtasks.json the main agent Wrote into the worktree has been
	// consumed (or rejected) — remove it so it never pollutes the dev branch
	// or gets mistaken for a fresh decomposition on the next turn.
	if workDir != "" {
		if rerr := os.Remove(subTasksFilePath(workDir)); rerr != nil && !os.IsNotExist(rerr) {
			log.Printf("[orchestrate] %s: failed to remove %s: %v", reqID, subTasksFilePath(workDir), rerr)
		}
	}
	if payload == nil {
		return
	}

	// Single Tx: batch + N sub_tasks commit atomically. A mid-Tx failure
	// rolls back everything; the user can safely retry by clicking StartCoding
	// again (the GetActiveByRequirement gate above will then be empty).
	tx, terr := h.db.Begin()
	if terr != nil {
		log.Printf("[auto-orchestrate] %s: begin tx: %v", reqID, terr)
		return
	}
	// Defer Rollback on every error path; Commit clears it via the named return.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	obID, berr := h.batchSvc.CreateWithTx(tx, reqID, orchestratorSID, modelName, workDir, claudeConfigID, len(payload.Subtasks))
	if berr != nil {
		log.Printf("[auto-orchestrate] %s: create batch: %v", reqID, berr)
		return
	}
	for i, t := range payload.Subtasks {
		if _, cerr := h.subTaskSvc.CreateWithBatchTx(tx, reqID, t.Title, t.Prompt, modelName, orchestratorSID, obID, i+1); cerr != nil {
			log.Printf("[auto-orchestrate] %s: create child %d (%s): %v", reqID, i+1, t.Title, cerr)
			return
		}
	}
	if cerr := tx.Commit(); cerr != nil {
		log.Printf("[auto-orchestrate] %s: commit: %v", reqID, cerr)
		return
	}
	committed = true
	log.Printf("[auto-orchestrate] %s: committed batch %s with %d children — scheduler tick will dispatch", reqID, obID, len(payload.Subtasks))

	// Wake the queue immediately so the first child doesn't wait the full
	// tick interval. Kick is non-blocking; nil-check because main.go may
	// wire the queue AFTER the first batch has been created in tests.
	if h.orchQueue != nil {
		h.orchQueue.Kick()
	}
}

// resolveSubtasksPayload turns the main agent's turn output into a concrete
// sub-task list. The channels are tried in strict reliability order:
//
//  1. Write-captured subtasks.json (structured tool_use input — the primary
//     channel; cannot be mangled by prose / code fences).
//  2. ```json fence + [SUBTASKS_READY] sentinel in the reply text.
//  3. Markdown breakdown table heuristic.
//  4. A cheap single-shot LLM extractor over the raw reply (one retry with
//     the parse error fed back).
//  5. A single fallback child covering the whole requirement — the UX
//     promise is "开始开发后一定有子 Agent 在工作", so the pipeline never
//     stalls at zero children.
//
// Returns nil only when there is nothing to dispatch at all (empty reply
// AND empty requirement prompt source). Shared by tryAutoOrchestrate and
// ReOrchestrate so the manual re-split behaves identically.
func (h *WizardHandler) resolveSubtasksPayload(
	reqID, finalResult, capturedJSON string,
	req *model.Requirement,
) *orchestratorPayload {
	// 1. Write-captured JSON (primary).
	if p := decodeSubtasksPayload(capturedJSON); p != nil {
		log.Printf("[orchestrate] %s: using Write-captured subtasks.json (%d subtasks)", reqID, len(p.Subtasks))
		return p
	}
	if strings.TrimSpace(capturedJSON) != "" {
		log.Printf("[orchestrate] %s: Write-captured subtasks.json failed to decode, falling through to text parsing", reqID)
	}

	if strings.TrimSpace(finalResult) != "" {
		// 2. Sentinel + JSON text block.
		if p, ok := extractSubtasksPayload(finalResult); ok {
			log.Printf("[orchestrate] %s: using sentinel+JSON text block (%d subtasks)", reqID, len(p.Subtasks))
			return p
		}
		// 3. Markdown table heuristic.
		if p := extractSubtasksFromMarkdown(finalResult); p != nil {
			log.Printf("[orchestrate] %s: no sentinel; using markdown plan (%d subtasks)", reqID, len(p.Subtasks))
			return p
		}
		// 4. LLM extractor (single-shot, one retry with the parse error).
		if p := h.extractSubtasksWithLLM(reqID, finalResult); p != nil {
			log.Printf("[orchestrate] %s: using LLM-extracted subtasks (%d)", reqID, len(p.Subtasks))
			return p
		}
	}

	// 5. Single fallback child: the whole requirement as one task.
	title := req.Title
	prompt := "## 需求\n\n" + req.Title
	if d := strings.TrimSpace(req.Description); d != "" {
		prompt += "\n\n" + d
	}
	prompt += "\n\n> 说明：主 Agent 未能给出可用的任务拆分，请直接基于项目上下文完成整个需求。"
	if title == "" {
		title = "执行整个需求"
	}
	log.Printf("[auto-orchestrate] %s: all parse channels failed — dispatching whole requirement as one fallback child", reqID)
	return &orchestratorPayload{Subtasks: []orchestratedSubtask{{Title: truncateForLog(title, 40), Prompt: prompt}}}
}

// extractSubtasksWithLLM is fallback channel 4: asks a cheap single-shot
// claude call to convert the main agent's prose reply into the
// {"subtasks":[…]} JSON, retrying once with the decode error fed back.
// Returns nil when both attempts fail or the extraction yields no usable
// entries.
func (h *WizardHandler) extractSubtasksWithLLM(reqID, finalResult string) *orchestratorPayload {
	feedback := ""
	for attempt := 1; attempt <= 2; attempt++ {
		raw, err := h.llm.ExtractSubtasksJSON(finalResult, feedback)
		if err != nil {
			log.Printf("[orchestrate] %s: extractor attempt %d failed: %v", reqID, attempt, err)
			return nil
		}
		if p := decodeSubtasksPayload(extractJSON(raw)); p != nil {
			return p
		}
		feedback = truncateForLog(raw, 200)
		log.Printf("[orchestrate] %s: extractor attempt %d returned undecodable JSON, retrying", reqID, attempt)
	}
	return nil
}

// ExecuteOrchestratedChild is the per-child execution entry point invoked
// from scheduler.OrchestrationQueue.tick. The sub-task row has ALREADY been
// claimed (status=running) by ClaimNextPending before this method is called,
// so this function only:
//   1. Resolves session/job pre-mint identifiers (UpdateSession / UpdateJobID)
//   2. Spawns the claude CLI with the executor-role persona, forking the
//      orchestrator session
//   3. Writes the terminal artifact via Finish
//
// The 5s heartbeat ticker keeps batch_id_seq_run fresh so a backend crash
// surfaces within one recovery interval (RecoverInterrupted's 5min cutoff).
// Workdir / model / claude-config are pulled from the batch row so manual
// (batch_id='') and orchestrated children both produce identical runtime
// behavior; the only difference is how they were entered into the table.
func (h *WizardHandler) ExecuteOrchestratedChild(batch *model.OrchestrationBatch, st *model.SubTask) {
	if h.subTaskSvc == nil {
		return
	}
	reqID := st.RequirementID

	// Early-exit tracking. Every return path before the runClaudeStream
	// boundary sets earlyExitReason so the deferred guard below can persist
	// a clean error Finish. Without this, a panic / DB error / nil-dep early
	// return would leave the row stuck at status='running' forever — the
	// OrchestrationQueue's next tick would see it as in-flight and skip it,
	// while the sibling rows behind it would never be claimed.
	var earlyExitReason string
	var hbDone chan struct{}
	var job *store.Job
	var modelName string
	defer func() {
		if earlyExitReason == "" {
			return
		}
		// Close heartbeat if it was started (hbDone is initialized only
		// after roleConfig + the first two pre-mint writes succeed, so a
		// nil channel here means we exited even earlier).
		if hbDone != nil {
			close(hbDone)
		}
		if h.subTaskSvc == nil {
			return
		}
		log.Printf("[orchestrate] child %s (batch %s seq %d) early exit: %s",
			st.ID, batch.ID, st.BatchSeq, earlyExitReason)
		artifact := buildSubTaskArtifact(st, modelName, "❌ "+earlyExitReason, time.Now())
		if perr := h.subTaskSvc.Finish(st.ID, model.SubTaskStatusError, artifact, modelName,
			model.SubTaskTokens{}, 0, time.Time{}); perr != nil {
			log.Printf("[orchestrate] early-exit Finish %s failed: %v", st.ID, perr)
		}
		if job != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ " + earlyExitReason})
			job.Append(store.LogLine{Type: "done", Content: "❌ 子任务早退"})
			job.Finish(1, store.JobError)
		}
	}()

	// Pre-mint child session id (forked from the orchestrator/main session).
	childSID := util.NewUUID()
	if perr := h.subTaskSvc.UpdateSession(st.ID, childSID, batch.OrchestratorSessionID); perr != nil {
		log.Printf("[orchestrate] failed to persist child session for %s: %v", st.ID, perr)
	}
	job = h.jobs.Create(reqID)
	if perr := h.subTaskSvc.UpdateJobID(st.ID, job.ID); perr != nil {
		log.Printf("[orchestrate] failed to persist child job_id for %s: %v", st.ID, perr)
	}

	// Resolve executor role + claude config binding. Same priority as the
	// pre-batch dispatchOneChild: explicit batch config > model lookup >
	// executor-role fallback.
	execSystemPrompt, _, executorConfigID := h.roleConfig(executorRoleKey)
	modelName = batch.Model
	devCfgID := batch.ClaudeConfigID
	var finalConfigID string
	switch {
	case devCfgID != "":
		finalConfigID = devCfgID
	case modelName != "":
		if cid, cerr := h.claudeCfg.ResolveConfigForModel(modelName); cerr == nil && cid != "" {
			finalConfigID = cid
		} else {
			finalConfigID = executorConfigID
		}
	default:
		finalConfigID = executorConfigID
	}

	executorPrompt := "## 子任务\n\n" + st.Prompt + "\n\n" +
		"> 本任务通过 --fork-session 继承了主 Agent 的项目上下文与代码库访问权限。\n" +
		"> 如需补充信息，可正常读取项目文件或调用工具。\n" +
		"> 你是执行者：请直接动手实现本子任务并落盘代码改动，不要再做任务拆分。\n"

	// Heartbeat ticker: keeps batch_id_seq_run fresh for boot recovery. Capped
	// at the configured interval so a stuck Finish() can't leak past one
	// recovery cycle. Errors are best-effort — a failed heartbeat is logged
	// but doesn't stop execution.
	hbInterval := time.Duration(h.summaryKickIntervalSec) * time.Second
	if hbInterval <= 0 {
		hbInterval = 5 * time.Second
	}
	hbDone = make(chan struct{})
	go func() {
		ticker := time.NewTicker(hbInterval)
		defer ticker.Stop()
		for {
			select {
			case <-hbDone:
				return
			case <-ticker.C:
				if herr := h.subTaskSvc.MarkHeartbeat(st.ID); herr != nil {
					log.Printf("[orchestrate] heartbeat %s: %v", st.ID, herr)
				}
			}
		}
	}()

	// Load the requirement row for AgentServerID + ProjectID. Done at the
	// call site (rather than passing through the batch) so the same lookup
	// path SubTaskRunner.Run uses applies here.
	req, rerr := h.reqSvc.Get(reqID)
	if rerr != nil || req == nil {
		earlyExitReason = "无法加载需求行: " + errString(rerr)
		log.Printf("[orchestrate] requirement %s missing for child %s: %v", reqID, st.ID, rerr)
		return
	}

	cmd, cancel := h.llm.GenerateCode(llm.StreamOpts{
		Prompt:         executorPrompt,
		WorkDir:        batch.WorkDir,
		SystemPrompt:   execSystemPrompt,
		Model:          cliModelArg(modelName),
		ClaudeConfigID: finalConfigID,
		SessionID:      batch.OrchestratorSessionID,
		Resume:         true,
		Fork:           true,
		ForkSessionID:  childSID,
	})
	if cmd == nil {
		earlyExitReason = "GenerateCode 返回空 cmd"
		log.Printf("[orchestrate] child %s GenerateCode returned nil cmd", st.ID)
		return
	}
	defer cancel()

	job.Append(store.LogLine{Type: "phase", Content: "🤖 [编排] 子任务启动: " + st.Title})
	job.Append(store.LogLine{Type: "message", Content: "📝 提示词: " + truncateForLog(st.Prompt, 240)})
	job.SetModel(modelName)

	childUsage := h.usageCtxFor("sub_task", reqID, req.ProjectID, job.ID, modelName, "", st.Prompt)
	// Route orchestrated children to the parent requirement's Agent server
	// when it has one — same reasoning as runSubTask: the working tree lives
	// on that host, so a locally-spawned child would edit the wrong checkout.
	var out claudeStreamOutcome
	if req.AgentServerID != "" && h.agentSvrSvc != nil {
		out = h.runRemoteCoding(&remoteCodingInput{
			job:      job,
			serverID: req.AgentServerID,
			req: startCodingReq{
				RequirementTitle: req.Title + " / " + st.Title,
				RequirementID:    req.ID,
				BranchName:       req.BranchName,
				AgentServerID:    req.AgentServerID,
			},
			reqRow:        req,
			prompt:        executorPrompt,
			sourceSID:     batch.OrchestratorSessionID,
			fork:          true,
			sessionArg:    batch.OrchestratorSessionID,
			forkSessionID: childSID,
			model:         modelName,
			usage:         childUsage,
		})
	} else {
		out = runClaudeStream(jobSink{job}, cmd, "sub-task", childUsage)
	}

	// Stop the heartbeat BEFORE Finish so a slow MarkHeartbeat can't race
	// the Finish write. Finish flips status out of 'running', so any
	// post-Finish heartbeat is a no-op anyway, but closing the channel keeps
	// logs tidy.
	close(hbDone)
	hbDone = nil // signal "happy path" to the deferred guard

	status := model.SubTaskStatusDone
	artifactBody := out.finalResult
	if out.staleSession {
		status = model.SubTaskStatusError
		artifactBody = "❌ 源会话已失效（session 文件不存在），请重新发起 coding 后再试。"
	} else if out.errMsg != "" {
		status = model.SubTaskStatusError
		artifactBody = "❌ " + out.errMsg
	} else if out.finalResult == "" {
		status = model.SubTaskStatusError
		artifactBody = "❌ Claude 未返回结果，请重试"
	}
	if status != model.SubTaskStatusError {
		job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(out.finalResult)})
	} else {
		job.Append(store.LogLine{Type: "error", Content: artifactBody})
	}
	job.Append(store.LogLine{Type: "done", Content: "✅ 子任务完成！"})

	artifact := buildSubTaskArtifact(st, modelName, artifactBody, time.Now())
	tokens := model.SubTaskTokens{
		Input:         out.lastUsage.InputTokens,
		Output:        out.lastUsage.OutputTokens,
		CacheCreation: out.lastUsage.CacheCreationTokens,
		CacheRead:     out.lastUsage.CacheReadTokens,
	}
	if perr := h.subTaskSvc.Finish(st.ID, status, artifact, modelName, tokens, 0, time.Time{}); perr != nil {
		log.Printf("[orchestrate] failed to persist finish for %s: %v", st.ID, perr)
	}
	// Persist job log too (mirrors StartSubTask's defer — survives restart).
	lines, jstatus, exitCode := job.Snapshot()
	if perr := h.jobLogSvc.Save(job.ID, reqID, string(jstatus), exitCode, job.StartedAt, job.FinishedAt, lines, modelName); perr != nil {
		log.Printf("[orchestrate] failed to persist job log %s: %v", job.ID, perr)
	}
	job.Finish(0, store.JobDone)
	log.Printf("[orchestrate] child %s (batch %s seq %d) finished status=%s", st.ID, batch.ID, st.BatchSeq, status)
}

// RunOrchestratorSummary is the summary-round entry point invoked from
// scheduler.OrchestrationQueue.tick when a batch is in 'summarizing' state
// with summary_status='pending'. Responsibilities:
//
//  1. MarkSummary('running') at entry.
//  2. Issue MarkSummaryHeartbeat every 5s so boot recovery can detect a
//     crashed summary goroutine.
//  3. Run the orchestrator summary turn (same code path as the pre-batch
//     runOrchestratorSummary).
//  4. On success: MarkSummary('done') + MarkCompleted(batchID).
//  5. On failure: MarkSummary('error'); batch.status stays 'summarizing' so
//     the next tick re-arms the summary.
//
// The function is intentionally best-effort — it never panics, never blocks
// longer than the underlying claude subprocess, and tolerates DB errors by
// logging + bailing so the queue's next tick can retry.
func (h *WizardHandler) RunOrchestratorSummary(batchID string) {
	if h.batchSvc == nil {
		return
	}
	batch, err := h.batchSvc.Get(batchID)
	if err != nil || batch == nil {
		log.Printf("[orchestrate] summary: batch %s not found: %v", batchID, err)
		return
	}
	// Reserve the summary round. MarkSummary is unconditional so a stale
	// 'running' from a crashed goroutine is correctly overwritten.
	if serr := h.batchSvc.MarkSummary(batchID, model.SummaryRunning); serr != nil {
		log.Printf("[orchestrate] summary %s mark running: %v", batchID, serr)
		return
	}

	// Heartbeat so boot recovery can distinguish a live summary from an
	// orphaned one. Stops when summary work completes (deferred).
	hbInterval := time.Duration(h.summaryKickIntervalSec) * time.Second
	if hbInterval <= 0 {
		hbInterval = 5 * time.Second
	}
	hbDone := make(chan struct{})
	defer close(hbDone)
	go func() {
		ticker := time.NewTicker(hbInterval)
		defer ticker.Stop()
		for {
			select {
			case <-hbDone:
				return
			case <-ticker.C:
				if herr := h.batchSvc.MarkSummaryHeartbeat(batchID); herr != nil {
					log.Printf("[orchestrate] summary heartbeat %s: %v", batchID, herr)
				}
			}
		}
	}()

	// Children of this batch, in dispatch order. Use ListByBatch so manual
	// rows (batch_id='', batch_seq=0) are NOT pulled in — only the children
	// this batch actually owns.
	children, lerr := h.subTaskSvc.ListByBatch(batchID)
	if lerr != nil {
		log.Printf("[orchestrate] summary %s list children: %v", batchID, lerr)
		_ = h.batchSvc.MarkSummary(batchID, model.SummaryError)
		return
	}
	if len(children) == 0 {
		log.Printf("[orchestrate] summary %s: no children found; mark done", batchID)
		_ = h.batchSvc.MarkSummary(batchID, model.SummaryDone)
		_ = h.batchSvc.MarkCompleted(batchID)
		return
	}

	// Build the summary prompt: stitch each child's artifact in execution
	// order. Cap each child at 4KB so a chatty child doesn't blow up the
	// main agent's context.
	var summaryB strings.Builder
	summaryB.WriteString("所有编排子任务已完成。请基于以下子任务产物，输出一份 Markdown 汇总报告，")
	summaryB.WriteString("用于让用户一眼看到：\n1. 整体进展概述\n2. 各子任务的关键成果\n3. 修改的文件清单（按子任务组织）\n4. 整体遗留风险\n\n")
	summaryB.WriteString("## 子任务产物\n\n")
	for i, st := range children {
		fmt.Fprintf(&summaryB, "### %d. %s (%s)\n", i+1, st.Title, st.Status)
		body := st.Artifact
		if len(body) > 4096 {
			body = body[:4096] + "\n…（已截断）"
		}
		summaryB.WriteString(body)
		summaryB.WriteString("\n\n")
	}
	summaryB.WriteString("---\n请直接输出汇总报告 Markdown。")

	req, rerr := h.reqSvc.Get(batch.RequirementID)
	if rerr != nil || req == nil {
		log.Printf("[orchestrate] summary %s: requirement load: %v", batchID, rerr)
		_ = h.batchSvc.MarkSummary(batchID, model.SummaryError)
		return
	}

	// Resume the orchestrator session — it's the main-agent thread that
	// already saw the decompose prompt, so re-resuming lets it carry
	// forward the requirements/design context plus its own decompose
	// reasoning. ForkSession=false: we want a continuation, not a new
	// session (the summary is a follow-up message in the same thread).
	job := h.jobs.Create(batch.RequirementID)
	job.Append(store.LogLine{Type: "phase", Content: "📊 主 Agent 正在汇总子任务产物..."})
	job.SetModel(batch.Model)

	cmd, cancel := h.llm.GenerateCode(llm.StreamOpts{
		Prompt:         summaryB.String(),
		WorkDir:        batch.WorkDir,
		SystemPrompt:   "", // resumed session already has developer persona
		Model:          cliModelArg(batch.Model),
		ClaudeConfigID: batch.ClaudeConfigID,
		SessionID:      batch.OrchestratorSessionID,
		Resume:         true,
		Fork:           false,
	})
	defer cancel()

	summaryUsage := h.usageCtxFor("orchestrate_summary", batch.RequirementID, req.ProjectID, job.ID, batch.Model, "", "auto-summary")
	out := runClaudeStream(jobSink{job}, cmd, "orchestrate-summary", summaryUsage)

	if out.errMsg != "" || out.finalResult == "" {
		log.Printf("[orchestrate] summary %s turn failed: %s / empty=%v", batchID, out.errMsg, out.finalResult == "")
		job.Finish(1, store.JobError)
		// Keep batch.status='summarizing' so the next tick re-arms a fresh
		// summary attempt. summary_status='error' tells the UI to surface
		// the failure without flipping the whole batch to 'errored'.
		_ = h.batchSvc.MarkSummary(batchID, model.SummaryError)
		return
	}

	// Persist the Markdown summary on the requirement. The SubTaskPanel
	// reads it on the next GET and renders it above the children.
	if perr := h.reqSvc.UpdateCodingPlan(batch.RequirementID, out.finalResult); perr != nil {
		log.Printf("[orchestrate] summary %s persist coding_plan: %v", batchID, perr)
	}
	job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(out.finalResult)})
	job.Append(store.LogLine{Type: "done", Content: "✅ 汇总完成！"})
	job.Finish(0, store.JobDone)

	// Terminal: mark summary done + batch completed in order. A failure on
	// either is logged but does not undo the coding_plan write — the user
	// still gets the summary even if the bookkeeping lags one tick.
	if serr := h.batchSvc.MarkSummary(batchID, model.SummaryDone); serr != nil {
		log.Printf("[orchestrate] summary %s mark done: %v", batchID, serr)
	}
	if cerr := h.batchSvc.MarkCompleted(batchID); cerr != nil {
		log.Printf("[orchestrate] summary %s mark completed: %v", batchID, cerr)
	}
	log.Printf("[orchestrate] summary saved to requirements.coding_plan for %s (batch %s)", batch.RequirementID, batchID)
}

// silentSink is a streamSink that discards log output. AutoOrchestrate runs
// the orchestrator's main-agent turn inline (synchronously) so the
// streaming output is invisible to the user — only the terminal
// finalResult matters. We still go through runClaudeStream so we get the
// same token-usage recording as developer-chat.
