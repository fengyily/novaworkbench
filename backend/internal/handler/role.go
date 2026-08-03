package handler

import (
	"encoding/json"
	"net/http"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

type RoleHandler struct {
	svc *service.RoleService
}

func NewRoleHandler(svc *service.RoleService) *RoleHandler {
	return &RoleHandler{svc: svc}
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
	writeJSON(w, 200, role)
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
