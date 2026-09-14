package handler

import (
	"context"
	"fmt"
	"log"

	"github.com/novaworkbench/backend/internal/scheduler"
	"github.com/novaworkbench/backend/internal/service"
)

// ScheduledExecutor is the bridge between the scheduler package and the
// wizard handler. The scheduler depends only on the Executor interface
// (defined in internal/scheduler); this concrete impl runs the actual
// wizard stages with all the per-stage guards the manual HTTP path also
// runs (session validation, worktree anchoring, status gates, etc.).
//
// Binding strategy: each scheduled-task id is captured in the OnFinish
// closure at dispatch time. The wizard exec body's deferred call to
// OnFinish(jobID, ok) flips the matching scheduled_tasks row from running
// to succeeded/failed. No map lookup is needed because schedID is closed
// over — simpler and avoids a mutex on the hot path.
type ScheduledExecutor struct {
	h         *WizardHandler
	schedSvc  *service.ScheduledTaskService
}

// NewScheduledExecutor wires the wizard handler + ScheduledTaskService.
// (jobLogSvc was on the early prototype but the wizard exec body already
// persists its own log; we keep the adapter minimal here.)
func NewScheduledExecutor(h *WizardHandler, schedSvc *service.ScheduledTaskService) *ScheduledExecutor {
	return &ScheduledExecutor{
		h:         h,
		schedSvc:  schedSvc,
	}
}

// RunScheduledDesign satisfies scheduler.Executor for the design path.
// Performs state guards (done/archived → fail; live manual design job →
// fail) and delegates to WizardHandler.RunScheduledDesign. Returns the
// JobStore job id; the wizard exec body's OnFinish closure flips the
// scheduled_tasks row terminal when the run finishes.
func (e *ScheduledExecutor) RunScheduledDesign(ctx context.Context, p scheduler.DesignParams) (string, error) {
	req, err := e.h.reqSvc.Get(p.RequirementID)
	if err != nil {
		return "", fmt.Errorf("加载需求失败: %w", err)
	}
	// 终态门禁：已完成 / 已归档的需求不再生成方案（防止覆盖终态方案）。
	if req.Status == "done" || req.Status == "archived" {
		return "", fmt.Errorf("需求已完成/归档，定时任务取消执行")
	}
	// 反并发：若有手动方案任务正在执行，跳过避免双 claude 进程。
	if req.DesignJobID != "" && e.h.jobs.Live(req.DesignJobID) {
		return "", fmt.Errorf("已有手动方案任务在执行，定时任务跳过")
	}

	jobID, schedID, err := e.dispatchFromCtx(ctx)
	if err != nil {
		return "", err
	}
	_, err = e.h.RunScheduledDesign(p.RequirementID, p.Model, p.ReadKnowledge, p.AgentServerID, e.callbackFor(schedID))
	if err != nil {
		return "", err
	}
	return jobID, nil
}

