package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// ScheduleHandler exposes the /api/schedules CRUD + lifecycle endpoints.
// Storage and the atomic Claim live in service.ScheduledTaskService; this
// handler is the thin HTTP adapter that turns request bodies into service
// args and translates service errors into the {success, error} envelope.
//
// Routes (registered in main.go):
//
//	POST   /api/schedules                create
//	GET    /api/schedules                list (filters: status, task_type, requirement_id)
//	GET    /api/schedules/{id}           get one
//	POST   /api/schedules/{id}/cancel    pending → canceled (409 otherwise)
//	DELETE /api/schedules/{id}           delete (pending must be canceled first)
//
// All routes require the `menu.projects` permission — the same gate the
// "需求" navigation uses. There is no new permission key to keep the ACL
// catalog minimal.
type ScheduleHandler struct {
	svc     *service.ScheduledTaskService
	reqSvc  *service.RequirementService
}

// NewScheduleHandler wires the schedule + requirement services. reqSvc is
// used at create-time to validate the requirement_id and (when the row
// is read back) populate project_id / requirement_title for list display.
func NewScheduleHandler(svc *service.ScheduledTaskService, reqSvc *service.RequirementService) *ScheduleHandler {
	return &ScheduleHandler{svc: svc, reqSvc: reqSvc}
}

// Create body — mirrors the schedule JSON fields the frontend sends. We
// only require requirement_id + task_type + run_at; everything else is
// optional. model == "" means "use the role default" (resolved at
// dispatch time). CodingModel / CodingAgentServerID are only consumed
// when task_type == "design_and_coding" (the merged two-stage task);
// single-stage tasks ignore them.
type createScheduleReq struct {
	RequirementID       string `json:"requirement_id"`
	TaskType            string `json:"task_type"`
	RunAt               string `json:"run_at"`
	Model               string `json:"model"`
	ReadKnowledge       bool   `json:"read_knowledge"`
	BranchName          string `json:"branch_name"`
	BaseBranch          string `json:"base_branch"`
	AgentServerID       string `json:"agent_server_id"`
	SplitTasks          bool   `json:"split_tasks"`
	CodingModel         string `json:"coding_model"`
	CodingAgentServerID string `json:"coding_agent_server_id"`
	// Recurrence fields — empty Recurrence defaults to "once" (the historical
	// one-shot behavior, which requires RunAt). For "daily"/"weekly" the
	// server derives RunAt from the rule, so RunAt may be omitted.
	Recurrence          string `json:"recurrence"`
	RecurTime           string `json:"recur_time"` // "HH:MM"
	RecurDays           string `json:"recur_days"` // CSV 0-6, 0=Sunday (weekly)
	RecurTZ             string `json:"recur_tz"`   // IANA tz name
}

