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
	"runtime/debug"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

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
	// Inherit the parent requirement's execution environment for every child
	// (same rule as tryAutoOrchestrate). Best-effort lookup — an error leaves
	// agentServerID empty (本地), which matches the pre-feature behavior.
	childAgentServerID := ""
	if req, gerr := h.reqSvc.Get(reqID); gerr == nil && req != nil {
		childAgentServerID = req.AgentServerID
	}
	for i, t := range payload.Subtasks {
		if _, cerr := h.subTaskSvc.CreateWithBatchTx(tx, reqID, t.Title, t.Prompt, modelName, orchestratorSID, obID, i+1, childAgentServerID); cerr != nil {
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

// extractCodingPlan pulls the main agent's task breakdown section out of a
// freeform claude response. Tries sentinel-wrapped first (more robust against
// nested ## sections), falls back to a "## 任务分解" heading scan. Returns ""
// when no plan section is detected — callers persist "" to clear stale plans.
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
func rewritePersonaWorkDir(prompt, localWorkDir, remoteWorkDir string) string {
	if prompt == "" || localWorkDir == "" || localWorkDir == remoteWorkDir {
		return prompt
	}
	const label = "工作目录："
	const closeParen = "）"
	idx := strings.Index(prompt, label)
	if idx < 0 {
		return prompt
	}
	// Find the closing paren after the label. If absent, bail out
	// and leave the prompt alone — the label showed up but the
	// header structure we expect wasn't there.
	end := strings.Index(prompt[idx+len(label):], closeParen)
	if end < 0 {
		return prompt
	}
	end += idx + len(label)
	// Verify the slice between the label and the closing paren
	// actually equals localWorkDir. If it doesn't match (e.g. the
	// label appears in some unrelated text), return the prompt
	// unchanged rather than corrupting it.
	between := prompt[idx+len(label) : end]
	if between != localWorkDir {
		return prompt
	}
	return prompt[:idx+len(label)] + remoteWorkDir + prompt[end:]
}
// finalResult and verifies the [SUBTASKS_READY] sentinel is present. Returns
// nil when either is missing — caller treats that as "main agent answered a
// normal question, not a decompose request" and just renders the chat reply.
//
// We intentionally do NOT use the existing extractJSON() brace matcher
// because the agent typically wraps the JSON in a ```json fence; this
// function locates the first ```json block, then JSON-decodes its contents.
// Falls back to a brace match when no fence is found (more permissive, lets
// the agent omit the fence in low-token responses).
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
		// Inherit the parent requirement's execution environment so an
		// auto-orchestrated child runs where the code lives (and the
		// SubTaskCard badge shows the same environment as the main task).
		if _, cerr := h.subTaskSvc.CreateWithBatchTx(tx, reqID, t.Title, t.Prompt, modelName, orchestratorSID, obID, i+1, req.AgentServerID); cerr != nil {
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
	// Terminal-state fallback (same rationale as the coding goroutine in
	// wizard_coding.go): a panic here would otherwise leave this child's job
	// running forever — its card spins on "Claude 正在工作..." with no recovery
	// path, and only a backend restart clears it.
	//
	// Registered BEFORE the early-exit guard below, deliberately: defer is LIFO,
	// so the early-exit guard runs first and keeps ownership of its early-exit
	// artifact + "❌ 子任务早退" log lines (its Appends must land before the job
	// goes terminal — Append drops post-terminal lines). This fallback then only
	// ever acts on the panic path, where earlyExitReason is still "" and the
	// guard above returns early. job is assigned further down the function,
	// hence the nil check throughout.
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[orchestrate] panic recovered for child %s (batch %s seq %d): %v\n%s",
				st.ID, batch.ID, st.BatchSeq, rec, debug.Stack())
			if job != nil {
				job.Append(store.LogLine{Type: "error", Content: "❌ 内部异常，任务已中止: " + fmt.Sprint(rec)})
			}
		}
		// Never leave the job in JobRunning. Idempotent Finish makes the two
		// normal exits (early-exit guard, happy path) no-ops here.
		if job != nil {
			if _, status, _ := job.Snapshot(); status == store.JobRunning {
				job.Finish(1, store.JobError)
			}
		}
	}()
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

	// Load the requirement row for ProjectID + usage attribution + the legacy
	// environment fallback. Done at the call site (rather than passing through
	// the batch) so the same lookup path SubTaskRunner.Run uses applies here.
	req, rerr := h.reqSvc.Get(reqID)
	if rerr != nil || req == nil {
		earlyExitReason = "无法加载需求行: " + errString(rerr)
		log.Printf("[orchestrate] requirement %s missing for child %s: %v", reqID, st.ID, rerr)
		return
	}
	// The child runs where ITS OWN row says, not where the requirement currently
	// says: a manual override (or a requirement whose environment was changed
	// after this row was created) must be honored, and the SubTaskPanel card
	// renders the same resolution. resolveEffectiveAgentServer is shared with
	// SubTaskRunner.Run so the two dispatch paths can never drift apart.
	// NOTE: reqRow / usage below keep using `req` — the ProjectID and the usage
	// attribution stay with the requirement row, only the dispatch target moves.
	effectiveServerID := resolveEffectiveAgentServer(st, req)

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
	// Same cross-environment caveat Run prints, from the same helper.
	appendCrossEnvHint(job, effectiveServerID, req.AgentServerID, req.SyncMode)

	childUsage := h.usageCtxFor("sub_task", reqID, req.ProjectID, job.ID, modelName, "", st.Prompt)
	// Route orchestrated children to the environment resolved above — same
	// reasoning as runSubTask: the working tree lives on that host, so a
	// locally-spawned child would edit the wrong checkout.
	var out claudeStreamOutcome
	if effectiveServerID != "" && h.agentSvrSvc != nil {
		out = h.runRemoteCoding(&remoteCodingInput{
			job:      job,
			serverID: effectiveServerID,
			req: startCodingReq{
				RequirementTitle: req.Title + " / " + st.Title,
				RequirementID:    req.ID,
				BranchName:       req.BranchName,
				AgentServerID:    effectiveServerID,
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
		// Same three-way diagnostic split as SubTaskRunner.finishSubTask
		// — orchestrated children run through the same wizard_remote
		// path and encounter the same root cause when the parent's
		// jsonl is missing on the remote agent host.
		switch out.SessionFileMissingSide {
		case "remote":
			artifactBody = "❌ 远端 Agent 服务器上找不到源会话文件（上行失败 / 文件被清理）。建议：1) 重新发起 coding；2) 勾选「新会话（含需求上下文）」；3) 到「设置 → Agent 服务器 → 安装依赖」复检。"
		case "local":
			artifactBody = "❌ 本地 Claude 会话目录中找不到源会话文件。建议：1) 重新发起 coding；2) 勾选「新会话（含需求上下文）」。"
		case "sync-failed":
			artifactBody = "❌ 会话文件 SFTP 同步失败。建议：1) 重试；2) 检查 Agent 服务器磁盘与 ~/.claude/projects/ 写权限；3) 勾选「新会话（含需求上下文）」。"
		default:
			artifactBody = "❌ 源会话已失效（session 文件不存在），请重新发起 coding 后再试。"
		}
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
	// Bump attempts atomically so tickSummarizing's case SummaryError arm
	// can stop re-arming once we hit model.SummaryMaxAttempts (3).
	// Best-effort: a bump failure logs and proceeds — the cap is a guard
	// rail, not a correctness invariant.
	if _, aerr := h.batchSvc.BumpAndFetchSummaryAttempts(batchID); aerr != nil {
		log.Printf("[orchestrate] summary %s bump attempts: %v", batchID, aerr)
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
		// Auto-push收尾: even with no children the batch is complete, so honor
		// the auto_push intent. req is loaded below on the normal path; here we
		// fetch it directly (best-effort) so this early exit ships too.
		if r, rerr := h.reqSvc.Get(batch.RequirementID); rerr == nil && r != nil && r.AutoPush {
			go h.autoPushPR(r)
		}
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

	// Stale-session recovery：会话文件可能已被清理/失效。镜像 re-orchestrate
	// 的 134-142：清空 OrchestratorSessionID、铸新 SID、用 Resume=false 再来一次。
	if out.staleSession {
		log.Printf("[orchestrate] summary %s: orchestrator session stale, retrying fresh", batchID)
		job.Append(store.LogLine{Type: "phase", Content: "🔄 主 Agent 会话已失效，正在用全新会话重试汇总..."})
		if cerr := h.batchSvc.ClearOrchestratorSession(batchID); cerr != nil {
			log.Printf("[orchestrate] summary %s clear session: %v", batchID, cerr)
		}
		freshSID := util.NewUUID()
		if uerr := h.batchSvc.UpdateOrchestratorSession(batchID, freshSID); uerr != nil {
			log.Printf("[orchestrate] summary %s persist fresh sid: %v", batchID, uerr)
		}
		cmd2, cancel2 := h.llm.GenerateCode(llm.StreamOpts{
			Prompt:         summaryB.String(),
			WorkDir:        batch.WorkDir,
			SystemPrompt:   "",
			Model:          cliModelArg(batch.Model),
			ClaudeConfigID: batch.ClaudeConfigID,
			SessionID:      freshSID,
			Resume:         false,
			Fork:           false,
		})
		if cmd2 != nil {
			defer cancel2()
			out = runClaudeStream(jobSink{job}, cmd2, "orchestrate-summary-retry", summaryUsage)
		}
	}

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

	// Auto-push收尾 (拆分路径): all children + the summary are done, so
	// development is complete — trigger the "提交 → 推送 → 创建 PR" sub-task
	// when the requirement opted in. This is the split counterpart to the
	// non-split trigger in execStartCoding. Idempotent + goroutine-based, so a
	// summary re-run after a restart can't corrupt anything (git push -u / PR
	// creation both no-op when already applied).
	if req != nil && req.AutoPush {
		go h.autoPushPR(req)
	}
}
