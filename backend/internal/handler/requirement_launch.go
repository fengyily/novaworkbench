// Package handler — requirement creation dispatch.
//
// requirement_launch.go is the single backend file that materializes the
// "启动计划" introduced on the create-requirement form. When Create
// (requirement.go) decodes a non-nil LaunchSpec in the request body, it
// calls dispatchLaunch immediately after the requirement row is inserted
// and lets this file decide which of three concrete dispatches to fire:
//
//	1. scheduled               → assemble a model.ScheduledTask and call
//	                             schedSvc.Create
//	2. immediate + design_and_coding → prepare + go execArchitectDesign
//	                             (immediateDesignCallback chains into
//	                             RunScheduledCoding on success)
//	3. immediate + coding      → stamp status='developing' + RunScheduledCoding
//
// resolveLaunchTaskType enforces the flow → taskType mapping documented
// in requirements.design_docs ("flow → 启动类型映射"):
//
//	flow           | skip_analysis | skip_design | immediate            | scheduled
//	--------------- | ------------- | ----------- | -------------------- | ----------------------
//	direct         | true          | true        | coding               | coding
//	skip-analysis  | true          | false       | design_and_coding    | design_and_coding
//	full           | false         | false       | 400 LAUNCH_NEEDS_ANALYSIS | design_and_coding
//	idea           | any           | any         | 400 LAUNCH_NOT_ALLOWED    | 400 LAUNCH_NOT_ALLOWED
//
// "full + scheduled" is INTENTIONALLY allowed: the user may complete the
// analyst stage before the scheduled tick fires. If they don't, the row
// lands in `failed` with the architect-stage NO_SESSION message — the
// existing scheduled-task failure surface. The frontend surfaces this in
// a hint under the "定时执行" radio when flow=="full".
//
// Idea kind is rejected outright (mirrors wizard_immediate.go's
// immediatePreGate and schedule.go's Create idea check) so the
// LAUNCH_NOT_ALLOWED code stays a single-source-of-truth for both
// create-time and re-dispatch errors.
package handler

