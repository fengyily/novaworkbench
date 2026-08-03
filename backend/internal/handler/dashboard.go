package handler

import (
	"net/http"

	"github.com/novaworkbench/backend/internal/service"
)

type DashboardHandler struct {
	svc *service.ProjectService
}

func NewDashboardHandler(svc *service.ProjectService) *DashboardHandler {
	return &DashboardHandler{svc: svc}
}

func (h *DashboardHandler) Dashboard(w http.ResponseWriter, r *http.Request) {
	data, err := h.svc.Dashboard()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, data)
}
