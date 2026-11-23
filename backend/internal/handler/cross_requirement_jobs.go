package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// 「跨项目需求」的 AI 作业层：整体方案 / 按项目拆分 / 阶段验证。
//
// 三个作业共享同一套骨架：
//
//	启动 → 建 job → 抢并发闸门 → 登记 run → 返回 job_id
//	（后台）→ 备 prompt → claude CLI（只读）→ 解析 → 落库 → 结束 job
//
// 与 wizard_architect.go 的关系：**不复用**。那个文件与 *model.Requirement 的
// 状态机深度耦合（状态迁移 / design_docs / session / revision / 附件 / QA），
// 硬套进来会引入大量分支与回归风险。这里只复用稳定的底层件：llm.StreamOpts /
// runClaudeStream / store.JobStore / SSE helper。
//
// 运行形态：**仅本地**（D9）。远端执行要求 Agent Server 上已经 clone 了全部
// 参与仓库并同步到同一版本，MVP 不引入这个复杂度。

// crossJobStallTimeout — 跨项目作业的静默看门狗时限。
//
// 整体方案要读 N 个仓库，阶段验证要读更多，比纯文本转换慢得多，但不该无限等。
const crossJobStallTimeout = 10 * time.Minute

// crossJobReq — 启动跨项目作业的请求体。
type crossJobReq struct {
	// Model — 用户在 UI 上选的模型（空 = 用 architect persona 的默认模型）。
	Model string `json:"model"`
	// ClaudeConfigID — 用户选的 Claude 配置（空 = 当前激活配置）。
	ClaudeConfigID string `json:"claude_config_id"`
	// Effort — 用户选的推理档位（空 = 「方案设计」阶段的默认档位）。
	Effort string `json:"effort"`
}

// ── 整体方案 ───────────────────────────────────────────────────────────────

// StartDesign launches the overall-design job.
//
// Preconditions (rejected BEFORE a job is created — the user must be told
// why, not shown a spinner that dies):
//
//	no participating project → XREQ_NO_PROJECTS
//	a design job is running   → XREQ_IN_PROGRESS (409)
//	archived                  → XREQ_ARCHIVED
//
// Returns {"job_id": ...}; the design document arrives on the job's SSE stream.
func (h *CrossRequirementHandler) StartDesign(w http.ResponseWriter, r *http.Request) {
	xreqID := r.PathValue("id")
	if h.svc == nil || h.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	var body crossJobReq
	if err := decodeJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
		_ = err // a malformed body is not fatal — the persona defaults apply
	}

	xreq, err := h.svc.Get(xreqID)
	if err != nil {
		writeError(w, http.StatusNotFound, "CROSS_REQUIREMENT_NOT_FOUND", "跨项目需求不存在")
		return
	}
	if xreq.Status == model.XReqStatusArchived {
		writeError(w, http.StatusBadRequest, "XREQ_ARCHIVED", "已归档的跨项目需求不能生成方案")
		return
	}
	projects, err := h.svc.ListProjects(xreqID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	if len(projects) == 0 {
		writeError(w, http.StatusBadRequest, "XREQ_NO_PROJECTS", "请先选择参与项目")
		return
	}
	if existing, running := h.runs.peek(crossRunDesign, xreqID, ""); running {
		writeError(w, http.StatusConflict, "XREQ_IN_PROGRESS",
			fmt.Sprintf("已有整体方案任务在运行（job %s），请等待结束", existing))
		return
	}

	job := h.jobs.Create(xreqID)
	job.SetType("cross_design")

	if existing, ok := h.runs.acquire(crossRunDesign, xreqID, "", job.ID); !ok {
		job.Finish(1, store.JobError)
		writeError(w, http.StatusConflict, "XREQ_IN_PROGRESS",
			fmt.Sprintf("已有整体方案任务在运行（job %s），请等待结束", existing))
		return
	}

	systemPrompt, modelName, configID := h.crossRoleConfig()
	if strings.TrimSpace(body.Model) != "" {
		modelName = body.Model
	}
	if strings.TrimSpace(body.ClaudeConfigID) != "" {
		configID = body.ClaudeConfigID
	}
	h.runs.start(job.ID, xreqID, crossRunDesign, "", modelName)

	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})

	go h.runDesign(job, xreq, body, systemPrompt, modelName, configID)
}

