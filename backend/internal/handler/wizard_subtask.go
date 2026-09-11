package handler

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
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

// resolveSubTaskAgentServer picks the execution environment a new sub-task
// should run on. A nil explicit pointer means the request omitted the field
// entirely — inherit the parent requirement's environment (the "默认与主任务
// 一致" rule). A non-nil pointer is the user's deliberate choice and is
// honored verbatim, including an empty string which means 本地 (and must NOT
// fall back to a remote parent).
func resolveSubTaskAgentServer(explicit *string, fallback string) string {
	if explicit != nil {
		return *explicit
	}
	return fallback
}

// buildParentContext composes a Markdown block the runner prepends to a
// fresh-session sub-task's prompt, so the new session can answer the
// user's instruction without --resume-ing the (missing) parent jsonl.
//
// Source priority, with 32 KB hard cap matching readProjectContext's
// heuristic (wizard.go:135):
//
//  1. Requirement title + description (always included, top of block).
//  2. Requirement.DesignDocs — the architect-stage plan, when non-empty.
//     Truncated to fit the budget.
//  3. Recent turns from the parent's claude jsonl (if the local
//     Claude session file exists). Capped to last 10 user/assistant
//     turns, each entry capped to 400 chars, to fit the budget.
//  4. Recent sub-task artifact digests (top 5, first 200 chars each) so
//    the new session knows what siblings already did.
//  5. requirements.usage_snapshots — fallback for the recent-turns
//     block when no jsonl is on disk (mirrors what the wizard chat
//     renders when the user can't load the session).
//
// When the assembled text exceeds 32 KB, lower-priority sections are
// truncated in reverse-priority order (5 → 4 → 3 → 2 → 1) until the
// result fits. This guarantees the requirement title / description
// always survive — they're the most important context for any sub-task.
func buildParentContext(req *model.Requirement, subTaskSvc *service.SubTaskService, sourceSID string) string {
	if req == nil {
		return ""
	}
	const hardCap = 32 * 1024
	// 1) Title + description
	head := "## 父任务上下文\n\n"
	title := strings.TrimSpace(req.Title)
	desc := strings.TrimSpace(req.Description)
	if title != "" || desc != "" {
		head += "### 需求\n\n"
		if title != "" {
			head += "- 标题: " + title + "\n"
		}
		if desc != "" {
			head += "- 描述:\n\n" + desc + "\n"
		}
	}
	// 2) Design docs (may be JSON array; surface as plain Markdown if so).
	var designBlock string
	if d := strings.TrimSpace(req.DesignDocs); d != "" {
		designBlock = "\n### 设计方案\n\n" + truncateForContext(d, hardCap) + "\n"
	}
	// 3) Recent turns from jsonl (best-effort; missing file = empty block).
	var turnsBlock string
	if sourceSID != "" && req.WorktreePath != "" {
		if body := readParentJsonlTurns(req.WorktreePath, sourceSID); body != "" {
			turnsBlock = "\n### 父会话近期对话\n\n" + body + "\n"
		}
	}
	// 4) Sub-task artifact digests (top 5, first 200 chars each).
	var digestsBlock string
	if subTaskSvc != nil {
		if rows, err := subTaskSvc.List(req.ID); err == nil && len(rows) > 0 {
			count := 5
			if len(rows) < count {
				count = len(rows)
			}
			start := len(rows) - count
			digestsBlock = "\n### 已执行子任务摘要\n\n"
			for _, row := range rows[start:] {
				if row.Status != "done" && row.Status != "error" && row.Status != "stopped" {
					continue
				}
				digest := row.Artifact
				if len(digest) > 200 {
					digest = digest[:200] + "…"
				}
				digestsBlock += "- " + row.Title + " (" + row.Status + "): " + strings.TrimSpace(digest) + "\n"
			}
		}
	}
	// 5) Usage snapshots fallback (always included as a one-liner; cheap).
	var usageBlock string
	if req.UsageSnapshots != "" {
		usageBlock = "\n### 父任务用量快照\n\n" + truncateForContext(req.UsageSnapshots, 1024) + "\n"
	}
	// Assemble and trim down to cap.
	out := head + designBlock + turnsBlock + digestsBlock + usageBlock
	if len(out) <= hardCap {
		return out
	}
	// Reverse-priority trim. Drop digests first, then turns, then design,
	// then collapse the head's "描述" to 256 chars. The title + 标题 line
	// is always preserved.
	out = head
	if len(out) > hardCap {
		// Drop description body if even the title line doesn't fit.
		out = head[:0]
		out += "### 需求\n\n"
		if title != "" {
			out += "- 标题: " + title + "\n"
		}
		return out
	}
	if designBlock != "" && len(out)+len(designBlock) <= hardCap {
		out += designBlock
	}
	if turnsBlock != "" && len(out)+len(turnsBlock) <= hardCap {
		out += turnsBlock
	}
	if digestsBlock != "" && len(out)+len(digestsBlock) <= hardCap {
		out += digestsBlock
	}
	if usageBlock != "" && len(out)+len(usageBlock) <= hardCap {
		out += usageBlock
	}
	return out
}

