package handler

import (
	"errors"
	"net/http"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// ClaudeConfigHandler exposes the multi-config CRUD + activation API for the
// Claude CLI configuration. The auth token is never returned in full — only a
// masked preview + whether one is set.
type ClaudeConfigHandler struct {
	svc *service.ClaudeConfigService
}

func NewClaudeConfigHandler(svc *service.ClaudeConfigService) *ClaudeConfigHandler {
	return &ClaudeConfigHandler{svc: svc}
}

// toItem builds the masked API shape from a model. The auth token is replaced
// by a set-flag + preview; models/default_model pass through verbatim.
func toItem(c model.ClaudeConfig) model.ClaudeConfigItem {
	models := c.Models
	if models == nil {
		models = []string{}
	}
	return model.ClaudeConfigItem{
		ID:                c.ID,
		Name:              c.Name,
		BaseURL:           c.BaseURL,
		AuthTokenSet:      c.AuthToken != "",
		AuthTokenPreview:  service.MaskToken(c.AuthToken),
		Models:            models,
		DefaultModel:      c.DefaultModel,
		IsActive:          c.IsActive,
		CreatedAt:         c.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:         c.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

// List returns every config (token masked).
// GET /api/settings/claude/configs
func (h *ClaudeConfigHandler) List(w http.ResponseWriter, r *http.Request) {
	configs, err := h.svc.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	items := make([]model.ClaudeConfigItem, 0, len(configs))
	for _, c := range configs {
		items = append(items, toItem(c))
	}
	writeJSON(w, http.StatusOK, items)
}

// Create adds a new config. The first config auto-activates.
// POST /api/settings/claude/configs  body: CreateClaudeConfigReq
func (h *ClaudeConfigHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req model.CreateClaudeConfigReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "请求格式错误: "+err.Error())
		return
	}
	c, err := h.svc.Create(req.Name, req.BaseURL, req.AuthToken, req.Models, req.DefaultModel)
	if err != nil {
		if errors.Is(err, service.ErrDefaultModelNotInList) {
			writeError(w, http.StatusBadRequest, "INVALID_MODEL", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "VALIDATION", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toItem(*c))
}

// Update edits a config. An empty auth_token keeps the existing secret; an
// omitted (null) models field leaves the list unchanged.
// PUT /api/settings/claude/configs/{id}  body: UpdateClaudeConfigReq
func (h *ClaudeConfigHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req model.UpdateClaudeConfigReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "请求格式错误: "+err.Error())
		return
	}
	c, err := h.svc.Update(r.PathValue("id"), req.Name, req.BaseURL, req.AuthToken, req.ClearToken, req.Models, req.DefaultModel)
	if err != nil {
		if errors.Is(err, service.ErrConfigNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
			return
		}
		if errors.Is(err, service.ErrDefaultModelNotInList) {
			writeError(w, http.StatusBadRequest, "INVALID_MODEL", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "VALIDATION", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toItem(*c))
}

// Delete removes a config. The active config cannot be deleted.
// DELETE /api/settings/claude/configs/{id}
func (h *ClaudeConfigHandler) Delete(w http.ResponseWriter, r *http.Request) {
	err := h.svc.Delete(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, service.ErrConfigNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
			return
		}
		if errors.Is(err, service.ErrCannotDeleteActive) {
			writeErrorSuggestion(w, http.StatusBadRequest, "ACTIVE_CONFIG",
				err.Error(), "请先在列表中将其他配置「设为生效」，再删除此配置。")
			return
		}
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// activateResponse is returned by Activate: the fresh config list + what model
// was pushed into every role (so the UI can toast the side effect).
type activateResponse struct {
	Configs      []model.ClaudeConfigItem `json:"configs"`
	RolesUpdated bool                     `json:"roles_updated"`
	AppliedModel string                   `json:"applied_model"`
}

// Activate marks a config as the single active one and pushes its default
// model into every role (same transaction). Returns the fresh list.
// POST /api/settings/claude/configs/{id}/activate
func (h *ClaudeConfigHandler) Activate(w http.ResponseWriter, r *http.Request) {
	applied, err := h.svc.Activate(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, service.ErrConfigNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	configs, err := h.svc.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	items := make([]model.ClaudeConfigItem, 0, len(configs))
	for _, c := range configs {
		items = append(items, toItem(c))
	}
	writeJSON(w, http.StatusOK, activateResponse{
		Configs:      items,
		RolesUpdated: true,
		AppliedModel: applied,
	})
}

// Active returns the active config's model list + default model for the
// role-settings UI. Returns null when no config is active.
// GET /api/settings/claude/configs/active
func (h *ClaudeConfigHandler) Active(w http.ResponseWriter, r *http.Request) {
	models, defaultModel, err := h.svc.ActiveModels()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if models == nil {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":        models,
		"default_model": defaultModel,
	})
}