// runDesign is the goroutine body of the overall-design job.
func (h *CrossRequirementHandler) runDesign(job *store.Job, xreq *model.CrossRequirement, body crossJobReq,
	systemPrompt, modelName, configID string) {
	defer h.runs.release(crossRunDesign, xreq.ID, "")
	defer func() {
		if r := recover(); r != nil {
			h.finishCrossError(job, crossRunDesign, xreq.ID, "", "整体方案生成过程异常终止")
		}
	}()

	projects, err := h.crossProjectInputs(xreq.ID)
	if err != nil {
		h.finishCrossError(job, crossRunDesign, xreq.ID, "", "读取参与项目失败: "+err.Error())
		return
	}
	previous := service.DesignDocsToMarkdown(xreq.DesignDocs)
	promptText := prompt.BuildCrossDesignPrompt(xreq.Title, xreq.Description, crossAcceptance(xreq.AcceptanceCriteria), projects, previous)

	workDir, cleanup, err := crossWorkDir("design")
	if err != nil {
		h.finishCrossError(job, crossRunDesign, xreq.ID, "", "创建临时工作区失败: "+err.Error())
		return
	}
	defer cleanup()

	opts := llm.StreamOpts{
		Prompt:         promptText,
		WorkDir:        workDir,
		SystemPrompt:   systemPrompt,
		Model:          cliModelArg(modelName),
		Effort:         resolveEffort(h.llm, body.Effort, llm.StageDesign),
		ClaudeConfigID: configID,
		// The participating repositories must be readable, otherwise the
		// model cannot ground the contract in real file paths.
		AddDirs: h.crossProjectDirs(xreq.ID),
		// Read-only: producing a design must not touch any repository.
		DisallowedTools: []string{"Write", "Edit", "Bash", "NotebookEdit"},
	}
	promptText += h.crossClaudeDocBlock(xreq.ID, workDir)

	job.Append(store.LogLine{Type: "phase", Content: "🧭 正在生成跨项目整体方案…"})

	out := runClaudeStream(jobSink{job}, h.llm.StreamCmd(context.Background(), opts), "cross-design",
		h.crossUsageCtx("cross_design", xreq.ID, job.ID, modelName, configID), nil, nil, 0,
		withStallTimeout(crossJobStallTimeout))

	markdown := strings.TrimSpace(out.finalResult)
	if markdown == "" {
		markdown = strings.TrimSpace(out.planContent)
	}
	if markdown == "" {
		h.finishCrossError(job, crossRunDesign, xreq.ID, "", "未产出整体方案，请重试")
		return
	}
	if !prompt.HasCrossContractSection(markdown) {
		// The contract section is mandatory — a design without it would let
		// each repository invent its own interface. The report is still
		// surfaced so the user can see what came back.
		h.runs.setResult(job.ID, markdown, nil, "")
		job.Append(store.LogLine{Type: "error", Content: "⚠️ 整体方案缺少「跨项目接口契约」章节，请重新生成或手工补齐"})
		job.Append(store.LogLine{Type: "message", Content: markdown})
		h.runs.finish(job.ID, store.JobError, "整体方案缺少「跨项目接口契约」章节")
		job.Finish(1, store.JobError)
		return
	}

	if err := h.svc.SaveDesign(xreq.ID, markdown, "", job.ID); err != nil {
		h.finishCrossError(job, crossRunDesign, xreq.ID, "", "保存整体方案失败: "+err.Error())
		return
	}
	if err := h.svc.RefreshDerivedStatus(xreq.ID); err != nil {
		_ = err
	}
	h.runs.setResult(job.ID, markdown, nil, "")
	h.runs.finish(job.ID, store.JobDone, "")
	job.Append(store.LogLine{Type: "message", Content: prompt.CrossDesignSummary(len(projects))})
	job.Finish(0, store.JobDone)
}

// ── 按项目拆分 ─────────────────────────────────────────────────────────────