// truncateForContext truncates a string to fit a soft byte budget. Used by
// buildParentContext's individual sections before they are summed.
func truncateForContext(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n…[truncated]"
}

// readParentJsonlTurns walks <worktree>/.claude/projects/<slug>/<sid>.jsonl
// (using claudeSessionHome() + EncodeClaudeSlug) and returns up to 10
// recent user/assistant turns as a Markdown block. Returns "" on any error
// or when the file isn't on disk; callers always treat "" as "no recent
// turns available, fall back to usage_snapshots".
//
// The implementation reads the whole file once and walks it backwards —
// jsonl files are append-only so the last lines are the most recent, and a
// 10-turn extract is small enough that a full read is cheaper than a
// streaming reverse walk. Each turn's text is capped to 400 chars to keep
// the budget honest.
func readParentJsonlTurns(worktreePath, sourceSID string) string {
	if worktreePath == "" || sourceSID == "" {
		return ""
	}
	slug := util.EncodeClaudeSlug(worktreePath)
	jsonlPath := filepath.Join(claudeSessionHome(), "projects", slug, sourceSID+".jsonl")
	data, err := os.ReadFile(jsonlPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	// Walk backwards collecting user/assistant messages.
	type turn struct {
		role, text string
	}
	var turns []turn
	for i := len(lines) - 1; i >= 0 && len(turns) < 10; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var evt map[string]interface{}
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		etype, _ := evt["type"].(string)
		role := ""
		var text string
		switch etype {
		case "user":
			role = "用户"
			if msg, ok := evt["message"].(map[string]interface{}); ok {
				if content, ok := msg["content"].([]interface{}); ok {
					for _, block := range content {
						if b, ok := block.(map[string]interface{}); ok {
							if b["type"] == "text" {
								if s, ok := b["text"].(string); ok {
									text += s
								}
							}
						}
					}
				}
			}
		case "assistant":
			role = "助手"
			if msg, ok := evt["message"].(map[string]interface{}); ok {
				if content, ok := msg["content"].([]interface{}); ok {
					for _, block := range content {
						if b, ok := block.(map[string]interface{}); ok {
							if b["type"] == "text" {
								if s, ok := b["text"].(string); ok {
									text += s
								}
							}
						}
					}
				}
			}
		}
		if role == "" || text == "" {
			continue
		}
		if len(text) > 400 {
			text = text[:400] + "…"
		}
		turns = append(turns, turn{role, strings.TrimSpace(text)})
	}
	// Reverse to chronological order.
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	var b strings.Builder
	for _, t := range turns {
		b.WriteString("- ")
		b.WriteString(t.role)
		b.WriteString(": ")
		b.WriteString(t.text)
		b.WriteString("\n")
	}
	return b.String()
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
// fork controls session derivation: fork=true (StartSubTask / AdjustSubTask /
// RedoSubTask) mints a new session id and `--fork-session`s off the source;
// fork=false (ContinueSubTask) reuses parent.SessionID via `--resume` so the
// child continues the previous JSONL in place.
//
// freshSession is the 「新会话（含需求上下文）」 mode: skip --resume, inject
// a ## 父任务上下文 block at the top of the prompt, and stamp the row with
// an empty source_session_id (the column is preserved, the row just no
// longer claims to derive from any specific parent session). Currently
// only StartSubTask honors it; AdjustSubTask / RedoSubTask / ContinueSubTask
// keep their existing semantics because they explicitly fork off a known
// parent's session.
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
	fork bool,
	freshSession bool,
) {
	if h.subTaskRunner == nil {
		log.Printf("[sub-task] runner not wired, cannot run %s", st.ID)
		job.Append(store.LogLine{Type: "error", Content: "❌ 子任务执行器未初始化"})
		job.Finish(1, store.JobError)
		return
	}
	h.subTaskRunner.Run(req, st, job, newSID, sourceSID, body, modelOverride, configIDOverride, adjust, fork, freshSession)
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
// Body: { "prompt": "...", "title": "...", "freshSession"?: bool }  (title / freshSession optional)
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
//
// freshSession == true opts out of the --resume path entirely (the user
// saw the 「源会话已失效」 error and chose the 「新会话（含需求上下文）」
// recovery). The row is still inserted with the parent SID recorded so
// audit logs stay readable, but the runner overwrites source_session_id
// to "" when persisting via NewPendingSubTask (the column is not used at
// run time) — see SubTaskRunner.Run.
func (h *WizardHandler) StartSubTask(w http.ResponseWriter, r *http.Request) {
	if !h.requireSubTaskSvc(w) {
		return
	}
	var body struct {
		Prompt       string `json:"prompt"`
		Title        string `json:"title"`
		Model        string `json:"model"`
		FreshSession bool   `json:"freshSession"`
		// AgentServerID selects the child's execution environment. A pointer so
		// an omitted field (nil) defaults to the parent requirement's env
		// (inheritance), while an explicit "" means the user deliberately chose
		// 本地 and must not fall back to a remote parent.
		AgentServerID *string `json:"agent_server_id"`
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
	// Fresh-session path is the explicit "no parent session needed"
	// override: even when the requirement has no main-agent session
	// yet, the user can still start a sub-task with an injected parent
	// context. The legacy non-fresh path still 409s on missing parent
	// session so the contract there is unchanged.
	if sourceSID == "" && !body.FreshSession {
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

	// Persist the row with sourceSID resolved as usual. When freshSession
	// is true, we still record the parent SID on the row (audit-trail
	// integrity), but the runner drops --resume and writes back an empty
	// source_session_id — see SubTaskRunner.Run for the runtime side.
	// Resolve the execution environment: default to the parent requirement's
	// (inheritance) when the field is omitted, honor an explicit value
	// (including "" for 本地) otherwise.
	agentServerID := resolveSubTaskAgentServer(body.AgentServerID, req.AgentServerID)
	st, job, newSID, err := h.subTaskRunner.NewPendingSubTask(id, strings.TrimSpace(body.Title), body.Prompt, body.Model, sourceSID, agentServerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"job_id":      job.ID,
		"sub_task_id": st.ID,
	})

	go h.runSubTask(req, st, job, newSID, sourceSID, body.Prompt, body.Model, "", false, true, body.FreshSession)
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
	go h.runSubTask(req, st, job, newSID, parent.SessionID, body.Prompt, body.Model, "", true, true, false)
}

// RedoSubTask handles POST /api/requirements/{id}/sub-tasks/{sid}/redo.
//
// Body: { "model"?: "..." }
//
// Re-runs a FAILED sub-task on its EXISTING row (no new sub_tasks row
// inserted) with a fresh session id and a fresh JobStore job. The user
// keeps seeing the same card; only the status flips back to running and
// the artifact is overwritten when the new run finishes.
//
// Session-derivation strategy: the redo forks the requirement's main-agent
// session (coding_session_id → design_session_id fallback), NOT the
// parent's own session — so the new run starts from a clean slate, free of
// the partial / broken state the failed run left behind on the parent's
// JSONL. This mirrors the original RedoSubTask semantics ("从 source session
// 干净 fork 再跑") but without INSERTing a child row.
//
// The optional model override lets the user switch models on the retry;
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

	parent, err := h.subTaskSvc.Get(sid)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "sub-task not found")
		return
	}
	if parent.RequirementID != id {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "sub-task does not belong to this requirement")
		return
	}
	// Redo is scoped to failures — a done/pending/running/stopped row has
	// nothing to recover via redo. Stopped rows are explicitly Continue-able
	// (see ContinueSubTask), not Redo-able; the UI hides Redo on stopped
	// cards and uses Continue instead.
	if parent.Status != model.SubTaskStatusError {
		writeError(w, http.StatusConflict, "NOT_FAILED", "仅失败的任务可重做")
		return
	}

	sourceSID := subTaskSourceSID(req, parent.SourceSessionID)
	if sourceSID == "" {
		writeError(w, http.StatusConflict, "NO_SESSION",
			"无法解析可复用的源会话，请重新发起 coding 后再试")
		return
	}

	// In-place reset: clears artifact / job_id / token counters / cost /
	// duration / completed_at and flips status back to pending. The row id
	// (and prompt / title / source_session_id / source) are preserved.
	st, err := h.subTaskSvc.RedoReset(sid, body.Model)
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
		"job_id":         job.ID,
		"sub_task_id":    st.ID,
		"reused_session": "true",
	})

	// Re-use the shared spawn helper with adjust=false, fork=true and the
	// ORIGINAL prompt (st.Prompt) so the child re-executes the same task
	// from a clean fork off the requirement's main-agent session.
	go h.runSubTask(req, st, job, newSID, sourceSID, st.Prompt, body.Model, "", false, true, false)
}

