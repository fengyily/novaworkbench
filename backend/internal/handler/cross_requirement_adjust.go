package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// ── 跨项目需求 · 方案调整对话 ──────────────────────────────────────────────
//
// 整体方案跑完之后，用户常常发现方案里留着「待决策事项」——接口超时取多少、
// 幂等键用哪种策略、两个仓库的字段谁先谁后。现有出口只有两个：盲跑一次
// 「重新生成」（说不清要改什么），或者手工编辑 Markdown（没有讨论过程）。
//
// 本文件是第三条出口：一条挂在这个跨项目需求下的**调整对话**线程。
//
//	提问 / 拍板 → AI 应答（并复述「已确认结论」）→ 讨论记录落库
//	→ 用户点「按讨论结论重新补全方案」→ 方案作业重跑，提示词里带上这段
//	  讨论（见 prompt.BuildCrossDiscussionBlock）→ 产出修订后的完整方案
//
// 执行形态照抄单仓库的「阶段答疑」（wizard_qa.go）：POST 立即返回 job_id，
// 前端订阅 SSE；答案由**后端**落库（用户关掉浏览器答案不能丢）。
//
// ── 会话策略 ───────────────────────────────────────────────────────────────
//
// 首轮 **fork 整体方案的会话**（--resume <design_session_id> --fork-session
// --session-id <新 id>）：模型保留自己写方案时的探索记忆，回答「为什么这么定」
// 时不必重读 N 个仓库。后续轮 **resume** 线程自己的会话。两者都失效时降级为
// **fresh**：把当前方案原文 + 参与项目 + 约定 + 附件 + 待决策事项 + 此前问答
// 贴进提示词。
//
// 决策树与单项目的阶段答疑**共用一棵**（qa_session_plan.go 的
// resolveQASessionPlan）—— 两棵树 = 两套 bug。
//
// **fork 的 cwd 必须是整体方案作业的那个目录**：claude CLI 的会话转录按 cwd 的
// slug 分目录存放（~/.claude/projects/<slug-of-cwd>/<sid>.jsonl），换 cwd 就
// 找不到源会话 —— 仓库自己的结论见 wizard_qa.go 的锚定注释。整体方案作业跑在
// ~/.novaworkbench/xreq/<id>/design，调整对话也就跑在那里 —— 两条线程的转录
// 落在同一个目录、不同的 session id，互不冲突。
//
// ── 没有 fork 守卫 ────────────────────────────────────────────────────────
//
// 单项目的阶段答疑有一道 fork 守卫（requireAnchoredFork）：源会话若是在共享
// 检出里跑的，fork 之后模型会拿着共享检出的绝对路径去改文件。跨项目作业**不能
// 改代码**（Write / Edit / NotebookEdit 在协议层被禁用），而且它的"隔离"本来就
// 体现在每个仓库各有一份 detached worktree 上 —— 不存在改错检出目录的风险。
// Bash 虽已放开（只读探查用），但破坏性命令由 crossJobSink 的守卫挡住。所以
// 这道守卫在这里不适用，照抄一段只会制造一条永远不触发的分支。

// crossRunAdjust — 调整对话在作业登记处里的闸门键。
//
// 它只是**进程内互斥锁**，不是"什么在跑"的真相源：真相源是
// cross_requirement_qa_turns 里的 status 行（落库、跨重启）。锁存在的唯一
// 理由是挡住「两个 POST 同时通过检查、于是建了两轮」的竞态 —— 单项目的阶段
// 答疑就有这个竞态（它连锁都没有），这里顺手补上。
const crossRunAdjust = "adjust"

// 调整对话的失败分类。词表与 job_logs.error_kind / wizard_qa 的 qaErrorKind*
// 对齐，前端据此区分「模型压根没被问到」与「模型答了但丢包」。
const (
	adjustErrorKindEmptyResult = "empty-result"
	adjustErrorKindEmptyAfter  = "empty-result-after-fallback"
	adjustErrorKindStale       = "stale-session"
	adjustErrorKindCliCrash    = "cli-crash"
	adjustErrorKindTimeout     = "timeout"
)

