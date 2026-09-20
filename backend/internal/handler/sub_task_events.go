package handler

import (
	"fmt"
	"log"
	"net/http"
	"time"
)

// StreamSubTasks is the per-requirement SSE endpoint that fans out a
// "refresh" signal whenever any sub_tasks row for the requirement changes
// status (MarkRunning / Finish / FinishForSession / MarkStopped). The
// payload stays minimal — the frontend re-fetches GET /api/requirements/
// {id}/sub-tasks on every signal so the SubTaskPanel stays consistent with
// any column it depends on.
//
// Heartbeat: a ": keepalive" SSE comment every 15s so an idle connection
// doesn't get torn down by reverse proxies (nginx default proxy_read_timeout
// is 60s). The actual sub-task change signal travels on the dedicated chan
// delivered by SubTaskEventHub.Subscribe.
//
// Uses http.NewResponseController (Go 1.22) instead of w.(http.Flusher)
// type-assertion so the Flush call penetrates the middleware.Logger
// wrappedWriter wrapper — the assertion fails on a wrapped ResponseWriter
// that doesn't implement Flusher, returning a spurious 500. This mirrors
// the existing streamJobSSE pump in sse.go.
//
// GET /api/requirements/{id}/sub-tasks/stream
func (h *WizardHandler) StreamSubTasks(w http.ResponseWriter, r *http.Request) {
	if h.subTaskSvc == nil || h.subTaskSvc.Events() == nil {
		http.Error(w, "sub-task event hub not initialized", http.StatusServiceUnavailable)
		return
	}
	reqID := r.PathValue("id")
	if reqID == "" {
		http.Error(w, "requirement_id required", http.StatusBadRequest)
		return
	}

	// ResponseController penetrates middleware wrappers (Logger's
	// wrappedWriter) to reach the underlying net/http Flusher.
	rc := http.NewResponseController(w)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := h.subTaskSvc.Events().Subscribe(reqID)
	defer h.subTaskSvc.Events().Unsubscribe(reqID, ch)

	// Hello frame so the client knows the connection is live and the
	// server's reqID echoes back what the URL said (debug aid).
	fmt.Fprintf(w, "data: {\"type\":\"hello\",\"req_id\":\"%s\"}\n\n", reqID)
	rc.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	log.Printf("[sub-task-event] %s: SSE stream opened", reqID)
	defer log.Printf("[sub-task-event] %s: SSE stream closed", reqID)

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			rc.Flush()
		case <-ch:
			// A single change frame — frontend re-fetches on receipt.
			if _, err := fmt.Fprint(w, "data: {\"type\":\"changed\"}\n\n"); err != nil {
				return
			}
			rc.Flush()
		}
	}
}