// continueSubTaskPrompt is the fixed Chinese prompt used by ContinueSubTask.
// Mirrors the wording ContinueCoding (wizard_coding.go) ships to the main
// coding agent so the sub-task variant stays consistent — the child is asked
// to first survey the workspace, identify what the previous run left done vs.
// pending, then resume from the partial state and summarize in Chinese.
const continueSubTaskPrompt = "继续完成之前的子任务。请先检查当前代码与工作区状态，判断哪些部分已完成、哪些未完成或需要修复；然后基于子任务的原始 prompt 继续完成剩余工作、补齐缺失内容。最后用中文总结本次完成的内容。"

// ContinueSubTask handles POST /api/requirements/{id}/sub-tasks/{sid}/continue.
//
// Body: { "model"?: "..." }
//
// In-place resume of an interrupted / failed / stopped sub-task on the SAME
// session id (no fork, no new JSONL). Compared to RedoSubTask:
//
//   - Redo forks the requirement's main-agent session (clean start, new
//     session id, --fork-session).
//   - Continue reuses parent.SessionID and runs --resume <parent.SessionID>
//     so the child continues the same JSONL the previous attempt left off in,
//     inheriting the conversation history it had built up. The previous
//     artifact stays visible on the row until the new run's Finish overwrites
//     it, so a user who refreshes mid-run sees the partial report instead of
//     a blank card.
//
// Trigger conditions: parent.Status must be one of {error, stopped}.
// running/pending/done are NOT continue-eligible:
//   - running → clickable action is Stop, not Continue.
//   - pending → the existing run hasn't started yet; user should wait or
//     STOP+continue.
//   - done → already terminal; user should Redo if they want a re-run, or
//     Adjust if they want to push more instructions.
//
// parent.SessionID == "" → 409 NO_SESSION. This happens when the sub-task
// was created before the dev branch had a coding_session_id stamped on it,
// or after a clean re-init. The only recovery is Redo (which doesn't need a
// parent session id — it forks the main agent's session instead).
func (h *WizardHandler) ContinueSubTask(w http.ResponseWriter, r *http.Request) {
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
	parent, err := h.subTaskSvc.Get(sid)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "sub-task not found")
		return
	}
	if parent.RequirementID != id {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "sub-task does not belong to this requirement")
		return
	}
	if parent.Status != model.SubTaskStatusError && parent.Status != model.SubTaskStatusStopped {
		writeError(w, http.StatusConflict, "NOT_CONTINUABLE", "该子任务无法继续")
		return
	}
	if parent.SessionID == "" {
		writeError(w, http.StatusConflict, "NO_SESSION", "无法续接：原会话 id 为空，请改用「重做」")
		return
	}

	// In-place reset that PRESERVES the existing artifact (so a refresh mid-
	// run still shows the previous report). Token / cost / duration /
	// completed_at / job_id are cleared so the SubTaskCard header re-renders
	// from zero; model is updated only when the caller passed a non-empty
	// override.
	st, err := h.subTaskSvc.ContinueReset(sid, body.Model)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	newSID := parent.SessionID
	sourceSID := parent.SessionID
	job := h.jobs.Create(id)
	if perr := h.subTaskSvc.UpdateJobID(st.ID, job.ID); perr != nil {
		log.Printf("[sub-task continue] failed to persist job_id for %s: %v", st.ID, perr)
	}
	if perr := h.subTaskSvc.UpdateSession(st.ID, newSID, sourceSID); perr != nil {
		log.Printf("[sub-task continue] failed to persist session for %s: %v", st.ID, perr)
	}
	if body.Model != "" {
		if perr := h.subTaskSvc.UpdateModel(st.ID, body.Model); perr != nil {
			log.Printf("[sub-task continue] failed to persist model for %s: %v", st.ID, perr)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"job_id":         job.ID,
		"sub_task_id":    st.ID,
		"reused_session": "false",
	})

	// Continue reuses the parent's session id; --resume <parent.SessionID>
	// runs the child in the same JSONL the previous attempt appended to.
	// Run() with fork=false picks "## 继续执行" as the prompt header so the
	// child's contextualization stays consistent with the wizard's coding
	// ContinueCoding path.
	go h.runSubTask(req, st, job, newSID, sourceSID, continueSubTaskPrompt, body.Model, "", false, false, false)
}