// AskAdjustReq is the body of POST /api/cross-requirements/{id}/design/adjust.
type AskAdjustReq struct {
	// Question is the user's message (a question, or a decision).
	Question string `json:"question"`
	// Model / ClaudeConfigID are the optional per-request binding. Empty =
	// the architect persona's default.
	Model          string `json:"model"`
	ClaudeConfigID string `json:"claude_config_id"`
	// Effort — the reasoning-effort tier the user picked for this turn.
	Effort string `json:"effort"`
}

// AskAdjust records a question, dispatches the turn and returns the JobStore
// id to stream from.
//
// POST /api/cross-requirements/{id}/design/adjust
func (h *CrossRequirementHandler) AskAdjust(w http.ResponseWriter, r *http.Request) {
	xreqID := strings.TrimSpace(r.PathValue("id"))
	if h.svc == nil || h.jobs == nil || h.qaSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "方案调整服务未启用")
		return
	}
	var req AskAdjustReq
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	req.Question = strings.TrimSpace(req.Question)
	if req.Question == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "问题不能为空")
		return
	}

	xreq, err := h.svc.Get(xreqID)
	if err != nil {
		writeError(w, http.StatusNotFound, "CROSS_REQUIREMENT_NOT_FOUND", "跨项目需求不存在")
		return
	}
	if rejectIfTrashed(w, xreq) {
		return
	}
	if xreq.Status == model.XReqStatusArchived {
		writeError(w, http.StatusBadRequest, "XREQ_ARCHIVED", "已归档的跨项目需求不能调整方案")
		return
	}

	// There must be something to adjust. Without a plan the model would
	// improvise an answer about a design that never ran.
	design := service.DesignDocsToMarkdown(xreq.DesignDocs)
	if strings.TrimSpace(design) == "" {
		writeError(w, http.StatusBadRequest, "XREQ_NO_DESIGN", "还没有整体方案，先生成方案再来调整")
		return
	}

	// The overall-design job and the adjustment thread are mutually
	// exclusive: both write cross_requirements.design_session_id and both
	// run in the same working directory.
	if existing, running := h.runs.peek(crossRunDesign, xreqID, ""); running {
		writeError(w, http.StatusConflict, "XREQ_IN_PROGRESS",
			fmt.Sprintf("整体方案任务在运行（job %s），请等待结束", existing))
		return
	}
	if existing, running := h.runs.peek(crossRunAdjust, xreqID, ""); running {
		writeError(w, http.StatusConflict, "XREQ_ADJUST_RUNNING",
			fmt.Sprintf("上一轮方案调整还在进行中（job %s），请等待结束", existing))
		return
	}

	// Concurrency gate over the persisted thread: a turn that is still
	// running blocks the next question; a turn left `running` by a backend
	// restart (or a JobStore eviction) is repaired to `error` and the
	// question goes through.
	latest, err := h.qaSvc.LatestTurn(xreqID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	if latest != nil && latest.Status == model.QAStatusRunning {
		if latest.JobID != "" && h.jobs.Live(latest.JobID) {
			writeError(w, http.StatusConflict, "XREQ_ADJUST_RUNNING", "上一轮方案调整还在进行中，请等待结束")
			return
		}
		if err := h.qaSvc.MarkOrphaned(latest.ID, "后端已重启，请重新提问"); err != nil {
			log.Printf("[xreq-adjust] failed to repair orphaned turn %s: %v", latest.ID, err)
		}
	}

	// Session resolution: resume the thread's session, fork a new one off the
	// overall-design session, or start fresh.
	answered, err := h.qaSvc.LatestAnsweredTurn(xreqID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	plan := resolveCrossAdjustSession(xreq.DesignSessionID, answered)

	job := h.jobs.Create(xreqID)
	job.SetType("cross_design_adjust")

	if existing, ok := h.runs.acquire(crossRunAdjust, xreqID, "", job.ID); !ok {
		job.Finish(1, store.JobError)
		writeError(w, http.StatusConflict, "XREQ_ADJUST_RUNNING",
			fmt.Sprintf("上一轮方案调整还在进行中（job %s），请等待结束", existing))
		return
	}

	// The pre-minted session id is persisted WITH the turn, before the run
	// starts: a crash between "spawned" and "answered" then still leaves a
	// row that names the session the (half-written) transcript belongs to.
	turn, err := h.qaSvc.Create(&model.CrossRequirementQATurn{
		CrossRequirementID: xreqID,
		Question:           req.Question,
		JobID:              job.ID,
		SessionID:          plan.NewSessionID,
		SourceSessionID:    plan.SourceSID,
		CreatedBy:          currentUserID(r),
	})
	if err != nil {
		h.runs.release(crossRunAdjust, xreqID, "")
		job.Finish(1, store.JobError)
		writeError(w, http.StatusInternalServerError, "CREATE_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID, "turn_id": turn.ID})

	go h.runAdjust(job, xreq, turn, req, plan)
}

// AdjustState returns the adjustment thread plus whatever is running.
//
// GET /api/cross-requirements/{id}/design/adjust
//
// 读路径对回收站里的行也开放（与 GET /api/cross-requirements/{id} 一致）——
// 回收站里的行是只读的，但读它不违规。
func (h *CrossRequirementHandler) AdjustState(w http.ResponseWriter, r *http.Request) {
	xreqID := strings.TrimSpace(r.PathValue("id"))
	if h.qaSvc == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "none", "job_id": "", "running": false,
			"error_message": "", "turns": []model.CrossRequirementQATurn{},
		})
		return
	}
	h.repairOrphanedAdjustTurns(xreqID)
	turns, err := h.qaSvc.List(xreqID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	payload := map[string]any{
		"status":        "none",
		"job_id":        "",
		"running":       false,
		"error_message": "",
		"turns":         turns,
	}
	for i := len(turns) - 1; i >= 0; i-- {
		t := turns[i]
		if t.Status == model.QAStatusRunning {
			payload["status"] = "running"
			payload["job_id"] = t.JobID
			payload["running"] = true
			break
		}
		payload["status"] = "done"
		payload["job_id"] = t.JobID
		if t.Status == model.QAStatusError {
			payload["status"] = "error"
			payload["error_message"] = t.Answer
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

// ResetAdjust clears the whole adjustment thread.
//
// DELETE /api/cross-requirements/{id}/design/adjust
//
// 除了清掉对话，它还有第二个后果：整体方案作业读的就是这条线程，所以清空之后
// 下一次「按讨论结论重新补全方案」不再带这些结论。
func (h *CrossRequirementHandler) ResetAdjust(w http.ResponseWriter, r *http.Request) {
	xreqID := strings.TrimSpace(r.PathValue("id"))
	if h.qaSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "方案调整服务未启用")
		return
	}
	if _, err := h.svc.Get(xreqID); err != nil {
		writeError(w, http.StatusNotFound, "CROSS_REQUIREMENT_NOT_FOUND", "跨项目需求不存在")
		return
	}
	if err := h.qaSvc.ResetThread(xreqID); err != nil {
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteAdjustTurn removes one turn from the thread.
//
// DELETE /api/cross-requirements/{id}/design/adjust/turns/{tid}
func (h *CrossRequirementHandler) DeleteAdjustTurn(w http.ResponseWriter, r *http.Request) {
	tid := strings.TrimSpace(r.PathValue("tid"))
	if h.qaSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "方案调整服务未启用")
		return
	}
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

// StreamAdjustJob streams one adjustment turn's run.
//
// GET /api/cross-requirements/{id}/design/adjust/{job_id}/stream
func (h *CrossRequirementHandler) StreamAdjustJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job, ok := h.jobs.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found")
		return
	}
	if !crossJobBelongsTo(job, r.PathValue("id")) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found")
		return
	}
	streamJobSSE(w, r, job, func(status store.JobStatus, exitCode int) []byte {
		// The answer is NOT taken from here: the terminal frame only says
		// "the turn finished". The frontend re-reads the thread, which is
		// the authoritative copy (same rule as the stage Q&A panel).
		result := map[string]any{}
		errMsg := ""
		if turn := h.adjustTurnByJob(job); turn != nil {
			result["turn_id"] = turn.ID
			result["answer"] = turn.Answer
			result["status"] = turn.Status
			if turn.ErrorKind != "" {
				result["error_kind"] = turn.ErrorKind
			}
			if turn.Status == model.QAStatusError {
				errMsg = turn.Answer
			}
		}
		return crossDoneFrame(job, status, exitCode, errMsg, result)
	})
}

