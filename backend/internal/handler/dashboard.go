package handler

import (
	"net/http"

	"github.com/novaworkbench/backend/internal/middleware"
	"github.com/novaworkbench/backend/internal/service"
)

type DashboardHandler struct {
	svc *service.ProjectService
}

func NewDashboardHandler(svc *service.ProjectService) *DashboardHandler {
	return &DashboardHandler{svc: svc}
}

func (h *DashboardHandler) Dashboard(w http.ResponseWriter, r *http.Request) {
	var userID string
	isAdmin := true
	if u := middleware.CurrentUser(r); u != nil {
		userID = u.UserID
		isAdmin = u.IsAdmin
	}
	data, err := h.svc.DashboardForUser(userID, isAdmin)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, data)
}
