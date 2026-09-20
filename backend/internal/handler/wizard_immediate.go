// Package handler — wizard pipeline: "立即生成方案并开发" (immediate
// design-and-coding) endpoint.
//
// This file is the HTTP entry point for the user-driven counterpart of the
// scheduled `design_and_coding` task type. It mirrors
// ScheduledExecutor.RunScheduledDesignAndCoding (schedule_executor.go:269)
// and chains the architect-design → start-coding wizard stages in the same
// goroutine, but unlike the scheduled path it does NOT write any
// scheduled_tasks row — both stages only mutate the requirements row via
// reqSvc.UpdateStatus / reqSvc.UpdateCodingJob. The chainedDesignCallback /
// chainedCodingCallback here are bespoke (not the scheduler's) because the
// scheduler closures close over schedID and always touch scheduled_tasks;
// immediate has no row to flip.
//
// Lifecycle of an immediate run:
//
//	preGate → prepareArchitectDesign → execArchitectDesign (goroutine)
//	  ↓ on success → UpdateStatus('designed')
//	  ↓ construct codingRunParams → RunScheduledCoding → execStartCoding (goroutine)
//	  ↓ on success → UpdateStatus('developing')  (coding_job_id cleared by execStartCoding defer)
//
// Both stages are tracked by JobStore jobs so the frontend can attach SSE
// streams via GET /api/wizard/jobs/{id}/stream. design_job_id is persisted
// by prepareArchitectDesign; coding_job_id is persisted by execStartCoding's
// prologue — RequirementDetail's existing activeSchedJobId poller picks up
// the coding stream automatically without any frontend code change.
package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// Error codes for the immediate design-and-coding endpoint. They mirror the
// schedule_executor.go convention (TERMINAL / *JOB_ACTIVE / INVALID_STATUS
// are uppercase code constants used both here and in the scheduler path)
// so the frontend can reuse existing translation keys when the underlying
// cause is identical. IDEA_NOT_DEVELOPABLE stays at 400 because the kind
// check is pre-emptive — the requirement isn't allowed to enter the
// pipeline at all.
const (
	immediateErrTerminal            = "TERMINAL"
	immediateErrDesignJobActive     = "DESIGN_JOB_ACTIVE"
	immediateErrCodingJobActive     = "CODING_JOB_ACTIVE"
	immediateErrInvalidStatus       = "INVALID_STATUS"
	immediateErrIdeaNotDevelopable  = "IDEA_NOT_DEVELOPABLE"
	immediateErrMissingID           = "INVALID"
	immediateErrInvalidJSON         = "INVALID"
	immediateErrRequirementNotFound = "NOT_FOUND"
)

// designCodingImmediateReq is the JSON body for POST
// /api/wizard/requirements/{id}/design-and-coding. Field semantics mirror
// the scheduled_tasks table — design_* and coding_* hold the per-stage
// overrides, the rest are shared. *bool on AutoPushPR preserves the
// requirement row's persisted value when the field is omitted (mirrors
// codingRunParams' AutoPushPR semantics; legacy clients that POST without
// the field stay on the row's last-set intent).
type designCodingImmediateReq struct {
	ReadKnowledge       bool   `json:"read_knowledge"`
	DesignModel         string `json:"design_model"`
	DesignConfigID      string `json:"design_claude_config_id"`
	DesignAgentServerID string `json:"design_agent_server_id"`
	CodingModel         string `json:"coding_model"`
	CodingConfigID      string `json:"coding_claude_config_id"`
	CodingAgentServerID string `json:"coding_agent_server_id"`
	BranchName          string `json:"branch_name"`
	BaseBranch          string `json:"base_branch"`
	SplitTasks          bool   `json:"split_tasks"`
	AutoPushPR          *bool  `json:"auto_push_pr"`
	DevMode             string `json:"dev_mode"`
	SyncMode            string `json:"sync_mode"`
}

// RunImmediateDesignAndCoding is the HTTP entry point that chains the
// architect-design and start-coding wizard stages back-to-back, with
// per-stage model / Claude-config / Agent-server / branch / split_tasks
// overrides. Returns the design-stage JobStore job id; the frontend
// attaches SSE via GET /api/wizard/jobs/{id}/stream and the chained
// coding stage's job id is picked up by the existing
// requirements.coding_job_id poll loop — no client-side coordination
// beyond the initial POST is needed.
//
// Pre-gate (all synchronous, before any goroutine fires):
//
//	status ∈ {done, archived}                → 409 TERMINAL
//	jobs.Live(requirements.design_job_id)    → 409 DESIGN_JOB_ACTIVE
//	jobs.Live(requirements.coding_job_id)    → 409 CODING_JOB_ACTIVE
//	requirements.kind == "idea"              → 400 IDEA_NOT_DEVELOPABLE
//	status ∉ {draft, designing, designed, developing}
//	                                         → 409 INVALID_STATUS
//
// POST /api/wizard/requirements/{id}/design-and-coding → 200 {design_job_id}
func (h *WizardHandler) RunImmediateDesignAndCoding(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, 400, immediateErrMissingID, "missing requirement id")
		return
	}

	var body designCodingImmediateReq
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, 400, immediateErrInvalidJSON, "Invalid JSON: "+err.Error())
			return
		}
	}

	jobID, af := h.launchDesignAndCoding(id, &body)
	if af != nil {
		writeIfAPIError(w, af)
		return
	}
	writeJSON(w, 200, map[string]string{"design_job_id": jobID})
}

