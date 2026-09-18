// Package handler — create-time "启动计划" (launch plan) dispatch.
//
// This file is the whole of the feature "在需求创建的时候就决定定时/模型/
// 拆分/开发模式": POST /api/requirements may now carry a `launch` object
// (model.LaunchSpec) and the server dispatches it atomically right after the
// row is minted — no detour through the requirement detail page.
//
// It deliberately owns NO execution logic. Everything it needs already
// existed before this file:
//
//	立即「方案+开发」 → WizardHandler.launchDesignAndCoding (wizard_immediate.go)
//	立即「仅开发」    → WizardHandler.gateCodingEntry + RunScheduledCoding
//	                    (wizard_coding.go)
//	定时（一次性/每天/每周）→ ScheduledTaskService.Create + the parseRunAt /
//	                    validRecurTime / validRecurDays validators from
//	                    schedule.go
//
// What lives here is only the MAPPING from "what kind of requirement did the
// user just create" to "which of those entry points may legally fire", plus
// the translation of each entry point's failure into an *apiFailure.
//
// Why server-side rather than two chained frontend calls: the mapping below
// has to respect two hard gates that live deep in the wizard
// (wizard_architect.go NO_SESSION and the coding status gate), and there are
// three separate create entry points in the UI (ProjectDetail /
// RequirementsList / RequirementsCalendar). One server-side copy keeps them
// honest and lets scripts create-and-launch in a single call. It also means a
// failed dispatch can never orphan the requirement — see dispatchLaunch's
// contract below.
package handler

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// Error codes emitted by the launch dispatcher. LAUNCH_NEEDS_ANALYSIS is
// the one genuinely new code: it pre-empts the NO_SESSION failure the
// architect stage would raise for a brand-new requirement whose flow keeps
// the analyst stage (skip_analysis=false), because a fresh row has no
// analyst session to fork from.
const (
	launchErrNeedsAnalysis = "LAUNCH_NEEDS_ANALYSIS"
	launchErrIdea          = "IDEA_NOT_DEVELOPABLE"
	launchErrInvalidMode   = "INVALID_LAUNCH_MODE"
	launchErrCodingGate    = "LAUNCH_CODING_GATE"
)

// resolveLaunchTaskType maps a freshly created requirement + the requested
// launch mode onto one of the scheduled_tasks task types.
//
// The mapping is fully determined by the requirement's skip flags, which the
// create form derives from its 开发流程 picker:
//
//	flow           skip_analysis skip_design  immediate            scheduled
//	direct         true          true         ✅ coding            ✅ coding
//	skip-analysis  true          false        ✅ design_and_coding ✅ design_and_coding
//	full           false         false        ❌ NEEDS_ANALYSIS    ✅ design_and_coding
//
// The `full` + immediate rejection is not a policy choice — the architect
// stage hard-fails with NO_SESSION for a requirement that has neither a
// design nor an analysis session (wizard_architect.go). Rejecting it here
// gives the caller an actionable code instead of a confusing stage failure.
//
// `full` + scheduled IS allowed, deliberately: the task fires at some future
// moment, and "今天先和分析师聊清楚，今晚 11 点自动出方案并开发" is exactly the
// scenario scheduling exists for. If the analysis still isn't done when the
// timer fires, the scheduled_tasks row lands on failed + error_message — the
// pre-existing, user-visible failure semantics for every scheduled task.
//
// kind == "idea" is rejected for both modes: an idea is discussion-first and
// both wizard_immediate.go and schedule.go already refuse to develop one.
func resolveLaunchTaskType(req *model.Requirement, mode string) (string, *apiFailure) {
	if req.Kind == service.KindIdea {
		return "", fail(400, launchErrIdea, "「想法」类需求暂不支持启动计划，请先在详情页点击「📋 转为需求」升级")
	}
	if req.SkipDesign {
		return model.SchedTypeCoding, nil
	}
	if req.SkipAnalysis || mode == model.LaunchModeScheduled {
		return model.SchedTypeDesignCoding, nil
	}
	return "", fail(400, launchErrNeedsAnalysis, "「完整流程」的需求还没有需求分析会话，无法立即生成方案；请改为「定时执行」，或先完成需求分析后在详情页手动启动")
}

