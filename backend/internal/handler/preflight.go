// Package handler — preflight exposes the dependency-check registry at three
// endpoints. The same JobStore + SSE pattern wizard.go:1550 uses to stream
// background work to the browser is reused here, so the frontend can show a
// live "正在安装 Claude CLI..." panel with each tool line appended.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/novaworkbench/backend/internal/preflight"
	"github.com/novaworkbench/backend/internal/store"
)

type PreflightHandler struct {
	registry *preflight.Registry
	jobs     *store.JobStore
}

func NewPreflightHandler(registry *preflight.Registry, jobs *store.JobStore) *PreflightHandler {
	return &PreflightHandler{registry: registry, jobs: jobs}
}

// ClaudeBin returns the active CLAUDE_BIN env (or "" if defaulting to PATH).
// Surfaced so the UI can show "claude → /custom/path" alongside the check.
func ClaudeBin() string { return os.Getenv("CLAUDE_BIN") }

// ---- GET /api/preflight ---------------------------------------------------

func (h *PreflightHandler) Snapshot(w http.ResponseWriter, r *http.Request) {
	// Re-check on every read (LookPath is essentially free) so a UI refresh
	// after a manual install or after the background EnsureAll goroutine
	// finishes picks up the new state without requiring a POST.
	h.registry.CheckAll(r.Context())
	writeJSON(w, 200, map[string]interface{}{
		"deps":        h.registry.Snapshot(),
		"claude_bin":  ClaudeBin(),
		"autoinstall": os.Getenv("NOVA_AUTOINSTALL") != "0",
	})
}

// ---- POST /api/preflight/install -----------------------------------------

type installReq struct {
	Key string `json:"key"`
}

func (h *PreflightHandler) Install(w http.ResponseWriter, r *http.Request) {
	var body installReq
	_ = decodeJSON(r, &body)
	if body.Key == "" {
		writeError(w, 400, "INVALID", "missing dep key")
		return
	}
	if h.registry.Lookup(body.Key) == nil {
		writeErrorSuggestion(w, 404, "UNKNOWN_DEP", "未知的依赖项: "+body.Key,
			"可用的 key: claude / node / npm / git / docker")
		return
	}

	// Create a JobStore job (ring-buffer cap 50 — the same one wizard/runner/
	// review use; preflight installs are infrequent so eviction isn't a
	// concern in practice). The id is returned immediately and the work
	// runs in a goroutine, identical to wizardH.StartCoding.
	job := h.jobs.Create("preflight-" + body.Key)
	writeJSON(w, 200, map[string]string{"job_id": job.ID})

	go h.runInstall(job, body.Key)
}

func (h *PreflightHandler) runInstall(job *store.Job, key string) {
	defer func() {
		// Defensive: a panic in the install goroutine would otherwise leak
		// the job in JobRunning state forever (the SSE subscriber would hang).
		if rec := recover(); rec != nil {
			log.Printf("[preflight] install panic for %s: %v", key, rec)
			job.Append(store.LogLine{Type: "error", Content: fmt.Sprintf("panic: %v", rec)})
			job.Finish(1, store.JobError)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	err := h.registry.Install(ctx, key, job)
	if err != nil {
		job.Append(store.LogLine{Type: "error", Content: fmt.Sprintf("[preflight] %s 安装失败: %v", key, err)})
		job.Finish(1, store.JobError)
		log.Printf("[preflight] install %s failed: %v", key, err)
		return
	}
	job.Finish(0, store.JobDone)
	log.Printf("[preflight] install %s completed", key)
}

// ---- GET /api/preflight/jobs/{id} ----------------------------------------
// JSON snapshot — mirrors wizardH.GetJob. Useful when the SSE stream is
// unavailable (page reload after the goroutine finished) and the job is
// still in the ring buffer.

func (h *PreflightHandler) GetJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := h.jobs.Get(id)
	if !ok {
		writeError(w, 404, "NOT_FOUND", "job not found (in-memory ring buffer evicts after 50 jobs)")
		return
	}
	lines, status, exitCode := job.Snapshot()
	writeJSON(w, 200, map[string]interface{}{
		"job_id":      job.ID,
		"status":      status,
		"exit_code":   exitCode,
		"log":         lines,
		"started_at":  job.StartedAt,
		"finished_at": job.FinishedAt,
	})
}

// ---- GET /api/preflight/jobs/{id}/stream ---------------------------------
// SSE stream — replays existing lines then pushes new ones until the job
// finishes. Mirrors wizardH.StreamJob (wizard.go:1550) byte-for-byte except
// for the prefix on the final job_done frame.

func (h *PreflightHandler) StreamJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, 400, "INVALID", "missing job id")
		return
	}
	job, ok := h.jobs.Get(id)
	if !ok {
		writeError(w, 404, "NOT_FOUND", "job not found")
		return
	}

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	ch, _ := job.Subscribe()
	defer job.Unsubscribe(ch)

	for {
		select {
		case <-r.Context().Done():
			return
		case line, open := <-ch:
			if !open {
				_, status, exitCode := job.Snapshot()
				doneData, _ := json.Marshal(map[string]interface{}{
					"type":      "job_done",
					"status":    string(status),
					"exit_code": exitCode,
				})
				fmt.Fprintf(w, "data: %s\n\n", string(doneData))
				rc.Flush()
				return
			}
			data, _ := json.Marshal(line)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			rc.Flush()
		}
	}
}
