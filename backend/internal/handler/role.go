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
		// First gate: model must appear in SOME config's list (any platform).
		// ModelInAnyList returns true on lookup error and on empty DB, so a
		// transient failure here cannot block the save.
		if ok, werr := h.ccfg.ModelInAnyList(req.Model); werr == nil && !ok {
			resp.Warning = "模型不在任何 Claude 配置的模型列表中"
		} else if werr == nil {
			// Second gate (soft): warn when the model is in a non-active config.
			// Runtime still uses the active config's base URL / auth token, so
			// the user should confirm the active gateway actually serves this model.
			if inActive, _ := h.ccfg.ModelInActiveList(req.Model); !inActive {
				resp.Warning = "模型不在当前生效配置的模型列表中（运行时仍走生效配置的网关，请确认网关支持该模型）"
			}
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
