package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

type ProjectHandler struct {
	svc *service.ProjectService
}

func NewProjectHandler(svc *service.ProjectService) *ProjectHandler {
	return &ProjectHandler{svc: svc}
}

func (h *ProjectHandler) List(w http.ResponseWriter, r *http.Request) {
	projects, err := h.svc.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	if projects == nil {
		projects = []model.Project{}
	}
	writeJSON(w, http.StatusOK, projects)
}

func (h *ProjectHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, err := h.svc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *ProjectHandler) Add(w http.ResponseWriter, r *http.Request) {
	var req model.AddProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid JSON body")
		return
	}

	p, err := h.svc.Add(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "ADD_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *ProjectHandler) Remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := r.URL.Query()
	// delete_dir is the new param; purge is kept for backward compat.
	deleteDir := q.Get("delete_dir") == "true" || q.Get("purge") == "true"

	if err := h.svc.Remove(id, deleteDir); err != nil {
		msg := err.Error()
		switch {
		case strings.HasPrefix(msg, "PATH_OUT_OF_WORKSPACE"):
			writeError(w, http.StatusBadRequest, "PATH_OUT_OF_WORKSPACE", msg)
		case strings.HasPrefix(msg, "REMOVE_DIR_FAILED"):
			writeError(w, http.StatusInternalServerError, "REMOVE_DIR_FAILED", msg)
		default:
			writeError(w, http.StatusInternalServerError, "REMOVE_FAILED", msg)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "dir_deleted": deleteDir})
}

// Trash lists soft-deleted projects.
// GET /api/projects/trash
func (h *ProjectHandler) Trash(w http.ResponseWriter, r *http.Request) {
	projects, err := h.svc.ListTrash()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	if projects == nil {
		projects = []model.Project{}
	}
	writeJSON(w, http.StatusOK, projects)
}

// Restore re-clones a soft-deleted project's directory and un-deletes it.
// POST /api/projects/{id}/restore
func (h *ProjectHandler) Restore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, err := h.svc.Restore(id)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.HasPrefix(msg, "NO_REMOTE"):
			writeError(w, http.StatusBadRequest, "NO_REMOTE", msg)
		case strings.HasPrefix(msg, "DIR_EXISTS"):
			writeError(w, http.StatusConflict, "DIR_EXISTS", msg)
		case strings.HasPrefix(msg, "RESTORE_FAILED"):
			writeError(w, http.StatusInternalServerError, "RESTORE_FAILED", msg)
		default:
			writeError(w, http.StatusInternalServerError, "RESTORE_FAILED", msg)
		}
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// UpdatePlatform binds a platform token to a project.
// PATCH /api/projects/{id}/platform  body: {platform_type, platform_token_id}
func (h *ProjectHandler) UpdatePlatform(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		PlatformType    string `json:"platform_type"`
		PlatformTokenID string `json:"platform_token_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	if err := h.svc.UpdatePlatformConfig(id, req.PlatformType, req.PlatformTokenID); err != nil {
		writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", err.Error())
		return
	}
	p, _ := h.svc.Get(id)
	writeJSON(w, http.StatusOK, p)
}