// adjustTurnByJob finds the thread turn that belongs to a job.
func (h *CrossRequirementHandler) adjustTurnByJob(job *store.Job) *model.CrossRequirementQATurn {
	if h.qaSvc == nil || job == nil {
		return nil
	}
	turn, err := h.qaSvc.GetByJobID(job.ID)
	if err != nil {
		return nil
	}
	return turn
}

// adjustTurnRunning reports whether an adjustment turn is currently running.
//
// 真相源是**落库的轮次行**（status='running'）+ 作业是否还活着，而不是进程内
// 的登记处 —— 后者进程一死就没了。
func (h *CrossRequirementHandler) adjustTurnRunning(xreqID string) bool {
	if h.qaSvc == nil {
		return false
	}
	latest, err := h.qaSvc.LatestTurn(xreqID)
	if err != nil || latest == nil {
		return false
	}
	if latest.Status != model.QAStatusRunning {
		return false
	}
	if latest.JobID == "" {
		return false
	}
	return h.jobs.Live(latest.JobID)
}

// repairOrphanedAdjustTurns flips `running` turns whose job is gone back to
// `error`. Called by the read entry point so a thread never shows an eternal
// spinner after a backend restart.
func (h *CrossRequirementHandler) repairOrphanedAdjustTurns(xreqID string) {
	if h.qaSvc == nil {
		return
	}
	rows, err := h.qaSvc.List(xreqID)
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
			log.Printf("[xreq-adjust] failed to repair orphaned turn %s: %v", row.ID, err)
		}
	}
}

