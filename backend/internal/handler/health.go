package handler

import (
	"net/http"

	"github.com/novaworkbench/backend/internal/preflight"
)

// HealthHandler now also reports a dependency snapshot so a docker-compose
// healthcheck or load balancer can tell whether the AI pipeline is ready.
// `ready` is true when the Claude CLI is present (the gateway then has a
// binary to invoke). Node/npm/git/docker are advisory — they only affect
// whether the AI features / run-session feature work, not whether the API
// server itself is up.
type HealthHandler struct {
	registry *preflight.Registry
}

func NewHealthHandler(registry *preflight.Registry) *HealthHandler {
	return &HealthHandler{registry: registry}
}

func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	snapshot := h.registry.Snapshot()
	deps := map[string]bool{}
	ready := false
	for _, d := range snapshot {
		deps[d.Key] = d.Installed
		if d.Key == "claude" && d.Installed {
			ready = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"version": "0.1.0",
		"ready":   ready,
		"deps":    deps,
	})
}
