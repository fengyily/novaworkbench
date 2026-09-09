package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/novaworkbench/backend/internal/middleware"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

type ProjectHandler struct {
	svc     *service.ProjectService
	scanner *service.ScannerService
}

func NewProjectHandler(svc *service.ProjectService, scanner *service.ScannerService) *ProjectHandler {
	return &ProjectHandler{svc: svc, scanner: scanner}
}

func (h *ProjectHandler) List(w http.ResponseWriter, r *http.Request) {
	var userID string
	isAdmin := true
	if u := middleware.CurrentUser(r); u != nil {
		userID = u.UserID
		isAdmin = u.IsAdmin
	}
	projects, err := h.svc.ListForUser(userID, isAdmin)
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
	var userID string
	isAdmin := true
	if u := middleware.CurrentUser(r); u != nil {
		userID = u.UserID
		isAdmin = u.IsAdmin
	}
	// Non-admins can only fetch projects assigned to them (prevents ID
	// enumeration). Admins / the auth-bypass pass through.
	if !isAdmin && userID != "" {
		ok, err := h.svc.CanAccess(userID, false, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "project not found")
			return
		}
	}
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
		msg := err.Error()
		switch {
		case strings.HasPrefix(msg, "TOKEN_NOT_FOUND"):
			writeError(w, http.StatusBadRequest, "TOKEN_NOT_FOUND", msg)
		case strings.HasPrefix(msg, "PLATFORM_MISMATCH"):
			writeError(w, http.StatusBadRequest, "PLATFORM_MISMATCH", msg)
		default:
			writeError(w, http.StatusBadRequest, "ADD_FAILED", msg)
		}
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
		case strings.HasPrefix(msg, "TOKEN_NOT_FOUND"):
			writeError(w, http.StatusBadRequest, "TOKEN_NOT_FOUND", msg)
		case strings.HasPrefix(msg, "PLATFORM_MISMATCH"):
			writeError(w, http.StatusBadRequest, "PLATFORM_MISMATCH", msg)
		case strings.HasPrefix(msg, "RESTORE_FAILED"):
			writeError(w, http.StatusInternalServerError, "RESTORE_FAILED", msg)
		default:
			writeError(w, http.StatusInternalServerError, "RESTORE_FAILED", msg)
		}
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// Purge permanently removes a soft-deleted project (and its on-disk
// directory if still present) and all its child rows. Only callable on
// projects already in the trash — the service refuses active projects
// with NOT_IN_TRASH so a misclick on an active row can't wipe data.
//
// DELETE /api/projects/{id}/purge
func (h *ProjectHandler) Purge(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.svc.Purge(id); err != nil {
		msg := err.Error()
		switch {
		case strings.HasPrefix(msg, "NOT_IN_TRASH"):
			writeError(w, http.StatusBadRequest, "NOT_IN_TRASH", msg)
		case strings.HasPrefix(msg, "PROJECT_NOT_FOUND"), strings.Contains(msg, "project not found"):
			writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", msg)
		case strings.HasPrefix(msg, "PATH_OUT_OF_WORKSPACE"):
			writeError(w, http.StatusBadRequest, "PATH_OUT_OF_WORKSPACE", msg)
		case strings.HasPrefix(msg, "REMOVE_DIR_FAILED"):
			writeError(w, http.StatusInternalServerError, "REMOVE_DIR_FAILED", msg)
		default:
			writeError(w, http.StatusInternalServerError, "PURGE_FAILED", msg)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "purged"})
}

// UpdateBasicInfo edits the user-editable core fields of a project: display
// name, remote URL, project type, and local filesystem path. All four are
// optional in the request body — omitted fields are left unchanged, so the
// caller can send a partial edit without re-uploading the rest. Whitespace
// is trimmed server-side; an empty name or path is rejected with 400.
//
// PATCH /api/projects/{id}  body: {name?, remote_url?, project_type?, local_path?}
func (h *ProjectHandler) UpdateBasicInfo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name        *string `json:"name"`
		RemoteURL   *string `json:"remote_url"`
		ProjectType *string `json:"project_type"`
		LocalPath   *string `json:"local_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid JSON body")
		return
	}

	// Read the current row so partial patches can reuse unchanged fields.
	current, err := h.svc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", err.Error())
		return
	}

	name := current.Name
	if req.Name != nil {
		name = *req.Name
	}
	remoteURL := current.RemoteURL
	if req.RemoteURL != nil {
		remoteURL = *req.RemoteURL
	}
	projectType := current.ProjectType
	if req.ProjectType != nil {
		projectType = *req.ProjectType
	}
	localPath := current.LocalPath
	if req.LocalPath != nil {
		localPath = *req.LocalPath
	}

	if err := h.svc.UpdateBasicInfo(id, name, remoteURL, projectType, localPath); err != nil {
		msg := err.Error()
		switch {
		case strings.HasPrefix(msg, "PROJECT_NOT_FOUND"):
			writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", msg)
		case strings.HasPrefix(msg, "INVALID_NAME"):
			writeError(w, http.StatusBadRequest, "INVALID_NAME", msg)
		case strings.HasPrefix(msg, "INVALID_LOCAL_PATH"):
			writeError(w, http.StatusBadRequest, "INVALID_LOCAL_PATH", msg)
		case strings.HasPrefix(msg, "DUPLICATE_LOCAL_PATH"):
			writeError(w, http.StatusConflict, "DUPLICATE_LOCAL_PATH", msg)
		default:
			writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", msg)
		}
		return
	}

	p, _ := h.svc.Get(id)
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

// UpdateDescription saves a manually-edited project description and locks it
// from automatic regeneration.
// PUT /api/projects/{id}/description  body: {description}
func (h *ProjectHandler) UpdateDescription(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	if utf8.RuneCountInString(req.Description) > 500 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "简介不能超过 500 字")
		return
	}
	if err := h.svc.UpdateDescription(id, req.Description); err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", err.Error())
		return
	}
	p, _ := h.svc.Get(id)
	writeJSON(w, http.StatusOK, p)
}

// RegenerateDescription clears the manual lock and regenerates the AI summary
// from the current CLAUDE.md on demand.
// POST /api/projects/{id}/description/regenerate
func (h *ProjectHandler) RegenerateDescription(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := h.scanner.RegenerateDescription(id); err != nil {
		writeError(w, http.StatusInternalServerError, "REGENERATE_FAILED", err.Error())
		return
	}
	p, _ := h.svc.Get(id)
	writeJSON(w, http.StatusOK, p)
}

// BackfillDescriptions generates a description for every project that lacks one
// and isn't manually locked. Returns per-outcome counts.
// POST /api/projects/descriptions/backfill
func (h *ProjectHandler) BackfillDescriptions(w http.ResponseWriter, r *http.Request) {
	updated, skipped, failed, err := h.scanner.BackfillDescriptions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "BACKFILL_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{
		"updated": updated,
		"skipped": skipped,
		"failed":  failed,
	})
}