import (
	"errors"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// Error codes for the launch-spec path. Distinct from wizard_immediate.go's
// IDEA_NOT_DEVELOPABLE / NO_SESSION codes so the frontend can map them
// onto the new "启动计划" block's copy ("完整流程需先完成分析" / "idea 暂不
// 支持启动") without forking the existing translation keys. The schedule
// path also reuses schedule.go's existing 400 / 409 codes for the
// per-field validation messages so callers see a single vocabulary.
const (
	launchErrNotAllowed       = "LAUNCH_NOT_ALLOWED"
	launchErrNeedsAnalysis    = "LAUNCH_NEEDS_ANALYSIS"
	launchErrAlreadyScheduled = "ALREADY_SCHEDULED"
)

// resolveLaunchTaskType maps (req.Kind, req.SkipAnalysis, req.SkipDesign,
// mode) → a scheduled_tasks.task_type value, returning a 4xx *apiFailure
// for the two intentionally-rejected combos (idea / full + immediate).
//
// taskType is what scheduled_tasks and the immediate dispatcher's inner
// switch consume; the caller picks the right body shape per taskType. See
// the file header for the full mapping table.
func resolveLaunchTaskType(req *model.Requirement, mode string) (string, *apiFailure) {
	if req == nil {
		return "", fail(400, "INVALID", "requirement is required")
	}
	if req.Kind == service.KindIdea {
		return "", fail(400, launchErrNotAllowed, "idea 类型需求不支持启动计划")
	}
	// Coding-only when SkipDesign is set — the "直接开发" flow.
	if req.SkipDesign {
		return model.SchedTypeCoding, nil
	}
	// design_and_coding otherwise — covers both skip-analysis (explicit)
	// and full-flow-scheduled (analysis completes before tick).
	if req.SkipAnalysis {
		return model.SchedTypeDesignCoding, nil
	}
	// Full flow: scheduled path is allowed, immediate path needs a session.
	if mode == "scheduled" {
		return model.SchedTypeDesignCoding, nil
	}
	return "", fail(400, launchErrNeedsAnalysis, "完整流程需求需先完成需求分析才能立即执行方案")
}

// dispatchLaunch executes the appropriate dispatch for a freshly-created
// requirement that carried a non-nil LaunchSpec. It is the single entry
// point Create's launch branch invokes (see requirement.go's Create).
//
// Returns:
//   - mode: "immediate" or "scheduled" — surfaced to the response as the
//     display-only Requirement.LaunchMode.
//   - scheduleID: the scheduled_tasks.id when mode == "scheduled"; empty
//     for immediate dispatches (their job id is persisted onto the
//     requirement row by prepareArchitectDesign / RunScheduledCoding).
//   - af: a non-nil *apiFailure when dispatch failed. The caller is
//     REQUIRED to surface the failure as a 201-with-launch_error (Create
//     has already inserted the row — rolling back would lose the user's
//     input). See requirement.go for the launch_error wrapping.
//
// taskType comes from resolveLaunchTaskType; failures there are passed
// through verbatim so the frontend sees one consistent error code per
// reject reason.
func dispatchLaunch(req *model.Requirement, spec *model.LaunchSpec, h *WizardHandler, schedSvc *service.ScheduledTaskService) (mode string, scheduleID string, af *apiFailure) {
	if spec == nil {
		return "", "", fail(400, "INVALID", "launch spec is required")
	}
	if h == nil {
		return "", "", fail(500, "INTERNAL", "wizard handler unavailable")
	}
	if req == nil {
		return "", "", fail(400, "INVALID", "requirement is required")
	}

	switch spec.Mode {
	case "immediate":
		taskType, failure := resolveLaunchTaskType(req, "immediate")
		if failure != nil {
			return "", "", failure
		}
		// pre-gate mirrors wizard_immediate.go's HTTP entry check: status
		// must be one of the wizard-allowed set, no live design/coding job
		// may already exist, and kind != "idea". Required because
		// resolveLaunchTaskType only inspects req.Kind/SkipAnalysis/
		// SkipDesign — concurrent JobStore jobs and terminal statuses are
		// orthogonal to the dispatch mapping.
		if gate := h.immediatePreGate(req); gate != nil {
			return "", "", gate
		}
		switch taskType {
		case model.SchedTypeDesignCoding:
			return dispatchImmediateDesignAndCoding(req, spec, h)
		case model.SchedTypeCoding:
			return dispatchImmediateCoding(req, spec, h)
		default:
			return "", "", fail(500, "INTERNAL", "unexpected launch taskType: "+taskType)
		}
	case "scheduled":
		return dispatchScheduled(req, spec, schedSvc)
	default:
		return "", "", fail(400, "INVALID", "launch.mode must be one of immediate/scheduled")
	}
}

// dispatchImmediateDesignAndCoding handles the immediate / design_and_coding
// path. It delegates to WizardHandler.launchDesignAndCoding — the same
// helper the HTTP /api/wizard/requirements/{id}/design-and-coding entry
// point uses — so both paths share the chain
//
//	immediatePreGate → prepareArchitectDesign → execArchitectDesign (goroutine)
//	                   ↓ on success → immediateDesignCallback
//	                   ↓           → RunScheduledCoding (coding stage)
//
// in exactly one place. Failure codes are the immediateErr* constants
// from wizard_immediate.go, so the frontend's existing translation keys
// keep working when the same error is surfaced from the create flow.
func dispatchImmediateDesignAndCoding(req *model.Requirement, spec *model.LaunchSpec, h *WizardHandler) (string, string, *apiFailure) {
	body := designCodingImmediateReq{
		ReadKnowledge:       spec.ReadKnowledge,
		DesignModel:         spec.DesignModel,
		DesignConfigID:      spec.DesignClaudeConfigID,
		DesignAgentServerID: spec.DesignAgentServerID,
		CodingModel:         spec.CodingModel,
		CodingConfigID:      spec.CodingClaudeConfigID,
		CodingAgentServerID: spec.CodingAgentServerID,
		BranchName:          spec.BranchName,
		BaseBranch:          spec.BaseBranch,
		SplitTasks:          spec.SplitTasks,
		AutoPushPR:          spec.AutoPushPR,
		DevMode:             spec.DevMode,
		SyncMode:            spec.SyncMode,
	}
	if _, af := h.launchDesignAndCoding(req.ID, &body); af != nil {
		return "", "", af
	}
	return "immediate", "", nil
}

// dispatchImmediateCoding handles the immediate / coding path. It
// delegates the development-stage gate to WizardHandler.gateCodingEntry
// — the same gate ScheduledExecutor.RunScheduledCoding uses (after the
// "抽取共享门禁" refactor) — so a future edit can't accidentally
// diverge the two paths' allowed-status set, leaving one path runnable
// while the other 409s. gateCodingEntry also stamps status='developing'
// (the same fix schedule_executor applied for req_30080193f1c95255),
// so this function only needs to assemble codingRunParams and call
// RunScheduledCoding with a nil callback (Create's "launched at
// creation time" semantics — no scheduled_tasks row to flip on finish).
//
// Title / Desc / ProjectPath are read by execStartCoding from the
// requirement row itself (mirrors schedule_executor.go:122-136), and
// ProjectPath is filled in by resolveCodingProjectPath when the row is
// loaded — so this caller does not need to pre-fill them.
func dispatchImmediateCoding(req *model.Requirement, spec *model.LaunchSpec, h *WizardHandler) (string, string, *apiFailure) {
	updated, af := h.gateCodingEntry(req)
	if af != nil {
		return "", "", af
	}
	_ = updated // gateCodingEntry already promoted status + reloaded; execStartCoding reads the latest row again
	cp := &codingRunParams{
		RequirementID:  req.ID,
		Model:          spec.CodingModel,
		ClaudeConfigID: spec.CodingClaudeConfigID,
		ReadKnowledge:  spec.ReadKnowledge,
		AgentServerID:  spec.CodingAgentServerID,
		SplitTasks:     spec.SplitTasks,
		AutoPushPR:     spec.AutoPushPR,
		DevMode:        spec.DevMode,
		SyncMode:       spec.SyncMode,
		BranchName:     spec.BranchName,
		BaseBranch:     spec.BaseBranch,
	}
	if _, err := h.RunScheduledCoding(cp, nil); err != nil {
		return "", "", fail(500, "INTERNAL", "启动开发失败: "+err.Error())
	}
	return "immediate", "", nil
}

// dispatchScheduled handles the scheduled path: assemble a
// model.ScheduledTask, validate the recurrence block using the same
// helpers schedule.go's Create uses (parseRunAt / validRecurTime /
// validRecurDays — same package, unexported, intentionally reused so the
// 400 codes stay identical), then call schedSvc.Create.
//
// The kind-idea check is defense-in-depth: resolveLaunchTaskType already
// rejects idea with LAUNCH_NOT_ALLOWED. The schedule.go Create handler
// rejects with IDEA_NOT_DEVELOPABLE. We mirror the latter so an error
// surfaced from this path reads identically to one surfaced from the
// dedicated POST /api/schedules endpoint.
func dispatchScheduled(req *model.Requirement, spec *model.LaunchSpec, schedSvc *service.ScheduledTaskService) (string, string, *apiFailure) {
	if schedSvc == nil {
		return "", "", fail(500, "INTERNAL", "scheduled service unavailable")
	}
	taskType, failure := resolveLaunchTaskType(req, "scheduled")
	if failure != nil {
		return "", "", failure
	}
	if (taskType == model.SchedTypeCoding || taskType == model.SchedTypeDesignCoding) && req.Kind == service.KindIdea {
		return "", "", fail(400, "IDEA_NOT_DEVELOPABLE", "「想法」类需求暂不支持进入开发阶段")
	}
	if spec.Schedule == nil {
		return "", "", fail(400, "INVALID", "schedule block is required for scheduled launch")
	}
	recurrence := spec.Schedule.Recurrence
	if recurrence == "" {
		recurrence = model.SchedRecurOnce
	}
	var runAt time.Time
	switch recurrence {
	case model.SchedRecurOnce:
		if spec.Schedule.RunAt == "" {
			return "", "", fail(400, "INVALID", "schedule.run_at is required")
		}
		rt, err := parseRunAt(spec.Schedule.RunAt)
		if err != nil {
			return "", "", fail(400, "INVALID_RUN_AT", err.Error())
		}
		if rt.Before(time.Now().Add(30 * time.Second)) {
			return "", "", fail(400, "RUN_AT_TOO_SOON", "run_at must be at least 30 seconds in the future")
		}
		runAt = rt
	case model.SchedRecurDaily, model.SchedRecurWeekly:
		if !validRecurTime(spec.Schedule.RecurTime) {
			return "", "", fail(400, "INVALID_RECUR_TIME", "recur_time must be HH:MM (00:00-23:59)")
		}
		if recurrence == model.SchedRecurWeekly && !validRecurDays(spec.Schedule.RecurDays) {
			return "", "", fail(400, "INVALID_RECUR_DAYS", "recur_days must be a non-empty CSV of weekday numbers 0-6 (0=Sunday)")
		}
	default:
		return "", "", fail(400, "INVALID", "recurrence must be one of once/daily/weekly")
	}
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
		RecurTime:           spec.Schedule.RecurTime,
		RecurDays:           spec.Schedule.RecurDays,
		RecurTZ:             spec.Schedule.RecurTZ,
	}
	created, err := schedSvc.Create(t)
	if err != nil {
		if errors.Is(err, service.ErrAlreadyScheduled) {
			return "", "", fail(409, launchErrAlreadyScheduled, "该需求已存在同类型的待执行定时任务，请先取消或删除现有任务")
		}
		return "", "", fail(500, "INTERNAL", err.Error())
	}
	return "scheduled", created.ID, nil
}