// launchDesignAndCoding is the shared dispatch for an immediate
// design+coding run, reused by both the HTTP handler
// (RunImmediateDesignAndCoding above) and Create's launch spec
// (requirement_launch.go dispatchLaunch → immediate + design_and_coding
// branch). Pulled out so the chain
//
//	immediatePreGate → prepareArchitectDesign → execArchitectDesign (goroutine)
//
// is defined exactly once — the HTTP path and the launch-spec path both
// invoke this helper, so a future edit can't accidentally diverge them.
//
// Returns the JobStore design-stage job id; the chained coding-stage
// job id is picked up by RequirementDetail's existing activeSchedJobId
// poll loop without any frontend change (wizard_immediate.go prologue).
//
// Returns *apiFailure on any pre-flight rejection; the HTTP caller maps
// the failure verbatim via writeIfAPIError, and the launch-spec caller
// surfaces it through Create's launch_error display field. Error codes
// are the immediateErr* constants defined above so the frontend's
// existing translation keys keep working in both paths.
func (h *WizardHandler) launchDesignAndCoding(reqID string, body *designCodingImmediateReq) (string, *apiFailure) {
	reqRow, err := h.reqSvc.Get(reqID)
	if err != nil {
		return "", fail(404, immediateErrRequirementNotFound, "requirement not found")
	}
	if af := h.immediatePreGate(reqRow); af != nil {
		return "", af
	}
	// Prepare the architect stage synchronously — same prepare step the
	// HTTP /api/wizard/architect-design and the scheduler path share, so
	// validation (NO_SESSION, UNANCHORED_SESSION, WORKTREE_FAILED, …)
	// is identical. prepareArchitectDesign mints the JobStore job and
	// persists design_job_id on the requirement row before we return, so
	// a second concurrent POST hits DESIGN_JOB_ACTIVE on the next call.
	p, job, af := h.prepareArchitectDesign(context.Background(), reqID, body.DesignModel, body.DesignConfigID, body.DesignAgentServerID, body.ReadKnowledge)
	if af != nil {
		return "", af
	}
	cb := h.immediateDesignCallback(reqID, *body)
	go h.execArchitectDesign(p, job, cb)
	return job.ID, nil
}

// immediatePreGate centralizes the synchronous state checks before the
// design goroutine fires. A non-nil *apiFailure means the request was
// rejected; the caller writes it verbatim to the HTTP response.
//
// The Requirement struct does not carry CodingJobID (the column exists
// in DB but was never added to the model — see wizard_coding.go
// prologue), so coding_job_id is read via an inline column query.
// Empty result → no live coding job → guard is a no-op (matches the
// existing schedule_executor behavior).
func (h *WizardHandler) immediatePreGate(reqRow *model.Requirement) *apiFailure {
	switch reqRow.Status {
	case "done", "archived":
		return fail(409, immediateErrTerminal, "需求已完成/归档，无法立即触发")
	}
	if reqRow.DesignJobID != "" && h.jobs.Live(reqRow.DesignJobID) {
		return fail(409, immediateErrDesignJobActive, "已有方案任务在执行，请稍后再试")
	}
	codingJobID, _ := h.lookupCodingJobID(reqRow.ID)
	if codingJobID != "" && h.jobs.Live(codingJobID) {
		return fail(409, immediateErrCodingJobActive, "已有开发任务在执行，请稍后再试")
	}
	if reqRow.Kind == service.KindIdea {
		return fail(400, immediateErrIdeaNotDevelopable, "「想法」类需求暂不支持立即生成方案并开发，请先在详情页点击「📋 转为需求」升级")
	}
	switch reqRow.Status {
	case "draft", "designing", "designed", "developing":
		// allowed set; fall through
	default:
		return fail(409, immediateErrInvalidStatus, "当前需求状态不支持立即触发（请等待分析阶段完成，当前状态: "+reqRow.Status+"）")
	}
	return nil
}

// lookupCodingJobID reads the requirements.coding_job_id column directly.
// The Requirement struct does not expose this field (it's a DB-only column
// added in alterColumns) — we keep it inline rather than adding a struct
// field + Get/List plumbing just for this gate check.
func (h *WizardHandler) lookupCodingJobID(reqID string) (string, error) {
	var codingJobID string
	err := h.db.QueryRow("SELECT coding_job_id FROM requirements WHERE id = ?", reqID).Scan(&codingJobID)
	return codingJobID, err
}