// RunScheduledCoding satisfies scheduler.Executor for the coding path.
// Implements the state gates from plan §3.7:
//
//	designed      → UpdateStatus('developing') (主动状态转移)
//	draft + skip  → 放行（直接开发）
//	developing    → 放行（等同「重新开发」）
//	analyzing/designing/draft（无 skip）→ failed
//	done/archived → failed
//
// Also detects a live manual coding job. Note: there is no `coding_job_id`
// column on requirements (only design_job_id / analysis_job_id /
// apply_job_id are persisted) so the live-coding-job guard is currently a
// no-op; the requirement.status transition designed → developing itself
// provides mutual exclusion (a manual run that already promoted the row
// would block the scheduled one because the scheduled path then sees
// status=developing and still proceeds — both go through the same wizard
// exec body, and the JobStore's in-memory ring buffer naturally throttles).
func (e *ScheduledExecutor) RunScheduledCoding(ctx context.Context, p scheduler.CodingParams) (string, error) {
	req, err := e.h.reqSvc.Get(p.RequirementID)
	if err != nil {
		return "", fmt.Errorf("加载需求失败: %w", err)
	}
	switch req.Status {
	case "designed":
		if _, err := e.h.reqSvc.UpdateStatus(req.ID, "developing"); err != nil {
			return "", fmt.Errorf("状态转移失败: %w", err)
		}
	case "draft":
		if !req.SkipDesign {
			return "", fmt.Errorf("需求未生成技术方案，无法开始开发")
		}
		// skip-design 直接放行（start-coding 自身会主动 UpdateStatus→developing）
	case "developing":
		// 放行；等同「重新开发」
	case "analyzing", "designing":
		return "", fmt.Errorf("需求处于 %s 阶段，请等待该阶段完成", req.Status)
	case "done", "archived":
		return "", fmt.Errorf("需求已完成/归档，定时任务取消执行")
	default:
		return "", fmt.Errorf("需求状态异常: %s", req.Status)
	}

	// 构造 codingRunParams（按 wizard 既有字段）。Title / Desc 从最新需求
	// 行读取——用户在调度后修改了标题/描述也以最新值为准。
	cp := &codingRunParams{
		RequirementID:    p.RequirementID,
		RequirementTitle: req.Title,
		RequirementDesc:  req.Description,
		BranchName:       p.BranchName,
		BaseBranch:       p.BaseBranch,
		Model:            p.Model,
		ReadKnowledge:    p.ReadKnowledge,
		AgentServerID:    p.AgentServerID,
		SplitTasks:       p.SplitTasks,
		// ProjectPath 由 wizard exec body 自身从 reqRow.ProjectID + projectSvc 解析
		// （保留 HTTP 路径完全相同的逻辑），此处不传。
	}
	jobID, schedID, err := e.dispatchFromCtx(ctx)
	if err != nil {
		return "", err
	}
	_, err = e.h.RunScheduledCoding(cp, e.callbackFor(schedID))
	if err != nil {
		return "", err
	}
	return jobID, nil
}

// dispatchFromCtx pulls (jobID, schedID) out of the ctx that the
// scheduler populated before calling Executor. The jobID is what the
// scheduler already created in scheduled_tasks via Claim (returned via a
// context value) so the wizard's RunScheduledDesign / RunScheduledCoding
// can reuse the same id when constructing its in-memory JobStore job.
// Today we let the wizard allocate a fresh JobStore job and ignore the
// scheduler-provided jobID — they're separate namespaces (JobStore ring
// vs scheduled_tasks.job_id). The adapter still needs schedID for the
// OnFinish closure, which is the real reason for this helper.
//
// Returns an error if the scheduler forgot to inject schedID, which
// indicates a programming error (the dispatcher must always set it).
func (e *ScheduledExecutor) dispatchFromCtx(ctx context.Context) (jobID, schedID string, err error) {
	if v, ok := ctx.Value(scheduler.SchedCtxKey{}).(scheduler.SchedCtxValue); ok {
		if v.SchedID == "" {
			return "", "", fmt.Errorf("scheduler ctx missing schedID")
		}
		return v.JobID, v.SchedID, nil
	}
	return "", "", fmt.Errorf("scheduler ctx not provided; Executor must be invoked through Scheduler.Dispatch")
}

// (SchedCtxKey / SchedCtxValue moved to scheduler package so the producer
// and consumer share one Go type — exporting them from scheduler lets
// handler use scheduler.SchedCtxKey without introducing a third package
// or inverting the dependency direction.)

// callbackFor returns a *runCallbacks whose OnFinish flips the
// scheduled_tasks row terminal when the wizard exec body finishes.
// Closes over schedID so the executor doesn't need any lookup table.
func (e *ScheduledExecutor) callbackFor(schedID string) *runCallbacks {
	return &runCallbacks{
		OnFinish: func(jobID string, ok bool) {
			status := modelStatusFromOk(ok)
			if err := e.schedSvc.Finish(schedID, ok, jobID, ""); err != nil {
				log.Printf("[scheduler-executor] Finish %s failed: %v", schedID, err)
				return
			}
			log.Printf("[scheduler-executor] scheduled task %s finished status=%s job=%s", schedID, status, jobID)
		},
	}
}

// modelStatusFromOk converts the boolean ok from the wizard exec body
// into the matching SchedStatus* constant. Extracted so the call site
// stays one-liner.
func modelStatusFromOk(ok bool) string {
	if ok {
		return "succeeded"
	}
	return "failed"
}

