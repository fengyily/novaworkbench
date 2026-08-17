package handler

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

type RequirementHandler struct {
	svc  *service.RequirementService
	llm  *llm.Gateway
	jobs *store.JobStore
}

func NewRequirementHandler(svc *service.RequirementService, llmGateway *llm.Gateway, jobs *store.JobStore) *RequirementHandler {
	return &RequirementHandler{svc: svc, llm: llmGateway, jobs: jobs}
}

func (h *RequirementHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	items, err := h.svc.List(q.Get("project_id"), q.Get("status"), q.Get("priority"), q.Get("sprint"))
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, items)
}

func (h *RequirementHandler) Get(w http.ResponseWriter, r *http.Request) {
	item, err := h.svc.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "NOT_FOUND", err.Error())
		return
	}
	h.healStaleJobs(item)
	writeJSON(w, 200, item)
}

// healStaleJobs clears job-id pointers on the requirement that reference jobs
// which are no longer live (evicted from the in-memory ring buffer, or finished
// — the goroutine normally clears these itself on every terminal path, but a
// server restart / crash between the DB write of the id and the clearing call
// leaves a stale pointer that wedges the frontend: the architect-design panel
// gates its "⏳ …" spinner on !!req.design_job_id and hides the retry button
// behind !req.design_job_id, so a stale id = a perpetual spinner with no way
// out. Reconciling against the JobStore on every Get self-heals it: the next
// page load / refresh drops the stale id and the UI recovers on its own.
func (h *RequirementHandler) healStaleJobs(req *model.Requirement) {
	if h.jobs == nil {
		return
	}
	type pending struct {
		val    string
		clear  func(id, jobID string) error
		field  *string
	}
	checks := []pending{
		{req.DesignJobID, h.svc.UpdateDesignJob, &req.DesignJobID},
		{req.AnalysisJobID, h.svc.UpdateAnalysisJob, &req.AnalysisJobID},
		{req.ApplyJobID, h.svc.UpdateApplyJob, &req.ApplyJobID},
	}
	for _, c := range checks {
		if c.val == "" {
			continue
		}
		if h.jobs.Live(c.val) {
			continue
		}
		if err := c.clear(req.ID, ""); err != nil {
			log.Printf("[requirement] failed to clear stale job id %s for %s: %v", c.val, req.ID, err)
			continue
		}
		*c.field = ""
	}
}

func (h *RequirementHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req model.CreateRequirementReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	if req.ProjectID == "" || req.Description == "" {
		writeError(w, 400, "INVALID", "project_id and description are required")
		return
	}
	// Title is no longer entered by the user — distill it from the requirement
	// content via the LLM. Fall back to the first line of the content if the
	// LLM is unavailable (e.g. claude CLI not installed) so creation never fails.
	if req.Title == "" {
		// Reorganize the raw, free-form content into structured Markdown AND
		// distill a title in a single LLM round, so the title and body stay
		// consistent and the content is transmitted once. Each half falls back
		// independently on failure — creation must not fail just because the
		// formatter is unavailable.
		markdown, title, err := h.llm.GenerateDescriptionAndTitle(req.Description)
		switch {
		case err != nil:
			log.Printf("[requirement] GenerateDescriptionAndTitle failed: %v — using raw content and fallback title", err)
			req.Title = fallbackTitle(req.Description)
		case markdown == "" || title == "":
			// Shouldn't happen (the gateway returns an error in these cases),
			// but guard against a partial result by filling the missing half.
			if markdown != "" {
				req.Description = markdown
			}
			if title != "" {
				req.Title = title
			} else {
				req.Title = fallbackTitle(req.Description)
			}
		default:
			req.Description = markdown
			req.Title = title
		}
	}
	item, err := h.svc.Create(req)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 201, item)
}

// fallbackTitle derives a short title from the requirement content when the LLM
// is unavailable: the first non-empty line, capped at 60 runes. The title is
// only a display label (the full intent lives in the description, which the
// analyst chat reads from the DB), so we keep it readable rather than truncating
// hard at 20 runes.
func fallbackTitle(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return "新需求"
	}
	first := content
	if idx := strings.IndexByte(content, '\n'); idx >= 0 {
		first = strings.TrimSpace(content[:idx])
	}
	r := []rune(first)
	if len(r) > 60 {
		first = string(r[:60]) + "..."
	}
	return first
}

func (h *RequirementHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req model.CreateRequirementReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	item, err := h.svc.Update(r.PathValue("id"), req)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, item)
}

func (h *RequirementHandler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	var req model.UpdateStatusReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	item, err := h.svc.UpdateStatus(r.PathValue("id"), req.Status)
	if err != nil {
		writeError(w, 400, "INVALID_STATUS", err.Error())
		return
	}
	writeJSON(w, 200, item)
}

func (h *RequirementHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.PathValue("id")); err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// Archive turns a finished ("done") requirement into a project knowledge-base
// entry (final requirement + design docs). Returns the created/updated
// knowledge row. The requirement status moves to "archived".
func (h *RequirementHandler) Archive(w http.ResponseWriter, r *http.Request) {
	kb, err := h.svc.Archive(r.PathValue("id"))
	if err != nil {
		writeError(w, 400, "ARCHIVE_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, kb)
}

// Unarchive reverses Archive: status returns to "done" and the knowledge entry
// produced by archiving is removed.
func (h *RequirementHandler) Unarchive(w http.ResponseWriter, r *http.Request) {
	item, err := h.svc.Unarchive(r.PathValue("id"))
	if err != nil {
		writeError(w, 400, "UNARCHIVE_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, item)
}

func (h *RequirementHandler) GetChatHistory(w http.ResponseWriter, r *http.Request) {
	messages, err := h.svc.GetRefinementChat(r.PathValue("id"))
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, messages)
}

func (h *RequirementHandler) SaveChatHistory(w http.ResponseWriter, r *http.Request) {
	var req struct{ Messages string `json:"messages"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	if err := h.svc.SaveRefinementChat(r.PathValue("id"), req.Messages); err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// ClearAnalysisSession clears the stored claude analyst session id for a
// requirement, so the next analyst-chat turn mints a fresh conversation instead
// of --resume-ing a broken or over-long one. The chat's "clear" action calls
// this so the user can recover from a wedged session without leaving the chat.
// The displayed chat messages are cleared separately by the caller.
func (h *RequirementHandler) ClearAnalysisSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, 400, "INVALID", "missing requirement id")
		return
	}
	if err := h.svc.UpdateAnalysisSession(id, ""); err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
