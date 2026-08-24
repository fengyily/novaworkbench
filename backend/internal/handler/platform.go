package handler

import (
	"encoding/json"
	"net/http"
	"strings"

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

// Update edits an existing platform token's metadata + optional Git identity.
// The platform and the raw secret can't be changed in one shot here — secret
// rotation only happens when new_token is non-empty (keeps the existing PAT
// intact when the user only wants to fix the Git identity). Returns the
// updated row with the raw secret omitted.
// PUT /api/settings/tokens/{id}  body: {name, base_url, git_user_name?, git_user_email?, new_token?}
func (h *PlatformHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name         string `json:"name"`
		BaseURL      string `json:"base_url"`
		GitUserName  string `json:"git_user_name"`
		GitUserEmail string `json:"git_user_email"`
		NewToken     string `json:"new_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELDS", "name 不能为空")
		return
	}
	if err := h.svc.Update(id, req.Name, req.BaseURL, req.GitUserName, req.GitUserEmail, req.NewToken); err != nil {
		if strings.Contains(err.Error(), "token not found") {
			writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	tok, err := h.svc.Get(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	// Don't echo the raw secret back to the client.
	tok.Token = ""
	writeJSON(w, http.StatusOK, tok)
}
