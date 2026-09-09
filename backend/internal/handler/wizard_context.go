// wizard_context.go: extracted from wizard.go as part of the refactoring.

package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

type compressContextReq struct {
	RequirementID string `json:"requirement_id"`
	Step          string `json:"step"`
}

// compressContextDone is the JSON payload of the terminal "done" event for
// the compress-context SSE stream. Carrying the summary here (instead of a
// follow-up GET) lets the modal pop with a single round-trip; the persisted
// row is fetched separately by the frontend's requirementsApi.get() so the
// rest of the detail page (sidebar / stepper) reflects the new compressed_at
// timestamp too.
type compressContextDone struct {
	Step        string `json:"step"`
	Summary     string `json:"summary"`
	TokensUsed  int    `json:"tokens_used"`
	Model       string `json:"model"`
	StartedAt   int64  `json:"started_at_ms"`
	CompletedAt int64  `json:"completed_at_ms"`
}

// CompressContext is the implementation of POST /api/wizard/compress-context.
// It runs a single --resume turn asking Claude to summarize the current
// wizard stage's conversation, writes the result to requirements.{step}_
// context_summary + stamps compressed_at, and clears the matching session id
// — all in one transaction via service.UpdateContextSummary so the next turn
// in this stage starts fresh and sees the summary as a prompt prefix.
//
// Stream protocol (SSE under text/event-stream):
//
//	phase       — human-readable status line
//	message     — Claude's summary text as it streams in
//	usage       — mirror of the result.usage block (powers the live usage bar)
//	error       — terminal failure (no DB write happens)
//	done        — terminal success; carries {step, summary, tokens_used, model}
//
// Failure policy: on any error path (stream failure, stale session, missing
// [COMPRESS_COMPLETE] marker) we emit `error` + `done{success:false}` and
// DELIBERATELY skip the DB write — the session stays intact so the user can
// retry without losing context.
func (h *WizardHandler) CompressContext(w http.ResponseWriter, r *http.Request) {
	var req compressContextReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "invalid JSON: "+err.Error())
		return
	}
	if req.RequirementID == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "requirement_id is required")
		return
	}
	if !service.ValidContextSummaryStep(req.Step) {
		writeError(w, http.StatusBadRequest, "INVALID", "step must be one of: analyst_chat, architect_design, coding")
		return
	}

	rc := http.NewResponseController(w)
	writeSSEHeaders(w)
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	startedAtMs := time.Now().UnixMilli()

	requirement, err := h.reqSvc.Get(req.RequirementID)
	if err != nil {
		sendStatus(w, rc, "error", "未找到需求："+err.Error())
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	// Pick the matching session id; reject the request when there is no
	// resumable session so the user doesn't burn tokens summarizing nothing.
	var sourceSID string
	switch req.Step {
	case "analyst":
		sourceSID = requirement.AnalysisSessionID
	case "design":
		sourceSID = requirement.DesignSessionID
	case "coding":
		sourceSID = requirement.CodingSessionID
	}
	if sourceSID == "" {
		sendStatus(w, rc, "error", "该阶段尚无可压缩的会话（先与 AI 对话一轮再试）")
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	// Resolve project path the same way the other wizard stages do — anchor
	// the resumed turn to the requirement's worktree (or the project root when
	// no worktree exists) so absolute paths in the resume history still resolve.
	projectPath := ""
	defaultBranch := ""
	if proj, perr := h.projectSvc.Get(requirement.ProjectID); perr == nil {
		projectPath = proj.LocalPath
		defaultBranch = proj.DefaultBranch
	}
	workDir, werr := h.resolveWorkDir(requirement, projectPath, defaultBranch)
	if werr != nil {
		// Non-fatal: a missing worktree doesn't break --resume, which can run
		// without a cwd. Log it so debugging is possible but proceed.
		log.Printf("[compress-context] resolveWorkDir failed for %s: %v", req.RequirementID, werr)
		workDir = projectPath
	}

	// Use the analyst role's system prompt — compression is essentially a
	// summary-extraction skill, and reusing the analyst persona keeps the
	// output style consistent with the other analytical turns. The model
	// override follows the same precedence as other wizard handlers.
	systemPrompt, model, claudeConfigID := h.roleConfig("analyst")

	// Compression prompt (Chinese, fixed). The [COMPRESS_COMPLETE] sentinel
	// is parsed by this handler to know when Claude has finished writing —
	// text before the sentinel is the summary; text after (rare, in case the
	// model emits trailing chatter) is discarded. We DISALLOW every tool so
	// the resumed turn can only read the conversation history baked into
	// --resume and produce a single textual summary.
	const compressPrompt = `请把当前对话压缩为一段精炼的中文摘要（300-800 字），要求：
1. 保留关键决策、技术约束、已确认的需求点
2. 保留当前进展状态、待办事项
3. 保留涉及的具体文件、函数、模块名
4. 不要新增原对话没有的信息
5. 不要输出代码片段（除非是必须保留的命名/路径）
6. 末尾单独一行输出：[COMPRESS_COMPLETE]`

	noTools := []string{
		"Read", "Glob", "Grep", "Bash", "Write", "Edit",
		"WebFetch", "WebSearch", "NotebookEdit",
	}

	cmd := h.llm.StreamCmd(r.Context(), llm.StreamOpts{
		Prompt:          compressPrompt,
		WorkDir:         workDir,
		SessionID:       sourceSID,
		Resume:          true,
		SystemPrompt:    systemPrompt,
		Model:           cliModelArg(model),
		ClaudeConfigID:  claudeConfigID,
		DisallowedTools: noTools,
	})

	// Usage record: step is namespaced under "compress_<step>" so the existing
	// usageApi.requirement() aggregation includes compression cost without
	// confusing it with regular turn counts in the step dropdown.
	usageStep := "compress_" + req.Step
	uctx := h.usageCtxFor(usageStep, req.RequirementID, requirement.ProjectID, "", model, "", "")

	sink := sseSink{w: w, rc: rc}
	sink.emit(store.LogLine{Type: "phase", Content: "📦 正在压缩上下文..."})

	out := runClaudeStream(sink, cmd, "compress-context", uctx)

	if out.staleSession {
		// Disk session is gone (cleaned up, container restart, etc.). We can't
		// resume, so the right thing is to leave the (already-empty) sid
		// alone — the next regular turn will start a fresh session anyway.
		sendStatus(w, rc, "error", "⚠️ 该阶段的会话已过期（磁盘已被清理），无需压缩")
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}
	if out.errMsg != "" {
		sendStatus(w, rc, "error", "❌ "+out.errMsg)
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	// Extract the summary: everything before [COMPRESS_COMPLETE], trimmed.
	// A missing marker is treated as a soft failure so we don't store a half-
	// formed or empty summary that would mislead future turns.
	summary := out.finalResult
	if idx := strings.Index(summary, "[COMPRESS_COMPLETE]"); idx >= 0 {
		summary = summary[:idx]
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		sendStatus(w, rc, "error", "❌ Claude 未输出有效摘要（缺少 [COMPRESS_COMPLETE] 标记或摘要为空），请重试")
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	// Token count for the done payload — read straight from the stream
	// outcome we already have in scope (no DB roundtrip needed). The model
	// comes from the result event's actualModel (set by runClaudeStream) with
	// a fallback to the pre-dispatch role model.
	tokensUsed := out.lastUsage.InputTokens +
		out.lastUsage.OutputTokens +
		out.lastUsage.CacheCreationTokens +
		out.lastUsage.CacheReadTokens
	modelOut := out.actualModel
	if modelOut == "" {
		modelOut = model
	}

	// Persist + clear session id atomically. A failure here leaves the
	// summary un-saved so the user can retry — the original session stays
	// live (we never cleared the sid before the write).
	if perr := h.reqSvc.UpdateContextSummary(req.RequirementID, req.Step, summary); perr != nil {
		log.Printf("[compress-context] persist failed for %s step=%s: %v", req.RequirementID, req.Step, perr)
		sendStatus(w, rc, "error", "❌ 持久化失败："+perr.Error())
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	completedAtMs := time.Now().UnixMilli()
	done := compressContextDone{
		Step:        req.Step,
		Summary:     summary,
		TokensUsed:  tokensUsed,
		Model:       modelOut,
		StartedAt:   startedAtMs,
		CompletedAt: completedAtMs,
	}
	if b, mErr := json.Marshal(done); mErr == nil {
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":true,\"payload\":%s}\n\n", string(b))
	} else {
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":true}\n\n")
	}
	rc.Flush()
}

// GetContextSummary returns the persisted compression summary for one
// requirement + step. Optional endpoint — the frontend can derive this from
// the regular Requirement GET (which now exposes the *_context_summary +
// *_compressed_at fields), but exposing a dedicated endpoint keeps the
// "📦 已压缩" UI affordance cheap to refresh on its own.
//
// GET /api/wizard/requirement/{id}/context-summary?step=analyst_chat|architect_design|coding
func (h *WizardHandler) GetContextSummary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "missing requirement id")
		return
	}
	step := r.URL.Query().Get("step")
	if !service.ValidContextSummaryStep(step) {
		writeError(w, http.StatusBadRequest, "INVALID", "step must be one of: analyst_chat, architect_design, coding")
		return
	}
	summary, err := h.reqSvc.GetContextSummary(id, step)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requirement_id": id,
		"step":           step,
		"summary":        summary,
	})
}