// ── 会话决策 ───────────────────────────────────────────────────────────────

// resolveCrossAdjustSession decides how a new adjustment turn gets its
// context.
//
// It is the cross-requirement translation of the shared decision tree
// (qa_session_plan.go): the source session is the cross requirement's
// design_session_id instead of a requirement's stage session.
//
//  1. resume — the thread's own session is alive and still matches the
//     design session it forked from. Continue it.
//  2. fork   — a design session exists but the thread's session belongs to a
//     DIFFERENT design session: the plan was regenerated since, so the old
//     thread context describes a plan that no longer exists. Fork a NEW
//     session from the current design session. The old turns are kept.
//  3. fresh  — there is neither a design session to fork nor a thread session
//     to continue. Start over with the context pasted into the prompt.
//
// The "designSID == ''" condition in branch 1 covers "the design has no
// session id"（hand-edited, or a design produced before this feature
// existed）: the thread session is still alive, so the thread continues
// instead of needlessly re-opening a new segment.
func resolveCrossAdjustSession(designSID string, latest *model.CrossRequirementQATurn) qaSessionPlan {
	src := qaSessionSource{StageSID: designSID}
	if latest != nil {
		src.ThreadSID = latest.SessionID
		src.ThreadSourceSID = latest.SourceSessionID
	}
	return resolveQASessionPlan(src)
}

// ── 运行 ───────────────────────────────────────────────────────────────────

