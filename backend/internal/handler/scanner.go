package handler

import (
	"net/http"

	"github.com/novaworkbench/backend/internal/service"
)

type ScannerHandler struct {
	svc *service.ScannerService
}

func NewScannerHandler(svc *service.ScannerService) *ScannerHandler {
	return &ScannerHandler{svc: svc}
}

func (h *ScannerHandler) Scan(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if projectID == "" {
		writeError(w, 400, "INVALID", "project id is required")
		return
	}

	result, err := h.svc.Scan(projectID)
	if err != nil {
		writeError(w, 500, "SCAN_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, result)
}