// StartSplit launches the cross-project split job.
//
// Returns {"job_id": ...}; the candidate list arrives on the job's SSE stream
// and can also be polled through GET .../split/state.
func (h *CrossRequirementHandler) StartSplit(w http.ResponseWriter, r *http.Request) {
	xreqID := r.PathValue("id")
	if h.svc == nil || h.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	var body crossJobReq
	if err := decodeJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
		_ = err
	}

	xreq, err := h.svc.Get(xreqID)
	if err != nil {
		writeError(w, http.StatusNotFound, "CROSS_REQUIREMENT_NOT_FOUND", "跨项目需求不存在")
		return
	}
	if xreq.Status == model.XReqStatusArchived {
		writeError(w, http.StatusBadRequest, "XREQ_ARCHIVED", "已归档的跨项目需求不能拆分")
		return
	}
	design := service.DesignDocsToMarkdown(xreq.DesignDocs)
	if design == "" {
		writeError(w, http.StatusBadRequest, "XREQ_NO_DESIGN", "请先生成整体方案，再按项目拆分")
		return
	}
	projects, err := h.crossProjectInputs(xreqID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	if len(projects) == 0 {
		writeError(w, http.StatusBadRequest, "XREQ_NO_PROJECTS", "请先选择参与项目")
		return
	}
	if existing, running := h.runs.peek(crossRunSplit, xreqID, ""); running {
		writeError(w, http.StatusConflict, "XREQ_IN_PROGRESS",
			fmt.Sprintf("已有拆分任务在运行（job %s），请等待结束", existing))
		return
	}

	job := h.jobs.Create(xreqID)
	job.SetType("cross_split")

	if existing, ok := h.runs.acquire(crossRunSplit, xreqID, "", job.ID); !ok {
		job.Finish(1, store.JobError)
		writeError(w, http.StatusConflict, "XREQ_IN_PROGRESS",
			fmt.Sprintf("已有拆分任务在运行（job %s），请等待结束", existing))
		return
	}

	systemPrompt, modelName, configID := h.crossRoleConfig()
	if strings.TrimSpace(body.Model) != "" {
		modelName = body.Model
	}
	if strings.TrimSpace(body.ClaudeConfigID) != "" {
		configID = body.ClaudeConfigID
	}
	h.runs.start(job.ID, xreqID, crossRunSplit, "", modelName)

	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})

	go h.runSplit(job, xreq, body, design, projects, systemPrompt, modelName, configID)
}

// runSplit is the goroutine body of the cross-project split job.
func (h *CrossRequirementHandler) runSplit(job *store.Job, xreq *model.CrossRequirement, body crossJobReq,
	design string, projects []prompt.CrossProjectInput, systemPrompt, modelName, configID string) {
	defer h.runs.release(crossRunSplit, xreq.ID, "")
	defer func() {
		if r := recover(); r != nil {
			h.finishCrossError(job, crossRunSplit, xreq.ID, "", "拆分过程异常终止")
		}
	}()

	promptText := prompt.BuildCrossSplitPrompt(xreq.Title, design, projects)

	workDir, cleanup, err := crossWorkDir("split")
	if err != nil {
		h.finishCrossError(job, crossRunSplit, xreq.ID, "", "创建临时工作区失败: "+err.Error())
		return
	}
	defer cleanup()

	opts := llm.StreamOpts{
		Prompt:          promptText,
		WorkDir:         workDir,
		SystemPrompt:    systemPrompt,
		Model:           cliModelArg(modelName),
		Effort:          resolveEffort(h.llm, body.Effort, llm.StageDesign),
		ClaudeConfigID:  configID,
		AddDirs:         h.crossProjectDirs(xreq.ID),
		DisallowedTools: []string{"Write", "Edit", "Bash", "NotebookEdit"},
	}
	promptText += h.crossClaudeDocBlock(xreq.ID, workDir)

	job.Append(store.LogLine{Type: "phase", Content: "🧩 正在按项目拆分整体方案…"})

	out := runClaudeStream(jobSink{job}, h.llm.StreamCmd(context.Background(), opts), "cross-split",
		h.crossUsageCtx("cross_split", xreq.ID, job.ID, modelName, configID), nil, nil, 0,
		withStallTimeout(crossJobStallTimeout))

	jsonPart, _ := extractTrailingJSON(out.finalResult)
	if strings.TrimSpace(jsonPart) == "" {
		jsonPart = out.planContent
	}
	candidates := prompt.NormalizeCrossSplitCandidates(jsonPart, projects)
	if len(candidates) == 0 {
		h.finishCrossError(job, crossRunSplit, xreq.ID, "", "未能从整体方案中解析出候选子需求，请完善方案后重试")
		return
	}

	h.runs.setResult(job.ID, "", candidates, "")
	h.runs.finish(job.ID, store.JobDone, "")
	job.Append(store.LogLine{Type: "message", Content: prompt.CrossSplitSummary(candidates)})
	job.Finish(0, store.JobDone)
}

