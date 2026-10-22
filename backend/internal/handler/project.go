package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
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
	log.Printf("[gitlab-debug] handler.project.Add req: remote_url=%q platform_type=%q platform_token_id=%q branch=%q local_path=%q",
		req.RemoteURL, req.PlatformType, req.PlatformTokenID, req.Branch, req.LocalPath)

	p, err := h.svc.Add(req)
	if err != nil {
		writeServiceError(w, err)
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
		writeServiceError(w, err)
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
// name, remote URL, project type, local filesystem path, and the project's
// configured main branch. All five are optional in the request body —
// omitted fields are left unchanged, so the caller can send a partial edit
// without re-uploading the rest. Whitespace is trimmed server-side; an
// empty name or path is rejected with 400.
//
// PATCH /api/projects/{id}  body: {name?, remote_url?, project_type?, local_path?, default_branch?}
func (h *ProjectHandler) UpdateBasicInfo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name          *string `json:"name"`
		RemoteURL     *string `json:"remote_url"`
		ProjectType   *string `json:"project_type"`
		LocalPath     *string `json:"local_path"`
		DefaultBranch *string `json:"default_branch"`
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
	defaultBranch := current.DefaultBranch
	if req.DefaultBranch != nil {
		defaultBranch = *req.DefaultBranch
	}

	if err := h.svc.UpdateBasicInfo(id, name, remoteURL, projectType, localPath, defaultBranch); err != nil {
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

// SyncNow fetches origin/<default_branch> into the project's local repo and
// refreshes the sync badge columns. It is a soft sync — does not enforce
// clean tree / HEAD-on-base / no-unpushed gates (those belong to the wizard's
// architect stage via SyncDesignBase). Reuses EnsureClonedAndSynced so a
// missing directory is auto-cloned and any fetch failure is still stamped to
// sync_status=error.
//
// POST /api/projects/{id}/sync
func (h *ProjectHandler) SyncNow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var userID string
	isAdmin := true
	if u := middleware.CurrentUser(r); u != nil {
		userID = u.UserID
		isAdmin = u.IsAdmin
	}
	// Non-admins can only sync projects assigned to them (prevents ID
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
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	logf := func(msg string) { log.Printf("[project-sync %s] %s", id, msg) }
	_, _ = h.svc.EnsureClonedAndSynced(ctx, id, logf) // result swallowed — service stamps sync_status
	p, err := h.svc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// SetStar sets (or clears) the calling user's star on a project. The star is
// a personal view preference — it pins the project into the "⭐ 星标项目"
// block above the paginated /projects list and sorts it first on the
// dashboard — so the route is deliberately not gated on project.manage;
// read-only members may star too. Access control is the CanAccess check
// below, which keeps a non-admin from probing project IDs they can't see.
//
// PUT /api/projects/{id}/star   body: {"starred": true}
func (h *ProjectHandler) SetStar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	u := middleware.CurrentUser(r)
	if u == nil || u.UserID == "" {
		// middleware.Auth normally rejects first; this is the backstop for the
		// auth-disabled bypass, where there is no principal to hang a star on.
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "请先登录")
		return
	}
	var req struct {
		Starred bool `json:"starred"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	if !u.IsAdmin {
		ok, err := h.svc.CanAccess(u.UserID, false, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "project not found")
			return
		}
	}
	if err := h.svc.SetStar(u.UserID, id, req.Starred); err != nil {
		if strings.HasPrefix(err.Error(), "PROJECT_NOT_FOUND") {
			writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "STAR_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "starred": req.Starred})
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

// SetCommitLangOverride updates the user-pinned language override that the
// wizard pipeline uses when generating commit messages, push summaries, and
// PR titles/bodies. Pass override="" to clear the pin and revert to the
// auto-detected value (or "en" when nothing has been detected yet).
//
// PUT /api/projects/{id}/commit-lang-override  body: {"override": "zh" | "en" | "mixed" | ""}
func (h *ProjectHandler) SetCommitLangOverride(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "缺少项目 ID")
		return
	}
	var body struct {
		Override string `json:"override"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体格式错误")
		return
	}
	p, err := h.svc.SetCommitLangOverride(id, body.Override)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.HasPrefix(msg, "INVALID_OVERRIDE"):
			writeError(w, http.StatusBadRequest, "INVALID_OVERRIDE", msg)
		case strings.HasPrefix(msg, "project not found"):
			writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", msg)
		default:
			writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", msg)
		}
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// PreviewCommitStyle renders the commit/PR convention block the server
// WOULD inject for a given (preset, custom, lang), without touching the DB.
// The overview card calls it on a 400ms debounce so the read-only preview
// shows the user the exact text that will reach the prompt — 前端不做本地
// 复刻，预览与注入共用 service.BuildCommitStyleRules 这一个来源，预览说谎
// 的唯一可能就被消掉了。
//
// POST /api/projects/commit-style/preview
// body: {"preset": "none|conventional|custom", "custom": "...", "lang": "zh|en|mixed"}
func (h *ProjectHandler) PreviewCommitStyle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Preset string `json:"preset"`
		Custom string `json:"custom"`
		Lang   string `json:"lang"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体格式错误")
		return
	}
	rules := service.BuildCommitStyleRules(body.Preset, body.Custom, body.Lang)
	writeJSON(w, http.StatusOK, map[string]any{
		"example":  rules.Example,
		"commit":   rules.Commit,
		"pr_title": rules.PRTitle,
		"pr_body":  rules.PRBody,
	})
}

// SetCommitStyle persists the 「提交风格」 card — language pin + convention
// preset + free-text supplements — in one PUT. All three fields are written
// together so the prompt-building paths never read a half-saved combination.
//
// PUT /api/projects/{id}/commit-style
// body: {"lang": "zh|en|mixed|", "preset": "none|conventional|custom", "custom": "..."}
func (h *ProjectHandler) SetCommitStyle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "缺少项目 ID")
		return
	}
	var body struct {
		Lang   string `json:"lang"`
		Preset string `json:"preset"`
		Custom string `json:"custom"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体格式错误")
		return
	}
	p, err := h.svc.SetCommitStyle(id, body.Lang, body.Preset, body.Custom)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.HasPrefix(msg, "INVALID_LANG"),
			strings.HasPrefix(msg, "INVALID_PRESET"),
			strings.HasPrefix(msg, "INVALID_CUSTOM"):
			writeError(w, http.StatusBadRequest, "INVALID_COMMIT_STYLE", msg)
		case strings.HasPrefix(msg, "project not found"):
			writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", msg)
		default:
			writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", msg)
		}
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// SetCommitPushConfig updates the project's commit-message sourcing
// strategy ("仅脚本"/"仅LLM"/"脚本优先"/"LLM优先") and the optional
// shell snippet whose stdout becomes the commit message in
// script_only / script_first / llm_first modes. The chosen mode is
// honoured by handler.mergeGenerate (strategy dispatcher) the next
// time the user submits a requirement without a manual commit_message.
//
// PUT /api/projects/{id}/commit-push-config
// body: {"mode": "script_only|llm_only|script_first|llm_first", "script": "echo ..."}
func (h *ProjectHandler) SetCommitPushConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "缺少项目 ID")
		return
	}
	var body struct {
		Mode   string `json:"mode"`
		Script string `json:"script"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体格式错误")
		return
	}
	p, err := h.svc.SetCommitPushConfig(id, body.Mode, body.Script)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.HasPrefix(msg, "INVALID_MODE"):
			writeError(w, http.StatusBadRequest, "INVALID_MODE", msg)
		case strings.HasPrefix(msg, "INVALID_SCRIPT"):
			writeError(w, http.StatusBadRequest, "INVALID_SCRIPT", msg)
		case strings.HasPrefix(msg, "project not found"):
			writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", msg)
		default:
			writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", msg)
		}
		return
	}
	writeJSON(w, http.StatusOK, p)
}
