package handler

import (
	"net/http"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

type ACLHandler struct {
	svc *service.ACLService
}

func NewACLHandler(svc *service.ACLService) *ACLHandler {
	return &ACLHandler{svc: svc}
}

// ---- Users ---------------------------------------------------------------

// ListUsers GET /api/acl/users
func (h *ACLHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.svc.ListUsers()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	if users == nil {
		users = []model.User{}
	}
	// Fetch full profiles (with role+project IDs) — GetUser already hydrates.
	out := make([]model.User, 0, len(users))
	for _, u := range users {
		if full, err := h.svc.GetUser(u.ID); err == nil {
			out = append(out, *full)
		} else {
			out = append(out, u)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateUser POST /api/acl/users
func (h *ACLHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	var req model.CreateUserRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	u, err := h.svc.CreateUser(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "CREATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

// GetUser GET /api/acl/users/{id}
func (h *ACLHandler) GetUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	u, err := h.svc.GetUser(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// UpdateUser PUT /api/acl/users/{id}
func (h *ACLHandler) UpdateUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req model.UpdateUserRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	u, err := h.svc.UpdateUser(id, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "UPDATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// DeleteUser DELETE /api/acl/users/{id}
func (h *ACLHandler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.svc.DeleteUser(id); err != nil {
		writeError(w, http.StatusBadRequest, "DELETE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// AssignProjects PUT /api/acl/users/{id}/projects  body: {project_ids:[]}
// Convenience endpoint that only reassigns projects (the full PUT also covers
// this, but exposing it standalone keeps the UI simple).
func (h *ACLHandler) AssignProjects(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		ProjectIDs []string `json:"project_ids"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	if err := h.svc.AssignProjects(id, req.ProjectIDs); err != nil {
		writeError(w, http.StatusBadRequest, "ASSIGN_FAILED", err.Error())
		return
	}
	u, err := h.svc.GetUser(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// ---- ACL Roles ------------------------------------------------------------

// ListRoles GET /api/acl/roles
func (h *ACLHandler) ListRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := h.svc.ListACLRoles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	if roles == nil {
		roles = []model.ACLRole{}
	}
	writeJSON(w, http.StatusOK, roles)
}

// CreateRole POST /api/acl/roles
func (h *ACLHandler) CreateRole(w http.ResponseWriter, r *http.Request) {
	var req model.CreateACLRoleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	role, err := h.svc.CreateACLRole(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "CREATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, role)
}

// GetRole GET /api/acl/roles/{id}
func (h *ACLHandler) GetRole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	role, err := h.svc.GetACLRole(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, role)
}

// UpdateRole PUT /api/acl/roles/{id}
func (h *ACLHandler) UpdateRole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req model.UpdateACLRoleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	role, err := h.svc.UpdateACLRole(id, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "UPDATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, role)
}

// DeleteRole DELETE /api/acl/roles/{id}
func (h *ACLHandler) DeleteRole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.svc.DeleteACLRole(id); err != nil {
		writeError(w, http.StatusBadRequest, "DELETE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---- Permissions ---------------------------------------------------------

// ListPermissions GET /api/acl/permissions
func (h *ACLHandler) ListPermissions(w http.ResponseWriter, r *http.Request) {
	perms, err := h.svc.ListPermissions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	if perms == nil {
		perms = []model.Permission{}
	}
	writeJSON(w, http.StatusOK, perms)
}