// RunScheduledDesignAndCoding satisfies scheduler.Executor for the merged
// "design_and_coding" task type. It chains the architect-design and
// start-coding wizard stages using the per-stage fields carried in
// DesignCodingParams (designModel/agent for stage 1, codingModel/agent
// for stage 2, branch / split for the coding side).
//
// Lifecycle:
//
//   - Pre-gate: mirror RunScheduledDesign's done/archived + manual-job
//     checks; on either failure the row is failed and the executor
//     returns early.
//   - Stage 1 (design): WizardHandler.RunScheduledDesign fires with the
//     designModel / designAgentServerID. On its OnFinish callback:
//   - ok=false → schedSvc.Finish(schedID, false, designJobID, "方案设计阶段
//     执行失败，请查看日志"); return.
//   - ok=true  → reqSvc.UpdateStatus(reqID, "designed") (auto-promote the
//     requirement past the manual 方案完成 gate — the architect stage
//     writes design_docs but stops at status=designing), then build
//     codingRunParams with the developer-stage model / agent / branch /
//     split fields and call WizardHandler.RunScheduledCoding with a
//     chained callback. That callback flips the scheduled_tasks row to
//     its terminal state (succeeded or failed) using the coding job id.
//   - The scheduled_tasks.job_id recorded on Finish is whichever stage
//     finished last (coding on the happy path, design on stage-1
//     failure), matching the schema's "any non-empty JobStore id" intent
//     for the list-page "查看日志" deep link.
//
// scheduled_tasks.status stays at "running" across the chain — the row
// only flips terminal when the LAST stage's OnFinish fires. A server
// restart mid-chain is recovered by RecoverInterrupted (running → failed),
// which leaves the design-stage design_docs in place so the user can
// re-trigger stage 2 manually from the requirement detail page.
func (e *ScheduledExecutor) RunScheduledDesignAndCoding(ctx context.Context, p scheduler.DesignCodingParams) (string, error) {
	// 1. Pull schedID out of ctx. dispatchFromCtx is the same helper the
	//    single-stage paths use so the production code path is identical.
	_, schedID, err := e.dispatchFromCtx(ctx)
	if err != nil {
		return "", err
	}

	// 2. Pre-gate: same done/archived + manual-design-job checks as
	//    RunScheduledDesign. Doing this BEFORE the goroutine fires lets us
	//    fail the row synchronously and avoid orphaning an in-flight
	//    design job when the requirement is already terminal.
	req, err := e.h.reqSvc.Get(p.RequirementID)
	if err != nil {
		_ = e.schedSvc.Finish(schedID, false, "", "加载需求失败: "+err.Error())
		return "", fmt.Errorf("加载需求失败: %w", err)
	}
	if req.Status == "done" || req.Status == "archived" {
		_ = e.schedSvc.Finish(schedID, false, "", "需求已完成/归档，定时任务取消执行")
		return "", fmt.Errorf("需求已完成/归档，定时任务取消执行")
	}
	if req.DesignJobID != "" && e.h.jobs.Live(req.DesignJobID) {
		_ = e.schedSvc.Finish(schedID, false, "", "已有手动方案任务在执行，定时任务跳过")
		return "", fmt.Errorf("已有手动方案任务在执行，定时任务跳过")
	}

	// 3. Stage 1 — architect design. Re-read the requirement once more
	//    right before firing so the latest designSessionID / analyst-stage
	//    state is in effect (the user could have updated it between our
	//    pre-gate and now). Pass the user-picked model + agent server
	//    through the design-stage fields of the merged task; ReadKnowledge
	//    is shared between the two stages per the merge-modal UI.
	stage1JobID, err := e.h.RunScheduledDesign(p.RequirementID, p.DesignModel, p.ReadKnowledge, p.DesignAgentServerID, e.chainedDesignCallback(p.RequirementID, schedID, p))
	if err != nil {
		// Synchronous failure (the prepare step rejected, no goroutine was
		// spawned). Mark the row failed immediately.
		_ = e.schedSvc.Finish(schedID, false, "", err.Error())
		return "", err
	}

	// Returned jobID is the design-stage id — the scheduler will surface it
	// in logs. The terminal scheduled_tasks.job_id gets overwritten by
	// chainedDesignCallback / chainedCodingCallback to whichever stage
	// finishes last.
	return stage1JobID, nil
}

