package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// ── 阶段答疑对话 (Stage Q&A Chat) ──────────────────────────────────────────
//
// 需求详情页有两条"生产型" AI 会话:方案设计会话(architect,
// requirements.design_session_id)与开发会话(developer,
// requirements.coding_session_id)。它们承载着方案产出与代码实现的完整上下文,
// 但用户想快速问一句"这里为什么这么设计"时,现有入口都有副作用:
//
//   - refine-doc / apply-doc 是**改文档**流程,resume 阶段会话,没有会话时
//     还会铸新 id 写回 design_session_id;
//   - developer-chat / adjust-coding 会**续写或覆盖** coding_session_id,
//     答疑内容混进开发主线,污染后续 --resume 的上下文。
//
// 本文件实现第三条路径:从阶段会话 **fork** 出一个全新的独立会话,在里面多轮
// 答疑。fork 继承原会话的全部上下文(模型"记得"自己为什么这么设计 / 这么
// 写),但答疑的任何一轮都**不写回原会话** —— design_session_id /
// coding_session_id 及其 jsonl 转录零污染。答疑是纯问答:不改文件、不改方案
// 文档(协议层禁掉 Write / Edit,prompt 里再声明一次)。
//
// 执行形态照抄 AnalystChat 的 JobStore 模式:POST 立即返回 job_id,前端订阅
// GET /api/wizard/jobs/{id}/stream。持久化形态照抄 code_review_followups 的
// "一行一问一答" —— 后端 job 化执行要求答案由后端落库,用户发问后关掉浏览器
// 答案不能丢。

// qaChatDisallowedTools blocks the file-mutation tools during a Q&A turn.
// Read / Glob / Grep / Bash stay available so the answer can be grounded in
// the real code — the same read-only contract the developer "追加调整" chat
// uses (developerChatDisallowedTools).
var qaChatDisallowedTools = []string{"Write", "Edit"}

// qaStallTimeout is the stall-watchdog window for a Q&A run.
//
// CLAUDE_TIMEOUT does NOT govern this path (StreamCmd's ctx is owned by the
// caller), so the watchdog is the only thing standing between a hung proxy and
// a job that never finishes. The default 3-minute window is too tight for a
// forked turn that re-reads a few files before answering, so this raises it to
// the same 10 minutes the architect stage uses.
const qaStallTimeout = 10 * time.Minute

// qaHardTimeout is the absolute cap on one Q&A run. It is a backstop under the
// stall watchdog (which only fires when the stream goes quiet — a chatty but
// endless run would otherwise never trip it).
const qaHardTimeout = 30 * time.Minute

// qaSessionMode is how a Q&A turn gets its context.
const (
	// qaModeResume continues the thread's own QA session (--resume).
	qaModeResume = "resume"
	// qaModeFork forks the stage session into a brand-new QA session
	// (--resume <stage> --fork-session --session-id <new>).
	qaModeFork = "fork"
	// qaModeFresh starts a brand-new session with the context pasted into
	// the prompt — the degraded path.
	qaModeFresh = "fresh"
)

// qaSessionPlan is the pure result of resolving "which session should this
// turn run in". Kept as a value type (no I/O) so the whole decision tree can
// be unit-tested.
type qaSessionPlan struct {
	// Mode is one of qaModeResume / qaModeFork / qaModeFresh.
	Mode string
	// ResumeSID is the QA session to continue (qaModeResume only).
	ResumeSID string
	// SourceSID is the stage session to fork from (qaModeFork only).
	SourceSID string
	// NewSessionID is the session id the run should use:
	//   - qaModeResume: the session being continued;
	//   - qaModeFork / qaModeFresh: a freshly minted id, pre-assigned via
	//     --session-id so it is known (and persisted) before the run starts.
	NewSessionID string
	// Resumed is true when the prompt can use the SHORT form (the session
	// already holds the forked context). False for a fresh run, whose prompt
	// has to carry the context itself.
	Resumed bool
}