// StopSubTask handles POST /api/requirements/{id}/sub-tasks/{sid}/stop.
//
// Cancels a running sub-task's underlying claude subprocess and flips the row
// to SubTaskStatusStopped. The artifact is preserved (with the prior report
// prepended by a "⏹ 用户中止于 <RFC3339>" banner) so the user can still read
// what the run had produced before they pulled the plug — and a follow-up
// ContinueSubTask can pick up from the same JSONL.
//
// Mechanism:
//  1. Look up the JobStore job by sub_tasks.job_id.
//  2. job.Cancel() invokes the cancel func captured in SubTaskRunner.Run's
//     job.SetCmd(cmd, cancel). The gateway's exec.CommandContext chains
//     SIGTERM → WaitDelay 5s → SIGKILL automatically, so the goroutine
//     should always unwind within ~6s.
//  3. MarkStopped prepends the stop banner to artifact and flips status to
//     "stopped". Concurrent with MarkStopped, the runner's deferred
//     finishSubTask may still run; it re-reads sub_tasks.status and, if it
//     sees "stopped", writes only token/cost/duration via
//     UpdateRunStatsOnStop (preserving the banner).
//
// Guard rails:
//   - parent.Status must be "running" — clicking Stop on an already-terminal
//     row is a no-op and returns 409 NOT_RUNNING.
//   - req.AgentServerID != "" → 501 STOP_REMOTE_NOT_SUPPORTED. The remote
//     worker (Agent server) doesn't yet ship a kill RPC; rolling it out is
//     future work.
//   - If the JobStore has already evicted the job (ring buffer cap=50) OR
//     the job is in a terminal state (the goroutine finished after our
//     status check but before Cancel), return 409 JOB_GONE — user should
//     Continue or Redo.
//
// 6-second watchdog goroutine: in theory gateway's CommandContext chain
// always finishes the subprocess within 5s + the job.Finish path. If for
// any reason the JobStore job is still "running" after 6s (e.g. a stuck
// runClaudeStream), we re-call MarkStopped with the latest artifact to
// make sure the row is durable and the user sees a stopped state even if
// the SSE eventually fails to deliver a terminal frame.
func (h *WizardHandler) StopSubTask(w http.ResponseWriter, r *http.Request) {
	if !h.requireSubTaskSvc(w) {
		return
	}
	id := r.PathValue("id")
	sid := r.PathValue("sid")
	req, err := h.reqSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "requirement not found")
		return
	}
	parent, err := h.subTaskSvc.Get(sid)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "sub-task not found")
		return
	}
	if parent.RequirementID != id {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "sub-task does not belong to this requirement")
		return
	}
	if parent.Status != model.SubTaskStatusRunning {
		writeError(w, http.StatusConflict, "NOT_RUNNING", "该子任务未运行，无需停止")
		return
	}
	// Remote runs (Agent server dispatch) are not stoppable in v1 — the
	// worker process lives on a different host and would need a kill RPC
	// that the worker doesn't ship yet. Surface the limitation explicitly
	// (501, not 404) so the client can render a "stop is coming" hint. Use
	// the sub-task's resolved effective environment (its own agent_server_id,
	// falling back to the requirement's for legacy rows) so a child that
	// deliberately runs 本地 under a remote main stays stoppable.
	effectiveServerID := parent.AgentServerID
	if !parent.AgentServerIDSet {
		effectiveServerID = req.AgentServerID
	}
	if effectiveServerID != "" {
		writeError(w, http.StatusNotImplemented, "STOP_REMOTE_NOT_SUPPORTED",
			"远程 Agent 服务器执行暂不支持停止")
		return
	}
	job, ok := h.jobs.Get(parent.JobID)
	if !ok || job == nil {
		writeError(w, http.StatusConflict, "JOB_GONE",
			"任务已被回收，请用「继续」或「重做」重新发起")
		return
	}
	// RLock-snapshot the status to avoid racing the goroutine that may be
	// finishing the job concurrently.
	_, _, _ = job.Snapshot()
	_, jobStatus, _ := job.Snapshot()
	if jobStatus != store.JobRunning {
		writeError(w, http.StatusConflict, "JOB_GONE",
			"任务已被回收，请用「继续」或「重做」重新发起")
		return
	}

	// Snapshot the prior artifact BEFORE MarkStopped (which prepends the
	// banner). Used both for the immediate write and the watchdog retry.
	priorArtifact := parent.Artifact

	// Cancel the subprocess. Cancel itself returns immediately — the gateway
	// chains SIGTERM → WaitDelay 5s → SIGKILL in the background.
	job.Cancel()

	// Flip status to "stopped" + prepend the banner. This is the
	// authoritative write: even if the runner's deferred finishSubTask lerks
	// in afterwards, it observes status=stopped and skips the artifact
	// overwrite.
	if perr := h.subTaskSvc.MarkStopped(parent.ID, priorArtifact); perr != nil {
		log.Printf("[sub-task stop] failed to mark stopped for %s: %v", parent.ID, perr)
	}

	// 6-second watchdog: in case gateway's CommandContext doesn't unwind
	// the goroutine (stuck stream consumer, etc.), re-mark the row and
	// force-finish the job after a 6s grace period so SSE subscribers
	// always see a terminal frame.
	go func(parentID, parentJobID, prior string) {
		deadline := time.Now().Add(6 * time.Second)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for time.Now().Before(deadline) {
			<-ticker.C
			j, found := h.jobs.Get(parentJobID)
			if !found || j == nil {
				// Job already evicted from ring buffer → terminal.
				return
			}
			_, status, _ := j.Snapshot()
			if status != store.JobRunning {
				return
			}
		}
		// Still running after 6s — force-finish the JobStore job so SSE
		// subscribers unblock, and idempotently re-mark the row stopped.
		if j, found := h.jobs.Get(parentJobID); found && j != nil {
			j.Finish(0, store.JobDone)
		}
		if perr := h.subTaskSvc.MarkStopped(parentID, prior); perr != nil {
			log.Printf("[sub-task stop watchdog] failed to re-mark stopped for %s: %v", parentID, perr)
		}
		log.Printf("[sub-task stop] watchdog forced finish for %s", parentID)
	}(parent.ID, parent.JobID, priorArtifact)

	writeJSON(w, http.StatusOK, map[string]string{
		"status":      "stopping",
		"sub_task_id": parent.ID,
		"job_id":      parent.JobID,
	})
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