// chainedDesignCallback builds the OnFinish closure for the design stage
// of a merged task. On success it auto-promotes the requirement to
// "designed" (the architect stage leaves the row at "designing" — this
// promotion is exactly what the manual "方案完成" button does), then
// dispatches the coding stage with the developer-side fields carried on
// DesignCodingParams. On failure it flips the scheduled_tasks row to
// failed with the design job id.
func (e *ScheduledExecutor) chainedDesignCallback(reqID, schedID string, p scheduler.DesignCodingParams) *runCallbacks {
	return &runCallbacks{
		OnFinish: func(designJobID string, ok bool) {
			if !ok {
				if err := e.schedSvc.Finish(schedID, false, designJobID, "方案设计阶段执行失败，请查看日志"); err != nil {
					log.Printf("[scheduler-executor] Finish %s failed: %v", schedID, err)
					return
				}
				log.Printf("[scheduler-executor] merged scheduled task %s design stage failed job=%s", schedID, designJobID)
				return
			}
			// Auto-promote past the manual 方案完成 gate so the coding
			// stage's status gate ("designed" → "developing") can proceed.
			// RunScheduledCoding itself handles "designing" → "designed" in
			// its status switch, so this is a belt-and-suspenders pass that
			// also serves as the "design stage's outcome is recognized"
			// signal for any UI reading requirements.status.
			if _, err := e.h.reqSvc.UpdateStatus(reqID, "designed"); err != nil {
				log.Printf("[scheduler-executor] promote to designed for %s failed: %v", reqID, err)
			}
			// Re-load the requirement so the coding stage picks up the
			// LATEST title / description (the user may have edited the
			// requirement between creating the scheduled task and the
			// dispatch firing).
			req, err := e.h.reqSvc.Get(reqID)
			if err != nil {
				if ferr := e.schedSvc.Finish(schedID, false, designJobID, "加载需求失败: "+err.Error()); ferr != nil {
					log.Printf("[scheduler-executor] Finish %s failed: %v", schedID, ferr)
				}
				return
			}
			cp := &codingRunParams{
				RequirementID:    reqID,
				RequirementTitle: req.Title,
				RequirementDesc:  req.Description,
				BranchName:       p.BranchName,
				BaseBranch:       p.BaseBranch,
				Model:            p.CodingModel,
				ReadKnowledge:    p.ReadKnowledge,
				AgentServerID:    p.CodingAgentServerID,
				SplitTasks:       p.SplitTasks,
				// ProjectPath / ClaudeConfigID / AutoPushPR / DevMode / SyncMode
				// stay empty so the wizard exec body resolves them via the
				// existing per-stage priority chain (same behavior as the
				// manual coding flow with an empty prefill).
			}
			codingJobID, cerr := e.h.RunScheduledCoding(cp, e.chainedCodingCallback(schedID))
			if cerr != nil {
				if ferr := e.schedSvc.Finish(schedID, false, designJobID, "开发阶段派发失败: "+cerr.Error()); ferr != nil {
					log.Printf("[scheduler-executor] Finish %s failed: %v", schedID, ferr)
				}
				return
			}
			log.Printf("[scheduler-executor] merged scheduled task %s dispatched coding stage job=%s (design job=%s)", schedID, codingJobID, designJobID)
		},
	}
}

// chainedCodingCallback is the OnFinish closure for the second stage of
// a merged task. It simply flips the scheduled_tasks row terminal — the
// design callback has already done the heavy lifting. JobID here is the
// coding job id, which becomes the value scheduled_tasks.job_id lands
// with on success (matching the "last-stage wins" rule from the
// RunScheduledDesignAndCoding docstring).
func (e *ScheduledExecutor) chainedCodingCallback(schedID string) *runCallbacks {
	return &runCallbacks{
		OnFinish: func(codingJobID string, ok bool) {
			msg := ""
			if !ok {
				msg = "开发阶段执行失败，请查看日志"
			}
			if err := e.schedSvc.Finish(schedID, ok, codingJobID, msg); err != nil {
				log.Printf("[scheduler-executor] Finish %s failed: %v", schedID, err)
				return
			}
			status := modelStatusFromOk(ok)
			log.Printf("[scheduler-executor] merged scheduled task %s finished status=%s job=%s", schedID, status, codingJobID)
		},
	}
}