// Create persists a new scheduled task. The handler validates:
//
//	run_at parses as either datetime-local ("2006-01-02T15:04") or
//	RFC3339 and is at least 30 seconds in the future
//	task_type ∈ {design, coding}; kind=idea blocks the coding path
//	requirement_id exists (404)
//	no pending row already exists for (requirement_id, task_type) (409)
func (h *ScheduleHandler) Create(w http.ResponseWriter, r *http.Request) {
	var body createScheduleReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	if body.RequirementID == "" {
		writeError(w, 400, "INVALID", "requirement_id is required")
		return
	}
	if body.TaskType != model.SchedTypeDesign && body.TaskType != model.SchedTypeCoding && body.TaskType != model.SchedTypeDesignCoding {
		writeError(w, 400, "INVALID", fmt.Sprintf("task_type must be %q, %q or %q", model.SchedTypeDesign, model.SchedTypeCoding, model.SchedTypeDesignCoding))
		return
	}
	// Normalize + validate recurrence. Empty = "once" (backward compatible).
	recurrence := body.Recurrence
	if recurrence == "" {
		recurrence = model.SchedRecurOnce
	}
	var runAt time.Time
	switch recurrence {
	case model.SchedRecurOnce:
		if body.RunAt == "" {
			writeError(w, 400, "INVALID", "run_at is required")
			return
		}
		var err error
		runAt, err = parseRunAt(body.RunAt)
		if err != nil {
			writeError(w, 400, "INVALID_RUN_AT", err.Error())
			return
		}
		if runAt.Before(time.Now().Add(30 * time.Second)) {
			writeError(w, 400, "RUN_AT_TOO_SOON", "run_at must be at least 30 seconds in the future")
			return
		}
	case model.SchedRecurDaily, model.SchedRecurWeekly:
		// The service derives run_at from the rule; validate the rule shape
		// here so the user gets a 400 rather than a 500 from NextRunAt.
		if !validRecurTime(body.RecurTime) {
			writeError(w, 400, "INVALID_RECUR_TIME", "recur_time must be HH:MM (00:00-23:59)")
			return
		}
		if recurrence == model.SchedRecurWeekly && !validRecurDays(body.RecurDays) {
			writeError(w, 400, "INVALID_RECUR_DAYS", "recur_days must be a non-empty CSV of weekday numbers 0-6 (0=Sunday)")
			return
		}
	default:
		writeError(w, 400, "INVALID", fmt.Sprintf("recurrence must be %q, %q or %q", model.SchedRecurOnce, model.SchedRecurDaily, model.SchedRecurWeekly))
		return
	}

	// Lookup requirement for existence + kind gate + denormalized fields.
	req, err := h.reqSvc.Get(body.RequirementID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, 404, "REQUIREMENT_NOT_FOUND", "requirement not found")
			return
		}
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	if (body.TaskType == model.SchedTypeCoding || body.TaskType == model.SchedTypeDesignCoding) && req.Kind == "idea" {
		writeError(w, 400, "IDEA_NOT_DEVELOPABLE", "「想法」类需求暂不支持进入开发阶段")
		return
	}

	t := &model.ScheduledTask{
		TaskType:            body.TaskType,
		RequirementID:       body.RequirementID,
		ProjectID:           req.ProjectID,
		RequirementTitle:    req.Title,
		RunAt:               runAt,
		Model:               body.Model,
		ReadKnowledge:       body.ReadKnowledge,
		BranchName:          body.BranchName,
		BaseBranch:          body.BaseBranch,
		AgentServerID:       body.AgentServerID,
		SplitTasks:          body.SplitTasks,
		CodingModel:         body.CodingModel,
		CodingAgentServerID: body.CodingAgentServerID,
		Recurrence:          recurrence,
		RecurTime:           body.RecurTime,
		RecurDays:           body.RecurDays,
		RecurTZ:             body.RecurTZ,
	}
	created, err := h.svc.Create(t)
	if err != nil {
		if errors.Is(err, service.ErrAlreadyScheduled) {
			writeError(w, 409, "ALREADY_SCHEDULED", "该需求已存在同类型的待执行定时任务，请先取消或删除现有任务")
			return
		}
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 201, created)
}

// List returns rows matching the optional filters. status and
// requirement_id are pushed to SQL; task_type is filtered in Go because
// it isn't on the indexed-column whitelist (see plan §1.4 fact 4).
func (h *ScheduleHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	taskType := q.Get("task_type")
	requirementID := q.Get("requirement_id")
	out, err := h.svc.List(status, taskType, requirementID)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, out)
}

// Get returns one row. 404 when the row doesn't exist (or was deleted
// between List and Get).
func (h *ScheduleHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := h.svc.Get(id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			writeError(w, 404, "NOT_FOUND", "scheduled task not found")
			return
		}
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, t)
}

// Cancel transitions pending → canceled. Returns 409 with a clear
// message when the row is running / succeeded / failed / canceled so the
// UI can re-render the appropriate state without a retry.
func (h *ScheduleHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := h.svc.Cancel(id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			writeError(w, 404, "NOT_FOUND", "scheduled task not found")
			return
		}
		if errors.Is(err, service.ErrNotPending) {
			writeError(w, 409, "NOT_PENDING", "任务不在 pending 状态，无法取消")
			return
		}
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	t, err := h.svc.Get(id)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, t)
}

// updateScheduleReq is the PATCH body. Every field is a pointer so an
// omitted key means "leave unchanged" — a pause toggle sends only
// {"active": false} and must not blank out the recurrence rule.
type updateScheduleReq struct {
	Recurrence *string `json:"recurrence"`
	RecurTime  *string `json:"recur_time"`
	RecurDays  *string `json:"recur_days"`
	RecurTZ    *string `json:"recur_tz"`
	Model      *string `json:"model"`
	RunAt      *string `json:"run_at"`
	Active     *bool   `json:"active"`
}

