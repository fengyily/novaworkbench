package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
// dispatch time).
type createScheduleReq struct {
	RequirementID    string `json:"requirement_id"`
	TaskType         string `json:"task_type"`
	RunAt            string `json:"run_at"`
	Model            string `json:"model"`
	ReadKnowledge    bool   `json:"read_knowledge"`
	BranchName       string `json:"branch_name"`
	BaseBranch       string `json:"base_branch"`
	AgentServerID    string `json:"agent_server_id"`
	SplitTasks       bool   `json:"split_tasks"`
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
	if body.TaskType != model.SchedTypeDesign && body.TaskType != model.SchedTypeCoding {
		writeError(w, 400, "INVALID", fmt.Sprintf("task_type must be %q or %q", model.SchedTypeDesign, model.SchedTypeCoding))
		return
	}
	if body.RunAt == "" {
		writeError(w, 400, "INVALID", "run_at is required")
		return
	}
	runAt, err := parseRunAt(body.RunAt)
	if err != nil {
		writeError(w, 400, "INVALID_RUN_AT", err.Error())
		return
	}
	if runAt.Before(time.Now().Add(30 * time.Second)) {
		writeError(w, 400, "RUN_AT_TOO_SOON", "run_at must be at least 30 seconds in the future")
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
	if body.TaskType == model.SchedTypeCoding && req.Kind == "idea" {
		writeError(w, 400, "IDEA_NOT_DEVELOPABLE", "「想法」类需求暂不支持进入开发阶段")
		return
	}

	t := &model.ScheduledTask{
		TaskType:         body.TaskType,
		RequirementID:    body.RequirementID,
		ProjectID:        req.ProjectID,
		RequirementTitle: req.Title,
		RunAt:            runAt,
		Model:            body.Model,
		ReadKnowledge:    body.ReadKnowledge,
		BranchName:       body.BranchName,
		BaseBranch:       body.BaseBranch,
		AgentServerID:    body.AgentServerID,
		SplitTasks:       body.SplitTasks,
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