// dispatchLaunch executes the launch plan for a just-created requirement.
//
// Contract: the requirement row already exists and is NEVER rolled back. A
// non-nil *apiFailure means "the row is fine, the launch didn't happen" —
// RequirementHandler.Create still answers 201 and reports the reason via
// Requirement.LaunchError so the user keeps everything they typed and can
// start the work manually from the detail page (which has the full set of
// manual entry points).
//
// Returns (mode, scheduleID): mode echoes the dispatched LaunchSpec.Mode,
// scheduleID is the scheduled_tasks row id for the scheduled path (empty for
// immediate — the immediate path's job ids are already persisted on the
// requirement row by the wizard stages, and the detail page's existing
// active-jobs poll picks them up).
func (h *RequirementHandler) dispatchLaunch(req *model.Requirement, spec *model.LaunchSpec) (string, string, *apiFailure) {
	if spec == nil {
		return "", "", nil
	}
	switch spec.Mode {
	case model.LaunchModeImmediate, model.LaunchModeScheduled:
	default:
		return "", "", fail(400, launchErrInvalidMode, fmt.Sprintf("launch.mode 必须是 %q 或 %q", model.LaunchModeImmediate, model.LaunchModeScheduled))
	}

	taskType, af := resolveLaunchTaskType(req, spec.Mode)
	if af != nil {
		return "", "", af
	}

	if spec.Mode == model.LaunchModeScheduled {
		schedID, af := h.dispatchScheduledLaunch(req, spec, taskType)
		if af != nil {
			return "", "", af
		}
		return model.LaunchModeScheduled, schedID, nil
	}

	if af := h.dispatchImmediateLaunch(req, spec, taskType); af != nil {
		return "", "", af
	}
	return model.LaunchModeImmediate, "", nil
}

// dispatchScheduledLaunch writes the scheduled_tasks row. Recurrence
// validation reuses schedule.go's parseRunAt / validRecurTime /
// validRecurDays verbatim so a launch plan and the /schedules modal accept
// exactly the same inputs (including the 30-second run_at floor).
func (h *RequirementHandler) dispatchScheduledLaunch(req *model.Requirement, spec *model.LaunchSpec, taskType string) (string, *apiFailure) {
	if h.schedSvc == nil {
		return "", fail(500, "INTERNAL", "定时任务服务未初始化")
	}
	recurrence := spec.Recurrence
	if recurrence == "" {
		recurrence = model.SchedRecurOnce
	}
	var runAt time.Time
	switch recurrence {
	case model.SchedRecurOnce:
		if spec.RunAt == "" {
			return "", fail(400, "INVALID", "launch.run_at is required for recurrence=once")
		}
		parsed, err := parseRunAt(spec.RunAt)
		if err != nil {
			return "", fail(400, "INVALID_RUN_AT", err.Error())
		}
		if parsed.Before(time.Now().Add(30 * time.Second)) {
			return "", fail(400, "RUN_AT_TOO_SOON", "定时时间必须至少在 30 秒之后")
		}
		runAt = parsed
	case model.SchedRecurDaily, model.SchedRecurWeekly:
		// The service derives run_at from the rule; validate the rule shape
		// here so the user gets a 400 rather than a 500 out of NextRunAt.
		if !validRecurTime(spec.RecurTime) {
			return "", fail(400, "INVALID_RECUR_TIME", "recur_time must be HH:MM (00:00-23:59)")
		}
		if recurrence == model.SchedRecurWeekly && !validRecurDays(spec.RecurDays) {
			return "", fail(400, "INVALID_RECUR_DAYS", "recur_days must be a non-empty CSV of weekday numbers 0-6 (0=Sunday)")
		}
	default:
		return "", fail(400, "INVALID", fmt.Sprintf("recurrence must be %q, %q or %q", model.SchedRecurOnce, model.SchedRecurDaily, model.SchedRecurWeekly))
	}

	// AgentServerID / Model are the DESIGN-stage columns on scheduled_tasks
	// (the single-stage design task reuses them), CodingModel /
	// CodingAgentServerID are the developer stage. For task_type=coding the
	// scheduler reads Model / AgentServerID as the developer-stage values,
	// so fold the coding-side selection into them in that case — otherwise a
	// "直接开发 + 定时" plan would silently drop the picked model.
	t := &model.ScheduledTask{
		TaskType:            taskType,
		RequirementID:       req.ID,
		ProjectID:           req.ProjectID,
		RequirementTitle:    req.Title,
		RunAt:               runAt,
		Model:               spec.DesignModel,
		ReadKnowledge:       spec.ReadKnowledge,
		BranchName:          spec.BranchName,
		BaseBranch:          spec.BaseBranch,
		AgentServerID:       spec.DesignAgentServerID,
		SplitTasks:          spec.SplitTasks,
		CodingModel:         spec.CodingModel,
		CodingAgentServerID: spec.CodingAgentServerID,
		Recurrence:          recurrence,
		RecurTime:           spec.RecurTime,
		RecurDays:           spec.RecurDays,
		RecurTZ:             spec.RecurTZ,
	}
	if taskType == model.SchedTypeCoding {
		t.Model = spec.CodingModel
		t.AgentServerID = spec.CodingAgentServerID
	}
	// scheduled_tasks has no dev_mode column, but requirements DOES — and the
	// coding stage already falls back to requirements.dev_mode when the run
	// params leave it empty (wizard_coding.go: `if devMode == "" && reqRow !=
	// nil { devMode = reqRow.DevMode }`). Stamping the user's create-time
	// choice on the row is therefore enough to make it take effect whenever
	// the timer fires, with no schema change. UpdateDevMode normalizes
	// unknown values to "" (= backend default), so a malformed spec is safe.
	if spec.DevMode != "" {
		if uerr := h.svc.UpdateDevMode(req.ID, spec.DevMode); uerr != nil {
			log.Printf("[launch] UpdateDevMode %s failed: %v (ignored)", req.ID, uerr)
		}
	}
	created, err := h.schedSvc.Create(t)
	if err != nil {
		if errors.Is(err, service.ErrAlreadyScheduled) {
			// Practically unreachable for a brand-new requirement (the unique
			// pending constraint is per (requirement_id, task_type)), but map
			// it anyway so the frontend can reuse its existing message.
			return "", fail(409, "ALREADY_SCHEDULED", "该需求已存在同类型的待执行定时任务，请先取消或删除现有任务")
		}
		return "", fail(500, "INTERNAL", err.Error())
	}
	return created.ID, nil
}