// Update patches a scheduled task: edit the recurrence rule / model, or
// pause / resume via {"active": false/true}. Rule-affecting edits cause the
// service to recompute the next run_at from the rule. Returns the refreshed
// row so the UI re-renders without a second GET.
func (h *ScheduleHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body updateScheduleReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	patch := service.ScheduledTaskPatch{
		Recurrence: body.Recurrence,
		RecurTime:  body.RecurTime,
		RecurDays:  body.RecurDays,
		RecurTZ:    body.RecurTZ,
		Model:      body.Model,
		Active:     body.Active,
	}
	// Validate the recurrence enum when provided.
	if body.Recurrence != nil {
		switch *body.Recurrence {
		case model.SchedRecurOnce, model.SchedRecurDaily, model.SchedRecurWeekly:
		default:
			writeError(w, 400, "INVALID", fmt.Sprintf("recurrence must be %q, %q or %q", model.SchedRecurOnce, model.SchedRecurDaily, model.SchedRecurWeekly))
			return
		}
	}
	if body.RecurTime != nil && *body.RecurTime != "" && !validRecurTime(*body.RecurTime) {
		writeError(w, 400, "INVALID_RECUR_TIME", "recur_time must be HH:MM (00:00-23:59)")
		return
	}
	if body.RecurDays != nil && *body.RecurDays != "" && !validRecurDays(*body.RecurDays) {
		writeError(w, 400, "INVALID_RECUR_DAYS", "recur_days must be a CSV of weekday numbers 0-6 (0=Sunday)")
		return
	}
	if body.RunAt != nil && *body.RunAt != "" {
		rt, err := parseRunAt(*body.RunAt)
		if err != nil {
			writeError(w, 400, "INVALID_RUN_AT", err.Error())
			return
		}
		patch.RunAt = &rt
	}
	updated, err := h.svc.Update(id, patch)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			writeError(w, 404, "NOT_FOUND", "scheduled task not found")
			return
		}
		// Update's non-DB errors are rule-validation failures surfaced from
		// service.NextRunAt; return them as a 400 so the client can correct.
		writeError(w, 400, "INVALID", err.Error())
		return
	}
	writeJSON(w, 200, updated)
}

// Delete removes the row outright regardless of status — pending,
// running, succeeded, failed and canceled rows all delete. The earlier
// "pending must be canceled first" gate was removed because the
// scheduler's atomic Claim (`UPDATE ... WHERE id = ? AND status = ?`)
// already guards the race: if a pending row is deleted between the
// scheduler's Due() scan and its Claim(), Claim() sees 0 rows affected
// and the dispatcher skips it. Forbidding delete on pending only pushed
// the user into an awkward two-step dance with no real safety benefit.
func (h *ScheduleHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := h.svc.Delete(id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			writeError(w, 404, "NOT_FOUND", "scheduled task not found")
			return
		}
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"id": id, "status": "deleted"})
}

// parseRunAt accepts either the HTML datetime-local format
// "2006-01-02T15:04" (no timezone) or a full RFC3339 string. The RFC3339
// path is the canonical / bug-safe one: it includes a timezone offset so
// the absolute moment is unambiguous regardless of where the server runs.
// The frontend always sends RFC3339 (ScheduleModal converts the
// datetime-local picker value to "...T23:30:00+08:00" via
// toRFC3339Local before POSTing), which means RFC3339 is what we expect
// in practice. The datetime-local fallback is kept for backward
// compatibility — but be aware: it is interpreted as *server local*
// time, so a bare datetime-local string from a client in a different TZ
// than the server will be wrong by that offset. Prefer sending RFC3339.
func parseRunAt(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", s, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("无法解析时间 %q（期望 RFC3339 或 YYYY-MM-DDTHH:MM）", s)
}

// validRecurTime reports whether s is a well-formed "HH:MM" 24-hour string.
// Mirrors the parse the service does in NextRunAt so the handler can reject
// bad input with a 400 before it reaches the service layer.
func validRecurTime(s string) bool {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return false
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || h < 0 || h > 23 {
		return false
	}
	m, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || m < 0 || m > 59 {
		return false
	}
	return true
}

// validRecurDays reports whether s is a non-empty CSV of weekday numbers in
// [0,6] (0=Sunday, JS getDay convention). Used to validate weekly rules.
func validRecurDays(s string) bool {
	found := false
	for _, raw := range strings.Split(s, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		d, err := strconv.Atoi(raw)
		if err != nil || d < 0 || d > 6 {
			return false
		}
		found = true
	}
	return found
}