// immediateDesignCallback builds the OnFinish closure for the architect
// stage of an immediate run. On success it:
//
//  1. Promotes the requirement to status='designed' so the status chip
//     reflects the architect stage's outcome before the coding stage
//     starts its background job (display-only; RunScheduledCoding also
//     proactively writes 'developing' on entry per the req_30080193f1c95255
//     post-mortem).
//  2. Re-reads the requirement to pick up the latest title / description
//     in case the user edited them between the click and the callback.
//  3. Builds a codingRunParams from the developer-side fields of the
//     request body, resolving branch / base via the helpers below, and
//     calls RunScheduledCoding with immediateCodingCallback.
//
// On design failure it returns silently — the wizard exec body has
// already surfaced the error via the design JobStore job's terminal
// state, and there is no scheduled_tasks row to flip.
func (h *WizardHandler) immediateDesignCallback(reqID string, body designCodingImmediateReq) *runCallbacks {
	return &runCallbacks{
		OnFinish: func(designJobID string, ok bool) {
			if !ok {
				log.Printf("[immediate-design-coding] design stage failed for %s job=%s — not dispatching coding", reqID, designJobID)
				return
			}
			// Idempotent stamp: only write 'designed' when the row isn't already
			// there. Architect may be re-run on a row that's already 'designed'
			// (e.g. user previously designed but never coded, then chose
			// 立即执行) — the state machine rejects designed -> designed.
			if cur, gerr := h.reqSvc.Get(reqID); gerr == nil && cur != nil && cur.Status != "designed" {
				if _, err := h.reqSvc.UpdateStatus(reqID, "designed"); err != nil {
					log.Printf("[immediate-design-coding] UpdateStatus designed for %s failed: %v", reqID, err)
				}
			}
			req, err := h.reqSvc.Get(reqID)
			if err != nil {
				log.Printf("[immediate-design-coding] re-Get %s failed: %v", reqID, err)
				return
			}
			cp := &codingRunParams{
				RequirementID:    reqID,
				RequirementTitle: req.Title,
				RequirementDesc:  req.Description,
				BranchName:       resolveImmediateBranch(body.BranchName, reqID),
				BaseBranch:       resolveImmediateBase(h, body.BaseBranch, req.ProjectID),
				Model:            body.CodingModel,
				ClaudeConfigID:   body.CodingConfigID,
				ReadKnowledge:    body.ReadKnowledge,
				AgentServerID:    body.CodingAgentServerID,
				SplitTasks:       body.SplitTasks,
				AutoPushPR:       body.AutoPushPR,
				DevMode:          body.DevMode,
				SyncMode:         body.SyncMode,
			}
			codingJobID, cerr := h.RunScheduledCoding(cp, h.immediateCodingCallback(reqID))
			if cerr != nil {
				log.Printf("[immediate-design-coding] dispatch coding for %s failed: %v", reqID, cerr)
				return
			}
			log.Printf("[immediate-design-coding] dispatched coding stage for %s (design job=%s coding job=%s)", reqID, designJobID, codingJobID)
		},
	}
}

// immediateCodingCallback builds the OnFinish closure for the coding
// stage of an immediate run. On success it stamps status='developing'
// — the architect-stage callback's 'designed' stamp is overwritten here
// for the same reason RunScheduledCoding does it (see wizard_coding.go:
// preempts the "designed but coding actually finished" stuck-chip
// symptom). On failure it returns silently; the coding exec body has
// already surfaced the failure on the coding JobStore job.
//
// coding_job_id clearing is handled by execStartCoding's defer
// (wizard_coding.go:140-149) — we intentionally do NOT call
// UpdateCodingJob here to avoid double-writing the same column.
func (h *WizardHandler) immediateCodingCallback(reqID string) *runCallbacks {
	return &runCallbacks{
		OnFinish: func(codingJobID string, ok bool) {
			if !ok {
				log.Printf("[immediate-design-coding] coding stage failed for %s job=%s", reqID, codingJobID)
				return
			}
			if _, err := h.reqSvc.UpdateStatus(reqID, "developing"); err != nil {
				log.Printf("[immediate-design-coding] UpdateStatus developing for %s failed: %v", reqID, err)
				return
			}
			log.Printf("[immediate-design-coding] coding stage finished for %s job=%s status=developing", reqID, codingJobID)
		},
	}
}

// resolveImmediateBranch returns the explicit body field when set, else
// falls back to "feat/<id stripped of req_ prefix>" — the convention used
// by the ScheduleModal / wizard detail page when the user doesn't pin a
// branch name. Kept as a package-private helper (rather than a method on
// WizardHandler) so it's cheap to call from both the immediate callback
// and any future caller.
func resolveImmediateBranch(explicit, reqID string) string {
	if explicit != "" {
		return explicit
	}
	return "feat/" + strings.TrimPrefix(reqID, "req_")
}

// resolveImmediateBase returns the explicit body field when set, else
// falls back to the project's stored DefaultBranch, else "main" — same
// precedence chain execStartCoding uses inline (wizard_coding.go:200-216).
func resolveImmediateBase(h *WizardHandler, explicit, projectID string) string {
	if explicit != "" {
		return explicit
	}
	if projectID != "" {
		if proj, err := h.projectSvc.Get(projectID); err == nil && proj != nil && proj.DefaultBranch != "" {
			return proj.DefaultBranch
		}
	}
	return "main"
}
