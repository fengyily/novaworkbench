package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/novaworkbench/backend/internal/middleware"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// RequirementDirHandler is the HTTP surface for the directories a user has
// authorized Claude to read for one requirement. Routes (registered in
// main.go):
//
//	POST   /api/requirements/{id}/dirs            — grant a directory
//	GET    /api/requirements/{id}/dirs            — list the grants
//	DELETE /api/requirements/{id}/dirs/{dir_id}    — revoke a grant
//
// This is the directory-shaped sibling of AttachmentHandler. An attachment is
// an uploaded file that NovaWorkbench copies into its own store; a granted
// directory stays where it is and is handed to the claude session via
// `--add-dir`, which is what unblocks the plan-mode stages: a read of a path
// outside the session's working directory needs a permission decision, and a
// non-interactive run has nobody to make it.
type RequirementDirHandler struct {
	svc *service.RequirementDirService
}

// NewRequirementDirHandler builds the handler.
func NewRequirementDirHandler(svc *service.RequirementDirService) *RequirementDirHandler {
	return &RequirementDirHandler{svc: svc}
}

// Add — POST /api/requirements/{id}/dirs.
//
// JSON body: {"path": "<absolute path or ~/...>", "label": "<optional note>"}.
//
// The path is normalized (~ expansion, absolute + clean) and validated
// (must exist, must be a directory, must not be the filesystem root)
// before anything is written. Granting the same directory twice returns the
// existing row rather than creating a duplicate.
func (h *RequirementDirHandler) Add(w http.ResponseWriter, r *http.Request) {
	reqID := r.PathValue("id")
	if strings.TrimSpace(reqID) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUIREMENT_ID", "requirement id missing")
		return
	}
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "DIRS_DISABLED", "目录授权功能未启用")
		return
	}

	var body struct {
		Path  string `json:"path"`
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	if strings.TrimSpace(body.Path) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_PATH", "请填写目录路径")
		return
	}
	if len(body.Label) > service.MaxRequirementDirLabel {
		writeError(w, http.StatusBadRequest, "LABEL_TOO_LONG",
			fmt.Sprintf("说明 ≤ %d 字", service.MaxRequirementDirLabel))
		return
	}

	addedBy := ""
	if u := middleware.CurrentUser(r); u != nil {
		addedBy = u.UserID
	}

	d, err := h.svc.Add(reqID, body.Path, body.Label, addedBy)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrPathNotFound):
			writeError(w, http.StatusBadRequest, "PATH_NOT_FOUND",
				"目录不存在:"+err.Error())
		case errors.Is(err, service.ErrNotADirectory):
			writeError(w, http.StatusBadRequest, "NOT_A_DIRECTORY",
				"该路径不是目录 —— 请指定一个目录（文件请用附件上传）")
		case errors.Is(err, service.ErrRootPath):
			writeError(w, http.StatusBadRequest, "ROOT_PATH_REJECTED",
				"不能授权文件系统根目录 / —— 请指定更具体的子目录")
		default:
			writeError(w, http.StatusBadRequest, "ADD_DIR_FAILED", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

// requirementDirView is the list-time projection of a grant: the row plus
// whether the directory is still on disk. The grant is a path, so it can go
// stale (the directory was moved / unmounted) without the row changing —
// the UI shows a "missing" badge instead of silently offering a dead path.
//
// The CLI tolerates a missing --add-dir path (it ignores it), so a stale
// grant degrades to "no access", never to a failed run.
type requirementDirView struct {
	model.RequirementDir
	Exists bool `json:"exists"`
}

// List — GET /api/requirements/{id}/dirs. Returns every granted directory;
// an empty result is "[]", not null.
func (h *RequirementDirHandler) List(w http.ResponseWriter, r *http.Request) {
	reqID := r.PathValue("id")
	if h.svc == nil {
		writeJSON(w, http.StatusOK, []requirementDirView{})
		return
	}
	dirs, err := h.svc.List(reqID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	views := make([]requirementDirView, 0, len(dirs))
	for _, d := range dirs {
		views = append(views, requirementDirView{RequirementDir: d, Exists: service.Exists(d.Path)})
	}
	writeJSON(w, http.StatusOK, views)
}

// Delete — DELETE /api/requirements/{id}/dirs/{dir_id}. Revokes the grant.
// 404 when the grant does not exist or belongs to another requirement.
func (h *RequirementDirHandler) Delete(w http.ResponseWriter, r *http.Request) {
	reqID := r.PathValue("id")
	dirID := r.PathValue("dir_id")
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "DIRS_DISABLED", "目录授权功能未启用")
		return
	}
	if err := h.svc.Delete(reqID, dirID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "目录授权不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- shared helpers (package-level so the sub-task runner can use them
// without depending on WizardHandler) ---

// resolveAddDirs returns the directories granted to a requirement, ready to
// be passed to the claude CLI as `--add-dir`. Returns nil when the service
// isn't wired or nothing is granted, so callers can pass the result through
// unconditionally.
//
// A read failure (table missing on an old database, etc.) degrades to "no
// grants" rather than failing the run — the grant is an additive
// convenience, not a precondition for the stage.
func resolveAddDirs(svc *service.RequirementDirService, reqID string) []string {
	if svc == nil || strings.TrimSpace(reqID) == "" {
		return nil
	}
	paths, err := svc.Paths(reqID)
	if err != nil {
		return nil
	}
	return paths
}

// dirsManifestFor renders the granted directories as a Markdown block for
// prompt injection, so Claude knows the directories exist and that they were
// authorized for this requirement (rather than having to guess why a path
// outside the working directory is readable).
//
// Empty string when nothing is granted — callers append unconditionally.
func dirsManifestFor(svc *service.RequirementDirService, reqID string) string {
	if svc == nil || strings.TrimSpace(reqID) == "" {
		return ""
	}
	dirs, err := svc.List(reqID)
	if err != nil || len(dirs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 📂 已授权读取目录\n\n")
	b.WriteString("以下目录已通过 `--add-dir` 授权，可直接用 Read / Glob / Grep 读取，无需额外确认。\n\n")
	for _, d := range dirs {
		label := strings.TrimSpace(d.Label)
		if label != "" {
			fmt.Fprintf(&b, "- `%s` —— %s\n", d.Path, label)
		} else {
			fmt.Fprintf(&b, "- `%s`\n", d.Path)
		}
	}
	b.WriteString("\n需要了解目录结构时可用 Bash `ls`；优先在这些目录中查找证据。\n")
	return b.String()
}
