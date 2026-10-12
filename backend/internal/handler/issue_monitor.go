package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/platform"
	"github.com/novaworkbench/backend/internal/service"
)

// IssueMonitorHandler is the HTTP layer for the per-project "auto-import
// GitHub Issues" setting. Five routes:
//
//	GET    /api/projects/{id}/issue-monitor           → fetch one (404 when absent)
//	PUT    /api/projects/{id}/issue-monitor           → upsert
//	DELETE /api/projects/{id}/issue-monitor           → delete (idempotent)
//	POST   /api/projects/{id}/issue-monitor/test      → preview 5 matches without import
//	GET    /api/issue-monitors/active                 → ops visibility (all active monitors)
//
// Concurrency: PUT is upsert-by-project, so the handler never races
// against itself; the scheduler's queue holds per-monitor mutex so even
// a PUT arriving mid-tick is safe.
type IssueMonitorHandler struct {
	svc        *service.IssueMonitorService
	projectSvc *service.ProjectService
}

func NewIssueMonitorHandler(svc *service.IssueMonitorService, projectSvc *service.ProjectService) *IssueMonitorHandler {
	return &IssueMonitorHandler{svc: svc, projectSvc: projectSvc}
}

// Get returns the configured monitor row for a project, or 404 when none
// exists. The frontend's PUT flow seeds a blank form when it sees 404.
func (h *IssueMonitorHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "MISSING_ID", "project id missing")
		return
	}
	mon, err := h.svc.GetByProject(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LOOKUP_FAILED", err.Error())
		return
	}
	if mon == nil {
		writeError(w, http.StatusNotFound, "NOT_CONFIGURED", "尚未配置 Issue 监控")
		return
	}
	writeJSON(w, http.StatusOK, mon)
}

// PutRequest is the JSON body for PUT. Validation lives in the service
// layer (IssueMonitorService.normalize) so the UI never sees a
// platform-specific error message — the frontend relies on the API
// envelope's success flag.
type PutRequest struct {
	Label string `json:"label"`

	// 执行方式 / 开发流程
	LaunchMode string `json:"launch_mode"`
	SkipDesign bool   `json:"skip_design"`

	// 分阶段配置 — '' claude_config_id = global default config,
	// '' agent_server_id = local execution.
	DesignModel          string `json:"design_model"`
	DesignClaudeConfigID string `json:"design_claude_config_id"`
	DesignAgentServerID  string `json:"design_agent_server_id"`
	CodingModel          string `json:"coding_model"`
	CodingClaudeConfigID string `json:"coding_claude_config_id"`
	CodingAgentServerID  string `json:"coding_agent_server_id"`

	// 时间窗 (launch_mode=scheduled only)
	Recurrence string `json:"recurrence"`
	RecurTime  string `json:"recur_time"`
	RecurDays  string `json:"recur_days"`
	RecurTZ    string `json:"recur_tz"`

	AutoPush        bool `json:"auto_push"`
	CommentOnPR     bool `json:"comment_on_pr"`
	IntervalMinutes int  `json:"interval_minutes"`
	// MaxConcurrentLaunches caps how many imported-by-this-monitor
	// requirements may be in the wizard pipeline at once. Default 1
	// (conservative — set in Put when the field is 0). Excess imports
	// queue in issue_monitor_pending_launches until a slot frees.
	MaxConcurrentLaunches int  `json:"max_concurrent_launches"`
	Active                bool `json:"active"`
}

// Put upserts the monitor row for a project.
func (h *IssueMonitorHandler) Put(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "MISSING_ID", "project id missing")
		return
	}
	// 404 when the project itself is missing — without this guard a
	// malformed id would 500 deeper inside the platform layer.
	if _, err := h.projectSvc.Get(id); err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}
	var body PutRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	if body.LaunchMode == "" {
		body.LaunchMode = model.LaunchModeCreateOnly
	}
	if body.Recurrence == "" {
		body.Recurrence = model.SchedRecurDaily
	}
	if body.RecurTime == "" {
		body.RecurTime = "09:00"
	}
	if body.IntervalMinutes == 0 {
		body.IntervalMinutes = 5
	}
	if body.MaxConcurrentLaunches == 0 {
		body.MaxConcurrentLaunches = 1
	}
	mon := &model.IssueMonitor{
		ProjectID:             id,
		Label:                 body.Label,
		LaunchMode:            body.LaunchMode,
		SkipDesign:            body.SkipDesign,
		DesignModel:           body.DesignModel,
		DesignClaudeConfigID:  body.DesignClaudeConfigID,
		DesignAgentServerID:   body.DesignAgentServerID,
		CodingModel:           body.CodingModel,
		CodingClaudeConfigID:  body.CodingClaudeConfigID,
		CodingAgentServerID:   body.CodingAgentServerID,
		Recurrence:            body.Recurrence,
		RecurTime:             body.RecurTime,
		RecurDays:             body.RecurDays,
		RecurTZ:               body.RecurTZ,
		AutoPush:              body.AutoPush,
		CommentOnPR:           body.CommentOnPR,
		IntervalMinutes:       body.IntervalMinutes,
		MaxConcurrentLaunches: body.MaxConcurrentLaunches,
		Active:                body.Active,
	}
	if err := h.svc.Upsert(r.Context(), mon); err != nil {
		// Normalize errors so the UI can map them to fields; the service
		// uses GO-style errors.New/fmt.Errorf so a substring match here
		// is the cheapest dispatch without an error code table.
		writeError(w, http.StatusBadRequest, "UPSERT_FAILED", err.Error())
		return
	}
	// Re-fetch by project to surface server-side defaults (comment_posted
	// flip on update, id, created_at, etc.).
	out, err := h.svc.GetByProject(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "RELOAD_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// Delete removes the monitor row. Idempotent: a 2nd call returns 204.
func (h *IssueMonitorHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "MISSING_ID", "project id missing")
		return
	}
	if err := h.svc.DeleteByProject(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// previewResponse wraps the Test handler's result with a Count so the UI
// can render "命中 5 条" without re-counting. Mirrors platform.Issue
// directly (no slim shape) so the modal can show labels + state + URL
// without a second round-trip.
type previewResponse struct {
	Issues []platform.Issue `json:"issues"`
	Count  int              `json:"count"`
}

// Test previews up to 5 matching Issues without importing any
// requirement. The UI surfaces this as a "🧪 测试匹配" button that lets
// the operator verify the label spelling before turning the monitor on.
func (h *IssueMonitorHandler) Test(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "MISSING_ID", "project id missing")
		return
	}
	issues, err := h.svc.Test(r.Context(), id)
	if err != nil {
		// 499 when the client cancels (most common during a fast
		// project-switch); 4xx for the usual "no config / empty label /
		// resolve failure" cases.
		if errors.Is(err, context.Canceled) {
			writeError(w, 499, "CLIENT_CANCELLED", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "TEST_FAILED", err.Error())
		return
	}
	if issues == nil {
		issues = []platform.Issue{}
	}
	writeJSON(w, http.StatusOK, previewResponse{Issues: issues, Count: len(issues)})
}

// ListActive renders all active monitors regardless of project. Used by
// /api/issue-monitors/active for ops visibility.
func (h *IssueMonitorHandler) ListActive(w http.ResponseWriter, r *http.Request) {
	mons, err := h.svc.ListAllActive(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	if mons == nil {
		mons = []model.IssueMonitor{}
	}
	writeJSON(w, http.StatusOK, service.IssueMonitorListResponse{Items: mons})
}