// dispatchImmediateLaunch fires the work right now, through whichever wizard
// entry point taskType selected. Both branches return as soon as the
// background goroutine is spawned, so Create's HTTP response is never
// blocked on a claude run.
func (h *RequirementHandler) dispatchImmediateLaunch(req *model.Requirement, spec *model.LaunchSpec, taskType string) *apiFailure {
	if h.wizardH == nil {
		return fail(500, "INTERNAL", "wizard 服务未初始化")
	}
	if taskType == model.SchedTypeDesignCoding {
		jobID, af := h.wizardH.launchDesignAndCoding(req.ID, designCodingImmediateReq{
			ReadKnowledge:       spec.ReadKnowledge,
			DesignModel:         spec.DesignModel,
			DesignConfigID:      spec.DesignConfigID,
			DesignAgentServerID: spec.DesignAgentServerID,
			CodingModel:         spec.CodingModel,
			CodingConfigID:      spec.CodingConfigID,
			CodingAgentServerID: spec.CodingAgentServerID,
			BranchName:          spec.BranchName,
			BaseBranch:          spec.BaseBranch,
			SplitTasks:          spec.SplitTasks,
			AutoPushPR:          spec.AutoPushPR,
			DevMode:             spec.DevMode,
			SyncMode:            spec.SyncMode,
		})
		if af != nil {
			return af
		}
		log.Printf("[launch] requirement %s immediate design_and_coding dispatched design job=%s", req.ID, jobID)
		return nil
	}

	// taskType == coding: the requirement is "直接开发" (skip_design), so the
	// shared status gate promotes draft → developing and hands back the fresh
	// row we build codingRunParams from.
	gated, err := h.wizardH.gateCodingEntry(req)
	if err != nil {
		return fail(409, launchErrCodingGate, err.Error())
	}
	cp := &codingRunParams{
		RequirementID:    gated.ID,
		RequirementTitle: gated.Title,
		RequirementDesc:  gated.Description,
		BranchName:       resolveImmediateBranch(spec.BranchName, gated.ID),
		BaseBranch:       resolveImmediateBase(h.wizardH, spec.BaseBranch, gated.ProjectID),
		Model:            spec.CodingModel,
		ClaudeConfigID:   spec.CodingConfigID,
		ReadKnowledge:    spec.ReadKnowledge,
		AgentServerID:    spec.CodingAgentServerID,
		SplitTasks:       spec.SplitTasks,
		AutoPushPR:       spec.AutoPushPR,
		DevMode:          spec.DevMode,
		SyncMode:         spec.SyncMode,
	}
	jobID, err := h.wizardH.RunScheduledCoding(cp, nil)
	if err != nil {
		return fail(500, "INTERNAL", "开发阶段派发失败: "+err.Error())
	}
	// Persist coding_job_id so the detail page can attach the SSE stream on
	// arrival (and after a refresh). RunScheduledCoding intentionally leaves
	// this to its callers — the scheduler path does the same write. The
	// goroutine's terminal defer clears the column again.
	if uerr := h.svc.UpdateCodingJob(gated.ID, jobID); uerr != nil {
		log.Printf("[launch] UpdateCodingJob %s failed: %v", gated.ID, uerr)
	}
	log.Printf("[launch] requirement %s immediate coding dispatched job=%s", gated.ID, jobID)
	return nil
}