// runAdjust drives one adjustment turn end to end. Every exit path goes
// through finishAdjustTurn, so a crashed run never leaves a row stuck in
// `running`.
func (h *CrossRequirementHandler) runAdjust(job *store.Job, xreq *model.CrossRequirement,
	turn *model.CrossRequirementQATurn, req AskAdjustReq, plan qaSessionPlan) {
	defer h.runs.release(crossRunAdjust, xreq.ID, "")
	defer func() {
		if r := recover(); r != nil {
			h.finishAdjustTurn(job, turn, "", "方案调整过程异常终止", adjustErrorKindCliCrash, plan.NewSessionID, plan.SourceSID, false)
		}
	}()

	systemPrompt, modelName, configID := h.crossRoleConfig()
	if strings.TrimSpace(req.Model) != "" {
		modelName = req.Model
	}
	if strings.TrimSpace(req.ClaudeConfigID) != "" {
		configID = req.ClaudeConfigID
	}
	job.SetModel(modelName)

	// The adjustment reads the SAME tree the plan was written against: the
	// recorded baseline ledger, not a fresh fetch. Re-fetching would move the
	// ground under a conversation about a specific plan — and would cost one
	// git fetch per participating repository, per question.
	ws, wsErr := h.crossWorkspaceFromBases(xreq.ID)
	if wsErr != nil {
		h.finishAdjustTurn(job, turn, modelName, "读取参与项目失败: "+wsErr.Error(),
			adjustErrorKindCliCrash, plan.NewSessionID, plan.SourceSID, false)
		return
	}
	projects, err := h.crossProjectInputs(xreq.ID, ws)
	if err != nil {
		h.finishAdjustTurn(job, turn, modelName, "读取参与项目失败: "+err.Error(),
			adjustErrorKindCliCrash, plan.NewSessionID, plan.SourceSID, false)
		return
	}

	design := service.DesignDocsToMarkdown(xreq.DesignDocs)
	decisions := prompt.ExtractCrossPendingDecisions(design)

	// The history replay is read BEFORE the run so the prompt carries the
	// earlier exchanges. The turn being answered is still `running`, so it
	// never shows up here.
	history := h.adjustHistory(xreq.ID, turn.ID)

	promptText := h.crossAdjustPrompt(xreq, ws, projects, design, decisions, history, req.Question, plan.Resumed)

	// The fork has to run in the SAME directory the overall-design job used:
	// the CLI stores session transcripts per working directory, so a fork
	// from another cwd cannot find the source session.
	workDir, err := crossWorkspaceDir(xreq.ID, crossRunDesign)
	if err != nil {
		h.finishAdjustTurn(job, turn, modelName, "创建工作目录失败: "+err.Error(),
			adjustErrorKindCliCrash, plan.NewSessionID, plan.SourceSID, false)
		return
	}

	opts := h.crossStreamOpts(crossJobSpec{
		Prompt:         promptText,
		WorkDir:        workDir,
		SystemPrompt:   systemPrompt,
		Model:          modelName,
		Effort:         resolveEffort(h.llm, req.Effort, llm.StageDesign),
		ClaudeConfigID: configID,
		Workspace:      ws,
		SessionID:      plan.NewSessionID,
		Resume:         plan.Mode != qaModeFresh,
		Fork:           plan.Mode == qaModeFork,
		ForkSessionID:  plan.NewSessionID,
	})
	if plan.Mode == qaModeFork {
		// --resume carries the SOURCE session; --session-id names the new
		// forked one. The gateway takes them from two different fields.
		opts.SessionID = plan.SourceSID
		opts.ForkSessionID = plan.NewSessionID
	}

	job.Append(store.LogLine{Type: "phase", Content: "🧭 正在就整体方案与你讨论…"})

	attemptStart := time.Now()
	out := runClaudeStream(crossJobSink{jobSink: jobSink{job}, workDir: workDir, protected: opts.AddDirs},
		h.llm.StreamCmd(context.Background(), opts), "cross-design-adjust",
		h.crossUsageCtx("cross_design_adjust", xreq.ID, job.ID, modelName, configID), nil, nil, 0,
		withStallTimeout(crossJobStallTimeout))

	answer := strings.TrimSpace(out.finalResult)
	if answer == "" {
		answer = strings.TrimSpace(out.planContent)
	}

	// Self-healing: an empty answer that never reached the model is a failure
	// of the FORK (the inherited session carried a broken state), not of the
	// question. Retry once through the fresh path, which pastes the context
	// in and cannot inherit the polluted parent state.
	if answer == "" && out.emptyResult && plan.Mode != qaModeFresh {
		probe := probeClaudeTranscript(workDir, plan.NewSessionID, attemptStart)
		if shouldRetryQAFresh(out, probe, plan.Mode) {
			job.Append(store.LogLine{Type: "phase", Content: "⚠️ 继承的会话没有返回内容，改用「方案原文 + 讨论记录」作答"})
			fresh := qaSessionPlan{Mode: qaModeFresh, NewSessionID: newAdjustSessionID()}
			retryOpts := h.crossStreamOpts(crossJobSpec{
				Prompt: h.crossAdjustPrompt(xreq, ws, projects, design, decisions, history, req.Question, false),
				WorkDir:        workDir,
				SystemPrompt:   systemPrompt,
				Model:          modelName,
				Effort:         resolveEffort(h.llm, req.Effort, llm.StageDesign),
				ClaudeConfigID: configID,
				Workspace:      ws,
				SessionID:      fresh.NewSessionID,
			})
			out = runClaudeStream(crossJobSink{jobSink: jobSink{job}, workDir: workDir, protected: retryOpts.AddDirs},
				h.llm.StreamCmd(context.Background(), retryOpts), "cross-design-adjust",
				h.crossUsageCtx("cross_design_adjust", xreq.ID, job.ID, modelName, configID), nil, nil, 0,
				withStallTimeout(crossJobStallTimeout))
			answer = strings.TrimSpace(out.finalResult)
			if answer == "" {
				answer = strings.TrimSpace(out.planContent)
			}
			if answer == "" {
				h.finishAdjustTurn(job, turn, modelName, "AI 没有返回内容，请重试",
					adjustErrorKindEmptyAfter, fresh.NewSessionID, "", false)
				return
			}
			h.finishAdjustTurn(job, turn, modelName, answer, "", fresh.NewSessionID, "", false)
			return
		}
	}

	if answer == "" {
		kind := adjustErrorKindEmptyResult
		if out.errMsg != "" {
			kind = adjustErrorKindCliCrash
		}
		msg := out.errMsg
		if strings.TrimSpace(msg) == "" {
			msg = "AI 没有返回内容，请重试"
		}
		h.finishAdjustTurn(job, turn, modelName, msg, kind, plan.NewSessionID, plan.SourceSID, false)
		return
	}

	// The CLI reports the session it really opened; it wins over the
	// pre-minted id (the next turn has to resume the session that holds this
	// answer).
	sessionID := strings.TrimSpace(out.sessionID)
	if sessionID == "" {
		sessionID = plan.NewSessionID
	}
	h.finishAdjustTurn(job, turn, modelName, answer, "", sessionID, plan.SourceSID, plan.Resumed)
}