// SplitState reports the most recent split run of a cross requirement.
//
// 「没跑过」是正常状态不是错误：回 200 + {"status":"none"}。
func (h *CrossRequirementHandler) SplitState(w http.ResponseWriter, r *http.Request) {
	xreqID := r.PathValue("id")
	run := h.runs.latestFor(crossRunSplit, xreqID, "")
	if run == nil || run.Consumed {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":        "none",
			"job_id":        "",
			"running":       false,
			"error_message": "",
			"candidates":    []prompt.CrossSplitCandidate{},
			"model":         "",
			"created_at":    int64(0),
			"finished_at":   int64(0),
		})
		return
	}
	var created, finished int64
	if !run.CreatedAt.IsZero() {
		created = run.CreatedAt.UnixMilli()
	}
	if !run.FinishedAt.IsZero() {
		finished = run.FinishedAt.UnixMilli()
	}
	candidates := run.Candidates
	if candidates == nil {
		candidates = []prompt.CrossSplitCandidate{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        string(run.Status),
		"job_id":        run.JobID,
		"running":       run.Status == store.JobRunning,
		"error_message": run.Error,
		"candidates":    candidates,
		"model":         run.Model,
		"created_at":    created,
		"finished_at":   finished,
	})
}

// ── 阶段验证 ───────────────────────────────────────────────────────────────

// StartVerify launches the phase-verification job.
func (h *CrossRequirementHandler) StartVerify(w http.ResponseWriter, r *http.Request) {
	xreqID := r.PathValue("id")
	phaseID := r.PathValue("pid")
	if h.svc == nil || h.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	var body crossJobReq
	if err := decodeJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
		_ = err
	}

	if _, err := h.svc.GetPhase(xreqID, phaseID); err != nil {
		writeError(w, http.StatusNotFound, "PHASE_NOT_FOUND", "阶段不存在")
		return
	}
	if existing, running := h.runs.peek(crossRunVerify, xreqID, phaseID); running {
		writeError(w, http.StatusConflict, "XREQ_IN_PROGRESS",
			fmt.Sprintf("已有阶段验证任务在运行（job %s），请等待结束", existing))
		return
	}

	job := h.jobs.Create(xreqID)
	job.SetType("cross_verify")

	if existing, ok := h.runs.acquire(crossRunVerify, xreqID, phaseID, job.ID); !ok {
		job.Finish(1, store.JobError)
		writeError(w, http.StatusConflict, "XREQ_IN_PROGRESS",
			fmt.Sprintf("已有阶段验证任务在运行（job %s），请等待结束", existing))
		return
	}

	systemPrompt, modelName, configID := h.crossRoleConfig()
	if strings.TrimSpace(body.Model) != "" {
		modelName = body.Model
	}
	if strings.TrimSpace(body.ClaudeConfigID) != "" {
		configID = body.ClaudeConfigID
	}
	h.runs.start(job.ID, xreqID, crossRunVerify, phaseID, modelName)

	if err := h.svc.SetPhaseVerifyJob(phaseID, job.ID); err != nil {
		_ = err
	}

	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})

	go h.runVerify(job, xreqID, phaseID, body, systemPrompt, modelName, configID)
}

