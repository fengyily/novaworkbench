package handler

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

type KnowledgeHandler struct {
	svc *service.KnowledgeService
}

func NewKnowledgeHandler(svc *service.KnowledgeService) *KnowledgeHandler {
	return &KnowledgeHandler{svc: svc}
}

func (h *KnowledgeHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	projectID := q.Get("project_id")
	category := q.Get("category")
	sourceType := q.Get("source_type")
	search := q.Get("search")
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	items, total, err := h.svc.List(projectID, category, sourceType, search, limit, offset)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{"items": items, "total": total})
}

func (h *KnowledgeHandler) Get(w http.ResponseWriter, r *http.Request) {
	k, err := h.svc.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, 200, k)
}

func (h *KnowledgeHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req model.CreateKnowledgeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	k, err := h.svc.Create(req)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 201, k)
}

func (h *KnowledgeHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req model.CreateKnowledgeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	k, err := h.svc.Update(r.PathValue("id"), req)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, k)
}

func (h *KnowledgeHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.PathValue("id")); err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (h *KnowledgeHandler) ListForReview(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	items, err := h.svc.ListForReview(projectID)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, items)
}

func (h *KnowledgeHandler) BatchReview(w http.ResponseWriter, r *http.Request) {
	var req model.ReviewActionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	if err := h.svc.BatchReview(req); err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (h *KnowledgeHandler) Search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	projectID := r.URL.Query().Get("project_id")

	items, _, err := h.svc.List(projectID, "", "", q, 10, 0)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, items)
}