// finishAdjustTurn writes the terminal row of a turn and closes the job.
//
// Every exit path of runAdjust goes through here, so a crashed run never
// leaves a row stuck in `running`.
func (h *CrossRequirementHandler) finishAdjustTurn(job *store.Job, turn *model.CrossRequirementQATurn,
	modelName, answer, errorKind, sessionID, sourceSessionID string, resumed bool) {
	status := model.QAStatusDone
	if errorKind != "" {
		status = model.QAStatusError
	}
	if err := h.qaSvc.MarkFinished(turn.ID, status, answer, modelName, sessionID, sourceSessionID, resumed, errorKind); err != nil {
		log.Printf("[xreq-adjust] failed to persist turn %s: %v", turn.ID, err)
	}
	if errorKind != "" {
		job.Append(store.LogLine{Type: "error", Content: answer})
		job.Finish(1, store.JobError)
		return
	}
	job.Append(store.LogLine{Type: "message", Content: answer})
	job.Finish(0, store.JobDone)
}

// crossAdjustPrompt assembles the adjustment turn's prompt.
func (h *CrossRequirementHandler) crossAdjustPrompt(xreq *model.CrossRequirement, ws *crossWorkspace,
	projects []prompt.CrossProjectInput, design string, decisions []model.CrossPendingDecision,
	history []prompt.CrossAdjustTurn, question string, resumed bool) string {
	return prompt.BuildCrossAdjustPrompt(prompt.CrossAdjustPromptInput{
		Resumed:          resumed,
		Title:            xreq.Title,
		Description:      h.crossDescriptionForPrompt(xreq),
		DesignDocs:       design,
		Projects:         projects,
		Conventions:      h.crossConventions(xreq.ID, ws),
		AttachManifest:   h.crossAttachManifest(xreq.ID),
		PendingDecisions: decisions,
		History:          history,
		Question:         question,
	})
}

