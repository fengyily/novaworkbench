package handler

import (
	"encoding/json"
	"net/http"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

type PlatformHandler struct {
	svc *service.PlatformTokenService
}

func NewPlatformHandler(svc *service.PlatformTokenService) *PlatformHandler {
	return &PlatformHandler{svc: svc}
}

// List returns all platform tokens (without the raw secret).
// GET /api/settings/tokens
func (h *PlatformHandler) List(w http.ResponseWriter, r *http.Request) {
	tokens, err := h.svc.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if tokens == nil {
		tokens = []model.PlatformToken{}
	}
	writeJSON(w, http.StatusOK, tokens)
}

// Create adds a new platform token.
// POST /api/settings/tokens  body: {name, platform, base_url, token, git_user_name?, git_user_email?}
func (h *PlatformHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		Platform     string `json:"platform"`
		BaseURL      string `json:"base_url"`
		Token        string `json:"token"`
		GitUserName  string `json:"git_user_name"`
		GitUserEmail string `json:"git_user_email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	if req.Name == "" || req.Platform == "" || req.Token == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELDS", "name、platform、token 不能为空")
		return
	}
	if req.Platform != "github" && req.Platform != "gitlab" && req.Platform != "gitea" {
		writeError(w, http.StatusBadRequest, "INVALID_PLATFORM", "platform 必须是 github、gitlab 或 gitea")
		return
	}

	tok, err := h.svc.Create(req.Name, req.Platform, req.BaseURL, req.Token, req.GitUserName, req.GitUserEmail)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, tok)
}

// Delete removes a platform token.
// DELETE /api/settings/tokens/{id}
func (h *PlatformHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.svc.Delete(id); err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