// runVerify is the goroutine body of the phase-verification job.
func (h *CrossRequirementHandler) runVerify(job *store.Job, xreqID, phaseID string, body crossJobReq,
	systemPrompt, modelName, configID string) {
	defer h.runs.release(crossRunVerify, xreqID, phaseID)
	defer func() {
		if r := recover(); r != nil {
			h.finishCrossError(job, crossRunVerify, xreqID, phaseID, "阶段验证过程异常终止")
		}
	}()

	phase, err := h.svc.GetPhase(xreqID, phaseID)
	if err != nil {
		h.finishCrossError(job, crossRunVerify, xreqID, phaseID, "读取阶段失败: "+err.Error())
		return
	}
	xreq, err := h.svc.Get(xreqID)
	if err != nil {
		h.finishCrossError(job, crossRunVerify, xreqID, phaseID, "读取跨项目需求失败: "+err.Error())
		return
	}

	members := h.crossPhaseMembers(xreqID, phase)
	promptText := prompt.BuildCrossPhasePrompt(phase.Name, phase.Description, members,
		service.DesignDocsToMarkdown(xreq.DesignDocs))

	workDir, cleanup, err := crossWorkDir("verify")
	if err != nil {
		h.finishCrossError(job, crossRunVerify, xreqID, phaseID, "创建临时工作区失败: "+err.Error())
		return
	}
	defer cleanup()

	opts := llm.StreamOpts{
		Prompt:          promptText,
		WorkDir:         workDir,
		SystemPrompt:    systemPrompt,
		Model:           cliModelArg(modelName),
		Effort:          resolveEffort(h.llm, body.Effort, llm.StageDesign),
		ClaudeConfigID:  configID,
		AddDirs:         h.crossProjectDirs(xreqID),
		DisallowedTools: []string{"Write", "Edit", "Bash", "NotebookEdit"},
	}
	promptText += h.crossClaudeDocBlock(xreqID, workDir)

	job.Append(store.LogLine{Type: "phase", Content: "🔍 正在做阶段验证…"})

	out := runClaudeStream(jobSink{job}, h.llm.StreamCmd(context.Background(), opts), "cross-verify",
		h.crossUsageCtx("cross_verify", xreqID, job.ID, modelName, configID), nil, nil, 0,
		withStallTimeout(crossJobStallTimeout))

	report := strings.TrimSpace(out.finalResult)
	if report == "" {
		report = strings.TrimSpace(out.planContent)
	}
	if report == "" {
		h.finishCrossError(job, crossRunVerify, xreqID, phaseID, "未产出阶段验证报告，请重试")
		return
	}

	// The report is PERSISTED on the phase — it must survive a backend
	// restart (the JobStore ring buffer does not).
	if err := h.svc.SetPhaseVerifyReport(phaseID, report); err != nil {
		h.finishCrossError(job, crossRunVerify, xreqID, phaseID, "保存验证报告失败: "+err.Error())
		return
	}
	if err := h.svc.RefreshDerivedStatus(xreqID); err != nil {
		_ = err
	}
	h.runs.setResult(job.ID, "", nil, report)
	h.runs.finish(job.ID, store.JobDone, "")
	job.Append(store.LogLine{Type: "message", Content: prompt.CrossPhaseSummary(len(members))})
	job.Finish(0, store.JobDone)
}

// crossPhaseMembers builds the prompt-side view of a phase's members.
func (h *CrossRequirementHandler) crossPhaseMembers(xreqID string, phase *model.CrossRequirementPhase) []prompt.CrossPhaseMember {
	out := []prompt.CrossPhaseMember{}
	if phase == nil {
		return out
	}
	children, err := h.svc.ListChildren(xreqID)
	if err != nil {
		return out
	}
	byID := map[string]model.CrossRequirementChild{}
	for _, c := range children {
		byID[c.ID] = c
	}
	for _, item := range phase.Items {
		child, ok := byID[item.RequirementID]
		if !ok {
			continue
		}
		member := prompt.CrossPhaseMember{
			RequirementID: child.ID,
			Title:         child.Title,
			ProjectName:   child.ProjectName,
			Status:        child.Status,
		}
		if h.reqSvc != nil {
			if row, rErr := h.reqSvc.Get(child.ID); rErr == nil && row != nil {
				member.Description = row.Description
			}
		}
		if h.projectSvc != nil {
			if proj, pErr := h.projectSvc.Get(child.ProjectID); pErr == nil && proj != nil {
				member.ProjectPath = proj.LocalPath
			}
		}
		out = append(out, member)
	}
	return out
}

// ── SSE ────────────────────────────────────────────────────────────────────

// StreamDesignJob streams the overall-design job.
//
// The terminal frame carries the design document in `result.design` so the
// page can render it without a second round trip.
func (h *CrossRequirementHandler) StreamDesignJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job, ok := h.jobs.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found")
		return
	}
	streamJobSSE(w, r, job, func(status store.JobStatus, exitCode int) []byte {
		var result any
		var errMsg string
		if run := h.runs.get(jobID); run != nil {
			errMsg = run.Error
			result = map[string]any{"design": run.Design}
		}
		return crossDoneFrame(job, status, exitCode, errMsg, result)
	})
}

// StreamSplitJob streams the split job; the terminal frame carries the
// candidates in `result.candidates`.
func (h *CrossRequirementHandler) StreamSplitJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job, ok := h.jobs.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found")
		return
	}
	streamJobSSE(w, r, job, func(status store.JobStatus, exitCode int) []byte {
		var result any
		var errMsg string
		if run := h.runs.get(jobID); run != nil {
			errMsg = run.Error
			candidates := run.Candidates
			if candidates == nil {
				candidates = []prompt.CrossSplitCandidate{}
			}
			result = map[string]any{"candidates": candidates}
		}
		return crossDoneFrame(job, status, exitCode, errMsg, result)
	})
}