// resolveQASession decides how a new Q&A turn gets its context.
//
// Inputs:
//   - stageSID: the requirement's CURRENT stage session id (design_session_id
//     or coding_session_id), empty when the stage hasn't run (or the user
//     compressed it away);
//   - latest: the thread's newest ANSWERED turn (errors are skipped — a
//     failed turn may have left a half-written session behind), nil when the
//     thread is empty.
//
// Three branches:
//
//  1. resume — the thread's QA session is alive and still matches the stage
//     session it was forked from. Continue it.
//  2. fork   — the stage session exists but the thread's QA session belongs to
//     a DIFFERENT stage session: the user re-ran the stage (re-design /
//     re-develop), so the old thread context is stale. Fork a NEW QA session
//     from the current stage session. The old turns are kept for display.
//  3. fresh  — there is neither a stage session to fork nor a QA session to
//     continue. Start over with the context pasted into the prompt.
//
// The "S == ''" condition in branch 1 covers "the user compressed the stage
// session away": the QA session itself is still alive, so the thread continues
// instead of needlessly re-opening a new segment.
//
// The "latest.source_session_id == ''" condition covers a DEGRADED previous
// turn (the stage session was gone, so the turn ran fresh with the context
// pasted into the prompt): its session holds that context plus the earlier
// Q&A, so continuing it preserves the thread. Without this condition the very
// next question would re-fork from the stage session and silently drop the
// Q&A history the user just built up.
func resolveQASession(stageSID string, latest *model.RequirementQATurn) qaSessionPlan {
	stageSID = strings.TrimSpace(stageSID)
	qaSID := ""
	sourceSnapshot := ""
	if latest != nil {
		qaSID = strings.TrimSpace(latest.SessionID)
		sourceSnapshot = strings.TrimSpace(latest.SourceSessionID)
	}

	if qaSID != "" && (sourceSnapshot == "" || stageSID == sourceSnapshot || stageSID == "") {
		return qaSessionPlan{
			Mode:         qaModeResume,
			ResumeSID:    qaSID,
			// Carry the fork source forward. A resumed turn did NOT fork
			// again, so the thread's source session is still the one the
			// first turn forked from — dropping it would make the next
			// "did the stage get re-run?" comparison look like a mismatch
			// and trigger a needless re-fork.
			SourceSID:    sourceSnapshot,
			NewSessionID: qaSID,
			Resumed:      true,
		}
	}
	if stageSID != "" {
		return qaSessionPlan{
			Mode:         qaModeFork,
			SourceSID:    stageSID,
			NewSessionID: util.NewUUID(),
		}
	}
	return qaSessionPlan{
		Mode:         qaModeFresh,
		NewSessionID: util.NewUUID(),
	}
}

// AskStageQAReq is the body of POST /api/wizard/qa-chat.
type AskStageQAReq struct {
	RequirementID string `json:"requirement_id"`
	// Stage is "design" or "coding".
	Stage string `json:"stage"`
	// Question is the user's message.
	Question string `json:"question"`
	// Model / ClaudeConfigID are the optional per-request binding (the
	// frontend's ModelSelect). Empty = the stage role's default.
	Model          string `json:"model"`
	ClaudeConfigID string `json:"claude_config_id"`
}