// adjustHistory returns the thread's earlier answered turns, oldest first.
//
// The turn being answered is excluded by id (it is still `running`, but
// excluding it by id keeps the function honest if the caller ever passes a
// finished turn).
func (h *CrossRequirementHandler) adjustHistory(xreqID string, excludeTurnID string) []prompt.CrossAdjustTurn {
	if h.qaSvc == nil {
		return nil
	}
	rows, err := h.qaSvc.List(xreqID)
	if err != nil {
		log.Printf("[xreq-adjust] list turns of %s: %v", xreqID, err)
		return nil
	}
	out := make([]prompt.CrossAdjustTurn, 0, len(rows))
	for _, row := range rows {
		if row.ID == excludeTurnID || row.Status == model.QAStatusRunning {
			continue
		}
		if strings.TrimSpace(row.Answer) == "" {
			continue
		}
		out = append(out, prompt.CrossAdjustTurn{Question: row.Question, Answer: row.Answer})
	}
	return out
}

// crossDesignDiscussion reads the adjustment thread and renders it as the
// 「调整讨论记录」 block for the overall-design prompt.
//
// The design job calls this only when the user asked for 「按讨论结论重新补全
// 方案」; an empty thread leaves the prompt byte-identical to the one from
// before the feature existed.
func (h *CrossRequirementHandler) crossDesignDiscussion(xreqID string) []prompt.CrossAdjustTurn {
	if h.qaSvc == nil {
		return nil
	}
	rows, err := h.qaSvc.List(xreqID)
	if err != nil {
		log.Printf("[xreq-adjust] list turns of %s: %v", xreqID, err)
		return nil
	}
	out := make([]prompt.CrossAdjustTurn, 0, len(rows))
	for _, row := range rows {
		if row.Status == model.QAStatusRunning || strings.TrimSpace(row.Answer) == "" {
			continue
		}
		out = append(out, prompt.CrossAdjustTurn{Question: row.Question, Answer: row.Answer})
	}
	return out
}

// crossWorkspaceFromBases rebuilds the working-directory set of a cross
// requirement from the **recorded baseline ledger**.
//
// It deliberately does NOT re-fetch: the adjustment talks about a specific
// plan, and that plan was written against a specific commit. Moving the
// ground under the conversation would make the model's answers describe a
// tree the plan never saw — and would cost one git fetch per participating
// repository, per question.
//
// A recorded worktree that has since disappeared falls back to the shared
// checkout (same degradation the design job does).
func (h *CrossRequirementHandler) crossWorkspaceFromBases(xreqID string) (*crossWorkspace, error) {
	projects, err := h.svc.ListProjects(xreqID)
	if err != nil {
		return nil, err
	}
	bases, err := h.svc.ListBases(xreqID)
	if err != nil {
		return nil, err
	}
	byProject := map[string]model.CrossRequirementBase{}
	for _, b := range bases {
		byProject[b.ProjectID] = b
	}
	ws := &crossWorkspace{Projects: []crossWorkspaceProject{}}
	for _, p := range projects {
		item := crossWorkspaceProject{
			ProjectID: p.ProjectID,
			Name:       p.ProjectName,
			BaseBranch: strings.TrimSpace(p.BaseBranch),
		}
		if h.projectSvc != nil {
			if proj, perr := h.projectSvc.Get(p.ProjectID); perr == nil && proj != nil {
				item.LocalPath = strings.TrimSpace(proj.LocalPath)
			}
		}
		if b, ok := byProject[p.ProjectID]; ok {
			item.BaseSHA = b.BaseSHA
			if br := strings.TrimSpace(b.BaseBranch); br != "" {
				item.BaseBranch = br
			}
			if wp := strings.TrimSpace(b.WorktreePath); wp != "" {
				if st, serr := os.Stat(wp); serr == nil && st.IsDir() {
					item.WorktreePath = wp
				}
			}
		}
		if item.WorktreePath == "" && item.LocalPath == "" {
			item.Degraded = true
			item.DegradedReason = "该仓库没有可读路径"
		}
		ws.Projects = append(ws.Projects, item)
	}
	return ws, nil
}

// newAdjustSessionID mints a session id for the CLI's --session-id flag,
// which requires a valid UUID.
func newAdjustSessionID() string {
	return util.NewUUID()
}