// StreamVerifyJob streams the phase-verification job; the terminal frame
// carries the report in `result.report`.
func (h *CrossRequirementHandler) StreamVerifyJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job, ok := h.jobs.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found")
		return
	}
	streamJobSSE(w, r, job, func(status store.JobStatus, exitCode int) []byte {
		var result any
		var errMsg string
		if run := h.runs.get(jobID); run != nil {
			errMsg = run.Error
			result = map[string]any{"report": run.Report}
		}
		return crossDoneFrame(job, status, exitCode, errMsg, result)
	})
}

// crossDoneFrame builds the terminal SSE frame of a cross-requirement job.
func crossDoneFrame(job *store.Job, status store.JobStatus, exitCode int, errMsg string, result any) []byte {
	data, _ := json.Marshal(map[string]any{
		"type":          "job_done",
		"status":        string(status),
		"exit_code":     exitCode,
		"error_kind":    job.ErrorKind,
		"error_message": errMsg,
		"started_at":    job.StartedAt.UnixMilli(),
		"finished_at":   job.FinishedAt.UnixMilli(),
		"duration_ms":   job.FinishedAt.Sub(job.StartedAt).Milliseconds(),
		"result":        result,
	})
	return data
}

// ── 辅助 ───────────────────────────────────────────────────────────────────

// finishCrossError reports a failed job on the stream.
//
// The failure reason is recorded BOTH on the registry (so the state endpoint
// can serve it after the page is reopened) and as an "error" log line (so it
// shows up in the live stream).
func (h *CrossRequirementHandler) finishCrossError(job *store.Job, kind, xreqID, phaseID, msg string) {
	h.runs.finish(job.ID, store.JobError, msg)
	job.Append(store.LogLine{Type: "error", Content: msg})
	job.Finish(1, store.JobError)
}

// crossRoleConfig resolves the persona / model / claude config for a
// cross-requirement job.
//
// The architect persona is the closest fit (it is the "方案设计" role). A
// missing persona never blocks the run — the CLI defaults apply.
func (h *CrossRequirementHandler) crossRoleConfig() (systemPrompt, modelName, configID string) {
	if h.roleSvc == nil {
		return "", "", ""
	}
	r, err := h.roleSvc.GetByKey("architect")
	if err != nil || r == nil {
		return "", "", ""
	}
	if h.claudeCfg != nil {
		if cfg, cErr := h.claudeCfg.ResolveRoleConfig(r); cErr == nil && cfg != nil {
			return r.SystemPrompt, effectiveModelFromValues(r.Model, cfg.DefaultModel), cfg.ID
		}
	}
	return r.SystemPrompt, r.Model, ""
}

// crossUsageCtx builds the usage recorder context for a cross-requirement
// job. Token usage must be attributed to the run, otherwise the cost
// dashboards cannot see the AI work this feature triggers.
func (h *CrossRequirementHandler) crossUsageCtx(step, xreqID, jobID, modelName, configID string) *usageCtx {
	ctx := &usageCtx{
		Rec:            h.usageSvc,
		RequirementID:  xreqID,
		JobID:          jobID,
		Step:           step,
		Model:          modelName,
		ClaudeConfigID: configID,
		Summary:        "跨项目需求",
	}
	return ctx
}

// crossClaudeDocBlock renders the participating projects' CLAUDE.md
// conventions as a prompt block.
//
// The cross-requirement run works in a **scratch** directory, so the CLI's own
// project-memory channel has nothing to load — the conventions have to travel
// through the prompt. Each participating project contributes its own block
// (prefixed with the project name), because the model must know WHICH
// repository's conventions apply to which part of the design.
func (h *CrossRequirementHandler) crossClaudeDocBlock(xreqID, workDir string) string {
	if h.projectSvc == nil {
		return ""
	}
	projects, err := h.svc.ListProjects(xreqID)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range projects {
		doc := resolveClaudeDocInjection(h.projectSvc, h.settingSvc, p.ProjectID, workDir)
		block := doc.Block()
		if strings.TrimSpace(block) == "" {
			continue
		}
		name := strings.TrimSpace(p.ProjectName)
		if name == "" {
			name = p.ProjectID
		}
		b.WriteString("\n\n## 项目约定 · " + name + "\n")
		b.WriteString(block)
	}
	return b.String()
}