// AskStageQA records a question, dispatches the turn and returns the JobStore
// id to stream from.
//
// POST /api/wizard/qa-chat
func (h *WizardHandler) AskStageQA(w http.ResponseWriter, r *http.Request) {
	var req AskStageQAReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	req.RequirementID = strings.TrimSpace(req.RequirementID)
	req.Stage = strings.TrimSpace(req.Stage)
	req.Question = strings.TrimSpace(req.Question)
	if req.RequirementID == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "缺少需求 ID")
		return
	}
	if req.Stage != model.QAStageDesign && req.Stage != model.QAStageCoding {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "stage 只能是 design 或 coding")
		return
	}
	if req.Question == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "问题不能为空")
		return
	}
	if h.qaSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "答疑服务未启用")
		return
	}

	reqRow, err := h.reqSvc.Get(req.RequirementID)
	if err != nil {
		writeError(w, http.StatusNotFound, "REQUIREMENT_NOT_FOUND", "需求不存在")
		return
	}

	// The stage must have produced something to ask about. Without this the
	// model would improvise an answer about a stage that never ran.
	stageSID, roleKey := docStageSession(reqRow, req.Stage)
	if !qaStageHasArtifact(reqRow, req.Stage, stageSID) {
		writeError(w, http.StatusBadRequest, "NO_ARTIFACT", "该阶段还没有产物，无法答疑")
		return
	}

	// Concurrency gate: one question at a time per stage. A turn that is
	// still running blocks the next question; a turn left `running` by a
	// backend restart (or a JobStore eviction) is repaired to `error` and
	// the question goes through.
	latest, err := h.qaSvc.LatestTurn(req.RequirementID, req.Stage)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	if latest != nil && latest.Status == model.QAStatusRunning {
		if latest.JobID != "" && h.jobs.Live(latest.JobID) {
			writeError(w, http.StatusConflict, "QA_RUNNING", "上一轮答疑还在进行中，请等待结束")
			return
		}
		if err := h.qaSvc.MarkOrphaned(latest.ID, "后端已重启，请重新提问"); err != nil {
			log.Printf("[qa-chat] failed to repair orphaned turn %s: %v", latest.ID, err)
		}
	}

	// Session resolution: resume the thread's QA session, fork a new one off
	// the stage session, or start fresh.
	answered, err := h.qaSvc.LatestAnsweredTurn(req.RequirementID, req.Stage)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	plan := resolveQASession(stageSID, answered)

	// The pre-minted session id is persisted WITH the turn, before the run
	// starts: a crash between "spawned" and "answered" then still leaves a
	// row that names the session the (half-written) transcript belongs to.
	job := h.jobs.Create(req.RequirementID)
	job.SetType("qa_chat")

	turn, err := h.qaSvc.Create(&model.RequirementQATurn{
		RequirementID:   req.RequirementID,
		Stage:           req.Stage,
		Question:        req.Question,
		JobID:           job.ID,
		SessionID:       plan.NewSessionID,
		SourceSessionID: plan.SourceSID,
		CreatedBy:       currentUserID(r),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CREATE_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID, "turn_id": turn.ID})

	go h.runStageQA(job, turn, reqRow, req, plan, roleKey)
}

// qaStageHasArtifact reports whether the stage has produced anything worth
// asking about. The design stage has a plan document; the coding stage has a
// compressed context summary (there is no "coding document" to paste).
func qaStageHasArtifact(reqRow *model.Requirement, stage, stageSID string) bool {
	if strings.TrimSpace(stageSID) != "" {
		return true
	}
	if reqRow == nil {
		return false
	}
	if stage == model.QAStageDesign {
		return strings.TrimSpace(reqRow.DesignDocs) != ""
	}
	return strings.TrimSpace(reqRow.CodingContextSummary) != ""
}

