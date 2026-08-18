package handler

import (
	"encoding/json"
	"net/http"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

type RoleHandler struct {
	svc  *service.RoleService
	ccfg *service.ClaudeConfigService
}

func NewRoleHandler(svc *service.RoleService, ccfg *service.ClaudeConfigService) *RoleHandler {
	return &RoleHandler{svc: svc, ccfg: ccfg}
}

func (h *RoleHandler) List(w http.ResponseWriter, r *http.Request) {
	roles, err := h.svc.List()
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	if roles == nil {
		roles = []model.Role{}
	}
	writeJSON(w, 200, roles)
}

func (h *RoleHandler) Get(w http.ResponseWriter, r *http.Request) {
	role, err := h.svc.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, 200, role)
}

// roleUpdateResponse wraps the updated role plus an optional soft-validation
// warning. The warning is set when the chosen model is not in the active
// Claude config's model list (the value is still saved — the CLI will surface
// an error if the model id is invalid).
type roleUpdateResponse struct {
	Role    model.Role `json:"role"`
	Warning string     `json:"warning,omitempty"`
}

func (h *RoleHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req model.UpdateRoleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	role, err := h.svc.Update(r.PathValue("id"), req)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	resp := roleUpdateResponse{Role: *role}
	if req.Model != "" && h.ccfg != nil {
		ok, werr := h.ccfg.ModelInActiveList(req.Model)
		if werr == nil && !ok {
			resp.Warning = "模型不在当前配置的模型列表中"
		}
	}
	writeJSON(w, 200, resp)
}

// Reset restores a role's system prompt to the built-in default (model left
// untouched so the user's model choice persists).
func (h *RoleHandler) Reset(w http.ResponseWriter, r *http.Request) {
	role, err := h.svc.Reset(r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, 200, role)
}