// ----------------------------------------------------------------------------
// Sub-task (子任务) endpoints
//
// A sub-task is a manually-triggered child agent that runs under a
// requirement's developing stage. It forks the requirement's main-agent
// session (coding_session_id, with design_session_id as fallback) so every
// child agent shares the parent's context — project structure, design docs,
// prior conversation — but executes in its own claude process and writes its
// own Markdown artifact to sub_tasks.artifact on completion.
//
// Endpoints:
//   POST /api/requirements/{id}/sub-tasks     → start a new child agent
//   GET  /api/requirements/{id}/sub-tasks     → list child agents for the req
//   GET  /api/requirements/{id}/sub-tasks/{sid} → fetch one (incl. artifact)
//
// The SSE stream for a running sub-task reuses the existing
// /api/wizard/jobs/{jobId}/stream endpoint — the SubTaskPanel wires its
// createEventStream to that path with sub_task.job_id so the panel gets the
// exact same phase/tool_call/message/result event flow the developer stage
// already uses.
// ----------------------------------------------------------------------------

// requireSubTaskSvc is a small guard helper. When the service was never
// injected (legacy / standalone deployment) the sub-task endpoints must 503
// rather than 500 from a nil-pointer panic. Returns false after writing the
// 503 response; the caller returns immediately.