// ListStageQA returns one stage's Q&A thread, oldest first.
// GET /api/requirements/{id}/qa-chat/{stage}
func (h *WizardHandler) ListStageQA(w http.ResponseWriter, r *http.Request) {
	if h.qaSvc == nil {
		writeJSON(w, http.StatusOK, []model.RequirementQATurn{})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	stage := strings.TrimSpace(r.PathValue("stage"))
	if stage != model.QAStageDesign && stage != model.QAStageCoding {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "stage 只能是 design 或 coding")
		return
	}
	// Repair a turn left `running` by a backend restart so the thread does
	// not show an eternal spinner.
	h.repairOrphanedTurns(id, stage)
	rows, err := h.qaSvc.ListByStage(id, stage)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// ResetStageQA clears one stage's whole Q&A thread.
// DELETE /api/requirements/{id}/qa-chat/{stage}
func (h *WizardHandler) ResetStageQA(w http.ResponseWriter, r *http.Request) {
	if h.qaSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "答疑服务未启用")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	stage := strings.TrimSpace(r.PathValue("stage"))
	if stage != model.QAStageDesign && stage != model.QAStageCoding {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "stage 只能是 design 或 coding")
		return
	}
	if err := h.qaSvc.ResetThread(id, stage); err != nil {
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteStageQATurn removes one turn from the thread.
// DELETE /api/requirements/{id}/qa-chat/{stage}/turns/{tid}
func (h *WizardHandler) DeleteStageQATurn(w http.ResponseWriter, r *http.Request) {
	if h.qaSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "答疑服务未启用")
		return
	}
	tid := strings.TrimSpace(r.PathValue("tid"))
	if tid == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "缺少轮次 ID")
		return
	}
	if err := h.qaSvc.DeleteTurn(tid); err != nil {
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// repairOrphanedTurns flips `running` turns whose job is gone back to
// `error`. Called by the read entry points so a thread never shows an
// eternal spinner after a backend restart.
func (h *WizardHandler) repairOrphanedTurns(requirementID, stage string) {
	if h.qaSvc == nil {
		return
	}
	rows, err := h.qaSvc.ListByStage(requirementID, stage)
	if err != nil {
		return
	}
	for i := range rows {
		row := rows[i]
		if row.Status != model.QAStatusRunning {
			continue
		}
		if row.JobID != "" && h.jobs.Live(row.JobID) {
			continue
		}
		if err := h.qaSvc.MarkOrphaned(row.ID, "后端已重启，请重新提问"); err != nil {
			log.Printf("[qa-chat] failed to repair orphaned turn %s: %v", row.ID, err)
		}
	}
}

// runStageQA drives one Q&A turn end to end. Every exit path goes through
// finishQATurn, so a crashed run never leaves a row stuck in `running`.
func (h *WizardHandler) runStageQA(job *store.Job, turn *model.RequirementQATurn, reqRow *model.Requirement, req AskStageQAReq, plan qaSessionPlan, roleKey string) {
	ctx, cancel := context.WithTimeout(context.Background(), qaHardTimeout)
	defer cancel()

	systemPrompt, modelName, claudeConfigID := h.roleConfig(roleKey)
	if req.Model != "" {
		modelName = req.Model
	}
	claudeConfigID = h.resolveConfigIDForRun(req.ClaudeConfigID, modelName, claudeConfigID)
	job.SetModel(modelName)

	// The attachment manifest lets the answer cite the same files the
	// architect / developer saw. attachManifestFor is a no-op when there is
	// nothing attached.
	if m := h.attachManifestFor(req.RequirementID, ""); m != "" {
		systemPrompt = systemPrompt + "\n\n" + m
	}

	projectPath := ""
	defaultBranch := ""
	if proj, perr := h.projectSvc.Get(reqRow.ProjectID); perr == nil {
		projectPath = proj.LocalPath
		defaultBranch = proj.DefaultBranch
	}

	// The fork guard, evaluated BEFORE resolveWorkDir.
	//
	// It asks "was the SOURCE session created in an isolated worktree?" —
	// a property of the requirement row as it was fetched. It must run
	// before resolveWorkDir for two reasons:
	//
	//  1. resolveWorkDir may clear + rebuild a drifted worktree_path, after
	//     which the in-memory copy no longer reflects what the source
	//     session was made in.
	//  2. The guard's second argument is the PROJECT path (where an
	//     in-place session would have been created) — not the resolved
	//     worktree, which is a git repo by construction and would make the
	//     guard fire on every single run.
	//
	// A failure is a DOWNGRADE, not an error: QA is read-only, so a
	// shallower answer costs far less than a hard error.
	if plan.Mode == qaModeFork {
		if gerr := h.requireAnchoredFork(reqRow, projectPath); gerr != nil {
			log.Printf("[qa-chat] fork guard: %v — degrading to a fresh answer", gerr)
			job.Append(store.LogLine{Type: "phase", Content: "⚠️ " + gerr.Error() + "，改用「方案文档 + 仓库代码」作答"})
			plan = qaSessionPlan{Mode: qaModeFresh, NewSessionID: util.NewUUID()}
		}
	}

	// Anchor to the requirement's worktree so the forked conversation stays
	// rooted in the same directory as the stage that produced it — the
	// session jsonl lives under the slug of that directory, so a different
	// cwd would not find the source session (and the fork would degrade).
	workDir, err := h.resolveWorkDir(ctx, reqRow, projectPath, defaultBranch, false)
	if err != nil {
		h.finishQATurn(job, turn, modelName, "", err.Error(), false, plan.NewSessionID, plan.SourceSID)
		return
	}

	usage := h.usageCtxForConfig("qa_chat", req.RequirementID, reqRow.ProjectID, job.ID, modelName,
		fmt.Sprintf(`{"stage":%q,"question":%q}`, req.Stage, req.Question), qaSummaryText(req.Question), claudeConfigID)

	// The history replay (used by the fresh/degraded prompt) is read BEFORE
	// the run so the prompt carries the earlier Q&A. The turn being answered
	// is still `running`, so the status filter already excludes it.
	history := h.qaHistory(req.RequirementID, req.Stage, turn.ID)

	res := h.runQATurn(ctx, job, workDir, systemPrompt, modelName, claudeConfigID, reqRow, req, plan, history, usage)
	h.finishQATurn(job, turn, modelName, res.answer, res.errMsg, res.resumed, res.sessionID, res.sourceSessionID)
}

// qaTurnResult is the outcome of one turn's run.
type qaTurnResult struct {
	// answer is the model's reply (empty when the run failed).
	answer string
	// errMsg is the failure text (empty on success).
	errMsg string
	// resumed is true when the answer came from a session that held the
	// forked context.
	resumed bool
	// sessionID is the session the run actually used. The pre-minted id is
	// what we ASKED for; the CLI reports the id it really opened (they
	// differ when the CLI refuses our --session-id). The CLI wins, because
	// the next turn has to resume the session that holds this answer.
	sessionID string
	// sourceSessionID is the stage session the answer was forked from
	// ("" for a degraded / fresh run).
	sourceSessionID string
}

// runQATurn executes one turn.
//
// The three run modes map onto the CLI flags as follows:
//
//	resume → --resume <qa session>
//	fork   → --resume <stage session> --fork-session --session-id <new>
//	fresh  → --session-id <new>          (no --resume)
//
// A stale --resume (the source conversation is gone) degrades to a fresh run
// with the context pasted into the prompt — the answer gets shallower, but the
// user still gets one. Same for a failed fork guard: QA is read-only, so a
// shallower answer costs far less than a hard error.
func (h *WizardHandler) runQATurn(
	ctx context.Context,
	job *store.Job,
	workDir, systemPrompt, modelName, claudeConfigID string,
	reqRow *model.Requirement,
	req AskStageQAReq,
	plan qaSessionPlan,
	history []prompt.QATurn,
	usage *usageCtx,
) qaTurnResult {
	sink := jobSink{job}

	// The fresh form carries the requirement context; the resumed form does
	// not (the session already holds it).
	freshInput := qaFreshPromptInput(reqRow, req.Stage)

	run := func(p qaSessionPlan) claudeStreamOutcome {
		in := freshInput
		in.Stage = req.Stage
		in.Question = req.Question
		in.Resumed = p.Resumed
		if !in.Resumed {
			in.History = history
		}
		promptText := prompt.BuildStageQAPrompt(in)

		opts := llm.StreamOpts{
			Prompt:          promptText,
			WorkDir:         workDir,
			SystemPrompt:    systemPrompt,
			Model:           cliModelArg(modelName),
			ClaudeConfigID:  claudeConfigID,
			DisallowedTools: qaChatDisallowedTools,
		}
		// Authorized directories (--add-dir grants) — the Q&A session answers
		// "why is it designed this way", so it needs the same read access to
		// the granted reference material the other stages had.
		if reqRow != nil {
			opts.AddDirs = h.grantDirsFor(reqRow.ID, "")
		}
		switch p.Mode {
		case qaModeResume:
			opts.Resume = true
			opts.SessionID = p.ResumeSID
		case qaModeFork:
			opts.Resume = true
			opts.SessionID = p.SourceSID
			opts.Fork = true
			opts.ForkSessionID = p.NewSessionID
		default: // qaModeFresh
			opts.SessionID = p.NewSessionID
		}
		cmd := h.llm.StreamCmd(ctx, opts)
		return runClaudeStream(sink, cmd, "qa-chat", usage, nil, nil, 0, qaStallTimeout)
	}

	out := run(plan)
	resumed := plan.Resumed
	sourceSID := plan.SourceSID
	sessionID := plan.NewSessionID

	if out.staleSession && plan.Mode != qaModeFresh {
		// The source conversation is gone (the user re-ran the stage and the
		// CLI pruned the jsonl, or the stage ran on an Agent Server whose
		// transcript never came back down). Degrade to a fresh session with
		// the context pasted into the prompt.
		log.Printf("[qa-chat] stale session %s — falling back to a fresh answer", plan.NewSessionID)
		job.Append(store.LogLine{Type: "phase", Content: "⚠️ 原会话已失效，改用「方案文档 + 仓库代码」重新作答"})
		freshPlan := qaSessionPlan{Mode: qaModeFresh, NewSessionID: util.NewUUID()}
		out = run(freshPlan)
		resumed = false
		sourceSID = ""
		sessionID = freshPlan.NewSessionID
	}

	// The CLI reports the session id it actually opened. It wins over the
	// pre-minted one: the next turn has to resume the session that holds
	// this answer.
	if out.sessionID != "" {
		sessionID = out.sessionID
	}

	if out.finalResult == "" {
		if out.errMsg != "" {
			return qaTurnResult{errMsg: out.errMsg, resumed: resumed, sessionID: sessionID, sourceSessionID: sourceSID}
		}
		return qaTurnResult{errMsg: "Claude 未返回结果，请重试", resumed: resumed, sessionID: sessionID, sourceSessionID: sourceSID}
	}
	return qaTurnResult{
		answer:          out.finalResult,
		resumed:         resumed,
		sessionID:       sessionID,
		sourceSessionID: sourceSID,
	}
}

// qaFreshPromptInput assembles the context the fresh (degraded) prompt
// carries: the requirement identity plus the stage's own artifact.
func qaFreshPromptInput(reqRow *model.Requirement, stage string) prompt.StageQAPromptInput {
	in := prompt.StageQAPromptInput{Stage: stage}
	if reqRow == nil {
		return in
	}
	in.Title = reqRow.Title
	in.Description = reqRow.Description
	// The plan document is the artifact BOTH stages are about: the design
	// stage produced it, the coding stage implemented it. It goes into the
	// prompt for either stage.
	in.DesignDocs = reqRow.DesignDocs
	if stage == model.QAStageCoding {
		// The coding stage's own compressed context (when the user
		// compressed it) is extra material on top of the plan.
		in.CodingSummary = reqRow.CodingContextSummary
	}
	return in
}

// qaSummaryText bounds the question text stored in token_usage.meta's
// summary field. Rune-based: a byte cut would split a Chinese character and
// store mojibake in the usage table.
func qaSummaryText(s string) string {
	const max = 200
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// qaHistory returns the thread's answered turns (excluding the one being
// answered), oldest first. Errored turns are skipped — replaying a failure
// message as if it were an answer would only confuse the model.
func (h *WizardHandler) qaHistory(requirementID, stage, excludeID string) []prompt.QATurn {
	if h.qaSvc == nil {
		return nil
	}
	rows, err := h.qaSvc.ListByStage(requirementID, stage)
	if err != nil {
		return nil
	}
	out := make([]prompt.QATurn, 0, len(rows))
	for i := range rows {
		if rows[i].ID == excludeID || rows[i].Status != model.QAStatusDone {
			continue
		}
		// The answer goes in whole: BuildStageQAPrompt truncates it by RUNE
		// (qaAnswerMaxRunes). Byte-truncating here would cut a multi-byte
		// Chinese character in half and emit invalid UTF-8 into the prompt.
		out = append(out, prompt.QATurn{
			Question: rows[i].Question,
			Answer:   rows[i].Answer,
		})
	}
	return out
}

// finishQATurn is the single terminal path: persist the turn and close the
// job. An error is stored as the answer text so the thread shows what went
// wrong instead of an empty bubble.
//
// The answer goes through service.SanitizeMermaidBlocks for the same reason
// the review report does: the Q&A prompt invites a Mermaid diagram when the
// question is about a call chain, and a block the Mermaid 11 lexer rejects
// would render as a red error card inside the thread.
//
// sessionID / sourceSessionID are the terminal values of the turn's session
// bookkeeping. The session id was pre-minted when the row was created, so it
// only has to be corrected here when the CLI reported a different one.
func (h *WizardHandler) finishQATurn(job *store.Job, turn *model.RequirementQATurn, modelName, answer, errMsg string, resumed bool, sessionID, sourceSessionID string) {
	status := model.QAStatusDone
	body := service.SanitizeMermaidBlocks(answer)
	if errMsg != "" {
		status = model.QAStatusError
		body = errMsg
		job.Append(store.LogLine{Type: "error", Content: errMsg})
	}
	if h.qaSvc != nil && turn != nil {
		if sessionID == "" {
			sessionID = turn.SessionID
		}
		if err := h.qaSvc.MarkFinished(turn.ID, status, body, modelName, sessionID, sourceSessionID, resumed); err != nil {
			log.Printf("[qa-chat] persist turn %s: %v", turn.ID, err)
		}
	}
	if status == model.QAStatusError {
		job.Finish(1, store.JobError)
		return
	}
	job.Finish(0, store.JobDone)
}
