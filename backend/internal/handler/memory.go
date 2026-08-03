package handler

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

type MemoryHandler struct {
	svc *service.MemoryService
}

func NewMemoryHandler(svc *service.MemoryService) *MemoryHandler {
	return &MemoryHandler{svc: svc}
}

func (h *MemoryHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	projectID := q.Get("project_id")
	memType := q.Get("type")
	tags := q.Get("tags")
	search := q.Get("search")
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	items, total, err := h.svc.List(projectID, memType, tags, search, limit, offset)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{"items": items, "total": total})
}

func (h *MemoryHandler) Get(w http.ResponseWriter, r *http.Request) {
	m, err := h.svc.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, 200, m)
}

func (h *MemoryHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req model.CreateMemoryReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	if req.ProjectID == "" || req.Content == "" {
		writeError(w, 400, "INVALID", "project_id and content are required")
		return
	}
	m, err := h.svc.Create(req)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 201, m)
}

func (h *MemoryHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req model.CreateMemoryReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	m, err := h.svc.Update(r.PathValue("id"), req)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, m)
}

func (h *MemoryHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.PathValue("id")); err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}
